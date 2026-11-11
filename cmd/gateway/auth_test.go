package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

const authTestPassword = "correct horse battery"

func newAuthServer(t *testing.T, required bool) (*httptest.Server, *accounts.Service) {
	t.Helper()
	db, err := accounts.OpenDB(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := accounts.NewService(db, nil)
	reg := newConnRegistry()
	svc.SetConnections(reg)
	cfg := Config{SnapshotCacheMs: 500, MaxClients: 10}
	srv := httptest.NewServer(newMux(NewHub(cfg), NewValueHub(cfg), nil, &wsGate{acc: svc, reg: reg, required: required}))
	t.Cleanup(srv.Close)
	return srv, svc
}

// newUser registers a user and returns a session token.
func newUser(t *testing.T, svc *accounts.Service, email, role string) string {
	t.Helper()
	inv, err := svc.CreateInvite(0, role)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := svc.Register("127.0.0.1", inv.Code, email, authTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func cookie(token string) http.Header {
	return http.Header{"Cookie": {accounts.CookieName + "=" + token}}
}

func dial(t *testing.T, srv *httptest.Server, path string, hdr http.Header) (*websocket.Conn, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path, &websocket.DialOptions{HTTPHeader: hdr})
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	return conn, status, err
}

// request sends a same-origin JSON request with a session cookie (token may be "").
func request(t *testing.T, srv *httptest.Server, method, path, token, body string, extra map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Origin", srv.URL)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Cookie", accounts.CookieName+"="+token)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func readType(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	json.Unmarshal(b, &m)
	if m.Key != "" {
		return m.Type + ":" + m.Key
	}
	return m.Type
}

func TestAuth_FeedRequiresLogin(t *testing.T) {
	srv, svc := newAuthServer(t, true)
	if _, status, err := dial(t, srv, "/ws", nil); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("anonymous /ws: status %d err %v", status, err)
	}
	if _, status, _ := dial(t, srv, "/ws/value", nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous /ws/value: %d", status)
	}
	if code, _ := request(t, srv, "GET", "/api/arb/snapshot", "", "", nil); code != 401 {
		t.Fatalf("anonymous snapshot: %d", code)
	}
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	conn, _, err := dial(t, srv, "/ws", cookie(token))
	if err != nil {
		t.Fatalf("/ws with session: %v", err)
	}
	defer conn.CloseNow()
	if got := readType(t, conn); got != "snapshot" {
		t.Fatalf("first message = %s", got)
	}
	if code, _ := request(t, srv, "GET", "/api/value/snapshot", token, "", nil); code != 200 {
		t.Fatalf("snapshot with session: %d", code)
	}
	// A cookie sent from another site's page must not open the feed.
	if _, status, _ := dial(t, srv, "/ws", http.Header{"Cookie": {accounts.CookieName + "=" + token}, "Origin": {"http://evil.example"}}); status != http.StatusUnauthorized {
		t.Fatalf("cross-site /ws: %d", status)
	}
}

func TestAuth_ExtensionTicket(t *testing.T) {
	srv, svc := newAuthServer(t, true)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code, body := request(t, srv, "POST", "/api/me/extension-codes", token, `{"name":"Laptop"}`, nil)
	if code != 201 {
		t.Fatalf("create code: %d %s", code, body)
	}
	var c struct{ Code string }
	json.Unmarshal([]byte(body), &c)

	code, body = request(t, srv, "POST", "/api/auth/ws-ticket", "", `{}`,
		map[string]string{"Authorization": "Bearer " + c.Code, "Origin": "chrome-extension://abc"})
	var tk struct{ Ticket string }
	if json.Unmarshal([]byte(body), &tk); code != 200 || tk.Ticket == "" {
		t.Fatalf("ticket: %d %s", code, body)
	}
	conn, _, err := dial(t, srv, "/ws?ticket="+tk.Ticket, http.Header{"Origin": {"chrome-extension://abc"}})
	if err != nil {
		t.Fatalf("/ws with ticket: %v", err)
	}
	defer conn.CloseNow()
	if got := readType(t, conn); got != "snapshot" {
		t.Fatalf("first message = %s", got)
	}
	if _, status, _ := dial(t, srv, "/ws?ticket="+tk.Ticket, nil); status != http.StatusUnauthorized {
		t.Fatalf("a ticket must work once: %d", status)
	}
}

func TestAuth_StateNotifiesOtherSessionsAndLogoutCloses(t *testing.T) {
	srv, svc := newAuthServer(t, true)
	tokenA := newUser(t, svc, "u@example.com", accounts.RoleUser)
	_, tokenB, err := svc.Login("127.0.0.2", "u@example.com", authTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := dial(t, srv, "/ws", cookie(tokenA))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	readType(t, conn) // snapshot

	if code, _ := request(t, srv, "PUT", "/api/me/state/pins", tokenB, `{"value":[]}`, nil); code != 204 {
		t.Fatalf("put state: %d", code)
	}
	if got := readType(t, conn); got != "state:pins" {
		t.Fatalf("other session's socket got %s, want state:pins", got)
	}

	if code, _ := request(t, srv, "POST", "/api/auth/logout", tokenA, `{}`, nil); code != 204 {
		t.Fatalf("logout: %d", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil || websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("logout must close the socket with 1008, got %v", err)
	}
}

func TestAuth_TelegramIsAdminOnly(t *testing.T) {
	srv, svc := newAuthServer(t, true)
	user := newUser(t, svc, "u@example.com", accounts.RoleUser)
	admin := newUser(t, svc, "a@example.com", accounts.RoleAdmin)
	if code, _ := request(t, srv, "GET", "/api/telegram/status", "", "", nil); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
	if code, _ := request(t, srv, "GET", "/api/telegram/status", user, "", nil); code != 403 {
		t.Fatalf("user: %d", code)
	}
	if code, _ := request(t, srv, "GET", "/api/telegram/status", admin, "", nil); code != 200 {
		t.Fatalf("admin: %d", code)
	}
}

func TestAuth_NotRequiredKeepsFeedOpen(t *testing.T) {
	srv, _ := newAuthServer(t, false)
	conn, _, err := dial(t, srv, "/ws", nil)
	if err != nil {
		t.Fatalf("anonymous /ws with AUTH_REQUIRED=false: %v", err)
	}
	conn.CloseNow()
	if code, _ := request(t, srv, "GET", "/api/arb/snapshot", "", "", nil); code != 200 {
		t.Fatalf("snapshot: %d", code)
	}
	if code, _ := request(t, srv, "GET", "/api/telegram/status", "", "", nil); code != 200 {
		t.Fatalf("telegram: %d", code)
	}
}
