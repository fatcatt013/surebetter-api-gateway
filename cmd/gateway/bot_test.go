package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	srv, svc, bots, _ := newBotServerDB(t)
	return srv, svc, bots
}

// newBotServerDB is newBotServer that also returns the accounts database.
func newBotServerDB(t *testing.T) (*httptest.Server, *accounts.Service, *botHub, *sql.DB) {
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
	return srv, svc, bots, db
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

const (
	bootA = "0123456789abcdef"
	bootB = "fedcba9876543210"
)

// bootStatus is a status with tipsport and betano ready and boot ("" = no boot key).
func bootStatus(boot string) string {
	if boot == "" {
		return `{"books":[{"bookmaker":"tipsport","ready":true},{"bookmaker":"betano","ready":true}],"dryRun":true}`
	}
	return `{"books":[{"bookmaker":"tipsport","ready":true},{"bookmaker":"betano","ready":true}],"dryRun":true,"boot":"` + boot + `"}`
}

func TestBot_StatusBootValidation(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	dialBot(t, srv, code)
	for _, boot := range []string{"0123456789ABCDEF", "0123456789abcde", "0123456789abcdef0", "0123456789abcdeg", " 123456789abcdef"} {
		if s, b := postStatus(t, srv, code, bootStatus(boot)); s != 400 || !strings.Contains(b, `"bad_status"`) {
			t.Errorf("boot %q: %d %s, want 400 bad_status", boot, s, b)
		}
	}
	if s, b := postStatus(t, srv, code, `{"books":[],"boot":7}`); s != 400 || !strings.Contains(b, `"bad_status"`) {
		t.Errorf("boot a number: %d %s", s, b)
	}
	for _, body := range []string{bootStatus(bootA), bootStatus(""), `{"books":[],"boot":""}`} {
		if s, b := postStatus(t, srv, code, body); s != 204 {
			t.Errorf("%s: %d %s", body, s, b)
		}
	}
}

// The owner's read routes take a session only: a bot code (which may post logs and screenshots)
// cannot read them back.
func TestBot_ReadRoutesRefuseABotCode(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	for _, path := range []string{"/api/me/bot/logs", "/api/me/bot/logs.txt", "/api/me/bot/screenshots/0123456789abcdef"} {
		if s, b := request(t, srv, "GET", path, "", "", bearer(code)); s != 403 || !strings.Contains(b, `"session_required"`) {
			t.Errorf("GET %s with a bot code: %d %s, want 403 session_required", path, s, b)
		}
	}
}

// waitAgents waits until userID has n agents online.
func waitAgents(t *testing.T, bots *botHub, userID int64, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		bots.mu.Lock()
		got := len(bots.agents[userID])
		bots.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d agents online, want %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
