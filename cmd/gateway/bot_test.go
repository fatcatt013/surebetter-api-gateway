package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	bots := newBotHub(reg)
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
	time.Sleep(100 * time.Millisecond) // the replaced socket's cleanup must not take the new one offline
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
	if got := getBot(t, srv, token); got != `{"online":true,"books":[{"bookmaker":"tipsport","ready":true},{"bookmaker":"betano","ready":false}],"dryRun":false,"busy":false}` {
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
