package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// win is one window of a protocol-2 status, in bot mode.
func win(book string, number int, state string) string {
	return fmt.Sprintf(`{"id":"%s-%d","bookmaker":%q,"number":%d,"device":"desktop","state":%q,"mode":"bot","extension":true,"loggedIn":null,"proxy":null,"reason":""}`,
		book, number, book, number, state)
}

// p2Status is a protocol-2 status with boot ("" = none) and windows; the agent's own switch is dry run.
func p2Status(boot string, windows ...string) string {
	b := ""
	if boot != "" {
		b = `,"boot":"` + boot + `"`
	}
	return `{"proto":2,"books":[],"dryRun":true` + b + `,"windows":[` + strings.Join(windows, ",") + `]}`
}

// tipBetIdle is a protocol-2 status with tipsport-1 and betano-1 idle.
var tipBetIdle = p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "idle"))

// startAgentWith connects code's agent, posts status, and reads the two {"type":"bot"} messages that
// causes on the user's app socket.
func startAgentWith(t *testing.T, srv *httptest.Server, code, status string, app *websocket.Conn) *websocket.Conn {
	t.Helper()
	bot := dialBot(t, srv, code)
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on agent connect: %s", got)
	}
	if s, b := postStatus(t, srv, code, status); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on status: %s", got)
	}
	return bot
}

// startBot connects code's agent with tipsport-1 and betano-1 idle (protocol 2).
func startBot(t *testing.T, srv *httptest.Server, code string, app *websocket.Conn) *websocket.Conn {
	t.Helper()
	return startAgentWith(t, srv, code, tipBetIdle, app)
}

// tipBetOrder is a valid tipsport/betano order body with arbKey.
func tipBetOrder(arbKey string) string {
	return orderJSON(arbKey, legJSON("tipsport", "s1", 2.1, 100), legJSON("betano", "s2", 2.05, 102))
}

// orderAnswer is the 202 body of POST /api/me/bot/orders.
type orderAnswer struct {
	OrderID string `json:"order_id"`
	State   string `json:"state"`
	Reason  string `json:"reason"`
}

// postOrderOK posts body and expects 202.
func postOrderOK(t *testing.T, srv *httptest.Server, token, body string) orderAnswer {
	t.Helper()
	s, b := postOrder(t, srv, token, body)
	var a orderAnswer
	if json.Unmarshal([]byte(b), &a); s != 202 || len(a.OrderID) != 16 {
		t.Fatalf("order: %d %s", s, b)
	}
	return a
}

// readOrder reads the next message on the agent's socket as an order.
func readOrder(t *testing.T, bot *websocket.Conn) botOrderMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := bot.Read(ctx)
	if err != nil {
		t.Fatalf("agent read: %v", err)
	}
	var m struct {
		Type  string      `json:"type"`
		Order botOrderMsg `json:"order"`
	}
	if json.Unmarshal(b, &m); m.Type != "order" {
		t.Fatalf("agent got %s, want an order", b)
	}
	return m.Order
}

// expectEvent reads the next app message and checks it is a botEvent for id with event (and reason, when not "").
func expectEvent(t *testing.T, app *websocket.Conn, id, event, reason string) map[string]any {
	t.Helper()
	m := readJSON(t, app)
	if m["type"] != "botEvent" || m["order_id"] != id || m["event"] != event || (reason != "" && m["reason"] != reason) {
		t.Fatalf("app got %v, want botEvent %s %s %q", m, id, event, reason)
	}
	return m
}

// expectBot reads the next app message and checks it is {"type":"bot"}.
func expectBot(t *testing.T, app *websocket.Conn, what string) {
	t.Helper()
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app %s: %s, want bot", what, got)
	}
}

// placeOrder posts a tipsport/betano order with arbKey that goes at once, reads it on the agent's
// socket and the botEvent "sent" and {"type":"bot"} on the app's, and returns the agent's order.
func placeOrder(t *testing.T, srv *httptest.Server, token, arbKey string, bot, app *websocket.Conn) botOrderMsg {
	t.Helper()
	a := postOrderOK(t, srv, token, tipBetOrder(arbKey))
	if a.State != "sent" || a.Reason != "" {
		t.Fatalf("order %s: %+v, want sent", arbKey, a)
	}
	o := readOrder(t, bot)
	if o.OrderID != a.OrderID {
		t.Fatalf("agent got order %s, want %s", o.OrderID, a.OrderID)
	}
	expectEvent(t, app, a.OrderID, "sent", "")
	expectBot(t, app, "on the sent order")
	return o
}

