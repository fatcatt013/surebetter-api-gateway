package main

import (
	"encoding/json"
	"strings"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// The four kinds of bot alert (spec 2026-11-23-bot-alerts-design.md) and the severities.
const (
	alertAttention = "attention" // needs your attention: an outcome unknown, a stuck order
	alertWindow    = "window"    // a window logged out, blocked or down for 60 s
	alertAgent     = "agent"     // an agent offline for 2 min, and back
	alertOrder     = "order"     // a real order placed, cashed out, or stopped after it was accepted

	severityAttention = "attention"
	severityProblem   = "problem"
	severityInfo      = "info"
)

// Alert titles (the window ones are windowProblemTitles, the order results orderResultTitles).
const (
	titleOutcomeUnknown = "Outcome unknown"
	titleStuck          = "Stuck order"
	titleAgentOffline   = "Agent offline"
	titleAgentBack      = "Agent back"
)

// botAlert is one alert as the app gets it (GET /api/me/bot/alerts, {"type":"alert"}). AgentID 0, WindowID "" and
// OrderID "" = not about one; ResolvedAt and DismissedAt 0 = not.
type botAlert struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Severity    string `json:"severity"`
	AgentID     int64  `json:"agentId"`
	WindowID    string `json:"windowId"`
	OrderID     string `json:"orderId"`
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	CreatedAt   int64  `json:"createdAt"`
	ResolvedAt  int64  `json:"resolvedAt"`
	DismissedAt int64  `json:"dismissedAt"`
}

func alertOfRow(r accounts.BotAlertRow) botAlert {
	return botAlert{ID: r.ID, Kind: r.Kind, Severity: r.Severity, AgentID: r.AgentCodeID, WindowID: r.WindowID, OrderID: r.OrderID,
		Title: r.Title, Detail: r.Detail, CreatedAt: r.CreatedAt, ResolvedAt: r.ResolvedAt, DismissedAt: r.DismissedAt}
}

func (a botAlert) row(userID int64) accounts.BotAlertRow {
	return accounts.BotAlertRow{ID: a.ID, UserID: userID, Kind: a.Kind, Severity: a.Severity, AgentCodeID: a.AgentID, WindowID: a.WindowID,
		OrderID: a.OrderID, Title: a.Title, Detail: a.Detail, CreatedAt: a.CreatedAt, ResolvedAt: a.ResolvedAt, DismissedAt: a.DismissedAt}
}

// alertMessage is a as the {"type":"alert"} message of the user's app sockets.
func alertMessage(a botAlert) []byte {
	payload, _ := json.Marshal(struct {
		Type  string   `json:"type"`
		Alert botAlert `json:"alert"`
	}{"alert", a}) // plain struct: cannot fail
	return payload
}

// alertChannels is one kind's switches: in the app, to Telegram.
type alertChannels struct {
	App      bool `json:"app"`
	Telegram bool `json:"telegram"`
}

// alertSettings is a user's switches (GET/PUT /api/me/bot/alert-settings, bot_alert_settings.json).
type alertSettings struct {
	Attention alertChannels `json:"attention"`
	Window    alertChannels `json:"window"`
	Agent     alertChannels `json:"agent"`
	Order     alertChannels `json:"order"`
}

// defaultAlertSettings is what a user without a row has: everything in the app; attention and window problems to
// Telegram too.
func defaultAlertSettings() alertSettings {
	return alertSettings{Attention: alertChannels{true, true}, Window: alertChannels{true, true}, Agent: alertChannels{true, false},
		Order: alertChannels{true, false}}
}

// of is kind's switches (none for an unknown kind).
func (s alertSettings) of(kind string) alertChannels {
	switch kind {
	case alertAttention:
		return s.Attention
	case alertWindow:
		return s.Window
	case alertAgent:
		return s.Agent
	case alertOrder:
		return s.Order
	}
	return alertChannels{}
}

// alertSettingsOf reads a stored row over the defaults: a kind or switch it lacks keeps its default; an unreadable
// row is the defaults.
func alertSettingsOf(js string) alertSettings {
	st := defaultAlertSettings()
	if json.Unmarshal([]byte(js), &st) != nil {
		return defaultAlertSettings()
	}
	return st
}

// joinDetail joins the non-empty parts with " · ".
func joinDetail(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
}
