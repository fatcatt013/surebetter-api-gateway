package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// newBotServer is newAuthServer (AUTH_REQUIRED on) that also returns the bot hub.
func newBotServer(t *testing.T) (*httptest.Server, *accounts.Service, *botHub) {
	t.Helper()
	db, err := accounts.OpenDB(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := accounts.NewService(db, nil)
	reg := newConnRegistry()
	svc.SetConnections(reg)
	bots := newBotHub(reg, svc)
	bots.shotsDir = filepath.Join(t.TempDir(), "bot-screenshots")
	cfg := Config{SnapshotCacheMs: 500, MaxClients: 10}
	srv := httptest.NewServer(newMux(NewHub(cfg), NewValueHub(cfg), nil, &wsGate{acc: svc, reg: reg, required: true, bots: bots}))
	t.Cleanup(srv.Close)
	return srv, svc, bots
}

// newCode creates a code of kind ("extension" or "bot") for the session token's user.
func newCode(t *testing.T, srv *httptest.Server, token, kind string) string {
	t.Helper()
	status, body := request(t, srv, "POST", "/api/me/extension-codes", token, `{"name":"Test","kind":"`+kind+`"}`, nil)
	if status != 201 {
		t.Fatalf("create %s code: %d %s", kind, status, body)
	}
	var c struct{ Code string }
	json.Unmarshal([]byte(body), &c)
	return c.Code
}

func bearer(code string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + code}
}

// wsTicket trades a code for a WebSocket ticket.
func wsTicket(t *testing.T, srv *httptest.Server, code string) string {
	t.Helper()
	status, body := request(t, srv, "POST", "/api/auth/ws-ticket", "", `{}`, bearer(code))
	var tk struct{ Ticket string }
	if json.Unmarshal([]byte(body), &tk); status != 200 || tk.Ticket == "" {
		t.Fatalf("ticket: %d %s", status, body)
	}
	return tk.Ticket
}