// getView decodes GET /api/me/bot.
func getView(t *testing.T, srv *httptest.Server, token string) botView {
	t.Helper()
	var v botView
	if err := json.Unmarshal([]byte(getBot(t, srv, token)), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// setSettings stores userID's settings and drops the hub's cached copy.
func setSettings(t *testing.T, svc *accounts.Service, bots *botHub, userID int64, change func(*accounts.BotSettings)) {
	t.Helper()
	st, err := svc.BotSettings(userID)
	if err != nil {
		t.Fatal(err)
	}
	change(&st)
	if err := svc.SaveBotSettings(userID, st); err != nil {
		t.Fatal(err)
	}
	bots.mu.Lock()
	delete(bots.settings, userID)
	bots.mu.Unlock()
}

// orderRow reads one bot_orders row's state, reason, code id, dry run and legs.
func orderRow(t *testing.T, db *sql.DB, id string) (state, reason string, codeID int64, dryRun bool, legs []storedLeg) {
	t.Helper()
	var js string
	if err := db.QueryRow(`SELECT state, reason, code_id, dry_run, legs_json FROM bot_orders WHERE id = ?`, id).
		Scan(&state, &reason, &codeID, &dryRun, &js); err != nil {
		t.Fatalf("row %s: %v", id, err)
	}
	json.Unmarshal([]byte(js), &legs)
	return
}

func TestBot_StatusAndPresence(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	app := appSocket(t, srv, token)

	if got := getBot(t, srv, token); got != `{"online":false,"books":[],"dryRun":true,"busy":false,"update":false,"paused":false,"pausedReason":"",`+
		`"running":0,"waiting":0,"ordersAtOnce":1,"today":{"orders":0,"placed":0}}` {
		t.Fatalf("offline: %s", got)
	}
	if s, b := postStatus(t, srv, code, `{"books":[],"dryRun":true}`); s != 409 || !strings.Contains(b, `"bot_offline"`) {
		t.Fatalf("status without a bot socket: %d %s", s, b)
	}
	bot := dialBot(t, srv, code)
	expectBot(t, app, "on bot connect")
	// Online without windows: nothing can take an order, so busy; no protocol-2 agent, so update.
	if v := getView(t, srv, token); !v.Online || !v.Busy || !v.Update || len(v.Books) != 0 || !v.DryRun {
		t.Fatalf("online before any status: %+v", v)
	}
	// A Phase 1 status (books only, no proto): its books are listed, none ready.
	if s, b := postStatus(t, srv, code, `{"books":[{"bookmaker":"tipsport","ready":true}],"dryRun":false}`); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	expectBot(t, app, "on status change")
	if v := getView(t, srv, token); len(v.Books) != 1 || v.Books[0] != (botBookView{"tipsport", false, false}) || !v.Busy || !v.Update || !v.DryRun {
		t.Fatalf("Phase 1 agent: %+v (dryRun is the setting, not the agent's switch)", v)
	}
	// Protocol 2: an idle window makes its bookmaker ready and usable, a busy one usable, a down one neither.
	if s, b := postStatus(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "down"), win("fortuna", 1, "busy"))); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	expectBot(t, app, "on status change")
	if v := getView(t, srv, token); v.Busy || v.Update || len(v.Books) != 3 || v.Books[0] != (botBookView{"betano", false, false}) ||
		v.Books[1] != (botBookView{"fortuna", false, true}) || v.Books[2] != (botBookView{"tipsport", true, true}) {
		t.Fatalf("protocol 2: %+v", v)
	}
	// The same status again is not a change.
	if s, _ := postStatus(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "down"), win("fortuna", 1, "busy"))); s != 204 {
		t.Fatalf("repeated status: %d", s)
	}
	expectNoMessage(t, app, "app socket after an unchanged status")
	expectNoMessage(t, bot, "the bot socket (never gets bot messages)")
}

// update is true only while every online agent is older than protocol 2.
func TestBot_UpdateWhenEveryOnlineAgentIsOld(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	old, _ := newBotCode(t, srv, token, "Old PC")
	startAgentWith(t, srv, old, `{"books":[{"bookmaker":"tipsport","ready":true}],"dryRun":true}`, app)
	if v := getView(t, srv, token); !v.Update {
		t.Fatalf("only an old agent: %+v", v)
	}
	current, _ := newBotCode(t, srv, token, "New PC")
	startAgentWith(t, srv, current, p2Status("", win("tipsport", 1, "busy")), app)
	if v := getView(t, srv, token); v.Update || v.Books[0] != (botBookView{"tipsport", false, true}) {
		t.Fatalf("a protocol-2 agent too: %+v", v)
	}
}

func TestBotSched_OrderGoesWithWindowsAndTheModeSetting(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	app := appSocket(t, srv, token)
	code, codeID := newBotCode(t, srv, token, "Home PC")
	bot := startBot(t, srv, code, app)

	body := `{"arb_key":"football:abc:MATCH_WINNER:0.00","legs":[` + legJSON("tipsport", "s1", 2.1, 100.4) + `,` + legJSON("betano", "s2", 2.05, 102.3) +
		`],"match":"Lyon – Lille","market":"Corners 10.5","live":true,"profit_pct":2.4}`
	a := postOrderOK(t, srv, token, body)
	if a.State != "sent" {
		t.Fatalf("answer: %+v", a)
	}
	o := readOrder(t, bot)
	if o.OrderID != a.OrderID || o.ArbKey != "football:abc:MATCH_WINNER:0.00" || !o.DryRun || len(o.Legs) != 2 ||
		o.Legs[0].Bookmaker != "tipsport" || o.Legs[0].WindowID != "tipsport-1" || o.Legs[0].SourceID != "s1" || o.Legs[0].Stake != 100.4 ||
		o.Legs[1].Bookmaker != "betano" || o.Legs[1].WindowID != "betano-1" {
		t.Fatalf("agent got %+v", o)
	}
	expectEvent(t, app, a.OrderID, "sent", "")
	expectBot(t, app, "on the sent order")
	state, _, rowCode, dryRun, legs := orderRow(t, db, a.OrderID)
	if state != "running" || rowCode != codeID || !dryRun || len(legs) != 2 || legs[0].WindowID != "tipsport-1" || legs[1].WindowID != "betano-1" {
		t.Fatalf("row: %s %d %v %+v", state, rowCode, dryRun, legs)
	}
	var info string
	var stake int64
	db.QueryRow(`SELECT info_json, stake FROM bot_orders WHERE id = ?`, a.OrderID).Scan(&info, &stake)
	if info != `{"match":"Lyon – Lille","market":"Corners 10.5","live":true,"profitPct":2.4}` || stake != 203 {
		t.Fatalf("info %s stake %d", info, stake)
	}
	// Both windows are reserved: no free window, one running.
	if v := getView(t, srv, token); !v.Busy || v.Running != 1 || v.Waiting != 0 || v.Books[0].Ready || v.Books[1].Ready || v.Today.Orders != 1 {
		t.Fatalf("view: %+v", v)
	}

	// Live: the order carries the setting at the moment it is sent, not the agent's switch.
	if s, b := postEvent(t, srv, code, `{"order_id":"`+a.OrderID+`","event":"placed"}`); s != 204 {
		t.Fatalf("placed: %d %s", s, b)
	}
	expectEvent(t, app, a.OrderID, "placed", "")
	expectBot(t, app, "on the final event")
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) { st.DryRun = false })
	if o := placeOrder(t, srv, token, "arb-2", bot, app); o.DryRun {
		t.Fatalf("live setting: %+v", o)
	}
	if v := getView(t, srv, token); v.DryRun || v.Today.Orders != 2 || v.Today.Placed != 1 {
		t.Fatalf("view: %+v", v)
	}
}

