package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

const (
	// orderMemory is how long a running order waits for its final event before it ends outcome_unknown.
	orderMemory = 10 * time.Minute
	// waitExpiry is how long an order may wait for a free agent before it ends stopped ("Expired · …").
	waitExpiry = 20 * time.Second
)

const (
	orderWaiting = "waiting"
	orderRunning = "running"
)

// botLeg is one leg of an order, as the app sends it.
type botLeg struct {
	Bookmaker       string   `json:"bookmaker"`
	MatchURL        string   `json:"match_url"`
	SourceID        string   `json:"source_id"`
	SourceMarketRef string   `json:"source_market_ref"`
	MarketName      string   `json:"market_name"`
	Label           string   `json:"label"`
	Odds            float64  `json:"odds"`
	Stake           float64  `json:"stake"`
	OutcomeID       string   `json:"outcome_id"` // the leg's canonical side (OVER, UNDER, HOME, a team …); "" from an older app
	FairOdds        *float64 `json:"fair_odds"`  // consensus fair odds of this leg (rule 2); nil = unknown
}

// botOrderIn is the POST /api/me/bot/orders body. match, market, live and profit_pct are for display
// only; market_type, line, is_live, sport and rounding go to the agent (its rules), all optional.
type botOrderIn struct {
	ArbKey     string   `json:"arb_key"`
	Legs       []botLeg `json:"legs"`
	Match      string   `json:"match"`
	Market     string   `json:"market"`
	Live       bool     `json:"live"`
	ProfitPct  float64  `json:"profit_pct"`
	MarketType string   `json:"market_type"`
	Line       *float64 `json:"line"` // nil: the market has no line
	IsLive     bool     `json:"is_live"`
	Sport      string   `json:"sport"`
	Rounding   *int     `json:"rounding"` // 1, 10 or 100; nil = 1
}

// agentLeg is a leg as the agent receives it: the app's leg plus the window the gateway reserved.
type agentLeg struct {
	botLeg
	WindowID string `json:"window_id"`
}

// botOrderMsg is the order the agent's bot socket receives ({"type":"order","order":…}). dry_run,
// max_leg and rules_on are the user's settings when it is sent.
type botOrderMsg struct {
	OrderID    string     `json:"order_id"`
	ArbKey     string     `json:"arb_key"`
	DryRun     bool       `json:"dry_run"`
	Legs       []agentLeg `json:"legs"`
	MarketType string     `json:"market_type"`
	Line       *float64   `json:"line"`
	IsLive     bool       `json:"is_live"`
	Sport      string     `json:"sport"`
	Match      string     `json:"match"`
	Rounding   int        `json:"rounding"`
	MaxLeg     int64      `json:"max_leg"`  // Kč, 0 = no limit
	RulesOn    []string   `json:"rules_on"` // never null
}

// orderInfo is an order's display fields and the arb's fields the agent gets (bot_orders.info_json).
type orderInfo struct {
	Match      string   `json:"match"`
	Market     string   `json:"market"`
	Live       bool     `json:"live"`
	ProfitPct  float64  `json:"profitPct"`
	MarketType string   `json:"marketType"`
	Line       *float64 `json:"line"`
	IsLive     bool     `json:"isLive"`
	Sport      string   `json:"sport"`
	Rounding   int      `json:"rounding"` // 0 in rows older than Phase 3
}

// infoOf is in's display and arb fields as info_json keeps them (rounding 1 when absent).
func infoOf(in botOrderIn) orderInfo {
	rounding := 1
	if in.Rounding != nil {
		rounding = *in.Rounding
	}
	return orderInfo{Match: in.Match, Market: in.Market, Live: in.Live, ProfitPct: in.ProfitPct, MarketType: in.MarketType, Line: in.Line,
		IsLive: in.IsLive, Sport: in.Sport, Rounding: rounding}
}

// storedLeg is a leg as bot_orders.legs_json keeps it: the app's leg, its window and its last step.
type storedLeg struct {
	botLeg
	WindowID string `json:"windowId"`
	Step     string `json:"step"`
	StepAt   int64  `json:"stepAt"` // Unix ms; 0 = no step yet
}

// botOrder is a waiting or running order, in memory until its final state.
type botOrder struct {
	id      string
	userID  int64
	codeID  int64 // the agent it was sent to; only it may post the order's events. 0 while waiting
	arbKey  string
	boot    string // that agent's boot id when the order was sent; "" = none
	state   string // orderWaiting or orderRunning
	reason  string // why it waits
	dryRun  bool   // the setting when it was sent (while waiting: when it was posted)
	stake   int64  // Kč, the legs' stakes summed, rounded
	info    orderInfo
	legs    []storedLeg
	created time.Time
	sent    time.Time
	// dayChecked: the POST that created it just checked the day limit (cleared when that POST ends).
	dayChecked bool
}