// dialBot opens the bot socket the way the bot does: a ticket, then /ws?ticket=…&feed=0&bot=1.
func dialBot(t *testing.T, srv *httptest.Server, code string) *websocket.Conn {
	t.Helper()
	conn, status, err := dial(t, srv, "/ws?ticket="+wsTicket(t, srv, code)+"&feed=0&bot=1", nil)
	if err != nil {
		t.Fatalf("bot socket: status %d err %v", status, err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func userIDOf(t *testing.T, svc *accounts.Service, token string) int64 {
	t.Helper()
	p, ok, err := svc.AuthenticateSession(token)
	if err != nil || !ok {
		t.Fatalf("session: %v %v", ok, err)
	}
	return p.UserID
}

// waitBot waits until the hub shows userID's bot online (or offline).
func waitBot(t *testing.T, bots *botHub, userID int64, online bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for bots.view(userID).Online != online {
		if time.Now().After(deadline) {
			t.Fatalf("bot online never became %v", online)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// appSocket opens the app's /ws with a session and reads its snapshot, so the socket is
// registered when this returns.
func appSocket(t *testing.T, srv *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := dial(t, srv, "/ws?cid=app", cookie(token))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	if got := readType(t, conn); got != "snapshot" {
		t.Fatalf("app socket first message = %s", got)
	}
	return conn
}

// expectClosedBy waits for the server to close conn with status.
func expectClosedBy(t *testing.T, conn *websocket.Conn, status websocket.StatusCode, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := conn.Read(ctx)
	if err == nil {
		t.Fatalf("%s got %s, want a close", what, b)
	}
	if got := websocket.CloseStatus(err); got != status {
		t.Fatalf("%s: close status %d (%v), want %d", what, got, err, status)
	}
}

func TestBot_SocketNeedsBotCode(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	ext := newCode(t, srv, token, "extension")
	bot := newCode(t, srv, token, "bot")
	uid := userIDOf(t, svc, token)

	if _, status, _ := dial(t, srv, "/ws?ticket="+wsTicket(t, srv, ext)+"&feed=0&bot=1", nil); status != http.StatusForbidden {
		t.Fatalf("extension code ticket with bot=1: %d, want 403", status)
	}
	if _, status, _ := dial(t, srv, "/ws?feed=0&bot=1", cookie(token)); status != http.StatusForbidden {
		t.Fatalf("session with bot=1: %d, want 403", status)
	}
	// A bot code can do what an extension code can: a normal feed socket, which is not the bot.
	feed, _, err := dial(t, srv, "/ws?ticket="+wsTicket(t, srv, bot), nil)
	if err != nil {
		t.Fatalf("bot code feed socket: %v", err)
	}
	defer feed.CloseNow()
	if got := readType(t, feed); got != "snapshot" {
		t.Fatalf("bot code feed socket first message = %s", got)
	}
	if bots.view(uid).Online {
		t.Fatal("a feed socket must not make the bot online")
	}
	dialBot(t, srv, bot)
	waitBot(t, bots, uid, true)
}

func TestBot_NewSocketReplacesOld(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	uid := userIDOf(t, svc, token)

	first := dialBot(t, srv, code)
	waitBot(t, bots, uid, true)
	second := dialBot(t, srv, code)
	expectClosedBy(t, first, websocket.StatusNormalClosure, "the older bot socket")
	// Wait until the replaced socket's handler is gone (only the new socket is registered), then its
	// cleanup has run and must not have taken the new one offline.
	deadline := time.Now().Add(2 * time.Second)
	for {
		bots.reg.mu.Lock()
		n := len(bots.reg.clients)
		bots.reg.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replaced socket never unregistered (%d clients)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !bots.view(uid).Online {
		t.Fatal("closing the replaced socket took the bot offline")
	}
	second.Close(websocket.StatusNormalClosure, "")
	waitBot(t, bots, uid, false)
}

func TestBot_PresenceNotifiesAppSockets(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	other := appSocket(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser))

	bot := dialBot(t, srv, newCode(t, srv, token, "bot"))
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app socket on bot connect got %s, want bot", got)
	}
	bot.Close(websocket.StatusNormalClosure, "")
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app socket on bot disconnect got %s, want bot", got)
	}
	expectNoMessage(t, other, "another user's app socket")
}

// getBot returns the GET /api/me/bot body (trimmed) for a session.
func getBot(t *testing.T, srv *httptest.Server, token string) string {
	t.Helper()
	status, body := request(t, srv, "GET", "/api/me/bot", token, "", nil)
	if status != 200 {
		t.Fatalf("GET /api/me/bot: %d %s", status, body)
	}
	return strings.TrimSpace(body)
}

func postStatus(t *testing.T, srv *httptest.Server, code, body string) (int, string) {
	t.Helper()
	return request(t, srv, "POST", "/api/me/bot/status", "", body, bearer(code))
}

func TestBot_StatusAndPresence(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	app := appSocket(t, srv, token)

	if got := getBot(t, srv, token); got != `{"online":false,"books":[],"dryRun":true,"busy":false}` {
		t.Fatalf("offline: %s", got)
	}
	if s, b := postStatus(t, srv, code, `{"books":[],"dryRun":true}`); s != 409 || !strings.Contains(b, `"bot_offline"`) {
		t.Fatalf("status without a bot socket: %d %s", s, b)
	}

	bot := dialBot(t, srv, code)
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on bot connect: %s", got)
	}
	if got := getBot(t, srv, token); got != `{"online":true,"books":[],"dryRun":true,"busy":false}` {
		t.Fatalf("online before any status: %s", got)
	}

	live := `{"books":[{"bookmaker":"tipsport","ready":true},{"bookmaker":"betano","ready":false}],"dryRun":false}`
	if s, b := postStatus(t, srv, code, live); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on status change: %s", got)
	}
	if got := getBot(t, srv, token); got != `{"online":true,"books":[{"bookmaker":"betano","ready":false},{"bookmaker":"tipsport","ready":true}],"dryRun":false,"busy":false}` {
		t.Fatalf("after status: %s", got)
	}

	// A missing dryRun is dry run: never Live by omission.
	if s, b := postStatus(t, srv, code, `{"books":[{"bookmaker":"tipsport","ready":true}]}`); s != 204 {
		t.Fatalf("status without dryRun: %d %s", s, b)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on status change: %s", got)
	}
	if got := getBot(t, srv, token); got != `{"online":true,"books":[{"bookmaker":"tipsport","ready":true}],"dryRun":true,"busy":false}` {
		t.Fatalf("after status without dryRun: %s", got)
	}

	// The same status again is not a change.
	if s, _ := postStatus(t, srv, code, `{"books":[{"bookmaker":"tipsport","ready":true}],"dryRun":true}`); s != 204 {
		t.Fatalf("repeated status: %d", s)
	}
	expectNoMessage(t, app, "app socket after an unchanged status")
	expectNoMessage(t, bot, "the bot socket (never gets bot messages)")
}

func TestBot_StatusOnlyFromTheConnectedCode(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	first := newCode(t, srv, token, "bot")
	second := newCode(t, srv, token, "bot")

	dialBot(t, srv, first)
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on bot connect: %s", got)
	}
	if s, b := postStatus(t, srv, second, `{"books":[],"dryRun":true}`); s != 409 || !strings.Contains(b, `"bot_offline"`) {
		t.Fatalf("status from a code without the socket: %d %s", s, b)
	}
	if s, _ := postStatus(t, srv, first, `{"books":[],"dryRun":true}`); s != 204 {
		t.Fatalf("status from the connected code: %d", s)
	}
}

func TestBot_StatusRoutesNeedTheRightCredential(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	ext := newCode(t, srv, token, "extension")
	bot := newCode(t, srv, token, "bot")

	if s, b := request(t, srv, "POST", "/api/me/bot/status", token, `{}`, nil); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("status with a session: %d %s", s, b)
	}
	if s, b := postStatus(t, srv, ext, `{}`); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("status with an extension code: %d %s", s, b)
	}
	if s, _ := request(t, srv, "POST", "/api/me/bot/status", "", `{}`, nil); s != 401 {
		t.Fatalf("status without a credential: %d", s)
	}
	if s, b := request(t, srv, "GET", "/api/me/bot", "", "", bearer(bot)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("GET /api/me/bot with a code: %d %s", s, b)
	}
}