func TestBotSched_OnlyProtocol2AgentsWithWindows(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startAgentWith(t, srv, code, `{"books":[{"bookmaker":"tipsport","ready":true},{"bookmaker":"betano","ready":true}],"dryRun":true}`, app)
	if s, b := postOrder(t, srv, token, tipBetOrder("k")); s != 409 || !strings.Contains(b, `"bot_offline"`) {
		t.Fatalf("Phase 1 agent only: %d %s", s, b)
	}
	if s, b := postStatus(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "down"))); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	expectBot(t, app, "on status")
	if s, b := postOrder(t, srv, token, tipBetOrder("k")); s != 409 || !strings.Contains(b, `"book_not_ready"`) {
		t.Fatalf("betano window down: %d %s", s, b)
	}
	if s, b := postOrder(t, srv, token, orderJSON("k", legJSON("tipsport", "s1", 2.1, 100), legJSON("fortuna", "s3", 2, 100))); s != 409 || !strings.Contains(b, `"book_not_ready"`) {
		t.Fatalf("no fortuna window: %d %s", s, b)
	}
	// A busy window can become free: the order waits.
	if s, b := postStatus(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "busy"))); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	expectBot(t, app, "on status")
	a := postOrderOK(t, srv, token, tipBetOrder("k"))
	if a.State != "waiting" || a.Reason != "Needs a free Betano window" {
		t.Fatalf("answer: %+v", a)
	}
	expectEvent(t, app, a.OrderID, "waiting", "Needs a free Betano window")
	expectBot(t, app, "on the waiting order")
	// The next status that frees the window sends it.
	if s, b := postStatus(t, srv, code, tipBetIdle); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	if o := readOrder(t, bot); o.OrderID != a.OrderID || o.Legs[1].WindowID != "betano-1" {
		t.Fatalf("agent got %+v", o)
	}
	expectEvent(t, app, a.OrderID, "sent", "")
	expectBot(t, app, "on the sent order")
}

func TestBotSched_WaitReasonsName(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "busy"), win("sazkabet", 1, "busy")), app)
	a := postOrderOK(t, srv, token, orderJSON("k", legJSON("tipsport", "s1", 2.1, 100), legJSON("sazkabet", "s2", 2.05, 102)))
	if a.State != "waiting" || a.Reason != "Needs a free Tipsport and SazkaBet window" {
		t.Fatalf("both missing: %+v", a)
	}
}

// One order at once (the default): the second waits until the first's final event, then goes.
func TestBotSched_OrdersAtOnceAndTheFinalEventTrigger(t *testing.T) {
	srv, svc, _, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("tipsport", 2, "idle"),
		win("betano", 1, "idle"), win("betano", 2, "idle")), app)
	first := placeOrder(t, srv, token, "arb-1", bot, app)
	second := postOrderOK(t, srv, token, tipBetOrder("arb-2"))
	if second.State != "waiting" || second.Reason != "Orders at once: 1 running" {
		t.Fatalf("second: %+v", second)
	}
	expectEvent(t, app, second.OrderID, "waiting", "Orders at once: 1 running")
	expectBot(t, app, "on the waiting order")
	if v := getView(t, srv, token); v.Running != 1 || v.Waiting != 1 || v.Busy {
		t.Fatalf("view: %+v", v)
	}
	if s, b := postEvent(t, srv, code, `{"order_id":"`+first.OrderID+`","event":"stopped","reason":"odds dropped"}`); s != 204 {
		t.Fatalf("final: %d %s", s, b)
	}
	expectEvent(t, app, first.OrderID, "stopped", "odds dropped")
	expectEvent(t, app, second.OrderID, "sent", "")
	expectBot(t, app, "after the final event")
	// The first order's windows are free again: the second gets the windows idle longest
	// (tipsport-2 and betano-2 have been idle since the status; tipsport-1 and betano-1 since the final event).
	if o := readOrder(t, bot); o.OrderID != second.OrderID || o.Legs[0].WindowID != "tipsport-2" || o.Legs[1].WindowID != "betano-2" {
		t.Fatalf("agent got %+v", o)
	}
	if state, reason, _, _, _ := orderRow(t, db, first.OrderID); state != "stopped" || reason != "odds dropped" {
		t.Fatalf("first row: %s %s", state, reason)
	}
	if state, _, _, _, _ := orderRow(t, db, second.OrderID); state != "running" {
		t.Fatalf("second row: %s", state)
	}
}

func TestBotSched_SeveralAtOnceUseOtherWindows(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	setSettings(t, svc, bots, userIDOf(t, svc, token), func(st *accounts.BotSettings) { st.OrdersAtOnce = 2 })
	app := appSocket(t, srv, token)
	bot := startAgentWith(t, srv, newCode(t, srv, token, "bot"), p2Status("", win("tipsport", 1, "idle"), win("tipsport", 2, "idle"),
		win("betano", 1, "idle"), win("betano", 2, "idle"), win("betano", 3, "idle")), app)
	// Same idle-since for all: the lowest number first; reserved windows are never given twice.
	if o := placeOrder(t, srv, token, "arb-1", bot, app); o.Legs[0].WindowID != "tipsport-1" || o.Legs[1].WindowID != "betano-1" {
		t.Fatalf("first: %+v", o.Legs)
	}
	if o := placeOrder(t, srv, token, "arb-2", bot, app); o.Legs[0].WindowID != "tipsport-2" || o.Legs[1].WindowID != "betano-2" {
		t.Fatalf("second: %+v", o.Legs)
	}
	// Two running = ordersAtOnce: the reason names it although betano-3 is free.
	if a := postOrderOK(t, srv, token, tipBetOrder("arb-3")); a.State != "waiting" || a.Reason != "Orders at once: 2 running" {
		t.Fatalf("third: %+v", a)
	}
	// The same arb again while one is not final.
	if s, b := postOrder(t, srv, token, tipBetOrder("arb-2")); s != 409 || !strings.Contains(b, `"order_exists"`) {
		t.Fatalf("same arb key: %d %s", s, b)
	}
	if s, b := postOrder(t, srv, token, tipBetOrder("arb-3")); s != 409 || !strings.Contains(b, `"order_exists"`) {
		t.Fatalf("same arb key as a waiting order: %d %s", s, b)
	}
}

