package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

type ordersAnswer struct {
	Running []orderView `json:"running"`
	Waiting []orderView `json:"waiting"`
	History []orderView `json:"history"`
	Today   botToday    `json:"today"`
}

func getOrders(t *testing.T, srv *httptest.Server, token, query string) ordersAnswer {
	t.Helper()
	s, b := request(t, srv, "GET", "/api/me/bot/orders"+query, token, "", nil)
	var a ordersAnswer
	if err := json.Unmarshal([]byte(b), &a); s != 200 || err != nil || a.Running == nil || a.Waiting == nil || a.History == nil {
		t.Fatalf("GET orders%s: %d %s", query, s, b)
	}
	return a
}

func TestBotOrders_RunningWaitingAndHistory(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 13, 11, 0, 0, 0, time.UTC)} // 12:00 in Prague
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	app := appSocket(t, srv, token)
	code, codeID := newBotCode(t, srv, token, "Home PC")
	bot := startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 2, "idle")), app)

	// Yesterday (Prague): an order that ended.
	if err := svc.InsertBotOrder(accounts.BotOrderRow{ID: "00000000000000aa", UserID: uid, ArbKey: "old", State: "waiting", DryRun: true,
		Stake: 10, InfoJSON: `{"match":"Old","market":"","live":false,"profitPct":1}`, LegsJSON: "[]",
		CreatedAt: time.Date(2026, 11, 12, 22, 59, 0, 0, time.UTC).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.EndBotOrder("00000000000000aa", "placed", "", "[]", 10, 1); err != nil {
		t.Fatal(err)
	}
	ended := placeOrder(t, srv, token, "arb-1", bot, app)
	clk.advance(time.Second)
	if s, b := postEvent(t, srv, code, `{"order_id":"`+ended.OrderID+`","event":"stopped","reason":"odds dropped"}`); s != 204 {
		t.Fatalf("final: %d %s", s, b)
	}
	expectEvent(t, app, ended.OrderID, "stopped", "")
	expectBot(t, app, "after the final event")
	clk.advance(time.Second)
	body := `{"arb_key":"arb-2","legs":[` + legJSON("tipsport", "s1", 2.1, 100) + `,` + legJSON("betano", "s2", 2.05, 102.4) +
		`],"match":"Lyon – Lille","market":"Corners 10.5","live":true,"profit_pct":2.4}`
	running := postOrderOK(t, srv, token, body)
	readOrder(t, bot)
	expectEvent(t, app, running.OrderID, "sent", "")
	expectBot(t, app, "on the sent order")
	clk.advance(time.Second)
	if s, b := postEvent(t, srv, code, `{"order_id":"`+running.OrderID+`","event":"leg_step","bookmaker":"betano","step":"checkOdds"}`); s != 204 {
		t.Fatalf("leg_step: %d %s", s, b)
	}
	expectEvent(t, app, running.OrderID, "leg_step", "")
	waiting := postOrderOK(t, srv, token, tipBetOrder("arb-3"))

	got := getOrders(t, srv, token, "")
	if len(got.Running) != 1 || len(got.Waiting) != 1 || len(got.History) != 1 || got.Today != (botToday{Orders: 3, Placed: 0}) {
		t.Fatalf("orders: %+v", got)
	}
	r := got.Running[0]
	sent := clk.now().Add(-time.Second).UnixMilli()
	want := orderView{ID: running.OrderID, ArbKey: "arb-2", State: "running", DryRun: true, AgentID: codeID, Agent: "Home PC",
		Match: "Lyon – Lille", Market: "Corners 10.5", Live: true, ProfitPct: 2.4, Stake: 202, CreatedAt: sent, SentAt: sent,
		Legs: []orderLegView{{Bookmaker: "tipsport", WindowID: "tipsport-1", Odds: 2.1, Stake: 100},
			{Bookmaker: "betano", WindowID: "betano-2", Odds: 2.05, Stake: 102.4, Step: "checkOdds", StepAt: clk.now().UnixMilli()}}}
	if rj, wj := mustJSON(r), mustJSON(want); rj != wj {
		t.Fatalf("running:\n got %s\nwant %s", rj, wj)
	}
	if w := got.Waiting[0]; w.ID != waiting.OrderID || w.State != "waiting" || w.Reason != "Orders at once: 1 running" || w.AgentID != 0 ||
		w.Agent != "" || w.SentAt != 0 || w.Legs[0].WindowID != "" {
		t.Fatalf("waiting: %+v", w)
	}
	if h := got.History[0]; h.ID != ended.OrderID || h.State != "stopped" || h.Reason != "odds dropped" || h.Agent != "Home PC" ||
		h.AgentID != codeID || h.EndedAt == 0 || h.Legs[0].WindowID != "tipsport-1" || h.Legs[1].WindowID != "betano-2" {
		t.Fatalf("history: %+v", h)
	}
	// Another day: its history only; running and waiting are always now.
	got = getOrders(t, srv, token, "?day=2026-11-12")
	if len(got.History) != 1 || got.History[0].ID != "00000000000000aa" || got.History[0].Match != "Old" || len(got.Running) != 1 ||
		got.Today.Orders != 3 {
		t.Fatalf("yesterday: %+v", got)
	}
	for _, q := range []string{"?day=2026-13-01", "?day=yesterday", "?day=2026-11-1"} {
		if s, b := request(t, srv, "GET", "/api/me/bot/orders"+q, token, "", nil); s != 400 || !strings.Contains(b, `"bad_query"`) {
			t.Errorf("%s: %d %s", q, s, b)
		}
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/orders", "", "", bearer(code)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("with a bot code: %d %s", s, b)
	}
}

func TestBotOrders_Cancel(t *testing.T) {
	srv, svc, _, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startBot(t, srv, code, app)
	running := placeOrder(t, srv, token, "arb-1", bot, app)
	waiting := postOrderOK(t, srv, token, tipBetOrder("arb-2"))
	expectEvent(t, app, waiting.OrderID, "waiting", "")
	expectBot(t, app, "on the waiting order")
	del := func(id, token string, extra map[string]string) (int, string) {
		return request(t, srv, "DELETE", "/api/me/bot/orders/"+id, token, "", extra)
	}
	if s, b := del(running.OrderID, token, nil); s != 409 || !strings.Contains(b, `"order_running"`) {
		t.Fatalf("running: %d %s", s, b)
	}
	other := newUser(t, svc, "o@example.com", accounts.RoleUser)
	if s, b := del(waiting.OrderID, other, nil); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("another user's: %d %s", s, b)
	}
	if s, b := del(waiting.OrderID, token, map[string]string{"Origin": "http://evil.example"}); s != 403 || !strings.Contains(b, `"bad_origin"`) {
		t.Fatalf("cross-site: %d %s", s, b)
	}
	if s, b := del(waiting.OrderID, token, nil); s != 204 {
		t.Fatalf("cancel: %d %s", s, b)
	}
	expectEvent(t, app, waiting.OrderID, "stopped", "Cancelled")
	expectBot(t, app, "on the cancelled order")
	if state, reason, _, _, _ := orderRow(t, db, waiting.OrderID); state != "stopped" || reason != "Cancelled" {
		t.Fatalf("row: %s %s", state, reason)
	}
	if s, b := del(waiting.OrderID, token, nil); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("ended: %d %s", s, b)
	}
	if s, b := del("0123456789abcdef", token, nil); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("unknown: %d %s", s, b)
	}
}