func legJSON(book, sourceID string, odds, stake float64) string {
	return fmt.Sprintf(`{"bookmaker":%q,"match_url":"https://%s.example/match","source_id":%q,"source_market_ref":"","market_name":"","label":"","odds":%v,"stake":%v}`,
		book, book, sourceID, odds, stake)
}

func orderJSON(arbKey string, legs ...string) string {
	return fmt.Sprintf(`{"arb_key":%q,"legs":[%s]}`, arbKey, strings.Join(legs, ","))
}

func postOrder(t *testing.T, srv *httptest.Server, token, body string) (int, string) {
	t.Helper()
	return request(t, srv, "POST", "/api/me/bot/orders", token, body, nil)
}

// readJSON reads the next message on conn as a JSON object.
func readJSON(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not a JSON object: %s", b)
	}
	return m
}

// startBot connects code's bot, reports tipsport and betano ready, and reads the two {"type":"bot"}
// messages that causes on the user's app socket.
func startBot(t *testing.T, srv *httptest.Server, code string, app *websocket.Conn) *websocket.Conn {
	t.Helper()
	bot := dialBot(t, srv, code)
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on bot connect: %s", got)
	}
	if s, b := postStatus(t, srv, code, `{"books":[{"bookmaker":"tipsport","ready":true},{"bookmaker":"betano","ready":true}],"dryRun":true}`); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on status: %s", got)
	}
	return bot
}

// placeOrder sends a valid tipsport/betano order with arb_key "arb-1" and returns its id, after the
// bot socket read the order and the app socket read the busy {"type":"bot"}.
func placeOrder(t *testing.T, srv *httptest.Server, token string, bot, app *websocket.Conn) string {
	t.Helper()
	s, b := postOrder(t, srv, token, orderJSON("arb-1", legJSON("tipsport", "s1", 2.1, 100), legJSON("betano", "s2", 2.05, 102)))
	var res struct {
		OrderID string `json:"order_id"`
	}
	if json.Unmarshal([]byte(b), &res); s != 202 || res.OrderID == "" {
		t.Fatalf("order: %d %s", s, b)
	}
	if m := readJSON(t, bot); m["type"] != "order" {
		t.Fatalf("bot got %v, want an order", m)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on busy: %s", got)
	}
	return res.OrderID
}

func TestBot_OrderValidation(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	tip := legJSON("tipsport", "s1", 2.1, 100)
	bet := legJSON("betano", "s2", 2.05, 102)
	bad := map[string]string{
		"not JSON":       `{"arb_key":`,
		"no arb_key":     orderJSON("", tip, bet),
		"one leg":        orderJSON("k", tip),
		"three legs":     orderJSON("k", tip, bet, legJSON("fortuna", "s3", 3, 50)),
		"same bookmaker": orderJSON("k", tip, legJSON("tipsport", "s2", 2.05, 102)),
		"odds 1":         orderJSON("k", legJSON("tipsport", "s1", 1, 100), bet),
		"stake 0":        orderJSON("k", tip, legJSON("betano", "s2", 2.05, 0)),
		"no source_id":   orderJSON("k", tip, legJSON("betano", "", 2.05, 102)),
		"no bookmaker":   orderJSON("k", tip, legJSON("", "s2", 2.05, 102)),
	}
	// The shape is checked first: these are 400 although no bot is online.
	for name, body := range bad {
		if s, b := postOrder(t, srv, token, body); s != 400 || !strings.Contains(b, `"bad_order"`) {
			t.Errorf("%s: %d %s, want 400 bad_order", name, s, b)
		}
	}
	if s, b := postOrder(t, srv, token, orderJSON("k", tip, bet)); s != 409 || !strings.Contains(b, `"bot_offline"`) {
		t.Fatalf("valid order, no bot: %d %s", s, b)
	}
}