func TestBotSched_PickTheAgentWithFewestReservedThenLongestUnchanged(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	setSettings(t, svc, bots, userIDOf(t, svc, token), func(st *accounts.BotSettings) { st.OrdersAtOnce = 4 })
	app := appSocket(t, srv, token)
	two := p2Status("", win("tipsport", 1, "idle"), win("tipsport", 2, "idle"), win("betano", 1, "idle"), win("betano", 2, "idle"))
	a, _ := newBotCode(t, srv, token, "A")
	b, _ := newBotCode(t, srv, token, "B")
	aSock := startAgentWith(t, srv, a, two, app)
	clk.advance(time.Second)
	bSock := startAgentWith(t, srv, b, two, app)
	clk.advance(time.Second)

	placeOrder(t, srv, token, "arb-1", aSock, app) // both 0 reserved; A's count has been unchanged longer
	clk.advance(time.Second)
	placeOrder(t, srv, token, "arb-2", bSock, app) // B has fewer reserved
	clk.advance(time.Second)
	placeOrder(t, srv, token, "arb-3", aSock, app) // both 2; A's count changed first
	placeOrder(t, srv, token, "arb-4", bSock, app)
	expectNoMessage(t, aSock, "agent A after its two orders")
}

func TestBotSched_WaitingOrderExpiresAfter20s(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	clk := &botClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "busy")), app)
	a := postOrderOK(t, srv, token, tipBetOrder("arb-1"))
	expectEvent(t, app, a.OrderID, "waiting", "Needs a free Betano window")
	expectBot(t, app, "on the waiting order")

	clk.advance(waitExpiry - time.Millisecond)
	bots.tick()
	if v := getView(t, srv, token); v.Waiting != 1 {
		t.Fatalf("expired before 20 s: %+v", v)
	}
	clk.advance(time.Millisecond)
	defer func(d time.Duration) { orderSweepEvery = d }(orderSweepEvery)
	orderSweepEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { bots.runPrune(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	// No request needed: the tick ends it.
	expectEvent(t, app, a.OrderID, "stopped", "Expired · Needs a free Betano window")
	expectBot(t, app, "on the expired order")
	if state, reason, _, _, _ := orderRow(t, db, a.OrderID); state != "stopped" || reason != "Expired · Needs a free Betano window" {
		t.Fatalf("row: %s %s", state, reason)
	}
	if s, b := postEvent(t, srv, code, `{"order_id":"`+a.OrderID+`","event":"accepted"}`); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("event for the expired order: %d %s", s, b)
	}
}

// A running order without a final event after orderMemory ends outcome_unknown, which pauses.
func TestBotSched_NoResultAfterTenMinutesIsOutcomeUnknownAndPauses(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	clk := &botClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startBot(t, srv, code, app)
	o := placeOrder(t, srv, token, "arb-1", bot, app)

	clk.advance(orderMemory - time.Second)
	if s, b := postEvent(t, srv, code, `{"order_id":"`+o.OrderID+`","event":"accepted"}`); s != 204 {
		t.Fatalf("event within 10 min: %d %s", s, b)
	}
	expectEvent(t, app, o.OrderID, "accepted", "")
	clk.advance(time.Second)
	v := getView(t, srv, token)
	if v.Running != 0 || !v.Paused || v.PausedReason != "Outcome unknown · order "+o.OrderID[:8] {
		t.Fatalf("view: %+v", v)
	}
	expectEvent(t, app, o.OrderID, "outcome_unknown", "No result from the agent")
	expectBot(t, app, "on the ended order")
	if m := readJSON(t, bot); m["type"] != "pause" {
		t.Fatalf("agent got %v, want pause", m)
	}
	if st, _ := svc.BotSettings(uid); !st.Paused || st.PausedReason != "Outcome unknown · order "+o.OrderID[:8] {
		t.Fatalf("stored settings: %+v", st)
	}
	if state, reason, _, _, _ := orderRow(t, db, o.OrderID); state != "outcome_unknown" || reason != "No result from the agent" {
		t.Fatalf("row: %s %s", state, reason)
	}
	if s, b := postOrder(t, srv, token, tipBetOrder("arb-2")); s != 409 || !strings.Contains(b, `"bots_paused"`) {
		t.Fatalf("order while paused: %d %s", s, b)
	}
}

func TestBotSched_PauseOnUnknownOff(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	setSettings(t, svc, bots, userIDOf(t, svc, token), func(st *accounts.BotSettings) { st.PauseOnUnknown = false })
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startBot(t, srv, code, app)
	o := placeOrder(t, srv, token, "arb-1", bot, app)
	if s, b := postEvent(t, srv, code, `{"order_id":"`+o.OrderID+`","event":"outcome_unknown","reason":"no confirmation"}`); s != 204 {
		t.Fatalf("outcome_unknown: %d %s", s, b)
	}
	expectEvent(t, app, o.OrderID, "outcome_unknown", "no confirmation")
	expectBot(t, app, "after the final event")
	if v := getView(t, srv, token); v.Paused {
		t.Fatalf("paused although pauseOnUnknown is off: %+v", v)
	}
	placeOrder(t, srv, token, "arb-2", bot, app)
}