func (o *botOrder) legsJSON() string {
	js, _ := json.Marshal(o.legs) // plain structs: cannot fail
	return string(js)
}

// message is o as an agent receives it under the settings st, its legs in windows ("" = none).
func (o *botOrder) message(st accounts.BotSettings, windows [2]string) botOrderMsg {
	msg := botOrderMsg{OrderID: o.id, ArbKey: o.arbKey, DryRun: st.DryRun, MarketType: o.info.MarketType, Line: o.info.Line,
		IsLive: o.info.IsLive, Sport: o.info.Sport, Match: o.info.Match, Rounding: o.info.Rounding, MaxLeg: st.MaxLeg,
		RulesOn: rulesOnList(st.RulesOn)}
	for i, l := range o.legs {
		msg.Legs = append(msg.Legs, agentLeg{botLeg: l.botLeg, WindowID: windows[i]})
	}
	return msg
}

// effects is what a hub change must do once b.mu is released: database writes, botEvent messages
// and {"type":"bot"} notices.
type effects struct {
	writes []func() error
	events []userMsg
	users  []int64
}

type userMsg struct {
	userID  int64
	payload []byte
}

func (fx *effects) write(f func() error) { fx.writes = append(fx.writes, f) }

func (fx *effects) notify(userID int64) {
	if !slices.Contains(fx.users, userID) {
		fx.users = append(fx.users, userID)
	}
}

// release unlocks b.mu and carries out fx: the database writes first, in order (b.dbMu is taken
// before b.mu is released, so the writes of successive changes never overtake each other), then the
// botEvents, then one {"type":"bot"} per user. A failed write is logged.
func (b *botHub) release(fx *effects) {
	b.dbMu.Lock()
	b.mu.Unlock()
	for _, w := range fx.writes {
		if err := w(); err != nil {
			log.Error().Err(err).Msg("bot database write failed")
		}
	}
	b.dbMu.Unlock()
	for _, m := range fx.events {
		b.reg.sendUser(m.userID, m.payload)
	}
	for _, id := range fx.users {
		b.changed(id)
	}
}

// loadSettings caches userID's settings (call without b.mu). A database error is returned and
// leaves them uncached: they are never replaced by the defaults (a settings PUT answers 500, a new
// order is refused). A pause decided while they were unreadable (memPause) is applied to the
// settings read here and stored.
func (b *botHub) loadSettings(userID int64) error {
	b.mu.Lock()
	_, ok := b.settings[userID]
	b.mu.Unlock()
	if ok {
		return nil
	}
	st := accounts.DefaultBotSettings()
	if b.acc != nil {
		var err error
		if st, err = b.acc.BotSettings(userID); err != nil {
			log.Error().Err(err).Int64("user", userID).Msg("bot settings read failed")
			return err
		}
	}
	b.mu.Lock()
	if _, ok := b.settings[userID]; ok {
		b.mu.Unlock()
		return nil
	}
	fx := &effects{}
	if reason, ok := b.memPause[userID]; ok {
		delete(b.memPause, userID)
		st.Paused, st.PausedReason = true, reason
		b.saveSettingsLocked(userID, st, fx)
		log.Warn().Int64("user", userID).Str("reason", reason).Msg("bots pause stored once the settings could be read")
	} else {
		b.settings[userID] = st
	}
	b.release(fx)
	return nil
}

// settingsLocked is userID's cached settings, or the defaults with a pause decided in memory while
// they were unreadable (memPause). b.mu must be held.
func (b *botHub) settingsLocked(userID int64) accounts.BotSettings {
	if st, ok := b.settings[userID]; ok {
		return st
	}
	st := accounts.DefaultBotSettings()
	if reason, ok := b.memPause[userID]; ok {
		st.Paused, st.PausedReason = true, reason
	}
	return st
}

// saveSettingsLocked caches st and queues its write. b.mu must be held.
func (b *botHub) saveSettingsLocked(userID int64, st accounts.BotSettings, fx *effects) {
	b.settings[userID] = st
	if b.acc != nil {
		fx.write(func() error { return b.acc.SaveBotSettings(userID, st) })
	}
}