func TestBot_OrdersNeedASameSiteSession(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	body := orderJSON("k", legJSON("tipsport", "s1", 2.1, 100), legJSON("betano", "s2", 2.05, 102))

	if s, b := request(t, srv, "POST", "/api/me/bot/orders", "", body, bearer(code)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("order with a bot code: %d %s", s, b)
	}
	if s, b := request(t, srv, "POST", "/api/me/bot/orders", token, body, map[string]string{"Origin": "http://evil.example"}); s != 403 || !strings.Contains(b, `"bad_origin"`) {
		t.Fatalf("cross-site order: %d %s", s, b)
	}
	if s, _ := request(t, srv, "POST", "/api/me/bot/orders", "", body, nil); s != 401 {
		t.Fatalf("order without a credential: %d", s)
	}
}

func TestBot_OrderForwardedToOwnBotAndBusy(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	bot := startBot(t, srv, newCode(t, srv, token, "bot"), app)
	otherToken := newUser(t, svc, "o@example.com", accounts.RoleUser)
	otherApp := appSocket(t, srv, otherToken)
	otherBot := startBot(t, srv, newCode(t, srv, otherToken, "bot"), otherApp)

	tip := legJSON("tipsport", "s1", 2.1, 100)
	bet := legJSON("betano", "s2", 2.05, 102)
	fortuna := legJSON("fortuna", "s3", 2.05, 102)
	if s, b := postOrder(t, srv, token, orderJSON("k", tip, fortuna)); s != 409 || !strings.Contains(b, `"book_not_ready"`) {
		t.Fatalf("leg at a book the bot does not run: %d %s", s, b)
	}

	s, b := postOrder(t, srv, token, orderJSON("football:abc:MATCH_WINNER:0.00", tip, bet))
	var res struct {
		OrderID string `json:"order_id"`
	}
	if json.Unmarshal([]byte(b), &res); s != 202 || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(res.OrderID) {
		t.Fatalf("order: %d %s", s, b)
	}

	msg := readJSON(t, bot)
	order, _ := msg["order"].(map[string]any)
	legs, _ := order["legs"].([]any)
	if msg["type"] != "order" || order["order_id"] != res.OrderID || order["arb_key"] != "football:abc:MATCH_WINNER:0.00" || len(legs) != 2 {
		t.Fatalf("bot got %v", msg)
	}
	leg0, _ := legs[0].(map[string]any)
	if leg0["bookmaker"] != "tipsport" || leg0["source_id"] != "s1" || leg0["match_url"] != "https://tipsport.example/match" ||
		leg0["odds"] != 2.1 || leg0["stake"] != float64(100) {
		t.Fatalf("leg 0: %v", leg0)
	}
	for _, k := range []string{"source_market_ref", "market_name", "label"} {
		if _, ok := leg0[k]; !ok {
			t.Fatalf("leg 0 must carry %s even when empty: %v", k, leg0)
		}
	}

	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on busy: %s", got)
	}
	if got := getBot(t, srv, token); !strings.Contains(got, `"busy":true`) {
		t.Fatalf("after order: %s", got)
	}
	if s, b := postOrder(t, srv, token, orderJSON("k2", tip, bet)); s != 409 || !strings.Contains(b, `"bot_busy"`) {
		t.Fatalf("second order: %d %s", s, b)
	}
	// No agent has fortuna ready, busy or not: book_not_ready.
	if s, b := postOrder(t, srv, token, orderJSON("k3", tip, fortuna)); s != 409 || !strings.Contains(b, `"book_not_ready"`) {
		t.Fatalf("order no agent can take: %d %s", s, b)
	}
	if got := getBot(t, srv, otherToken); !strings.Contains(got, `"busy":false`) {
		t.Fatalf("another user's bot: %s", got)
	}
	expectNoMessage(t, otherBot, "another user's bot socket")
}

func TestBot_BusyClearsWhenBotSocketCloses(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	app := appSocket(t, srv, token)
	bot := startBot(t, srv, code, app)
	placeOrder(t, srv, token, bot, app)

	bot.Close(websocket.StatusNormalClosure, "")
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on bot disconnect: %s", got)
	}
	if got := getBot(t, srv, token); got != `{"online":false,"books":[],"dryRun":true,"busy":false}` {
		t.Fatalf("after disconnect: %s", got)
	}
	bot = startBot(t, srv, code, app)
	if got := getBot(t, srv, token); !strings.Contains(got, `"busy":false`) {
		t.Fatalf("after reconnect: %s", got)
	}
	placeOrder(t, srv, token, bot, app)
}