func TestBotSched_EventsAndLegSteps(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	clk := &botClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startBot(t, srv, code, app)
	o := placeOrder(t, srv, token, "arb-1", bot, app)
	ev := func(body string) {
		t.Helper()
		if s, b := postEvent(t, srv, code, body); s != 204 {
			t.Fatalf("%s: %d %s", body, s, b)
		}
	}
	ev(`{"order_id":"` + o.OrderID + `","event":"accepted"}`)
	if m := expectEvent(t, app, o.OrderID, "accepted", ""); m["arb_key"] != "arb-1" || m["step"] != nil || m["reason"] != nil {
		t.Fatalf("accepted: %v", m)
	}
	for name, body := range map[string]string{
		"leg_step without step":      `{"order_id":"` + o.OrderID + `","event":"leg_step","bookmaker":"tipsport"}`,
		"leg_step without bookmaker": `{"order_id":"` + o.OrderID + `","event":"leg_step","step":"navigate"}`,
		"step over 40":               `{"order_id":"` + o.OrderID + `","event":"leg_step","bookmaker":"tipsport","step":"` + strings.Repeat("s", 41) + `"}`,
	} {
		if s, b := postEvent(t, srv, code, body); s != 400 || !strings.Contains(b, `"bad_event"`) {
			t.Errorf("%s: %d %s", name, s, b)
		}
	}
	ev(`{"order_id":"` + o.OrderID + `","event":"leg_step","bookmaker":"tipsport","step":"navigate"}`)
	if m := expectEvent(t, app, o.OrderID, "leg_step", ""); m["bookmaker"] != "tipsport" || m["step"] != "navigate" {
		t.Fatalf("leg_step: %v", m)
	}
	clk.advance(time.Second)
	ev(`{"order_id":"` + o.OrderID + `","event":"leg_step","bookmaker":"tipsport","step":"ready"}`)
	expectEvent(t, app, o.OrderID, "leg_step", "")
	bots.mu.Lock()
	leg := bots.orders[o.OrderID].legs[0]
	bots.mu.Unlock()
	if leg.Step != "ready" || leg.StepAt != clk.now().UnixMilli() {
		t.Fatalf("leg 0 step: %+v", leg)
	}
	ev(`{"order_id":"` + o.OrderID + `","event":"leg_ready","bookmaker":"tipsport"}`)
	expectEvent(t, app, o.OrderID, "leg_ready", "")
	ev(`{"order_id":"` + o.OrderID + `","event":"placed"}`)
	expectEvent(t, app, o.OrderID, "placed", "")
	expectBot(t, app, "after the final event")
	state, _, _, _, legs := orderRow(t, db, o.OrderID)
	if state != "placed" || legs[0].Step != "ready" || legs[0].StepAt != clk.now().UnixMilli() || legs[1].Step != "" {
		t.Fatalf("row: %s %+v", state, legs)
	}
	if s, b := postEvent(t, srv, code, `{"order_id":"`+o.OrderID+`","event":"placed"}`); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("event after the final one: %d %s", s, b)
	}
	expectNoMessage(t, bot, "the bot socket (never gets botEvent)")
}

func TestBotSched_EventErrors(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	ext := newCode(t, srv, token, "extension")
	other, _ := newBotCode(t, srv, token, "Other agent")
	app := appSocket(t, srv, token)
	bot := startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "idle"), win("fortuna", 1, "busy")), app)
	o := placeOrder(t, srv, token, "arb-1", bot, app)
	waiting := postOrderOK(t, srv, token, orderJSON("arb-2", legJSON("tipsport", "s1", 2.1, 100), legJSON("fortuna", "s3", 2, 100)))

	if s, b := postEvent(t, srv, code, `{"order_id":"`+o.OrderID+`","event":"exploded"}`); s != 400 || !strings.Contains(b, `"bad_event"`) {
		t.Fatalf("unknown event: %d %s", s, b)
	}
	if s, b := postEvent(t, srv, code, `{"order_id":"0123456789abcdef","event":"accepted"}`); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("unknown order: %d %s", s, b)
	}
	if s, b := postEvent(t, srv, code, `{"order_id":"`+waiting.OrderID+`","event":"accepted"}`); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("a waiting order (sent to nobody): %d %s", s, b)
	}
	if s, b := postEvent(t, srv, other, `{"order_id":"`+o.OrderID+`","event":"placed"}`); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("another agent of the same user: %d %s", s, b)
	}
	otherUser := newCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "bot")
	if s, b := postEvent(t, srv, otherUser, `{"order_id":"`+o.OrderID+`","event":"placed"}`); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("another user's order: %d %s", s, b)
	}
	if s, b := request(t, srv, "POST", "/api/me/bot/events", token, `{"order_id":"`+o.OrderID+`","event":"placed"}`, nil); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("event with a session: %d %s", s, b)
	}
	if s, b := postEvent(t, srv, ext, `{"order_id":"`+o.OrderID+`","event":"placed"}`); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("event with an extension code: %d %s", s, b)
	}
	if v := getView(t, srv, token); v.Running != 1 {
		t.Fatalf("rejected events must not end the order: %+v", v)
	}
}

// An agent that reconnects mid-order keeps its windows reserved; its final event is still taken.
func TestBotSched_ReconnectKeepsTheReservation(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	setSettings(t, svc, bots, userIDOf(t, svc, token), func(st *accounts.BotSettings) { st.OrdersAtOnce = 2 })
	code := newCode(t, srv, token, "bot")
	app := appSocket(t, srv, token)
	withBoot := p2Status(bootA, win("tipsport", 1, "idle"), win("betano", 1, "idle"))
	bot := startAgentWith(t, srv, code, withBoot, app)
	o := placeOrder(t, srv, token, "arb-1", bot, app)

	bot.Close(websocket.StatusNormalClosure, "")
	expectBot(t, app, "on agent disconnect")
	bot = startAgentWith(t, srv, code, withBoot, app) // same process; its status still says idle
	a := postOrderOK(t, srv, token, tipBetOrder("arb-2"))
	if a.State != "waiting" || a.Reason != "Needs a free Tipsport and Betano window" {
		t.Fatalf("windows still reserved: %+v", a)
	}
	expectEvent(t, app, a.OrderID, "waiting", "")
	expectBot(t, app, "on the waiting order")
	// A status without a boot id ends nothing either (and is no change for the app).
	if s, b := postStatus(t, srv, code, tipBetIdle); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	if s, b := postEvent(t, srv, code, `{"order_id":"`+o.OrderID+`","event":"placed"}`); s != 204 {
		t.Fatalf("final event after reconnect: %d %s", s, b)
	}
	expectEvent(t, app, o.OrderID, "placed", "")
	expectEvent(t, app, a.OrderID, "sent", "")
	expectBot(t, app, "after the final event")
	if got := readOrder(t, bot); got.OrderID != a.OrderID {
		t.Fatalf("agent got %+v", got)
	}
}

