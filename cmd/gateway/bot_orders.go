package main

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// orderLegView is one leg in the Orders screen.
type orderLegView struct {
	Bookmaker string   `json:"bookmaker"`
	WindowID  string   `json:"windowId"`
	Label     string   `json:"label"`
	Odds      float64  `json:"odds"`
	Stake     float64  `json:"stake"`
	Step      string   `json:"step"`
	StepAt    int64    `json:"stepAt"`
	OutcomeID string   `json:"outcomeId"`
	Placed    *bool    `json:"placed"`   // null until a final event reports the legs
	FairOdds  *float64 `json:"fairOdds"` // consensus fair odds the app sent; null = unknown
	// LimitLevel is the leg's level in the agent's limit index when the order was sent (0–2); null =
	// not sent yet (waiting) or an order older than Phase 3b.
	LimitLevel *int `json:"limitLevel"`
}

// orderView is one order in GET /api/me/bot/orders.
type orderView struct {
	ID         string          `json:"id"`
	ArbKey     string          `json:"arbKey"`
	State      string          `json:"state"`
	Reason     string          `json:"reason"`
	DryRun     bool            `json:"dryRun"`
	AgentID    int64           `json:"agentId"`
	Agent      string          `json:"agent"`
	Match      string          `json:"match"`
	Market     string          `json:"market"`
	Live       bool            `json:"live"`
	ProfitPct  float64         `json:"profitPct"`
	Stake      int64           `json:"stake"`
	CreatedAt  int64           `json:"createdAt"`
	SentAt     int64           `json:"sentAt"`
	EndedAt    int64           `json:"endedAt"`
	Legs       []orderLegView  `json:"legs"`
	MarketType string          `json:"marketType"`
	Line       *float64        `json:"line"`
	IsLive     bool            `json:"isLive"`
	Sport      string          `json:"sport"`
	Rounding   int             `json:"rounding"`
	Rule       string          `json:"rule"`     // from the last flow; "" = none
	Playbook   string          `json:"playbook"` // from the last flow; "" = none
	Flow       json.RawMessage `json:"flow"`     // the agent's last flow object; null = none
	// Checkable: ended, real (not a dry run) and outcome_unknown or with a stuck flow: the user can
	// mark it checked (human mode). CheckedAt: when they did (Unix ms); 0 = not checked.
	Checkable bool  `json:"checkable"`
	CheckedAt int64 `json:"checkedAt"`
}

// checkableOf: an ended (final state), real order that ended outcome_unknown or whose last flow has
// a non-null stuck.
func checkableOf(state string, dryRun bool, flow string) bool {
	if dryRun || !botEvents[state] {
		return false
	}
	if state == "outcome_unknown" {
		return true
	}
	var f struct {
		Stuck json.RawMessage `json:"stuck"`
	}
	return flow != "" && json.Unmarshal([]byte(flow), &f) == nil && len(f.Stuck) > 0 && string(f.Stuck) != "null"
}

func legViews(legs []storedLeg) []orderLegView {
	out := make([]orderLegView, 0, len(legs))
	for _, l := range legs {
		out = append(out, orderLegView{Bookmaker: l.Bookmaker, WindowID: l.WindowID, Label: l.Label, Odds: l.Odds, Stake: l.Stake,
			Step: l.Step, StepAt: l.StepAt, OutcomeID: l.OutcomeID, Placed: l.Placed, FairOdds: l.FairOdds,
			LimitLevel: l.LimitLevel})
	}
	return out
}

// withDetails fills v's arb fields from info (rounding 1 for orders stored before Phase 3) and its
// flow, rule and playbook from the stored flow ("" = none).
func (v orderView) withDetails(info orderInfo, flow string) orderView {
	v.MarketType, v.Line, v.IsLive, v.Sport, v.Rounding = info.MarketType, info.Line, info.IsLive, info.Sport, info.Rounding
	if v.Rounding == 0 {
		v.Rounding = 1
	}
	if flow != "" {
		var f struct {
			Rule     string `json:"rule"`
			Playbook string `json:"playbook"`
		}
		json.Unmarshal([]byte(flow), &f) // checked by compactFlow when it was posted
		v.Flow, v.Rule, v.Playbook = json.RawMessage(flow), f.Rule, f.Playbook
	}
	return v
}

// viewOf is a waiting or running order as the Orders screen shows it; names maps code ids to agent names.
func (o *botOrder) viewOf(names map[int64]string) orderView {
	v := orderView{ID: o.id, ArbKey: o.arbKey, State: o.state, Reason: o.reason, DryRun: o.dryRun, AgentID: o.codeID, Agent: names[o.codeID],
		Match: o.info.Match, Market: o.info.Market, Live: o.info.Live, ProfitPct: o.info.ProfitPct, Stake: o.stake,
		CreatedAt: o.created.UnixMilli(), Legs: legViews(o.legs)}
	if o.state == orderRunning {
		v.SentAt = o.sent.UnixMilli()
	}
	return v.withDetails(o.info, o.flow)
}

