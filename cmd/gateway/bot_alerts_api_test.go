package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func insertAlert(t *testing.T, svc *accounts.Service, id string, userID int64, kind, severity string, created int64) {
	t.Helper()
	if err := svc.InsertBotAlert(accounts.BotAlertRow{ID: id, UserID: userID, Kind: kind, Severity: severity, Title: "T " + id,
		CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
}

type alertsBody struct {
	Alerts []botAlert `json:"alerts"`
	Open   int        `json:"open"`
}

func TestBotAlertsAPI_ListDismissAndTellTheApp(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	other := userIDOf(t, svc, newUser(t, svc, "o@example.com", accounts.RoleUser))
	insertAlert(t, svc, "a1", uid, "window", "problem", 100)
	insertAlert(t, svc, "a2", uid, "order", "info", 200)
	insertAlert(t, svc, "a3", uid, "attention", "attention", 300)
	insertAlert(t, svc, "b1", other, "window", "problem", 400)
	list := func(query string) alertsBody {
		t.Helper()
		s, b := request(t, srv, "GET", "/api/me/bot/alerts"+query, token, "", nil)
		var got alertsBody
		if s != 200 || json.Unmarshal([]byte(b), &got) != nil {
			t.Fatalf("GET alerts%s: %d %s", query, s, b)
		}
		return got
	}
	ids := func(b alertsBody) string {
		var out []string
		for _, a := range b.Alerts {
			out = append(out, a.ID)
		}
		return strings.Join(out, ",")
	}
	if got := list(""); ids(got) != "a3,a2,a1" || got.Open != 2 {
		t.Fatalf("list: %s open %d", ids(got), got.Open)
	}
	if got := list("?limit=1"); ids(got) != "a3" || got.Open != 2 {
		t.Fatalf("limit 1: %s open %d", ids(got), got.Open)
	}
	for _, q := range []string{"?limit=0", "?limit=201", "?limit=x"} {
		if s, b := request(t, srv, "GET", "/api/me/bot/alerts"+q, token, "", nil); s != 400 || !strings.Contains(b, `"bad_query"`) {
			t.Fatalf("%s: %d %s", q, s, b)
		}
	}

	app := appSocket(t, srv, token)
	s, b := request(t, srv, "POST", "/api/me/bot/alerts/a1/dismiss", token, `{}`, nil)
	var dismissed botAlert
	if s != 200 || json.Unmarshal([]byte(b), &dismissed) != nil || dismissed.ID != "a1" || dismissed.DismissedAt == 0 {
		t.Fatalf("dismiss: %d %s", s, b)
	}
	if m := readJSON(t, app); m["type"] != "alert" || m["alert"].(map[string]any)["id"] != "a1" || m["alert"].(map[string]any)["dismissedAt"] == float64(0) {
		t.Fatalf("app got %v", m)
	}
	if s, b := request(t, srv, "POST", "/api/me/bot/alerts/b1/dismiss", token, `{}`, nil); s != 404 || !strings.Contains(b, `"unknown_alert"`) {
		t.Fatalf("another user's alert: %d %s", s, b)
	}
	if got := list(""); ids(got) != "a3,a2" || got.Open != 1 {
		t.Fatalf("after dismiss: %s open %d", ids(got), got.Open)
	}
	if s, b := request(t, srv, "POST", "/api/me/bot/alerts/dismiss-all", token, `{}`, nil); s != 200 || strings.TrimSpace(b) != `{"dismissed":2}` {
		t.Fatalf("dismiss all: %d %s", s, b)
	}
	for i := 0; i < 2; i++ {
		if m := readJSON(t, app); m["type"] != "alert" {
			t.Fatalf("app got %v", m)
		}
	}
	if got := list(""); len(got.Alerts) != 0 || got.Open != 0 {
		t.Fatalf("after dismiss all: %+v", got)
	}
	if s, _ := request(t, srv, "GET", "/api/me/bot/alerts", "", "", bearer(newCode(t, srv, token, "bot"))); s != 403 {
		t.Fatalf("bot code: %d", s)
	}
}

func TestBotAlertsAPI_Settings(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	bots.alerts = newAlertEngine(time.Now())
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	defaults := `{"attention":{"app":true,"telegram":true},"window":{"app":true,"telegram":true},"agent":{"app":true,"telegram":false},"order":{"app":true,"telegram":false}}`
	if s, b := request(t, srv, "GET", "/api/me/bot/alert-settings", token, "", nil); s != 200 || strings.TrimSpace(b) != defaults {
		t.Fatalf("defaults: %d %s", s, b)
	}
	put := `{"attention":{"app":true,"telegram":false},"window":{"app":false,"telegram":true},"agent":{"app":true,"telegram":true},"order":{"app":true,"telegram":true}}`
	if s, b := request(t, srv, "PUT", "/api/me/bot/alert-settings", token, put, nil); s != 200 || strings.TrimSpace(b) != put {
		t.Fatalf("put: %d %s", s, b)
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/alert-settings", token, "", nil); s != 200 || strings.TrimSpace(b) != put {
		t.Fatalf("after put: %d %s", s, b)
	}
	bots.mu.Lock()
	cached := bots.alerts.settings[uid]
	bots.mu.Unlock()
	if !cached.Order.Telegram || cached.Window.App {
		t.Fatalf("engine cache: %+v", cached)
	}
	for name, body := range map[string]string{
		"a kind missing":   `{"attention":{"app":true,"telegram":true},"window":{"app":true,"telegram":true},"agent":{"app":true,"telegram":false}}`,
		"a switch missing": `{"attention":{"app":true},"window":{"app":true,"telegram":true},"agent":{"app":true,"telegram":false},"order":{"app":true,"telegram":false}}`,
		"not a boolean":    `{"attention":{"app":"yes","telegram":true},"window":{"app":true,"telegram":true},"agent":{"app":true,"telegram":false},"order":{"app":true,"telegram":false}}`,
		"not JSON":         `{`,
	} {
		if s, b := request(t, srv, "PUT", "/api/me/bot/alert-settings", token, body, nil); s != 400 || !strings.Contains(b, `"bad_alert_settings"`) {
			t.Fatalf("%s: %d %s", name, s, b)
		}
	}
}