// A new boot id ends every running order of that agent as outcome_unknown and pauses.
func TestBotSched_NewBootEndsEveryOrderOfTheAgent(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	setSettings(t, svc, bots, userIDOf(t, svc, token), func(st *accounts.BotSettings) { st.OrdersAtOnce = 2 })
	code := newCode(t, srv, token, "bot")
	app := appSocket(t, srv, token)
	bot := startAgentWith(t, srv, code, p2Status(bootA, win("tipsport", 1, "idle"), win("tipsport", 2, "idle"),
		win("betano", 1, "idle"), win("betano", 2, "idle")), app)
	first := placeOrder(t, srv, token, "arb-1", bot, app)
	second := placeOrder(t, srv, token, "arb-2", bot, app)

	if s, b := postStatus(t, srv, code, p2Status(bootB, win("tipsport", 1, "idle"), win("betano", 1, "idle"))); s != 204 {
		t.Fatalf("status from the restarted process: %d %s", s, b)
	}
	expectEvent(t, app, first.OrderID, "outcome_unknown", "agent restarted")
	expectEvent(t, app, second.OrderID, "outcome_unknown", "agent restarted")
	expectBot(t, app, "after the ended orders")
	if m := readJSON(t, bot); m["type"] != "pause" {
		t.Fatalf("agent got %v, want pause", m)
	}
	v := getView(t, srv, token)
	if v.Running != 0 || !v.Paused || v.PausedReason != "Outcome unknown · order "+first.OrderID[:8] {
		t.Fatalf("view: %+v", v)
	}
	for _, id := range []string{first.OrderID, second.OrderID} {
		if state, reason, _, _, _ := orderRow(t, db, id); state != "outcome_unknown" || reason != "agent restarted" {
			t.Fatalf("row %s: %s %s", id, state, reason)
		}
	}
	expectNoMessage(t, app, "app after one bot message per change")
}

func TestBotSched_ChecksInContractOrder(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)} // 13:00 in Prague
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	err := func(body, want string) {
		t.Helper()
		if s, b := postOrder(t, srv, token, body); s != 409 || !strings.Contains(b, `"`+want+`"`) {
			t.Fatalf("want %s: %d %s", want, s, b)
		}
	}
	// No agent: paused beats everything after the shape.
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) { st.Paused, st.PausedReason = true, "Paused" })
	err(tipBetOrder("k"), "bots_paused")
	if s, b := postOrder(t, srv, token, `{"arb_key":`); s != 400 || !strings.Contains(b, `"bad_order"`) {
		t.Fatalf("shape first: %d %s", s, b)
	}
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) {
		st.Paused, st.PausedReason, st.MaxLeg, st.MaxOrder = false, "", 100, 201
	})
	err(orderJSON("k", legJSON("tipsport", "s1", 2.1, 100.5), legJSON("betano", "s2", 2, 50)), "limit_leg")
	err(orderJSON("k", legJSON("tipsport", "s1", 2.1, 100), legJSON("betano", "s2", 2, 101.5)), "limit_leg")
	err(orderJSON("k", legJSON("tipsport", "s1", 2.1, 100), legJSON("betano", "s2", 2, 100)), "bot_offline") // 200 ≤ 201
	err(orderJSON("k", legJSON("tipsport", "s1", 2.1, 100), legJSON("betano", "s2", 2, 99)), "bot_offline")
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) { st.MaxOrder = 150 })
	err(orderJSON("k", legJSON("tipsport", "s1", 2.1, 100), legJSON("betano", "s2", 2, 51)), "limit_order")

	// Day limit: live only; today's live stake of waiting, running, placed and outcome_unknown orders counts.
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) { st.MaxLeg, st.MaxOrder, st.PerDay, st.DryRun = 0, 0, 1000, false })
	from, _ := pragueDayOf(clk.now())
	for i, r := range []struct {
		state  string
		dryRun bool
		stake  int64
		at     time.Time
	}{
		{"placed", false, 500, from},                         // counts
		{"outcome_unknown", false, 200, from.Add(time.Hour)}, // counts
		{"stopped", false, 5000, from.Add(time.Hour)},        // stopped: no
		{"placed", true, 5000, from.Add(time.Hour)},          // dry run: no
		{"placed", false, 5000, from.Add(-time.Millisecond)}, // yesterday (Prague): no
	} {
		id := fmt.Sprintf("%016x", i+1)
		if e := svc.InsertBotOrder(accounts.BotOrderRow{ID: id, UserID: uid, ArbKey: id, State: "waiting", DryRun: r.dryRun, Stake: r.stake,
			InfoJSON: "{}", LegsJSON: "[]", CreatedAt: r.at.UnixMilli()}); e != nil {
			t.Fatal(e)
		}
		if e := svc.EndBotOrder(id, r.state, "", "[]", r.stake, r.at.UnixMilli()+1); e != nil {
			t.Fatal(e)
		}
	}
	err(orderJSON("k", legJSON("tipsport", "s1", 2.1, 150), legJSON("betano", "s2", 2, 150.5)), "limit_day") // 700 + 301 > 1000
	err(orderJSON("k", legJSON("tipsport", "s1", 2.1, 150), legJSON("betano", "s2", 2, 150)), "bot_offline") // 700 + 300 = 1000
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) { st.DryRun = true })
	err(orderJSON("k", legJSON("tipsport", "s1", 2.1, 1500), legJSON("betano", "s2", 2, 1500)), "bot_offline") // dry run: no day limit
}