// rowView is a stored order as the Orders screen shows it.
func rowView(r accounts.BotOrderRow) orderView {
	var info orderInfo
	var legs []storedLeg
	json.Unmarshal([]byte(r.InfoJSON), &info) // written by the hub; unreadable → empty
	json.Unmarshal([]byte(r.LegsJSON), &legs)
	return orderView{ID: r.ID, ArbKey: r.ArbKey, State: r.State, Reason: r.Reason, DryRun: r.DryRun, AgentID: r.CodeID, Agent: r.Agent,
		Match: info.Match, Market: info.Market, Live: info.Live, ProfitPct: info.ProfitPct, Stake: r.Stake, CreatedAt: r.CreatedAt,
		SentAt: r.SentAt, EndedAt: r.EndedAt, Legs: legViews(legs), Checkable: checkableOf(r.State, r.DryRun, r.FlowJSON),
		CheckedAt: r.CheckedAt}.withDetails(info, r.FlowJSON)
}

// ordersView is the GET /api/me/bot/orders body.
type ordersView struct {
	Running []orderView `json:"running"`
	Waiting []orderView `json:"waiting"`
	History []orderView `json:"history"`
	Today   botToday    `json:"today"`
}

// maxHistory is the most ended orders GET /api/me/bot/orders lists for a day.
const maxHistory = 500

// handleGetOrders lists the running and waiting orders (now, oldest first), the orders of ?day=
// (Prague, default today) that ended (newest first, at most 500) and today's counts.
func (b *botHub) handleGetOrders(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	now := b.now()
	from, to := pragueDayOf(now)
	todayFrom, todayTo := from, to
	if day := r.URL.Query().Get("day"); day != "" {
		var ok bool
		if from, to, ok = parsePragueDay(day); !ok {
			accounts.WriteError(w, http.StatusBadRequest, "bad_query")
			return
		}
	}
	codes, err := b.acc.ListExtensionCodes(userID)
	if err != nil {
		log.Error().Err(err).Msg("bot code names failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	names := map[int64]string{}
	for _, c := range codes {
		names[c.ID] = c.Name
	}
	out := ordersView{Running: []orderView{}, Waiting: []orderView{}, History: []orderView{}}
	b.loadSettings(userID)
	b.mu.Lock()
	fx := &effects{}
	b.expireLocked(fx)
	for _, o := range b.ordersLocked(userID, orderRunning) {
		out.Running = append(out.Running, o.viewOf(names))
	}
	for _, o := range b.ordersLocked(userID, orderWaiting) {
		out.Waiting = append(out.Waiting, o.viewOf(names))
	}
	b.release(fx)
	rows, err := b.acc.EndedBotOrders(userID, from.UnixMilli(), to.UnixMilli(), maxHistory)
	if err == nil {
		var day accounts.BotOrderDayStats
		if day, err = b.acc.BotOrderDay(userID, todayFrom.UnixMilli(), todayTo.UnixMilli()); err == nil {
			out.Today = botToday{Orders: day.Orders, Placed: day.Placed}
		}
	}
	if err != nil {
		log.Error().Err(err).Msg("bot orders read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	for _, row := range rows {
		out.History = append(out.History, rowView(row))
	}
	accounts.WriteJSON(w, http.StatusOK, out)
}

// handleCancelOrder ends a waiting order stopped · "Cancelled". 409 order_running: it was sent;
// 404 unknown_order: ended, unknown or another user's.
func (b *botHub) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	b.loadSettings(userID)
	b.mu.Lock()
	fx := &effects{}
	b.expireLocked(fx)
	o := b.orders[r.PathValue("id")]
	code := ""
	switch {
	case o == nil || o.userID != userID:
		code = "unknown_order"
	case o.state == orderRunning:
		code = "order_running"
	default:
		b.endLocked(o, "stopped", "Cancelled", true, fx)
	}
	b.release(fx)
	switch code {
	case "unknown_order":
		accounts.WriteError(w, http.StatusNotFound, code)
	case "order_running":
		accounts.WriteError(w, http.StatusConflict, code)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleOrderChecked is POST /api/me/bot/orders/{id}/checked: the user checked an ended order that
// needed a check (checkableOf) on the bookmaker's site. It stores checked_at once (a second call
// keeps the first time) and answers {"checkedAt"}; the order's agent is told ({"type":"routine"},
// protocol 4: it re-reads its checked ids and clears the window's needs-check flag), then the app
// ({"type":"bot"}). 404 unknown_order: unknown or another user's; 409 not_checkable.
func (b *botHub) handleOrderChecked(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	row, found, err := b.acc.BotOrderByID(userID, r.PathValue("id"))
	if err == nil && found && checkableOf(row.State, row.DryRun, row.FlowJSON) {
		row.CheckedAt, err = b.acc.MarkBotOrderChecked(userID, row.ID, b.now().UnixMilli())
	}
	switch {
	case err != nil:
		log.Error().Err(err).Msg("bot order checked failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	case !found:
		accounts.WriteError(w, http.StatusNotFound, "unknown_order")
		return
	case row.CheckedAt == 0:
		accounts.WriteError(w, http.StatusConflict, "not_checkable")
		return
	}
	b.mu.Lock()
	b.sendRoutineLocked(userID, row.CodeID)
	b.mu.Unlock()
	b.changed(userID)
	accounts.WriteJSON(w, http.StatusOK, map[string]int64{"checkedAt": row.CheckedAt})
}
