package main

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// Auto mode's memory of the user's orders (contract §5.4): today's orders as small records, rebuilt from bot_orders
// at the first use of the Prague day and kept current by the scheduler's transitions (autoTrackLocked); and the auto
// pause triggers (§6). Everything here is guarded by botHub.mu.

// autoRec is one of the user's orders of the day as auto mode reads it (app and auto, any state).
type autoRec struct {
	id, source, eventID, arbKey string
	dry                         bool   // class dry (dry_run)
	state                       string // waiting, running, placed, stopped, outcome_unknown or cashed_out
	stake                       int64  // as bot_orders.stake: the ordered stakes while open, then the counted stake
	legs                        []storedLeg
	accepted                    bool  // the agent accepted it (read back from a row: it has a flow or a leg step)
	stuck                       bool  // its flow went stuck
	firstSeen                   int64 // the arb's first_detected_at (auto orders); 0 = unknown
	created, sent, ended        int64 // Unix ms; 0 = not yet
}

func (r *autoRec) open() bool { return r.state == orderWaiting || r.state == orderRunning }

// oneSided: an ended order with exactly one leg reported placed, or cashed out (contract §1).
func (r *autoRec) oneSided() bool {
	return r.state == "cashed_out" || (!r.open() && placedLegs(r.legs) == 1)
}

func placedLegs(legs []storedLeg) int {
	n := 0
	for _, l := range legs {
		if l.Placed != nil && *l.Placed {
			n++
		}
	}
	return n
}

// legCounts: the leg's stake counts for its bookmaker's day (book_day_stake): while its order is open, on
// outcome_unknown, or when the leg was placed (reported, else the order placed or cashed out).
func (r *autoRec) legCounts(l storedLeg) bool {
	switch {
	case r.open(), r.state == "outcome_unknown":
		return true
	case l.Placed != nil:
		return *l.Placed
	}
	return r.state == "placed" || r.state == "cashed_out"
}

// autoDay is a user's orders created in one Prague day [from, to) (Unix ms).
type autoDay struct {
	from, to int64
	recs     map[string]*autoRec
}

// autoDayFrom is the start of the current Prague day (Unix ms): the skip-log counts' day.
func (b *botHub) autoDayFrom() int64 {
	from, _ := pragueDayOf(b.now())
	return from.UnixMilli()
}

// autoDayLocked is userID's orders of today, rebuilt from bot_orders when the day is not loaded or is over. The
// rebuild runs fx's queued writes first and reads under b.dbMu, as overDayLocked does, so it sees every earlier
// change; the open orders' memory (accepted, stuck) wins over their rows. b.mu must be held.
func (b *botHub) autoDayLocked(userID int64, fx *effects) (*autoDay, error) {
	from, to := pragueDayOf(b.now())
	if d := b.auto.days[userID]; d != nil && d.from == from.UnixMilli() {
		return d, nil
	}
	d := &autoDay{from: from.UnixMilli(), to: to.UnixMilli(), recs: map[string]*autoRec{}}
	if b.acc != nil {
		b.dbMu.Lock()
		for _, w := range fx.writes {
			if err := w(); err != nil {
				log.Error().Err(err).Msg("bot database write failed")
			}
		}
		fx.writes = nil
		rows, err := b.acc.BotOrdersOfDay(userID, d.from, d.to)
		b.dbMu.Unlock()
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			d.recs[row.ID] = recOfRow(row)
		}
	}
	b.auto.days[userID] = d
	for _, o := range b.orders {
		if o.userID == userID {
			b.autoTrackLocked(o, o.state)
		}
	}
	return d, nil
}

// recOfRow is a stored order as a record.
func recOfRow(r accounts.BotOrderRow) *autoRec {
	var legs []storedLeg
	var info orderInfo
	json.Unmarshal([]byte(r.LegsJSON), &legs) // written by the hub; unreadable → none
	json.Unmarshal([]byte(r.InfoJSON), &info)
	stepped := slices.ContainsFunc(legs, func(l storedLeg) bool { return l.StepAt > 0 })
	return &autoRec{id: r.ID, source: rowSource(r.Source), eventID: r.EventID, arbKey: r.ArbKey, dry: r.DryRun, state: r.State,
		stake: r.Stake, legs: legs, accepted: r.SentAt > 0 && (r.FlowJSON != "" || stepped), stuck: flowIsStuck(r.FlowJSON),
		firstSeen: info.FirstDetectedAt, created: r.CreatedAt, sent: r.SentAt, ended: r.EndedAt}
}