// A waiting order that is sent live is checked against the day limit again, counting what this
// same change sends: here the first of two orders fits, the second ends "Over the day limit".
func TestBotSched_DayLimitAgainWhenAWaitingOrderIsSentLive(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	clk := &botClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) { st.OrdersAtOnce, st.PerDay = 2, 1000 }) // dry run
	from, _ := pragueDayOf(clk.now())
	if err := svc.InsertBotOrder(accounts.BotOrderRow{ID: "00000000000000aa", UserID: uid, ArbKey: "earlier", State: "waiting", Stake: 500,
		InfoJSON: "{}", LegsJSON: "[]", CreatedAt: from.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.EndBotOrder("00000000000000aa", "placed", "", "[]", 500, from.UnixMilli()+1); err != nil { // live, 500 today
		t.Fatal(err)
	}
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("tipsport", 2, "idle"),
		win("betano", 1, "busy"), win("betano", 2, "busy")), app)
	// Posted in dry run: no day check yet; both wait for a Betano window.
	first := postOrderOK(t, srv, token, tipBetOrder("arb-1")) // 100 + 102 → 202
	expectEvent(t, app, first.OrderID, "waiting", "Needs a free Betano window")
	expectBot(t, app, "on the first waiting order")
	clk.advance(time.Second)
	second := postOrderOK(t, srv, token, orderJSON("arb-2", legJSON("tipsport", "s1", 2.1, 150), legJSON("betano", "s2", 2.05, 150))) // 300
	expectEvent(t, app, second.OrderID, "waiting", "Needs a free Betano window")
	expectBot(t, app, "on the second waiting order")

	// The owner switches to Live, then the Betano windows free up.
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) { st.DryRun = false })
	if s, b := postStatus(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("tipsport", 2, "idle"),
		win("betano", 1, "idle"), win("betano", 2, "idle"))); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	// 500 + 202 ≤ 1000: sent live. 500 + 202 + 300 > 1000: ended.
	if o := readOrder(t, bot); o.OrderID != first.OrderID || o.DryRun {
		t.Fatalf("agent got %+v", o)
	}
	expectEvent(t, app, first.OrderID, "sent", "")
	expectEvent(t, app, second.OrderID, "stopped", "Over the day limit")
	expectBot(t, app, "after the status")
	if state, _, _, dryRun, _ := orderRow(t, db, first.OrderID); state != "running" || dryRun {
		t.Fatalf("first row: %s dry run %v", state, dryRun)
	}
	if state, reason, _, _, _ := orderRow(t, db, second.OrderID); state != "stopped" || reason != "Over the day limit" {
		t.Fatalf("second row: %s %s", state, reason)
	}
	if v := getView(t, srv, token); v.Running != 1 || v.Waiting != 0 || v.Paused {
		t.Fatalf("view: %+v", v)
	}
	expectNoMessage(t, bot, "the agent (the second order was never sent)")
}

// An order posted live already counts against the day limit while it waits: it is sent when it
// still fits (here after the limit was not changed).
func TestBotSched_LiveWaitingOrderThatStillFitsIsSent(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	setSettings(t, svc, bots, userIDOf(t, svc, token), func(st *accounts.BotSettings) { st.DryRun, st.PerDay = false, 202 })
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "busy")), app)
	a := postOrderOK(t, srv, token, tipBetOrder("arb-1")) // 202 = the whole day limit
	expectEvent(t, app, a.OrderID, "waiting", "")
	expectBot(t, app, "on the waiting order")
	if s, b := postStatus(t, srv, code, tipBetIdle); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	if o := readOrder(t, bot); o.OrderID != a.OrderID || o.DryRun {
		t.Fatalf("agent got %+v (its own waiting row must not count twice)", o)
	}
}

// When the chosen agent's socket does not take the order, the next agent that can is tried; when
// none can, the order waits.
func TestBotSched_FullSendBufferTriesTheNextAgent(t *testing.T) {
	bots := newBotHub(newConnRegistry(), nil)
	clk := &botClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	bots.agents[7] = map[int64]*botPresence{}
	agent := func(codeID int64, reservedAt time.Time, full bool) *Client {
		c := &Client{send: make(chan []byte, 1), bot: true}
		c.principal.UserID, c.principal.CodeID = 7, codeID
		if full {
			c.send <- []byte("x")
		}
		bots.agents[7][codeID] = &botPresence{client: c, proto: botProto, reservedAt: reservedAt, idleSince: map[string]time.Time{},
			windows: []botWindow{{ID: "tipsport-1", Bookmaker: "tipsport", Number: 1, State: "idle"}, {ID: "betano-1", Bookmaker: "betano", Number: 1, State: "idle"}}}
		return c
	}
	stuck := agent(70, clk.now().Add(-time.Hour), true) // unchanged longest: chosen first
	ok := agent(71, clk.now(), false)
	bots.settings[7] = accounts.BotSettings{DryRun: true, OrdersAtOnce: 2, PauseOnUnknown: true}
	in := botOrderIn{ArbKey: "k", Legs: []botLeg{{Bookmaker: "tipsport", SourceID: "a", Odds: 2, Stake: 1}, {Bookmaker: "betano", SourceID: "b", Odds: 2, Stake: 1}}}
	res, err := bots.submit(7, in)
	if err != nil || res.State != "sent" {
		t.Fatalf("order: %+v %v", res, err)
	}
	if len(ok.send) != 1 || len(stuck.send) != 1 || bots.orders[res.OrderID].codeID != 71 {
		t.Fatalf("the order must go to the agent whose socket drains: %d %d", len(ok.send), len(stuck.send))
	}
	in.ArbKey = "k2"
	res, err = bots.submit(7, in)
	if err != nil || res.State != "waiting" || res.Reason != "Needs a free Tipsport and Betano window" {
		t.Fatalf("71 reserved, 70 stuck: %+v %v", res, err)
	}
}

