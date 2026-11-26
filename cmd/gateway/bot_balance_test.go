package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// winBal is a window (as win) with a balance field: raw JSON ("null", `{"amount":…,"at":…}`); "" = no field.
func winBal(book string, number int, state, balance string) string {
	w := win(book, number, state)
	if balance == "" {
		return w
	}
	return strings.TrimSuffix(w, "}") + `,"balance":` + balance + `}`
}

// protoStatus is a status of protocol proto with windows.
func protoStatus(proto int, windows ...string) string {
	return fmt.Sprintf(`{"proto":%d,"books":[],"dryRun":true,"windows":[%s]}`, proto, strings.Join(windows, ","))
}

// bal is a balance object read at at (Unix ms).
func bal(amount float64, at int64) string { return fmt.Sprintf(`{"amount":%v,"at":%d}`, amount, at) }

// rawFleetWindows is each window of the first agent in GET /api/me/bot/agents, field by field as raw JSON.
func rawFleetWindows(t *testing.T, srv *httptest.Server, token string) []map[string]json.RawMessage {
	t.Helper()
	s, body := request(t, srv, "GET", "/api/me/bot/agents", token, "", nil)
	var res struct {
		Agents []struct {
			Windows []map[string]json.RawMessage `json:"windows"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &res); s != 200 || err != nil || len(res.Agents) == 0 {
		t.Fatalf("GET /api/me/bot/agents: %d %s", s, body)
	}
	return res.Agents[0].Windows
}

// The window balance as the app sees it: an object or null for a protocol-7 agent (a window without the field is
// null), absent for an older agent and an offline one. A change of balance alone does not tell the app.
func TestBotBalance_FleetEncoding(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := dialBot(t, srv, code)
	readType(t, app)
	post := func(body string) {
		t.Helper()
		if s, b := postStatus(t, srv, code, body); s != 204 {
			t.Fatalf("status %s: %d %s", body, s, b)
		}
	}
	post(protoStatus(7, winBal("tipsport", 1, "idle", bal(4230.5, 1795710000000)), winBal("fortuna", 1, "idle", "null"),
		winBal("betano", 1, "idle", "")))
	expectBot(t, app, "first status")
	ws := rawFleetWindows(t, srv, token)
	if len(ws) != 3 || string(ws[0]["balance"]) != `{"amount":4230.5,"at":1795710000000}` || string(ws[1]["balance"]) != "null" ||
		string(ws[2]["balance"]) != "null" {
		t.Fatalf("protocol 7 windows: %s / %s / %s", ws[0]["balance"], ws[1]["balance"], ws[2]["balance"])
	}
	// The balance changes alone: no {"type":"bot"} (checked below, as the last read on app).
	post(protoStatus(7, winBal("tipsport", 1, "idle", bal(12000, 1795710060000)), winBal("fortuna", 1, "idle", bal(10, 1795710060000)),
		winBal("betano", 1, "idle", "null")))
	if ws := rawFleetWindows(t, srv, token); string(ws[1]["balance"]) != `{"amount":10,"at":1795710060000}` {
		t.Fatalf("second status: %s", ws[1]["balance"])
	}
	// A negative amount or time is a bad status.
	for _, b := range []string{bal(-1, 1), bal(1, -1), `"412 Kč"`} {
		if s, body := postStatus(t, srv, code, protoStatus(7, winBal("tipsport", 1, "idle", b))); s != 400 || !strings.Contains(body, "bad_status") {
			t.Fatalf("balance %s: %d %s", b, s, body)
		}
	}
	expectNoMessage(t, app, "app socket after a balance-only change")
	app.Close(websocket.StatusNormalClosure, "")

	// Protocol 6: the field is never shown, even when sent.
	app = appSocket(t, srv, token)
	post(protoStatus(6, winBal("tipsport", 1, "idle", bal(5, 1)), winBal("fortuna", 1, "idle", "null")))
	readType(t, app)
	for i, w := range rawFleetWindows(t, srv, token) {
		if _, ok := w["balance"]; ok {
			t.Fatalf("protocol 6 window %d has a balance: %s", i, w["balance"])
		}
	}
	// Offline: no balance.
	post(protoStatus(7, winBal("tipsport", 1, "idle", bal(5, 1))))
	readType(t, app)
	bot.Close(websocket.StatusNormalClosure, "")
	readType(t, app)
	for i, w := range rawFleetWindows(t, srv, token) {
		if _, ok := w["balance"]; ok || string(w["state"]) != `"down"` {
			t.Fatalf("offline window %d: %s %s", i, w["state"], w["balance"])
		}
	}
}
