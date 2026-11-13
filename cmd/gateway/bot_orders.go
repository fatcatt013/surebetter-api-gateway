package main

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// orderLegView is one leg in the Orders screen.
type orderLegView struct {
	Bookmaker string  `json:"bookmaker"`
	WindowID  string  `json:"windowId"`
	Label     string  `json:"label"`
	Odds      float64 `json:"odds"`
	Stake     float64 `json:"stake"`
	Step      string  `json:"step"`
	StepAt    int64   `json:"stepAt"`
}

// orderView is one order in GET /api/me/bot/orders.
type orderView struct {
	ID        string         `json:"id"`
	ArbKey    string         `json:"arbKey"`
	State     string         `json:"state"`
	Reason    string         `json:"reason"`
	DryRun    bool           `json:"dryRun"`
	AgentID   int64          `json:"agentId"`
	Agent     string         `json:"agent"`
	Match     string         `json:"match"`
	Market    string         `json:"market"`
	Live      bool           `json:"live"`
	ProfitPct float64        `json:"profitPct"`
	Stake     int64          `json:"stake"`
	CreatedAt int64          `json:"createdAt"`
	SentAt    int64          `json:"sentAt"`
	EndedAt   int64          `json:"endedAt"`
	Legs      []orderLegView `json:"legs"`
}

func legViews(legs []storedLeg) []orderLegView {
	out := make([]orderLegView, 0, len(legs))
	for _, l := range legs {
		out = append(out, orderLegView{Bookmaker: l.Bookmaker, WindowID: l.WindowID, Label: l.Label, Odds: l.Odds, Stake: l.Stake,
			Step: l.Step, StepAt: l.StepAt})
	}
	return out
}

// viewOf is a waiting or running order as the Orders screen shows it; names maps code ids to agent names.
func (o *botOrder) viewOf(names map[int64]string) orderView {
	v := orderView{ID: o.id, ArbKey: o.arbKey, State: o.state, Reason: o.reason, DryRun: o.dryRun, AgentID: o.codeID, Agent: names[o.codeID],
		Match: o.info.Match, Market: o.info.Market, Live: o.info.Live, ProfitPct: o.info.ProfitPct, Stake: o.stake,
		CreatedAt: o.created.UnixMilli(), Legs: legViews(o.legs)}
	if o.state == orderRunning {
		v.SentAt = o.sent.UnixMilli()
	}
	return v
}

// rowView is a stored order as the Orders screen shows it.
func rowView(r accounts.BotOrderRow) orderView {
	var info orderInfo
	var legs []storedLeg
	json.Unmarshal([]byte(r.InfoJSON), &info) // written by the hub; unreadable → empty
	json.Unmarshal([]byte(r.LegsJSON), &legs)
	return orderView{ID: r.ID, ArbKey: r.ArbKey, State: r.State, Reason: r.Reason, DryRun: r.DryRun, AgentID: r.CodeID, Agent: r.Agent,
		Match: info.Match, Market: info.Market, Live: info.Live, ProfitPct: info.ProfitPct, Stake: r.Stake, CreatedAt: r.CreatedAt,
		SentAt: r.SentAt, EndedAt: r.EndedAt, Legs: legViews(legs)}
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