// Two orders at once for one window pair: one goes, the other waits.
func TestBotSched_ConcurrentOrdersOneWindowPair(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	setSettings(t, svc, bots, userIDOf(t, svc, token), func(st *accounts.BotSettings) { st.OrdersAtOnce = 2 })
	app := appSocket(t, srv, token)
	startBot(t, srv, newCode(t, srv, token, "bot"), app)

	answers := make(chan orderAnswer, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest("POST", srv.URL+"/api/me/bot/orders", strings.NewReader(tipBetOrder(key)))
			req.Header.Set("Cookie", accounts.CookieName+"="+token)
			req.Header.Set("Origin", srv.URL)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				answers <- orderAnswer{State: err.Error()}
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			var a orderAnswer
			json.Unmarshal(b, &a)
			answers <- a
		}(fmt.Sprintf("arb-%d", i))
	}
	close(start)
	wg.Wait()
	close(answers)
	got := map[string]int{}
	for a := range answers {
		got[a.State]++
	}
	if got["sent"] != 1 || got["waiting"] != 1 {
		t.Fatalf("answers: %v, want one sent and one waiting", got)
	}
}

func TestBotSched_RecoverOrdersAfterAGatewayRestart(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	_ = srv
	for _, id := range []string{"00000000000000a1", "00000000000000b2"} {
		if err := svc.InsertBotOrder(accounts.BotOrderRow{ID: id, UserID: uid, ArbKey: id, State: "waiting", DryRun: true, Stake: 1,
			InfoJSON: "{}", LegsJSON: "[]", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.MarkBotOrderSent("00000000000000b2", 0, true, "[]", 2); err != nil {
		t.Fatal(err)
	}
	bots.recoverOrders()
	if st, _ := svc.BotSettings(uid); !st.Paused || st.PausedReason != "Outcome unknown · order 00000000" {
		t.Fatalf("settings: %+v", st)
	}
	rows, _ := svc.EndedBotOrders(uid, 0, 10, 10)
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	for _, r := range rows {
		if r.Reason != "Gateway restarted" || (r.ID == "00000000000000a1" && r.State != "stopped") || (r.ID == "00000000000000b2" && r.State != "outcome_unknown") {
			t.Fatalf("row: %+v", r)
		}
	}
}

// A leg stake above 10 000 000 Kč is a bad order: the stake sum could not overflow into a
// negative number that slips under the day limit and poisons the day's live stake.
func TestBotSched_LegStakeCapKeepsTheDayLimitSound(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) { st.DryRun, st.PerDay = false, 1000 })
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	startBot(t, srv, code, app)
	for name, body := range map[string]string{
		"1e19 + 10":         orderJSON("k", legJSON("tipsport", "s1", 2.1, 1e19), legJSON("betano", "s2", 2, 10)),
		"just over the cap": orderJSON("k", legJSON("tipsport", "s1", 2.1, 10), legJSON("betano", "s2", 2, 10_000_000.5)),
	} {
		if s, b := postOrder(t, srv, token, body); s != 400 || !strings.Contains(b, `"bad_order"`) {
			t.Errorf("%s: %d %s, want 400 bad_order", name, s, b)
		}
	}
	// At the cap the shape is fine; the day limit refuses it.
	if s, b := postOrder(t, srv, token, orderJSON("k", legJSON("tipsport", "s1", 2.1, 10_000_000), legJSON("betano", "s2", 2, 10))); s != 409 ||
		!strings.Contains(b, `"limit_day"`) {
		t.Fatalf("at the cap: %d %s, want 409 limit_day", s, b)
	}
	if v := getView(t, srv, token); v.Today.Orders != 0 || v.Running != 0 || v.Waiting != 0 {
		t.Fatalf("view: %+v", v)
	}
	// The day's live stake is untouched: an order that fits is still sent.
	if a := postOrderOK(t, srv, token, tipBetOrder("arb-1")); a.State != "sent" {
		t.Fatalf("order that fits: %+v", a)
	}
}

// A waiting order is checked again when it is sent: paused, the leg limit and the order limit (set
// while it waited) end it stopped with the reason instead of sending it.
func TestBotSched_PauseAndLimitsAgainWhenAWaitingOrderIsSent(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		change       func(*accounts.BotSettings)
	}{
		{"paused", "Paused", func(st *accounts.BotSettings) { st.Paused, st.PausedReason = true, "Paused" }},
		{"leg limit", "Over the leg limit", func(st *accounts.BotSettings) { st.MaxLeg = 101 }},
		{"order limit", "Over the order limit", func(st *accounts.BotSettings) { st.MaxOrder = 201 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, svc, bots, db := newBotServerDB(t)
			token := newUser(t, svc, "u@example.com", accounts.RoleUser)
			uid := userIDOf(t, svc, token)
			app := appSocket(t, srv, token)
			code := newCode(t, srv, token, "bot")
			bot := startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "busy")), app)
			a := postOrderOK(t, srv, token, tipBetOrder("arb-1")) // legs 100 + 102
			expectEvent(t, app, a.OrderID, "waiting", "Needs a free Betano window")
			expectBot(t, app, "on the waiting order")
			setSettings(t, svc, bots, uid, c.change)
			if s, b := postStatus(t, srv, code, tipBetIdle); s != 204 {
				t.Fatalf("status: %d %s", s, b)
			}
			expectEvent(t, app, a.OrderID, "stopped", c.reason)
			if state, reason, _, _, _ := orderRow(t, db, a.OrderID); state != "stopped" || reason != c.reason {
				t.Fatalf("row: %s %s", state, reason)
			}
			expectNoMessage(t, bot, "the agent (the order was never sent)")
		})
	}
}