// autoTrackLocked copies o, now in state, into its user's day when that day is loaded and o was created in it.
// b.mu must be held.
func (b *botHub) autoTrackLocked(o *botOrder, state string) {
	d := b.auto.days[o.userID]
	created := o.created.UnixMilli()
	if d == nil || created < d.from || created >= d.to {
		return
	}
	r := d.recs[o.id]
	if r == nil {
		r = &autoRec{id: o.id}
		d.recs[o.id] = r
	}
	r.source, r.eventID, r.arbKey, r.dry, r.state = o.sourceOrApp(), o.eventID, o.arbKey, o.dryRun, state
	r.legs = slices.Clone(o.legs)
	r.accepted = o.accepted
	r.stuck = r.stuck || o.stuckSeen || flowIsStuck(o.flow)
	r.firstSeen, r.created = o.info.FirstDetectedAt, created
	if !o.sent.IsZero() {
		r.sent = o.sent.UnixMilli()
	}
	r.stake = o.stake
	if botEvents[state] { // final
		r.stake = o.countedStake(state)
		r.ended = b.now().UnixMilli()
	}
}

// lastRec is the newest record that keep accepts (by creation, then id); nil = none.
func (d *autoDay) lastRec(keep func(*autoRec) bool) *autoRec {
	var last *autoRec
	for _, r := range d.recs {
		if keep(r) && (last == nil || r.created > last.created || (r.created == last.created && r.id > last.id)) {
			last = r
		}
	}
	return last
}

// ── auto pause (contract §6) ──────────────────────────────────────────────────

// autoStreakStops is how many auto orders in a row may end stopped after accepted before auto pauses.
const autoStreakStops = 5

// autoOrderEndedLocked applies the auto pause triggers of o ending in state: outcome_unknown, a one-sided result, and
// the stop streak of auto orders (a placed auto order resets it). b.mu must be held.
func (b *botHub) autoOrderEndedLocked(o *botOrder, state string, fx *effects) {
	rec := autoRec{state: state, legs: o.legs}
	switch {
	case state == "outcome_unknown":
		b.autoPauseLocked(o.userID, "Outcome unknown · order "+shortID(o.id), o.id, o.codeID, o.dryRun, fx)
	case rec.oneSided():
		b.autoPauseLocked(o.userID, "One leg placed · order "+shortID(o.id), o.id, o.codeID, o.dryRun, fx)
	}
	if !o.isAuto() {
		return
	}
	switch {
	case state == "placed":
		b.auto.streak[o.userID] = 0
	case state == "stopped" && o.accepted:
		b.auto.streak[o.userID]++
		if b.auto.streak[o.userID] >= autoStreakStops {
			b.auto.streak[o.userID] = 0
			b.autoPauseLocked(o.userID, "5 orders in a row stopped", "", 0, o.dryRun, fx)
		}
	}
}

// autoStuckLocked applies the stuck trigger: o's first flow with stuck not null. b.mu must be held.
func (b *botHub) autoStuckLocked(o *botOrder, fx *effects) {
	b.autoPauseLocked(o.userID, "Stuck order · order "+shortID(o.id), o.id, o.codeID, o.dryRun, fx)
}

// autoPauseLocked pauses userID's auto mode with reason, for orderID ("" = the streak) of agent codeID, unless the
// mode is off or the settings are not loaded. Already paused: the order id joins pausedOrders (at most 20, the oldest
// dropped) and the reason stays. A new pause raises the "Auto paused" attention alert (dry runs too, "· Dry run"
// added). b.mu must be held.
func (b *botHub) autoPauseLocked(userID int64, reason, orderID string, codeID int64, dry bool, fx *effects) {
	st, ok := b.settings[userID]
	if !ok {
		return
	}
	if _, mode := b.autoModeLocked(userID); mode == autoOff {
		return
	}
	ids := pausedOrdersOf(st.AutoPausedOrders)
	if st.AutoPausedReason != "" {
		if orderID == "" || slices.Contains(ids, orderID) {
			return
		}
		ids = append(ids, orderID)
		if len(ids) > maxPausedOrders {
			ids = ids[len(ids)-maxPausedOrders:]
		}
		st.AutoPausedOrders = mustJSON(ids)
		b.saveSettingsLocked(userID, st, fx)
		fx.notify(userID)
		return
	}
	ids = []string{}
	if orderID != "" {
		ids = append(ids, orderID)
	}
	st.AutoPausedReason, st.AutoPausedOrders = reason, mustJSON(ids)
	b.saveSettingsLocked(userID, st, fx)
	fx.notify(userID)
	log.Warn().Int64("user", userID).Str("reason", reason).Msg("bot auto paused")
	if b.alerts == nil {
		return
	}
	detail := reason
	if dry {
		detail = joinDetail(reason, "Dry run")
	}
	b.raiseLocked(userID, botAlert{Kind: alertAttention, Severity: severityAttention, AgentID: codeID, OrderID: orderID,
		Title: titleAutoPaused, Detail: detail}, nil, fx)
}

// autoCooldown is how long a match rests after an auto order on it stopped after it was accepted.
const autoCooldown = 2 * time.Minute

// autoTries is the most auto orders of one class on one arb key a day.
const autoTries = 3