func postEvent(t *testing.T, srv *httptest.Server, code, body string) (int, string) {
	t.Helper()
	return request(t, srv, "POST", "/api/me/bot/events", "", body, bearer(code))
}

// botClock is a settable clock for botHub.now (read by handler goroutines, hence the lock).
type botClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *botClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *botClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestBot_EventsRelayedAndFinalClearsBusy(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	app := appSocket(t, srv, token)
	bot := startBot(t, srv, code, app)
	id := placeOrder(t, srv, token, bot, app)

	if s, b := postEvent(t, srv, code, `{"order_id":"`+id+`","event":"accepted"}`); s != 204 {
		t.Fatalf("accepted: %d %s", s, b)
	}
	m := readJSON(t, app)
	if m["type"] != "botEvent" || m["order_id"] != id || m["arb_key"] != "arb-1" || m["event"] != "accepted" {
		t.Fatalf("app got %v", m)
	}
	if _, ok := m["reason"]; ok {
		t.Fatalf("no reason was posted, none may be relayed: %v", m)
	}

	if s, b := postEvent(t, srv, code, `{"order_id":"`+id+`","event":"leg_ready","bookmaker":"tipsport"}`); s != 204 {
		t.Fatalf("leg_ready: %d %s", s, b)
	}
	if m := readJSON(t, app); m["event"] != "leg_ready" || m["bookmaker"] != "tipsport" {
		t.Fatalf("app got %v", m)
	}
	if got := getBot(t, srv, token); !strings.Contains(got, `"busy":true`) {
		t.Fatalf("a non-final event must keep the bot busy: %s", got)
	}

	if s, b := postEvent(t, srv, code, `{"order_id":"`+id+`","event":"stopped","bookmaker":"betano","reason":"betano: odds dropped 2.05 → 2.00"}`); s != 204 {
		t.Fatalf("stopped: %d %s", s, b)
	}
	if m := readJSON(t, app); m["event"] != "stopped" || m["bookmaker"] != "betano" || m["reason"] != "betano: odds dropped 2.05 → 2.00" {
		t.Fatalf("app got %v", m)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app after the final event: %s, want bot (busy cleared)", got)
	}
	if got := getBot(t, srv, token); !strings.Contains(got, `"busy":false`) {
		t.Fatalf("after the final event: %s", got)
	}
	// A final event ends the order: later events for it are unknown.
	if s, b := postEvent(t, srv, code, `{"order_id":"`+id+`","event":"placed"}`); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("event after the final one: %d %s", s, b)
	}
	expectNoMessage(t, bot, "the bot socket (never gets botEvent)")
}

func TestBot_EventErrors(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	ext := newCode(t, srv, token, "extension")
	app := appSocket(t, srv, token)
	bot := startBot(t, srv, code, app)
	id := placeOrder(t, srv, token, bot, app)

	if s, b := postEvent(t, srv, code, `{"order_id":"`+id+`","event":"exploded"}`); s != 400 || !strings.Contains(b, `"bad_event"`) {
		t.Fatalf("unknown event: %d %s", s, b)
	}
	if s, b := postEvent(t, srv, code, `{"order_id":"`+id+`"`); s != 400 || !strings.Contains(b, `"bad_event"`) {
		t.Fatalf("not JSON: %d %s", s, b)
	}
	if s, b := postEvent(t, srv, code, `{"order_id":"0123456789abcdef","event":"accepted"}`); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("unknown order: %d %s", s, b)
	}
	otherCode := newCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "bot")
	if s, b := postEvent(t, srv, otherCode, `{"order_id":"`+id+`","event":"placed"}`); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("another user's order: %d %s", s, b)
	}
	if s, b := request(t, srv, "POST", "/api/me/bot/events", token, `{"order_id":"`+id+`","event":"placed"}`, nil); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("event with a session: %d %s", s, b)
	}
	if s, b := postEvent(t, srv, ext, `{"order_id":"`+id+`","event":"placed"}`); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("event with an extension code: %d %s", s, b)
	}
	if got := getBot(t, srv, token); !strings.Contains(got, `"busy":true`) {
		t.Fatalf("rejected events must not end the order: %s", got)
	}
}

