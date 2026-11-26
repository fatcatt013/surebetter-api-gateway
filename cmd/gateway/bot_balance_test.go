package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// The enough-money rule: an exempt agent's window always pays; a protocol-7 window needs a fresh known balance of at
// least the stake (12 min inclusive).
func TestBotBalance_CanPay(t *testing.T) {
	b := newBotHub(nil, nil)
	clk := &botClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	b.now = clk.now
	now := clk.now().UnixMilli()
	cases := []struct {
		name  string
		proto int
		bal   *botBalance
		stake float64
		want  bool
	}{
		{"protocol 6 without a balance", 6, nil, 500, true},
		{"protocol 7 null", 7, &botBalance{Null: true}, 500, false},
		{"protocol 7 none", 7, nil, 500, false},
		{"fresh, exactly the stake", 7, &botBalance{Amount: 500, At: now}, 500, true},
		{"fresh, more", 7, &botBalance{Amount: 4230.5, At: now}, 500, true},
		{"fresh, low", 7, &botBalance{Amount: 499.99, At: now}, 500, false},
		{"12 min old", 7, &botBalance{Amount: 1000, At: now - balanceFreshMs}, 500, true},
		{"12 min and 1 ms old", 7, &botBalance{Amount: 1000, At: now - balanceFreshMs - 1}, 500, false},
		{"stale but exempt", 6, &botBalance{Amount: 1000, At: 0}, 500, true},
	}
	for _, c := range cases {
		p := &botPresence{proto: c.proto}
		if got := b.canPay(p, &botWindow{Balance: c.bal}, c.stake); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Two agents, one exempt (protocol 6) and one of protocol 7 whose Tipsport window cannot pay: the order goes to the
// exempt one, although the other would be picked first (it connected first).
func TestBotSched_BalanceMixedAgents(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	now := time.Now().UnixMilli()
	a, _ := newBotCode(t, srv, token, "A")
	b, _ := newBotCode(t, srv, token, "B")
	botA := startAgentWith(t, srv, a, protoStatus(7, winBal("tipsport", 1, "idle", bal(50, now)), winBal("betano", 1, "idle", bal(5000, now))), app)
	botB := startAgentWith(t, srv, b, protoStatus(6, win("tipsport", 1, "idle"), win("betano", 1, "idle")), app)
	ans := postOrderOK(t, srv, token, tipBetOrder("arb-1")) // 100 / 102
	if ans.State != "sent" {
		t.Fatalf("order %+v, want sent", ans)
	}
	if o := readOrder(t, botB); o.OrderID != ans.OrderID {
		t.Fatalf("exempt agent got %+v", o)
	}
	expectNoMessage(t, botA, "the agent that cannot pay")
}

// An app order waits for money with the §3.2 texts and goes on the status whose fresh read covers the stake.
func TestBotSched_WaitsForMoneyThenGoes(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	now := clk.now().UnixMilli()
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	tip := func(balance string) string {
		return protoStatus(7, winBal("tipsport", 1, "idle", balance), winBal("tipsport", 2, "idle", bal(100, now)),
			winBal("betano", 1, "idle", bal(5000, now)))
	}
	bot := startAgentWith(t, srv, code, tip(bal(412.9, now)), app)
	a := postOrderOK(t, srv, token, orderJSON("arb-1", legJSON("tipsport", "s1", 2.1, 500), legJSON("betano", "s2", 2.05, 102)))
	if a.State != "waiting" || a.Reason != "Not enough money at Tipsport (412 Kč)" {
		t.Fatalf("order %+v", a)
	}
	expectEvent(t, app, a.OrderID, "waiting", "Not enough money at Tipsport (412 Kč)")
	expectBot(t, app, "on the waiting order")
	post := func(body string) {
		t.Helper()
		if s, b := postStatus(t, srv, code, body); s != 204 {
			t.Fatalf("status: %d %s", s, b)
		}
	}
	// The largest fresh balance of the book's free windows; a stale one does not count.
	post(protoStatus(7, winBal("tipsport", 1, "idle", bal(12000.5, now-balanceFreshMs-1)), winBal("tipsport", 2, "idle", bal(499, now)),
		winBal("betano", 1, "idle", bal(5000, now))))
	expectEvent(t, app, a.OrderID, "waiting", "Not enough money at Tipsport (499 Kč)")
	post(tip("null"))
	expectEvent(t, app, a.OrderID, "waiting", "Not enough money at Tipsport (100 Kč)")
	post(protoStatus(7, winBal("tipsport", 1, "idle", "null"), winBal("tipsport", 2, "idle", bal(12000, now-balanceFreshMs-1)),
		winBal("betano", 1, "idle", bal(5000, now))))
	expectEvent(t, app, a.OrderID, "waiting", "Tipsport balance unknown")
	// A covering fresh read: it goes, on that window.
	clk.advance(time.Second)
	post(tip(bal(500, clk.now().UnixMilli())))
	expectEvent(t, app, a.OrderID, "sent", "")
	if o := readOrder(t, bot); o.OrderID != a.OrderID || o.Legs[0].WindowID != "tipsport-1" {
		t.Fatalf("agent got %+v", o)
	}
}

// status7 posts the agent's windows as a protocol-7 status.
func (e *autoEnv) status7(windows ...string) {
	e.t.Helper()
	if s, b := postStatus(e.t, e.srv, e.code, protoStatus(7, windows...)); s != 204 {
		e.t.Fatalf("status: %d %s", s, b)
	}
}

// The picker's balance reasons in place of no_free_window (Dry run and Log only alike), the first leg first; an
// exempt agent sends.
func TestAuto_BalanceReasons(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent()
	now := e.clk.now().UnixMilli()
	// 2.1 / 2.1 split 25 000: 12 500 / 12 500.
	e.setAuto("dry", "", `{"mode":"split","total":25000,"max_leg":0,"anchor_book":null,"rounding":1}`, "")
	tip := func(state, balance string) string { return winBal("tipsport", 1, state, balance) }
	fort := func(state, balance string) string { return winBal("fortuna", 1, state, balance) }
	steps := []struct {
		tip, fort    string
		reason, text string
	}{
		{tip("idle", bal(12000.7, now)), fort("idle", bal(20000, now)), "balance_low", "Not enough money at Tipsport: 12 000 < 12 500 Kč"},
		{tip("idle", "null"), fort("idle", bal(20000, now)), "balance_unknown", "Tipsport balance unknown"},
		{tip("idle", bal(20000, now-balanceFreshMs-1)), fort("idle", bal(20000, now)), "balance_unknown", "Tipsport balance unknown"},
		{tip("idle", "null"), fort("idle", bal(10, now)), "balance_unknown", "Tipsport balance unknown"},
		{tip("idle", bal(412, now)), fort("idle", "null"), "balance_low", "Not enough money at Tipsport: 412 < 12 500 Kč"},
		{tip("busy", bal(20000, now)), fort("idle", bal(10, now)), "balance_low", "Not enough money at Fortuna: 10 < 12 500 Kč"},
		{tip("busy", bal(20000, now)), fort("idle", bal(20000, now)), "no_free_window", "No free Tipsport window"},
	}
	for i, s := range steps {
		e.status7(s.tip, s.fort)
		e.expect(e.arb2(fmt.Sprintf("tennis-live:s%d", i), "T", 1), s.reason, s.text)
	}
	if e.orderCount() != 0 {
		t.Fatal("no order is kept for a window that cannot pay")
	}
	// Log only: the same rule.
	e.bots.autoPhase = "A"
	e.setAuto("log", "", `{"mode":"split","total":25000,"max_leg":0,"anchor_book":null,"rounding":1}`, "")
	e.status7(tip("idle", bal(12000, now)), fort("idle", bal(20000, now)))
	e.expect(e.arb2("tennis-live:log", "T", 1), "balance_low", "Not enough money at Tipsport: 12 000 < 12 500 Kč")
	e.status7(tip("idle", bal(12500, now)), fort("idle", bal(12500, now)))
	if l := e.expect(e.arb2("tennis-live:log2", "T", 1), "", ""); l.Kind != "would_send" {
		t.Fatalf("exactly the stake: %+v", l)
	}
	// Dry run with covering balances: sent. An exempt agent (protocol 6) sends without balances.
	e.bots.autoPhase = "B"
	e.setAuto("dry", "", `{"mode":"split","total":25000,"max_leg":0,"anchor_book":null,"rounding":1}`, "")
	e.sendNext(e.arb2("tennis-live:ok", "T", 1))
	e2 := newAutoEnv(t, "B")
	e2.agent()
	if s, b := postStatus(t, e2.srv, e2.code, protoStatus(6, tip("idle", bal(1, 0)), fort("idle", "null"))); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	e2.setAuto("dry", "", `{"mode":"split","total":25000,"max_leg":0,"anchor_book":null,"rounding":1}`, "")
	e2.sendNext(e2.arb2("tennis-live:ok", "T", 1))
}

// limits.placedPerDay: 0–1000 whole, missing = 0 (off); placed auto orders of the class count (stopped,
// outcome_unknown, the other class and app orders do not); checked right after auto_day_count.
func TestAuto_PlacedPerDay(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(tipFortIdle...)
	for _, l := range []string{`{"limits":{"placedPerDay":1001}}`, `{"limits":{"placedPerDay":-1}}`, `{"limits":{"placedPerDay":2.5}}`} {
		if s, b := e.put(l); s != 400 || !strings.Contains(b, `"bad_auto"`) {
			t.Errorf("%s: %d %s, want 400 bad_auto", l, s, b)
		}
	}
	if s, b := e.put(`{"limits":{"atOnce":1}}`); s != 200 || !strings.Contains(b, `"perDay":20,"placedPerDay":0,`) {
		t.Fatalf("missing placedPerDay: %d %s", s, b)
	}
	lim := func(perDay, placed int) string {
		return fmt.Sprintf(`{"atOnce":1,"perDay":%d,"placedPerDay":%d,"stakePerDay":0,"gapSec":0,"books":[],"perBook":{}}`, perDay, placed)
	}
	e.setAuto("dry", "", "", lim(20, 2))
	if s, b := request(t, e.srv, "GET", "/api/me/bot/auto", e.token, "", nil); s != 200 || !strings.Contains(b, `"placedPerDay":2`) {
		t.Fatalf("GET: %d %s", s, b)
	}
	rec := func(id, source string, dry bool, state string) {
		e.injectRec(autoRec{id: id, source: source, eventID: "tennis-live:" + id, arbKey: "tennis-live:" + id + ":T:1", dry: dry, state: state})
	}
	rec("dryplac1", "auto", true, "placed")
	rec("drystop1", "auto", true, "stopped")
	rec("dryunkn1", "auto", true, "outcome_unknown")
	rec("drycash1", "auto", true, "cashed_out")
	rec("liveplc1", "auto", false, "placed")
	rec("dryapp01", "app", true, "placed")
	o := e.sendNext(e.arb2("tennis-live:aa", "T", 1)) // one placed of class dry: under 2
	e.event(`{"order_id":"` + o["order_id"].(string) + `","event":"placed"}`)
	e.expect(e.arb2("tennis-live:bb", "T", 1), "auto_day_placed", "Placed arbs today: 2")
	e.setAuto("dry", "", "", lim(5, 2)) // 5 auto orders of the class today: auto_day_count first
	e.expect(e.arb2("tennis-live:cc", "T", 1), "auto_day_count", "Auto orders today: 5")
	e.setAuto("dry", "", "", lim(20, 0)) // off
	e.sendNext(e.arb2("tennis-live:dd", "T", 1))
	if c := e.counts()["auto_day_placed"]; c != 1 {
		t.Fatalf("counts %v", e.counts())
	}
}