// botEventMsg is the {"type":"botEvent"} message the user's non-bot /ws sockets receive.
type botEventMsg struct {
	Type      string `json:"type"`
	OrderID   string `json:"order_id"`
	ArbKey    string `json:"arb_key"`
	Event     string `json:"event"`
	Bookmaker string `json:"bookmaker,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Step      string `json:"step,omitempty"`
}

// relayLocked queues ev as a botEvent of o for the user's non-bot sockets. b.mu must be held.
func (b *botHub) relayLocked(fx *effects, o *botOrder, ev botEventMsg) {
	ev.Type, ev.OrderID, ev.ArbKey = "botEvent", o.id, o.arbKey
	payload, _ := json.Marshal(ev) // plain struct: cannot fail
	fx.events = append(fx.events, userMsg{userID: o.userID, payload: payload})
}

var (
	errBotOffline   = errors.New("bot_offline")
	errBookNotReady = errors.New("book_not_ready")
	errBotsPaused   = errors.New("bots_paused")
	errOrderExists  = errors.New("order_exists")
	errLimitLeg     = errors.New("limit_leg")
	errLimitOrder   = errors.New("limit_order")
	errLimitDay     = errors.New("limit_day")
)

// validOrder checks the shape: an arb key (at most 256 bytes), exactly 2 legs at different
// bookmakers, each with a selection id, odds above 1 and a stake above 0 and at most maxKc (so
// stake sums never overflow); match and market at most 200 characters and a finite profit_pct;
// then the arb fields (validArbFields).
func validOrder(o botOrderIn) bool {
	if strings.TrimSpace(o.ArbKey) == "" || len(o.ArbKey) > 256 || len(o.Legs) != 2 || o.Legs[0].Bookmaker == o.Legs[1].Bookmaker ||
		runeLen(o.Match) > 200 || runeLen(o.Market) > 200 || math.IsNaN(o.ProfitPct) || math.IsInf(o.ProfitPct, 0) || !validArbFields(o) {
		return false
	}
	for _, l := range o.Legs {
		if strings.TrimSpace(l.Bookmaker) == "" || l.Bookmaker != strings.TrimSpace(l.Bookmaker) || strings.TrimSpace(l.SourceID) == "" || !(l.Odds > 1) || !(l.Stake > 0) || !(l.Stake <= maxKc) {
			return false
		}
	}
	return true
}

// validArbFields checks the fields the agent's rules read: market_type at most 64 and sport at most
// 40 characters, rounding absent or 1, 10 or 100, each leg's outcome_id at most 120 characters and
// its fair_odds absent, null or above 1.
func validArbFields(o botOrderIn) bool {
	if runeLen(o.MarketType) > 64 || runeLen(o.Sport) > 40 || (o.Rounding != nil && *o.Rounding != 1 && *o.Rounding != 10 && *o.Rounding != 100) {
		return false
	}
	for _, l := range o.Legs {
		if runeLen(l.OutcomeID) > 120 || (l.FairOdds != nil && !(*l.FairOdds > 1)) {
			return false
		}
	}
	return true
}

// submitted is the 202 answer to a new order.
type submitted struct {
	OrderID string `json:"order_id"`
	State   string `json:"state"` // "sent" or "waiting"
	Reason  string `json:"reason,omitempty"`
}

// submit takes a valid order: the checks (paused, same arb key, limits, agents) in the contract's
// order, then the row, then it goes now or waits.
func (b *botHub) submit(userID int64, in botOrderIn) (submitted, error) {
	if err := b.loadSettings(userID); err != nil {
		return submitted{}, err // never decided on the defaults
	}
	b.mu.Lock()
	fx := &effects{}
	b.expireLocked(fx)
	res, err := b.submitLocked(userID, in, fx)
	b.release(fx)
	return res, err
}

func (b *botHub) submitLocked(userID int64, in botOrderIn, fx *effects) (submitted, error) {
	st := b.settingsLocked(userID)
	if st.Paused {
		return submitted{}, errBotsPaused
	}
	for _, o := range b.orders {
		if o.userID == userID && o.arbKey == in.ArbKey {
			return submitted{}, errOrderExists
		}
	}
	sum := in.Legs[0].Stake + in.Legs[1].Stake
	if st.MaxLeg > 0 && (in.Legs[0].Stake > float64(st.MaxLeg) || in.Legs[1].Stake > float64(st.MaxLeg)) {
		return submitted{}, errLimitLeg
	}
	if st.MaxOrder > 0 && sum > float64(st.MaxOrder) {
		return submitted{}, errLimitOrder
	}
	stake := int64(math.Round(sum))
	now := b.now()
	dayChecked := !st.DryRun && st.PerDay > 0 && b.acc != nil
	if dayChecked {
		from, to := pragueDayOf(now)
		b.dbMu.Lock() // the writes of earlier changes are done once it is ours
		day, err := b.acc.BotOrderDay(userID, from.UnixMilli(), to.UnixMilli())
		b.dbMu.Unlock()
		if err != nil {
			return submitted{}, err
		}
		if stake > st.PerDay-day.LiveStake {
			return submitted{}, errLimitDay
		}
	}
	online, capable := false, false
	for _, p := range b.agents[userID] {
		if p.proto < botProto {
			continue
		}
		online = true
		capable = capable || (p.hasWindow(in.Legs[0].Bookmaker) && p.hasWindow(in.Legs[1].Bookmaker))
	}
	if !online {
		return submitted{}, errBotOffline
	}
	if !capable {
		return submitted{}, errBookNotReady
	}
	o := &botOrder{id: newOrderID(), userID: userID, arbKey: in.ArbKey, state: orderWaiting, dryRun: st.DryRun, stake: stake,
		info: infoOf(in), created: now, dayChecked: dayChecked}
	for _, l := range in.Legs {
		o.legs = append(o.legs, storedLeg{botLeg: l})
	}
	b.orders[o.id] = o
	if b.acc != nil {
		row := accounts.BotOrderRow{ID: o.id, UserID: userID, ArbKey: o.arbKey, State: orderWaiting, DryRun: o.dryRun, Stake: stake,
			InfoJSON: mustJSON(o.info), LegsJSON: o.legsJSON(), CreatedAt: now.UnixMilli()}
		fx.write(func() error { return b.acc.InsertBotOrder(row) })
	}
	fx.notify(userID)
	b.dispatchLocked(userID, fx)
	o.dayChecked = false // from now on a live send checks the day limit again
	if o.state == orderRunning {
		return submitted{OrderID: o.id, State: "sent"}, nil
	}
	return submitted{OrderID: o.id, State: orderWaiting, Reason: o.reason}, nil
}

// shortID is the first 8 characters of an order id, as the app shows it.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func mustJSON(v any) string {
	js, _ := json.Marshal(v) // plain structs: cannot fail
	return string(js)
}

// hasWindow reports whether the agent has a window of book that is idle or busy (one that can become free).
func (p *botPresence) hasWindow(book string) bool {
	for _, w := range p.windows {
		if w.Bookmaker == book && (w.State == "idle" || w.State == "busy") {
			return true
		}
	}
	return false
}

// windowState is the state of the agent's window id in its last status ("" = no such window).
func (p *botPresence) windowState(id string) string {
	for _, w := range p.windows {
		if w.ID == id {
			return w.State
		}
	}
	return ""
}

// reservedLocked maps userID's agents to their reserved windows: code id → window id → order id.
// A window is reserved from the moment an order is sent to it until the order's final state. b.mu
// must be held.
func (b *botHub) reservedLocked(userID int64) map[int64]map[string]string {
	out := map[int64]map[string]string{}
	for _, o := range b.orders {
		if o.userID != userID || o.state != orderRunning {
			continue
		}
		if out[o.codeID] == nil {
			out[o.codeID] = map[string]string{}
		}
		for _, l := range o.legs {
			out[o.codeID][l.WindowID] = o.id
		}
	}
	return out
}

// commandWindowsLocked maps userID's agents to the windows that have a window command waiting for
// the agent's answer: code id → window ids. The scheduler leaves them alone meanwhile. b.mu must be held.
func (b *botHub) commandWindowsLocked(userID int64) map[int64]map[string]bool {
	out := map[int64]map[string]bool{}
	for _, pc := range b.commands {
		if pc.userID != userID || pc.windowID == "" {
			continue
		}
		if out[pc.codeID] == nil {
			out[pc.codeID] = map[string]bool{}
		}
		out[pc.codeID][pc.windowID] = true
	}
	return out
}

// ordersLocked is userID's orders in state, oldest first (by creation, then id). b.mu must be held.
func (b *botHub) ordersLocked(userID int64, state string) []*botOrder {
	var out []*botOrder
	for _, o := range b.orders {
		if o.userID == userID && o.state == state {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(x, y *botOrder) int {
		if c := x.created.Compare(y.created); c != 0 {
			return c
		}
		return strings.Compare(x.id, y.id)
	})
	return out
}

// freeWindowLocked is the agent's window of book to use: idle, not reserved, no window command
// pending for it, the one idle longest (gateway time of the first status that showed it idle and
// unreserved), then the lowest number. "" = none. b.mu must be held.
func (b *botHub) freeWindowLocked(p *botPresence, book string, reserved map[string]string, commanded map[string]bool) string {
	var pick *botWindow
	var pickSince time.Time
	for i := range p.windows {
		w := &p.windows[i]
		if w.Bookmaker != book || w.State != "idle" || reserved[w.ID] != "" || commanded[w.ID] {
			continue
		}
		since, ok := p.idleSince[w.ID]
		if !ok {
			since = b.now()
		}
		if pick == nil || since.Before(pickSince) || (since.Equal(pickSince) && w.Number < pick.Number) {
			pick, pickSince = w, since
		}
	}
	if pick == nil {
		return ""
	}
	return pick.ID
}

// pickLocked chooses the agent and windows for o among userID's protocol-2 agents (except skip)
// that have a free window of both legs' bookmakers: fewest reserved windows, then the one whose
// reserved count changed longest ago, then the lowest code id. nil: none. b.mu must be held.
func (b *botHub) pickLocked(o *botOrder, reserved map[int64]map[string]string, skip map[int64]bool) (int64, *botPresence, [2]string) {
	commanded := b.commandWindowsLocked(o.userID)
	var bestID int64
	var best *botPresence
	var bestWins [2]string
	for codeID, p := range b.agents[o.userID] {
		if skip[codeID] || p.proto < botProto {
			continue
		}
		w0 := b.freeWindowLocked(p, o.legs[0].Bookmaker, reserved[codeID], commanded[codeID])
		w1 := b.freeWindowLocked(p, o.legs[1].Bookmaker, reserved[codeID], commanded[codeID])
		if w0 == "" || w1 == "" {
			continue
		}
		n, bestN := len(reserved[codeID]), len(reserved[bestID])
		if best == nil || n < bestN || (n == bestN && (p.reservedAt.Before(best.reservedAt) ||
			(p.reservedAt.Equal(best.reservedAt) && codeID < bestID))) {
			bestID, best, bestWins = codeID, p, [2]string{w0, w1}
		}
	}
	return bestID, best, bestWins
}

// startLocked sends waiting order o if it can go now: fewer running orders than ordersAtOnce and an
// agent that can take it. Its two windows are reserved at once; the order becomes running (row,
// botEvent "sent", {"type":"bot"}). An agent whose socket does not take the order (send buffer
// full: not draining) is left out and the pick repeated. Before it is sent the settings are checked
// again (they may have changed while it waited): paused → it ends stopped · "Paused"; a leg over
// maxLeg or the legs over maxOrder → "Over the leg limit" / "Over the order limit"; sent live, it
// must still fit the day limit (overDayLocked), else "Over the day limit". false: not sent (it
// waits, or it ended). b.mu must be held.
func (b *botHub) startLocked(o *botOrder, fx *effects) bool {
	st := b.settingsLocked(o.userID)
	if len(b.ordersLocked(o.userID, orderRunning)) >= st.OrdersAtOnce {
		return false
	}
	reserved := b.reservedLocked(o.userID)
	skip := map[int64]bool{}
	for {
		codeID, p, wins := b.pickLocked(o, reserved, skip)
		if p == nil {
			return false
		}
		if reason := overSettings(o, st); reason != "" {
			b.endLocked(o, "stopped", reason, true, fx)
			return false
		}
		if !st.DryRun && st.PerDay > 0 && !o.dayChecked {
			over, err := b.overDayLocked(o, st, fx)
			if err != nil {
				log.Error().Err(err).Str("order_id", o.id).Msg("bot day limit check failed")
				return false // waits; tried again on the next trigger
			}
			if over {
				b.endLocked(o, "stopped", "Over the day limit", true, fx)
				return false
			}
		}
		payload, _ := json.Marshal(struct {
			Type  string      `json:"type"`
			Order botOrderMsg `json:"order"`
		}{"order", o.message(st, wins)})
		if !p.client.trySend(payload) {
			skip[codeID] = true
			continue
		}
		now := b.now()
		o.state, o.codeID, o.boot, o.sent, o.dryRun, o.reason = orderRunning, codeID, p.boot, now, st.DryRun, ""
		for i := range o.legs {
			o.legs[i].WindowID = wins[i]
			delete(p.idleSince, wins[i])
		}
		p.reservedAt = now
		if b.acc != nil {
			id, dryRun, legs := o.id, o.dryRun, o.legsJSON()
			fx.write(func() error { return b.acc.MarkBotOrderSent(id, codeID, dryRun, legs, now.UnixMilli()) })
		}
		b.relayLocked(fx, o, botEventMsg{Event: "sent"})
		fx.notify(o.userID)
		log.Info().Int64("user", o.userID).Int64("code", codeID).Str("order_id", o.id).Str("arb_key", o.arbKey).
			Strs("windows", wins[:]).Bool("dry_run", o.dryRun).Msg("bot order sent")
		return true
	}
}

// overSettings is why o may not be sent under st ("" = it may): paused, or over the leg or order limit.
func overSettings(o *botOrder, st accounts.BotSettings) string {
	switch {
	case st.Paused:
		return "Paused"
	case st.MaxLeg > 0 && (o.legs[0].Stake > float64(st.MaxLeg) || o.legs[1].Stake > float64(st.MaxLeg)):
		return "Over the leg limit"
	case st.MaxOrder > 0 && o.legs[0].Stake+o.legs[1].Stake > float64(st.MaxOrder):
		return "Over the order limit"
	}
	return ""
}

// overDayLocked reports whether sending o live now would take its Prague day's live stake over
// st.PerDay. The writes fx has queued so far are carried out first (under b.dbMu, as release
// would), so the query sees every order sent or ended in this change too; o's own row is counted
// only once. b.mu must be held.
func (b *botHub) overDayLocked(o *botOrder, st accounts.BotSettings, fx *effects) (bool, error) {
	if b.acc == nil {
		return false, nil
	}
	b.dbMu.Lock()
	for _, w := range fx.writes {
		if err := w(); err != nil {
			log.Error().Err(err).Msg("bot database write failed")
		}
	}
	fx.writes = nil
	from, to := pragueDayOf(o.created)
	day, err := b.acc.BotOrderDay(o.userID, from.UnixMilli(), to.UnixMilli())
	b.dbMu.Unlock()
	if err != nil {
		return false, err
	}
	used := day.LiveStake
	if !o.dryRun { // its row (posted live, still waiting) is already in the sum
		used -= o.stake
	}
	return o.stake > st.PerDay-used, nil
}

// waitReasonLocked says why o cannot go now: "Orders at once: <n> running", else the bookmakers
// the best placed protocol-2 agent (fewest missing, then lowest code id) has no free window of:
// "Needs a free <Book> window" or "Needs a free <Book> and <Book> window". b.mu must be held.
func (b *botHub) waitReasonLocked(o *botOrder) string {
	st := b.settingsLocked(o.userID)
	if n := len(b.ordersLocked(o.userID, orderRunning)); n >= st.OrdersAtOnce {
		return fmt.Sprintf("Orders at once: %d running", n)
	}
	reserved := b.reservedLocked(o.userID)
	commanded := b.commandWindowsLocked(o.userID)
	codes := make([]int64, 0, len(b.agents[o.userID]))
	for codeID := range b.agents[o.userID] {
		codes = append(codes, codeID)
	}
	slices.Sort(codes)
	var missing []string
	found := false
	for _, codeID := range codes {
		p := b.agents[o.userID][codeID]
		if p.proto < botProto {
			continue
		}
		var m []string
		for _, l := range o.legs {
			if b.freeWindowLocked(p, l.Bookmaker, reserved[codeID], commanded[codeID]) == "" {
				m = append(m, l.Bookmaker)
			}
		}
		if !found || len(m) < len(missing) {
			found, missing = true, m
		}
	}
	if len(missing) == 0 { // no agent at all, or one whose socket does not drain
		missing = []string{o.legs[0].Bookmaker, o.legs[1].Bookmaker}
	}
	if len(missing) == 1 {
		return "Needs a free " + bookLabel(missing[0]) + " window"
	}
	return "Needs a free " + bookLabel(missing[0]) + " and " + bookLabel(missing[1]) + " window"
}

// dispatchLocked sends every waiting order of userID that can go now, oldest first (one that
// cannot go does not block later ones). One that still waits gets its reason; a changed reason is
// relayed (botEvent "waiting"). b.mu must be held.
func (b *botHub) dispatchLocked(userID int64, fx *effects) {
	for _, o := range b.ordersLocked(userID, orderWaiting) {
		if b.startLocked(o, fx) || b.orders[o.id] != o { // sent, or ended (over the day limit)
			continue
		}
		if r := b.waitReasonLocked(o); r != o.reason {
			o.reason = r
			b.relayLocked(fx, o, botEventMsg{Event: orderWaiting, Reason: r})
		}
	}
}

// endLocked ends o in a final state: it leaves memory (its windows are free again: idle since now
// if the agent's last status shows them idle), its row is updated, the app is told (botEvent when
// relay, then {"type":"bot"}). outcome_unknown pauses the user when pauseOnUnknown is on. b.mu must be held.
func (b *botHub) endLocked(o *botOrder, state, reason string, relay bool, fx *effects) {
	if b.orders[o.id] != o {
		return // already ended
	}
	delete(b.orders, o.id)
	now := b.now()
	if o.state == orderRunning {
		if p := b.presenceLocked(o.userID, o.codeID); p != nil {
			p.reservedAt = now
			for _, l := range o.legs {
				if p.windowState(l.WindowID) == "idle" {
					p.idleSince[l.WindowID] = now
				}
			}
		}
	}
	if b.acc != nil {
		id, legs, stake := o.id, o.legsJSON(), o.stake
		fx.write(func() error { return b.acc.EndBotOrder(id, state, reason, legs, stake, now.UnixMilli()) })
	}
	if relay {
		b.relayLocked(fx, o, botEventMsg{Event: state, Reason: reason})
	}
	fx.notify(o.userID)
	log.Info().Int64("user", o.userID).Str("order_id", o.id).Str("state", state).Str("reason", reason).Msg("bot order ended")
	if st := b.settingsLocked(o.userID); state == "outcome_unknown" && st.PauseOnUnknown && !st.Paused {
		b.pauseLocked(o.userID, "Outcome unknown · order "+shortID(o.id), fx)
	}
}

var pauseMsg = []byte(`{"type":"pause"}`)

// pauseLocked pauses userID with reason: the setting is saved, every waiting order ends stopped ·
// "Paused", and each protocol-2 agent socket gets {"type":"pause"} (the agent stops the pairs that
// have not committed). New orders are refused while paused. Settings that could not be read are
// not overwritten: the pause is kept in memory (memPause) until they can. b.mu must be held.
func (b *botHub) pauseLocked(userID int64, reason string, fx *effects) {
	if st, ok := b.settings[userID]; ok {
		st.Paused, st.PausedReason = true, reason
		b.saveSettingsLocked(userID, st, fx)
	} else {
		b.memPause[userID] = reason
		log.Warn().Int64("user", userID).Str("reason", reason).Msg("bots paused in memory only: settings unreadable")
	}
	for _, o := range b.ordersLocked(userID, orderWaiting) {
		b.endLocked(o, "stopped", "Paused", true, fx)
	}
	for codeID, p := range b.agents[userID] {
		if p.proto >= botProto {
			sendPauseLocked(userID, codeID, p)
		}
	}
	fx.notify(userID)
	log.Warn().Int64("user", userID).Str("reason", reason).Msg("bots paused")
}

// sendPauseLocked sends {"type":"pause"} to the agent's socket; a socket that does not take it
// (send buffer full) is logged. An agent that misses it still gets it with its next socket's first
// protocol-2 status (setStatus). b.mu must be held.
func sendPauseLocked(userID, codeID int64, p *botPresence) {
	if !p.client.trySend(pauseMsg) {
		log.Warn().Int64("user", userID).Int64("code", codeID).Msg("bot pause not sent: socket does not take it")
	}
}

// expireLocked ends waiting orders older than waitExpiry (stopped · "Expired · <wait reason>") and
// running orders without a final event after orderMemory (outcome_unknown · "No result from the
// agent"). b.mu must be held.
func (b *botHub) expireLocked(fx *effects) {
	now := b.now()
	var due []*botOrder
	for _, o := range b.orders {
		if (o.state == orderWaiting && !now.Before(o.created.Add(waitExpiry))) || (o.state == orderRunning && !now.Before(o.sent.Add(orderMemory))) {
			due = append(due, o)
		}
	}
	slices.SortFunc(due, func(x, y *botOrder) int { return x.created.Compare(y.created) })
	for _, o := range due {
		if o.state == orderWaiting {
			b.endLocked(o, "stopped", "Expired · "+o.reason, true, fx)
		} else {
			b.endLocked(o, "outcome_unknown", "No result from the agent", true, fx)
		}
	}
}

// tick expires orders and retries every user's waiting orders (the scheduler's 1 s beat).
func (b *botHub) tick() {
	b.mu.Lock()
	fx := &effects{}
	b.expireLocked(fx)
	var users []int64
	for _, o := range b.orders {
		if o.state == orderWaiting && !slices.Contains(users, o.userID) {
			users = append(users, o.userID)
		}
	}
	slices.Sort(users)
	for _, id := range users {
		b.dispatchLocked(id, fx)
	}
	b.release(fx)
}

// refreshIdleLocked keeps the agent's idle-since times: a window idle and unreserved in its last
// status keeps its time (or gets now), any other window loses it. b.mu must be held.
func (b *botHub) refreshIdleLocked(userID, codeID int64, p *botPresence) {
	reserved := b.reservedLocked(userID)[codeID]
	now := b.now()
	keep := map[string]bool{}
	for _, w := range p.windows {
		if w.State == "idle" && reserved[w.ID] == "" {
			keep[w.ID] = true
			if _, ok := p.idleSince[w.ID]; !ok {
				p.idleSince[w.ID] = now
			}
		}
	}
	for id := range p.idleSince {
		if !keep[id] {
			delete(p.idleSince, id)
		}
	}
}

func (b *botHub) handleOrder(w http.ResponseWriter, r *http.Request) {
	var in botOrderIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || !validOrder(in) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_order")
		return
	}
	userID := principalOf(r).UserID
	res, err := b.submit(userID, in)
	switch {
	case err == nil:
		log.Info().Int64("user", userID).Str("order_id", res.OrderID).Str("arb_key", in.ArbKey).Str("state", res.State).Msg("bot order taken")
		accounts.WriteJSON(w, http.StatusAccepted, res)
	case errors.Is(err, errBotsPaused), errors.Is(err, errOrderExists), errors.Is(err, errLimitLeg), errors.Is(err, errLimitOrder),
		errors.Is(err, errLimitDay), errors.Is(err, errBotOffline), errors.Is(err, errBookNotReady):
		accounts.WriteError(w, http.StatusConflict, err.Error())
	default:
		log.Error().Err(err).Msg("bot order failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
	}
}

// botEvents lists the events an agent may post; true = final (the order is over).
var botEvents = map[string]bool{
	"accepted":        false,
	"leg_ready":       false,
	"leg_step":        false,
	"placed":          true,
	"stopped":         true,
	"outcome_unknown": true,
}

var errUnknownOrder = errors.New("unknown_order")

// botEventIn is the POST /api/me/bot/events body.
type botEventIn struct {
	OrderID   string `json:"order_id"`
	Event     string `json:"event"`
	Bookmaker string `json:"bookmaker"`
	Reason    string `json:"reason"`
	Step      string `json:"step"`
}

// event relays one order event, posted by codeID's agent, to the user's non-bot /ws sockets.
// leg_step records the leg's step and its time. A final event ends the order (row, reservation)
// and retries the waiting orders. errUnknownOrder: the order is unknown, ended, waiting, another
// user's, or was sent to another agent.
func (b *botHub) event(userID, codeID int64, in botEventIn) error {
	b.loadSettings(userID)
	b.mu.Lock()
	fx := &effects{}
	b.expireLocked(fx)
	o := b.orders[in.OrderID]
	if o == nil || o.userID != userID || o.state != orderRunning || o.codeID != codeID {
		b.release(fx)
		return errUnknownOrder
	}
	if in.Event == "leg_step" {
		for i := range o.legs {
			if o.legs[i].Bookmaker == in.Bookmaker {
				o.legs[i].Step, o.legs[i].StepAt = in.Step, b.now().UnixMilli()
			}
		}
	}
	b.relayLocked(fx, o, botEventMsg{Event: in.Event, Bookmaker: in.Bookmaker, Reason: in.Reason, Step: in.Step})
	if botEvents[in.Event] {
		b.endLocked(o, in.Event, in.Reason, false, fx)
		b.dispatchLocked(userID, fx)
	}
	b.release(fx)
	return nil
}

func (b *botHub) handleEvent(w http.ResponseWriter, r *http.Request) {
	var in botEventIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		accounts.WriteError(w, http.StatusBadRequest, "bad_event")
		return
	}
	if _, ok := botEvents[in.Event]; !ok || runeLen(in.Step) > 40 || (in.Event == "leg_step" && (in.Step == "" || in.Bookmaker == "")) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_event")
		return
	}
	p := principalOf(r)
	switch err := b.event(p.UserID, p.CodeID, in); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errUnknownOrder):
		accounts.WriteError(w, http.StatusNotFound, "unknown_order")
	default:
		log.Error().Err(err).Msg("bot event failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
	}
}

// recoverOrders ends the orders a previous gateway process left open (it lost them from memory):
// waiting → stopped, running → outcome_unknown, both "Gateway restarted"; a user with pauseOnUnknown
// on is paused for an outcome_unknown one. Call once at start, before serving.
func (b *botHub) recoverOrders() {
	if b.acc == nil {
		return
	}
	closed, err := b.acc.CloseOpenBotOrders("Gateway restarted", b.now().UnixMilli())
	if err != nil {
		log.Error().Err(err).Msg("bot orders recovery failed")
		return
	}
	for _, o := range closed {
		log.Warn().Int64("user", o.UserID).Str("order_id", o.ID).Str("state", o.State).Msg("bot order ended: gateway restarted")
		if o.State != "outcome_unknown" {
			continue
		}
		b.loadSettings(o.UserID)
		b.mu.Lock()
		fx := &effects{}
		if st := b.settingsLocked(o.UserID); st.PauseOnUnknown && !st.Paused {
			b.pauseLocked(o.UserID, "Outcome unknown · order "+shortID(o.ID), fx)
		}
		b.release(fx)
	}
}