func TestBot_FinalEventAfterReconnectIsRelayed(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	app := appSocket(t, srv, token)
	bot := startBot(t, srv, code, app)
	id := placeOrder(t, srv, token, bot, app)

	bot.Close(websocket.StatusNormalClosure, "")
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on bot disconnect: %s", got)
	}
	startBot(t, srv, code, app)
	if s, b := postEvent(t, srv, code, `{"order_id":"`+id+`","event":"placed"}`); s != 204 {
		t.Fatalf("final event after reconnect: %d %s", s, b)
	}
	if m := readJSON(t, app); m["type"] != "botEvent" || m["order_id"] != id || m["arb_key"] != "arb-1" || m["event"] != "placed" {
		t.Fatalf("app got %v", m)
	}
	// The new socket was never busy with this order, so its busy flag does not change.
	expectNoMessage(t, app, "app socket after the old order's final event")
}

func TestBot_OrdersForgottenAfterTenMinutes(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now // before any request touches the hub
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	app := appSocket(t, srv, token)
	bot := startBot(t, srv, code, app)
	id := placeOrder(t, srv, token, bot, app)

	clk.advance(orderMemory - time.Second)
	if s, b := postEvent(t, srv, code, `{"order_id":"`+id+`","event":"accepted"}`); s != 204 {
		t.Fatalf("event within 10 min: %d %s", s, b)
	}
	if m := readJSON(t, app); m["event"] != "accepted" {
		t.Fatalf("app got %v", m)
	}
	clk.advance(2 * time.Second)
	// Forgetting the order frees the bot: GET stops showing busy and the app is told.
	if got := getBot(t, srv, token); !strings.Contains(got, `"busy":false`) {
		t.Fatalf("busy must clear when its order is forgotten: %s", got)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on forgotten order: %s, want bot", got)
	}
	if s, b := postEvent(t, srv, code, `{"order_id":"`+id+`","event":"placed"}`); s != 404 || !strings.Contains(b, `"unknown_order"`) {
		t.Fatalf("event after 10 min: %d %s", s, b)
	}
	placeOrder(t, srv, token, bot, app) // a new order is accepted (202)
}

// The order path alone also frees a bot whose order was forgotten (no GET in between).
func TestBot_ForgottenOrderDoesNotBlockNewOrder(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	bot := startBot(t, srv, newCode(t, srv, token, "bot"), app)
	placeOrder(t, srv, token, bot, app)
	clk.advance(orderMemory + time.Second)
	s, b := postOrder(t, srv, token, orderJSON("arb-2", legJSON("tipsport", "s1", 2.1, 100), legJSON("betano", "s2", 2.05, 102)))
	if s != 202 {
		t.Fatalf("order after the old one was forgotten: %d %s", s, b)
	}
}

func TestBot_OrderValidationLimits(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	tip := legJSON("tipsport", "s1", 2.1, 100)
	bet := legJSON("betano", "s2", 2.05, 102)
	bad := map[string]string{
		"arb_key over 256 bytes": orderJSON(strings.Repeat("k", 257), tip, bet),
		"padded bookmaker":       orderJSON("k", tip, legJSON(" betano", "s2", 2.05, 102)),
		"padded same bookmaker":  orderJSON("k", tip, legJSON("tipsport ", "s2", 2.05, 102)),
	}
	for name, body := range bad {
		if s, b := postOrder(t, srv, token, body); s != 400 || !strings.Contains(b, `"bad_order"`) {
			t.Errorf("%s: %d %s, want 400 bad_order", name, s, b)
		}
	}
	if s, b := postOrder(t, srv, token, orderJSON(strings.Repeat("k", 256), tip, bet)); s != 409 || !strings.Contains(b, `"bot_offline"`) {
		t.Fatalf("256-byte arb_key must pass validation: %d %s", s, b)
	}
}

func TestBot_ReplacementWhileBusy(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	uid := userIDOf(t, svc, token)
	app := appSocket(t, srv, token)
	first := startBot(t, srv, code, app)
	placeOrder(t, srv, token, first, app)

	second := startBot(t, srv, code, app) // replaces the busy socket
	expectClosedBy(t, first, websocket.StatusNormalClosure, "the older bot socket")
	if got := getBot(t, srv, token); !strings.Contains(got, `"busy":false`) {
		t.Fatalf("a replacement starts idle: %s", got)
	}
	placeOrder(t, srv, token, second, app)
	time.Sleep(150 * time.Millisecond) // let the old socket's cleanup run
	if v := bots.view(uid); !v.Online || !v.Busy {
		t.Fatalf("the old socket's close must not touch the new socket's busy order: %+v", v)
	}
}

