package main

import (
	"encoding/json"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// orderResultTitles: the final states of a real order that make an info alert.
var orderResultTitles = map[string]string{"placed": "Placed", "cashed_out": "Cashed out", "stopped": "Stopped"}

// flowIsStuck: the flow object (compactFlow's) has a non-null stuck.
func flowIsStuck(flow string) bool {
	var f struct {
		Stuck json.RawMessage `json:"stuck"`
	}
	return flow != "" && json.Unmarshal([]byte(flow), &f) == nil && len(f.Stuck) > 0 && string(f.Stuck) != "null"
}

// orderWhereLocked names o's windows and agent: "Tipsport 1, Betano 1 (Home PC)". b.mu must be held.
func (b *botHub) orderWhereLocked(o *botOrder) string {
	p := b.presenceLocked(o.userID, o.codeID)
	var wins []string
	for _, l := range o.legs {
		if l.WindowID == "" {
			continue
		}
		label := bookLabel(l.Bookmaker)
		if p != nil {
			for _, w := range p.windows {
				if w.ID == l.WindowID {
					label = windowText(w)
				}
			}
		}
		wins = append(wins, label)
	}
	name := ""
	if o.codeID != 0 {
		name = b.agentNameLocked(o.codeID)
	}
	switch {
	case len(wins) == 0:
		return name
	case name == "":
		return strings.Join(wins, ", ")
	}
	return strings.Join(wins, ", ") + " (" + name + ")"
}

// alertOrderEndLocked raises the alert of a real order that ends in state: outcome_unknown (attention; "· Bots
// paused" when this end pauses the user), placed, cashed_out, or stopped after the agent accepted it (info). Dry runs
// never alert. b.mu must be held.
func (b *botHub) alertOrderEndLocked(o *botOrder, state, reason string, pausing bool, fx *effects) {
	if b.alerts == nil || o.dryRun {
		return
	}
	where := b.orderWhereLocked(o)
	switch {
	case state == "outcome_unknown":
		d := joinDetail("Order "+shortID(o.id), where, reason)
		if pausing {
			d = joinDetail(d, "Bots paused")
		}
		b.raiseLocked(o.userID, botAlert{Kind: alertAttention, Severity: severityAttention, AgentID: o.codeID, OrderID: o.id,
			Title: titleOutcomeUnknown, Detail: d}, nil, fx)
	case state == "placed" || state == "cashed_out" || (state == "stopped" && o.accepted):
		d := joinDetail("Order "+shortID(o.id), o.info.Match, where)
		if state != "placed" {
			d = joinDetail(d, reason)
		}
		b.raiseLocked(o.userID, botAlert{Kind: alertOrder, Severity: severityInfo, AgentID: o.codeID, OrderID: o.id,
			Title: orderResultTitles[state], Detail: d}, nil, fx)
	}
}

// alertStuckLocked raises the attention alert of a real order that entered stuck mode. b.mu must be held.
func (b *botHub) alertStuckLocked(o *botOrder, fx *effects) {
	if b.alerts == nil {
		return
	}
	b.raiseLocked(o.userID, botAlert{Kind: alertAttention, Severity: severityAttention, AgentID: o.codeID, OrderID: o.id,
		Title: titleStuck, Detail: joinDetail("Order "+shortID(o.id), b.orderWhereLocked(o))}, nil, fx)
}

// alertRecoveredLocked raises the attention alert of a real order the previous gateway process left running (it ends
// outcome_unknown · "Gateway restarted"). b.mu must be held.
func (b *botHub) alertRecoveredLocked(o accounts.BotOrderRow, pausing bool, fx *effects) {
	if b.alerts == nil || o.DryRun {
		return
	}
	d := joinDetail("Order "+shortID(o.ID), o.Agent, o.Reason)
	if pausing {
		d = joinDetail(d, "Bots paused")
	}
	b.raiseLocked(o.UserID, botAlert{Kind: alertAttention, Severity: severityAttention, AgentID: o.CodeID, OrderID: o.ID,
		Title: titleOutcomeUnknown, Detail: d}, nil, fx)
}

// resolveOrderAlerts resolves the open attention alerts of userID's order orderID (marked Checked) and tells the app.
// Call it without b.mu.
func (b *botHub) resolveOrderAlerts(userID int64, orderID string) {
	if b.acc == nil {
		return
	}
	rows, err := b.acc.ResolveBotOrderAlerts(userID, orderID, b.now().UnixMilli())
	if err != nil {
		log.Error().Err(err).Str("order_id", orderID).Msg("bot alerts: resolving a checked order's alerts failed")
		return
	}
	for _, row := range rows {
		b.reg.sendUser(userID, alertMessage(alertOfRow(row)))
	}
}