func TestBot_FullSendBufferIsOfflineNotBusy(t *testing.T) {
	bots := newBotHub(newConnRegistry(), nil)
	c := &Client{send: make(chan []byte, 1), bot: true}
	c.principal.UserID, c.principal.CodeID = 7, 70
	c.send <- []byte("x") // full
	bots.agents[7] = map[int64]*botPresence{70: {client: c, books: []botBook{{"tipsport", true}, {"betano", true}}, dryRun: true}}
	o := botOrderMsg{ArbKey: "k", Legs: []botLeg{{Bookmaker: "tipsport", SourceID: "a", Odds: 2, Stake: 1}, {Bookmaker: "betano", SourceID: "b", Odds: 2, Stake: 1}}}
	if _, err := bots.order(7, o); !errors.Is(err, errBotOffline) {
		t.Fatalf("full buffer: %v, want bot_offline", err)
	}
	if bots.view(7).Busy {
		t.Fatal("a refused order must not set busy")
	}
}

func TestBot_BadEventBeatsUnknownOrder(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	if s, b := postEvent(t, srv, code, `{"order_id":"0123456789abcdef","event":"exploded"}`); s != 400 || !strings.Contains(b, `"bad_event"`) {
		t.Fatalf("bad event + unknown order: %d %s", s, b)
	}
}

// newBotCode creates a bot code named name for the session token's user and returns the code and its id.
func newBotCode(t *testing.T, srv *httptest.Server, token, name string) (string, int64) {
	t.Helper()
	status, body := request(t, srv, "POST", "/api/me/extension-codes", token, `{"name":"`+name+`","kind":"bot"}`, nil)
	var c struct {
		ID   int64  `json:"id"`
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(body), &c); status != 201 || c.Code == "" {
		t.Fatalf("create bot code %s: %d %s", name, status, body)
	}
	return c.Code, c.ID
}

// startAgent connects code's agent, posts books (a status JSON "books" array) with dryRun, and reads
// the two {"type":"bot"} messages that causes on the user's app socket.
func startAgent(t *testing.T, srv *httptest.Server, code, books string, dryRun bool, app *websocket.Conn) *websocket.Conn {
	t.Helper()
	bot := dialBot(t, srv, code)
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on agent connect: %s", got)
	}
	if s, b := postStatus(t, srv, code, fmt.Sprintf(`{"books":%s,"dryRun":%v}`, books, dryRun)); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on status: %s", got)
	}
	return bot
}

const (
	booksTipBet = `[{"bookmaker":"tipsport","ready":true},{"bookmaker":"betano","ready":true}]`
	booksTipFor = `[{"bookmaker":"tipsport","ready":true},{"bookmaker":"fortuna","ready":true}]`
)

func TestBot_SeveralAgentsOnline(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	app := appSocket(t, srv, token)
	home, homeID := newBotCode(t, srv, token, "Home PC")
	vps, vpsID := newBotCode(t, srv, token, "VPS Praha")

	homeSock := dialBot(t, srv, home)
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on first agent: %s", got)
	}
	vpsSock := dialBot(t, srv, vps)
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on second agent: %s", got)
	}
	// Both agents have a registry row once the app has been told.
	rows, err := svc.ListBotAgents(uid)
	if err != nil || len(rows) != 2 || rows[0].CodeID != homeID || rows[1].CodeID != vpsID {
		t.Fatalf("registry: %+v %v", rows, err)
	}
	// Another code's socket does not close the first agent's.
	if s, _ := postStatus(t, srv, home, `{"books":[],"dryRun":true}`); s != 204 {
		t.Fatalf("first agent's status after the second connected: %d", s)
	}
	vpsSock.Close(websocket.StatusNormalClosure, "")
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on second agent leaving: %s", got)
	}
	if !bots.view(uid).Online {
		t.Fatal("one agent left, the user's bot must stay online")
	}
	homeSock.Close(websocket.StatusNormalClosure, "")
	waitBot(t, bots, uid, false)
}

func TestBot_AggregateView(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	a, _ := newBotCode(t, srv, token, "A")
	b, _ := newBotCode(t, srv, token, "B")
	aSock := startAgent(t, srv, a, booksTipBet, false, app)
	startAgent(t, srv, b, booksTipFor, true, app)

	if got := getBot(t, srv, token); got != `{"online":true,"books":[{"bookmaker":"betano","ready":true},{"bookmaker":"fortuna","ready":true},{"bookmaker":"tipsport","ready":true}],"dryRun":true,"busy":false}` {
		t.Fatalf("two idle agents: %s", got)
	}
	// A takes a tipsport/betano order: its books stop counting; betano is known only on a busy agent.
	placeOrder(t, srv, token, aSock, app)
	if got := getBot(t, srv, token); got != `{"online":true,"books":[{"bookmaker":"betano","ready":false},{"bookmaker":"fortuna","ready":true},{"bookmaker":"tipsport","ready":true}],"dryRun":true,"busy":false}` {
		t.Fatalf("one agent busy: %s", got)
	}
	s, body := postOrder(t, srv, token, orderJSON("arb-2", legJSON("tipsport", "s1", 2.1, 100), legJSON("fortuna", "s3", 2.05, 102)))
	if s != 202 {
		t.Fatalf("order for B: %d %s", s, body)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on B busy: %s", got)
	}
	if got := getBot(t, srv, token); got != `{"online":true,"books":[{"bookmaker":"betano","ready":false},{"bookmaker":"fortuna","ready":false},{"bookmaker":"tipsport","ready":false}],"dryRun":true,"busy":true}` {
		t.Fatalf("both agents busy: %s", got)
	}
}

func TestBot_OrderGoesToTheAgentIdleLongest(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	a, _ := newBotCode(t, srv, token, "A")
	b, _ := newBotCode(t, srv, token, "B")
	aSock := startAgent(t, srv, a, booksTipBet, true, app)
	clk.advance(time.Second)
	bSock := startAgent(t, srv, b, booksTipBet, true, app)

	first := placeOrder(t, srv, token, aSock, app) // A has been idle longer
	second := placeOrder(t, srv, token, bSock, app)
	if s, body := postOrder(t, srv, token, orderJSON("arb-3", legJSON("tipsport", "s1", 2.1, 100), legJSON("betano", "s2", 2.05, 102))); s != 409 || !strings.Contains(body, `"bot_busy"`) {
		t.Fatalf("both busy: %d %s", s, body)
	}

	clk.advance(time.Second)
	if s, body := postEvent(t, srv, b, `{"order_id":"`+second+`","event":"placed"}`); s != 204 {
		t.Fatalf("B's final event: %d %s", s, body)
	}
	readType(t, app) // botEvent
	readType(t, app) // bot (B idle)
	clk.advance(time.Second)
	if s, body := postEvent(t, srv, a, `{"order_id":"`+first+`","event":"placed"}`); s != 204 {
		t.Fatalf("A's final event: %d %s", s, body)
	}
	readType(t, app) // botEvent
	readType(t, app) // bot (A idle)
	// B became idle first, so B gets the next order.
	placeOrder(t, srv, token, bSock, app)
	expectNoMessage(t, aSock, "agent A (B has been idle longer)")
}

func TestBot_OrderErrorsWithSeveralAgents(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	a, _ := newBotCode(t, srv, token, "A")
	b, _ := newBotCode(t, srv, token, "B")
	aSock := startAgent(t, srv, a, booksTipBet, true, app)
	startAgent(t, srv, b, booksTipFor, true, app)
	placeOrder(t, srv, token, aSock, app) // A busy; B is idle but has no betano

	tip := legJSON("tipsport", "s1", 2.1, 100)
	if s, body := postOrder(t, srv, token, orderJSON("k", tip, legJSON("betano", "s2", 2.05, 102))); s != 409 || !strings.Contains(body, `"bot_busy"`) {
		t.Fatalf("only the busy agent has both books: %d %s", s, body)
	}
	if s, body := postOrder(t, srv, token, orderJSON("k", tip, legJSON("sazkabet", "s4", 2.05, 102))); s != 409 || !strings.Contains(body, `"book_not_ready"`) {
		t.Fatalf("no agent has both books: %d %s", s, body)
	}
}

func TestBot_EventsOnlyFromTheChosenAgent(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	a, _ := newBotCode(t, srv, token, "A")
	b, _ := newBotCode(t, srv, token, "B")
	aSock := startAgent(t, srv, a, booksTipBet, true, app)
	startAgent(t, srv, b, booksTipFor, true, app)
	id := placeOrder(t, srv, token, aSock, app)

	if s, body := postEvent(t, srv, b, `{"order_id":"`+id+`","event":"placed"}`); s != 404 || !strings.Contains(body, `"unknown_order"`) {
		t.Fatalf("event from another agent of the same user: %d %s", s, body)
	}
	if got := getBot(t, srv, token); !strings.Contains(got, `"betano","ready":false`) {
		t.Fatalf("a rejected event must not free agent A: %s", got)
	}
	if s, body := postEvent(t, srv, a, `{"order_id":"`+id+`","event":"placed"}`); s != 204 {
		t.Fatalf("event from the chosen agent: %d %s", s, body)
	}
}
