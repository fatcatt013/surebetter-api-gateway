package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// loginRig is a bot server with the logins on (a fixed key), one user with a session, a bot code and the user's app
// socket, and a settable clock.
type loginRig struct {
	t     *testing.T
	srv   *httptest.Server
	svc   *accounts.Service
	bots  *botHub
	clk   *botClock
	token string
	uid   int64
	code  string
	agent int64
	app   *websocket.Conn
}

func newLoginRig(t *testing.T) *loginRig {
	t.Helper()
	srv, svc, bots := newBotServer(t)
	bots.logins = mustCipher(t, testLoginKey(0), "")
	clk := &botClock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	bots.alerts = newAlertEngine(clk.now().Add(-time.Hour))
	bots.alerts.startChecked = true
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	p, ok, err := svc.AuthenticateCode(code)
	if err != nil || !ok {
		t.Fatalf("code: %v %v", ok, err)
	}
	return &loginRig{t: t, srv: srv, svc: svc, bots: bots, clk: clk, token: token, uid: userIDOf(t, svc, token), code: code,
		agent: p.CodeID, app: appSocket(t, srv, token)}
}

var httpsHeader = map[string]string{"X-Forwarded-Proto": "https"}

// do sends a request (session token or bot code, never both) and returns status, body and headers.
func (r *loginRig) do(method, path, token, code, body string, hdr map[string]string) (int, string, http.Header) {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Origin", r.srv.URL)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Cookie", accounts.CookieName+"="+token)
	}
	if code != "" {
		req.Header.Set("Authorization", "Bearer "+code)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// app and agentReq are HTTPS requests of the app (session) and of the agent (bot code).
func (r *loginRig) appReq(method, path, body string) (int, string) {
	r.t.Helper()
	s, b, _ := r.do(method, path, r.token, "", body, httpsHeader)
	return s, b
}

func (r *loginRig) agentReq(method, path, body string) (int, string) {
	r.t.Helper()
	s, b, _ := r.do(method, path, "", r.code, body, httpsHeader)
	return s, b
}

func (r *loginRig) loginPath(window string) string {
	return "/api/me/bot/agents/" + i64s(r.agent) + "/windows/" + window + "/login"
}

func i64s(n int64) string { return strconv.FormatInt(n, 10) }

// put saves a login and expects 200.
func (r *loginRig) put(window, username, password string) loginView {
	r.t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	s, b := r.appReq("PUT", r.loginPath(window), string(body))
	var v loginView
	if json.Unmarshal([]byte(b), &v); s != 200 {
		r.t.Fatalf("PUT %s: %d %s", window, s, b)
	}
	return v
}

// list is GET /api/me/bot/logins.
func (r *loginRig) list() []loginView {
	r.t.Helper()
	s, b := r.appReq("GET", "/api/me/bot/logins", "")
	var out struct{ Logins []loginView }
	if json.Unmarshal([]byte(b), &out); s != 200 || out.Logins == nil {
		r.t.Fatalf("GET logins: %d %s", s, b)
	}
	return out.Logins
}

// dialAgent opens the bot socket, over "wss" (X-Forwarded-Proto: https) when secure, and posts a status of proto with
// tipsport-1 and tipsport-2 idle; it reads the two {"type":"bot"} messages on the app socket.
func (r *loginRig) dialAgent(secure bool, proto int) *websocket.Conn {
	r.t.Helper()
	hdr := http.Header{}
	if secure {
		hdr.Set("X-Forwarded-Proto", "https")
	}
	conn, status, err := dial(r.t, r.srv, "/ws?ticket="+wsTicket(r.t, r.srv, r.code)+"&feed=0&bot=1", hdr)
	if err != nil {
		r.t.Fatalf("bot socket: %d %v", status, err)
	}
	r.t.Cleanup(func() { conn.CloseNow() })
	expectBot(r.t, r.app, "on agent connect")
	st := strings.Replace(p2Status("", win("tipsport", 1, "idle"), win("tipsport", 2, "idle")), `"proto":2`, `"proto":`+i64s(int64(proto)), 1)
	if s, b := postStatus(r.t, r.srv, r.code, st); s != 204 {
		r.t.Fatalf("status: %d %s", s, b)
	}
	expectBot(r.t, r.app, "on status")
	return conn
}

// expectNothing checks that conn gets no message within d (it closes conn: call it last on that socket).
func expectNothing(t *testing.T, conn *websocket.Conn, d time.Duration, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if _, b, err := conn.Read(ctx); err == nil {
		t.Fatalf("%s: got %s", what, b)
	}
}

func loginRoutes(r *loginRig) []struct{ method, path, body string } {
	return []struct{ method, path, body string }{
		{"GET", "/api/me/bot/logins", ""},
		{"PUT", r.loginPath("tipsport-1"), `{"username":"u","password":"p"}`},
		{"DELETE", r.loginPath("tipsport-1"), ""},
		{"GET", "/api/me/bot/logins/mine", ""},
		{"POST", "/api/me/bot/logins/tipsport-1/reveal", `{}`},
		{"POST", "/api/me/bot/logins/tipsport-1/result", `{"result":"ok"}`},
	}
}

func TestBotLogins_OffWithoutAKey(t *testing.T) {
	r := newLoginRig(t)
	r.bots.logins = nil
	for _, rt := range loginRoutes(r) {
		for _, creds := range [][2]string{{r.token, ""}, {"", r.code}, {"", ""}} {
			if s, b, _ := r.do(rt.method, rt.path, creds[0], creds[1], rt.body, httpsHeader); s != 503 || !strings.Contains(b, `"logins_unavailable"`) {
				t.Fatalf("%s %s: %d %s", rt.method, rt.path, s, b)
			}
		}
	}
}

func TestBotLogins_HTTPSRequired(t *testing.T) {
	r := newLoginRig(t)
	for _, rt := range loginRoutes(r) {
		for name, hdr := range map[string]map[string]string{"no header": nil, "http": {"X-Forwarded-Proto": "http"},
			"HTTPS": {"X-Forwarded-Proto": "HTTPS"}, "list": {"X-Forwarded-Proto": "https, http"}} {
			for _, creds := range [][2]string{{r.token, ""}, {"", r.code}, {"", ""}} {
				if s, b, _ := r.do(rt.method, rt.path, creds[0], creds[1], rt.body, hdr); s != 403 || !strings.Contains(b, `"https_required"`) {
					t.Fatalf("%s %s %s: %d %s", rt.method, rt.path, name, s, b)
				}
			}
		}
		// Over HTTPS, the auth comes next.
		if s, b, _ := r.do(rt.method, rt.path, "", "", rt.body, httpsHeader); s != 401 {
			t.Fatalf("%s %s without credentials: %d %s", rt.method, rt.path, s, b)
		}
	}
	// BOT_LOGINS_ALLOW_HTTP: plain HTTP is let through.
	r.bots.loginsAllowHTTP = true
	if s, b, _ := r.do("GET", "/api/me/bot/logins", r.token, "", "", nil); s != 200 {
		t.Fatalf("allowed http: %d %s", s, b)
	}
	if s, b, _ := r.do("GET", "/api/me/bot/logins/mine", "", r.code, "", nil); s != 200 {
		t.Fatalf("allowed http (agent): %d %s", s, b)
	}
}

func TestSecureRequest(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	if secureRequest(req, false) || !secureRequest(req, true) {
		t.Fatal("plain request")
	}
	req.TLS = &tls.ConnectionState{}
	if !secureRequest(req, false) {
		t.Fatal("TLS request")
	}
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Add("X-Forwarded-Proto", "https")
	if !secureRequest(req, false) {
		t.Fatal("forwarded https")
	}
	req.Header.Add("X-Forwarded-Proto", "https")
	if secureRequest(req, false) {
		t.Fatal("two forwarded headers")
	}
}

func TestBotLogins_PutValidation(t *testing.T) {
	r := newLoginRig(t)
	path := r.loginPath("tipsport-1")
	long := strings.Repeat("a", 101)
	for name, body := range map[string]string{
		"not JSON":           `{`,
		"not an object":      `["u","p"]`,
		"no password":        `{"username":"u"}`,
		"extra field":        `{"username":"u","password":"p","x":1}`,
		"number":             `{"username":"u","password":1}`,
		"null":               `{"username":"u","password":null}`,
		"empty user":         `{"username":"   ","password":"p"}`,
		"empty password":     `{"username":"u","password":""}`,
		"user too long":      `{"username":"` + long + `","password":"p"}`,
		"password too long":  `{"username":"u","password":"` + strings.Repeat("é", 201) + `"}`,
		"control in user":    `{"username":"u\u0007","password":"p"}`,
		"control in pass":    `{"username":"u","password":"p\n"}`,
		"DEL in pass":        `{"username":"u","password":"p\u007f"}`,
		"body over 1 KiB":    `{"username":"u","password":"` + strings.Repeat("p", 1100) + `"}`,
		"trailing junk":      `{"username":"u","password":"p"} x`,
		"duplicate username": `{"username":"u","username":"v"}`,
	} {
		if s, b := r.appReq("PUT", path, body); s != 400 || !strings.Contains(b, `"bad_login"`) {
			t.Fatalf("%s: %d %s", name, s, b)
		}
	}
	for _, w := range []string{"tipsport", "tipsport-0", "tipsport-100", "Tipsport-1", "tip_sport-1", strings.Repeat("a", 31) + "-1", "tipsport-01"} {
		if s, b := r.appReq("PUT", r.loginPath(w), `{"username":"u","password":"p"}`); s != 400 || !strings.Contains(b, `"bad_login"`) {
			t.Fatalf("window %q: %d %s", w, s, b)
		}
		if s, b := r.appReq("DELETE", r.loginPath(w), ""); s != 400 || !strings.Contains(b, `"bad_login"`) {
			t.Fatalf("DELETE window %q: %d %s", w, s, b)
		}
	}
	// Edge sizes are fine: 100-rune user name (surrounding spaces trimmed), 200-rune password, window 99.
	v := r.put("tipsport-99", "  "+strings.Repeat("ž", 100)+" ", strings.Repeat("é", 200))
	if v.Username != strings.Repeat("ž", 100) || v.Bookmaker != "tipsport" || v.WindowID != "tipsport-99" {
		t.Fatalf("edge sizes: %+v", v)
	}
	// Not the user's agent: another user's code, a revoked code, an extension code, not a number.
	other := newUser(t, r.svc, "o@example.com", accounts.RoleUser)
	otherCode := newCode(t, r.srv, other, "bot")
	op, _, _ := r.svc.AuthenticateCode(otherCode)
	revoked, _ := r.svc.CreateCode(r.uid, "Old", accounts.KindBot)
	r.svc.RevokeExtensionCode(r.uid, revoked.ID)
	ext, _ := r.svc.CreateCode(r.uid, "Ext", accounts.KindExtension)
	for _, id := range []string{i64s(op.CodeID), i64s(revoked.ID), i64s(ext.ID), "x", "0"} {
		p := "/api/me/bot/agents/" + id + "/windows/tipsport-1/login"
		if s, b := r.appReq("PUT", p, `{"username":"u","password":"p"}`); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
			t.Fatalf("agent %s: %d %s", id, s, b)
		}
		if s, b := r.appReq("DELETE", p, ""); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
			t.Fatalf("DELETE agent %s: %d %s", id, s, b)
		}
	}
	// A bot code cannot use the app's routes, a session cannot use the agent's.
	if s, b, _ := r.do("PUT", path, "", r.code, `{"username":"u","password":"p"}`, httpsHeader); s != 403 {
		t.Fatalf("bot code PUT: %d %s", s, b)
	}
	if s, b, _ := r.do("GET", "/api/me/bot/logins/mine", r.token, "", "", httpsHeader); s != 403 {
		t.Fatalf("session mine: %d %s", s, b)
	}
}

func TestBotLogins_PutListDeleteAndMessages(t *testing.T) {
	r := newLoginRig(t)
	agent := r.dialAgent(true, 5)
	v := r.put("tipsport-1", " Jan.Novak ", "Heslo-1é")
	if v.AgentID != r.agent || v.WindowID != "tipsport-1" || v.Bookmaker != "tipsport" || v.Username != "Jan.Novak" ||
		v.UpdatedAt != r.clk.now().UnixMilli() || v.Failing != "" || v.LastResult != "" || v.LastAt != 0 || v.LastOkAt != 0 {
		t.Fatalf("PUT answer: %+v", v)
	}
	if got := readType(t, agent); got != "logins" {
		t.Fatalf("agent got %s", got)
	}
	expectBot(t, r.app, "after PUT")
	// The answer and the list never carry the password.
	s, b := r.appReq("GET", "/api/me/bot/logins", "")
	if s != 200 || strings.Contains(b, "Heslo") || strings.Contains(b, "password") {
		t.Fatalf("list: %d %s", s, b)
	}
	if l := r.list(); len(l) != 1 || l[0] != v {
		t.Fatalf("list: %+v", l)
	}
	// The agent's own list: window, updatedAt, failing only.
	s, b = r.agentReq("GET", "/api/me/bot/logins/mine", "")
	if s != 200 || b != `{"logins":[{"windowId":"tipsport-1","updatedAt":`+i64s(v.UpdatedAt)+`,"failing":""}]}`+"\n" {
		t.Fatalf("mine: %d %s", s, b)
	}
	if s, b := r.appReq("DELETE", r.loginPath("tipsport-1"), ""); s != 204 {
		t.Fatalf("DELETE: %d %s", s, b)
	}
	if got := readType(t, agent); got != "logins" {
		t.Fatalf("agent after DELETE got %s", got)
	}
	expectBot(t, r.app, "after DELETE")
	if s, b := r.appReq("DELETE", r.loginPath("tipsport-1"), ""); s != 204 {
		t.Fatalf("DELETE again: %d %s", s, b)
	}
	if l := r.list(); len(l) != 0 {
		t.Fatalf("after delete: %+v", l)
	}
}

func TestBotLogins_LoginsMessageOnlyToProtocol5(t *testing.T) {
	r := newLoginRig(t)
	agent := r.dialAgent(true, 4)
	r.put("tipsport-1", "u", "p")
	expectBot(t, r.app, "after PUT")
	expectNothing(t, agent, 300*time.Millisecond, "a protocol-4 agent")
}

func TestBotLogins_ListShowsKeyLost(t *testing.T) {
	r := newLoginRig(t)
	r.put("tipsport-1", "jan", "p1")
	// A row sealed with a key this gateway no longer has.
	lost := mustCipher(t, testLoginKey(9), "")
	secret, keyID, tag, _ := lost.seal(r.uid, r.agent, "tipsport-2", "eva", "p2")
	if err := r.svc.SaveBotLogin(accounts.BotLoginRow{UserID: r.uid, CodeID: r.agent, WindowID: "tipsport-2", Bookmaker: "tipsport",
		Secret: secret, KeyID: keyID, UserTag: tag, UpdatedAt: 5}); err != nil {
		t.Fatal(err)
	}
	l := r.list()
	if len(l) != 2 || l[0].Username != "jan" || l[0].Failing != "" || l[1].Username != "" || l[1].Failing != "key_lost" {
		t.Fatalf("list: %+v", l)
	}
	if s, b := r.agentReq("GET", "/api/me/bot/logins/mine", ""); s != 200 || !strings.Contains(b, `"windowId":"tipsport-2","updatedAt":5,"failing":"key_lost"`) {
		t.Fatalf("mine: %d %s", s, b)
	}
	// Putting the right key back (as the previous one) heals it.
	r.bots.logins = mustCipher(t, testLoginKey(0), testLoginKey(9))
	if l := r.list(); l[1].Username != "eva" || l[1].Failing != "" {
		t.Fatalf("healed: %+v", l[1])
	}
}

func TestBotLogins_RevealSocketChecks(t *testing.T) {
	r := newLoginRig(t)
	r.put("tipsport-1", "jan", "p1")
	if s, b := r.agentReq("POST", "/api/me/bot/logins/tipsport-1/reveal", `{}`); s != 409 || !strings.Contains(b, `"agent_offline"`) {
		t.Fatalf("no socket: %d %s", s, b)
	}
	plain := r.dialAgent(false, 5)
	if s, b := r.agentReq("POST", "/api/me/bot/logins/tipsport-1/reveal", `{}`); s != 403 || !strings.Contains(b, `"https_required"`) {
		t.Fatalf("plain ws socket: %d %s", s, b)
	}
	plain.Close(websocket.StatusNormalClosure, "")
	waitBot(t, r.bots, r.uid, false)
	expectBot(t, r.app, "on agent disconnect")
	r.dialAgent(true, 5)
	s, b, hdr := r.do("POST", "/api/me/bot/logins/tipsport-1/reveal", "", r.code, `{}`, httpsHeader)
	if s != 200 || b != `{"password":"p1","username":"jan"}`+"\n" || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("reveal: %d %s %v", s, b, hdr)
	}
}

func TestBotLogins_RevealOrderCapAndDay(t *testing.T) {
	r := newLoginRig(t)
	r.dialAgent(true, 5)
	reveal := func(w string) (int, string) { return r.agentReq("POST", "/api/me/bot/logins/"+w+"/reveal", `{}`) }
	if s, b := reveal("tipsport-1"); s != 404 || !strings.Contains(b, `"no_login"`) {
		t.Fatalf("no row: %d %s", s, b)
	}
	// Another agent's window is not this agent's.
	other, _ := r.svc.CreateCode(r.uid, "Other", accounts.KindBot)
	secret, keyID, tag, _ := r.bots.logins.seal(r.uid, other.ID, "betano-1", "x", "y")
	r.svc.SaveBotLogin(accounts.BotLoginRow{UserID: r.uid, CodeID: other.ID, WindowID: "betano-1", Bookmaker: "betano", Secret: secret, KeyID: keyID, UserTag: tag})
	if s, _ := reveal("betano-1"); s != 404 {
		t.Fatalf("another agent's window: %d", s)
	}
	r.put("tipsport-1", "jan", "p1")
	for i := 0; i < revealsPerDay; i++ {
		if s, b := reveal("tipsport-1"); s != 200 {
			t.Fatalf("reveal %d: %d %s", i+1, s, b)
		}
	}
	if s, b := reveal("tipsport-1"); s != 429 || !strings.Contains(b, `"too_many_reveals"`) {
		t.Fatalf("over the cap: %d %s", s, b)
	}
	// A failing login is refused before the cap is looked at.
	r.svc.SetBotLoginResult(r.agent, "tipsport-1", "wrong_password", 1)
	if s, b := reveal("tipsport-1"); s != 409 || !strings.Contains(b, `"login_failing"`) {
		t.Fatalf("failing: %d %s", s, b)
	}
	// A save clears failing and the count; the next Prague day starts again too.
	r.put("tipsport-1", "jan", "p2")
	for i := 0; i < revealsPerDay; i++ {
		reveal("tipsport-1")
	}
	if s, _ := reveal("tipsport-1"); s != 429 {
		t.Fatalf("cap after save: %d", s)
	}
	r.clk.advance(14 * time.Hour) // 2026-11-26 00:00 Prague
	if s, b := reveal("tipsport-1"); s != 200 || !strings.Contains(b, `"p2"`) {
		t.Fatalf("next day: %d %s", s, b)
	}
	// key_lost and a secret that does not decrypt are failing.
	r.bots.logins = mustCipher(t, testLoginKey(5), "")
	if s, b := reveal("tipsport-1"); s != 409 || !strings.Contains(b, `"login_failing"`) {
		t.Fatalf("key lost: %d %s", s, b)
	}
	r.bots.logins = mustCipher(t, testLoginKey(0), "")
	row, _, _ := r.svc.BotLogin(r.agent, "tipsport-1")
	row.WindowID = "tipsport-2" // the secret moved to another row
	r.svc.SaveBotLogin(row)
	if s, b := reveal("tipsport-2"); s != 409 || !strings.Contains(b, `"login_failing"`) {
		t.Fatalf("moved secret: %d %s", s, b)
	}
}

func TestBotLogins_Result(t *testing.T) {
	r := newLoginRig(t)
	agent := r.dialAgent(true, 5)
	result := func(w, body string) (int, string) { return r.agentReq("POST", "/api/me/bot/logins/"+w+"/result", body) }
	for name, body := range map[string]string{"unknown": `{"result":"fine"}`, "none": `{}`, "not JSON": `x`,
		"long detail": `{"result":"ok","detail":"` + strings.Repeat("d", 121) + `"}`,
		"big body":    `{"result":"ok","x":"` + strings.Repeat("d", 1100) + `"}`} {
		if s, b := result("tipsport-1", body); s != 400 || !strings.Contains(b, `"bad_result"`) {
			t.Fatalf("%s: %d %s", name, s, b)
		}
	}
	if s, b := result("tipsport-1", `{"result":"ok"}`); s != 404 || !strings.Contains(b, `"no_login"`) {
		t.Fatalf("no row: %d %s", s, b)
	}
	r.put("tipsport-1", "Jan", "p")
	readType(t, agent)
	expectBot(t, r.app, "after PUT 1")
	r.put("tipsport-2", "jan", "q")
	readType(t, agent)
	expectBot(t, r.app, "after PUT 2")
	r.clk.advance(time.Minute)
	if s, b := result("tipsport-1", `{"result":"ok","detail":"`+strings.Repeat("ř", 120)+`"}`); s != 204 {
		t.Fatalf("ok: %d %s", s, b)
	}
	expectBot(t, r.app, "after a result")
	now := r.clk.now().UnixMilli()
	if l := r.list(); l[0].LastResult != "ok" || l[0].LastAt != now || l[0].LastOkAt != now || l[1].LastAt != 0 {
		t.Fatalf("after ok: %+v", l)
	}
	r.clk.advance(time.Minute)
	if s, b := result("tipsport-1", `{"result":"wrong_password"}`); s != 204 {
		t.Fatalf("wrong_password: %d %s", s, b)
	}
	if got := readType(t, agent); got != "logins" {
		t.Fatalf("agent after failing: %s", got)
	}
	l := r.list()
	if l[0].Failing != "wrong_password" || l[0].LastResult != "wrong_password" || l[0].LastOkAt != now || l[1].Failing != "wrong_password" {
		t.Fatalf("failing spread by user name: %+v", l)
	}
	// The login alert went to the app.
	var alert map[string]any
	for alert == nil {
		m := readJSON(t, r.app)
		if m["type"] == "alert" {
			alert = m["alert"].(map[string]any)
		}
	}
	if alert["title"] != "Login refused" || alert["detail"] != "Tipsport 1 (Test) · wrong password" || alert["kind"] != "attention" ||
		alert["windowId"] != "tipsport-1" || alert["orderId"] != "" {
		t.Fatalf("alert: %v", alert)
	}
}

// The status's logging_in and loggedIn (contract §5.3).
func TestBotLogins_StatusFields(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	startAgentWith(t, srv, code, tipBetIdle, app)
	for _, v := range []string{`"yes"`, `1`, `{}`} {
		st := strings.Replace(p2Status("", win("tipsport", 1, "idle")), `"loggedIn":null`, `"loggedIn":`+v, 1)
		if s, b := postStatus(t, srv, code, st); s != 400 || !strings.Contains(b, `"bad_status"`) {
			t.Fatalf("loggedIn %s: %d %s", v, s, b)
		}
	}
	for _, v := range []string{"true", "false", "null"} {
		st := strings.Replace(p2Status("", win("tipsport", 1, "logging_in")), `"loggedIn":null`, `"loggedIn":`+v, 1)
		if s, b := postStatus(t, srv, code, st); s != 204 {
			t.Fatalf("loggedIn %s: %d %s", v, s, b)
		}
	}
}

// A logging_in window is held like a busy one: the order is accepted, waits, and goes when the window is idle.
func TestBotLogins_LoggingInWindowMakesTheOrderWait(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "logging_in")), app)
	if v := getView(t, srv, token); len(v.Books) != 2 || v.Books[0].Bookmaker != "betano" || v.Books[0].Ready || !v.Books[0].Usable {
		t.Fatalf("view: %+v", v.Books)
	}
	a := postOrderOK(t, srv, token, tipBetOrder("k"))
	if a.State != "waiting" || a.Reason != "Needs a free Betano window" {
		t.Fatalf("answer: %+v", a)
	}
	expectEvent(t, app, a.OrderID, "waiting", "Needs a free Betano window")
	expectBot(t, app, "on the waiting order")
	if s, b := postStatus(t, srv, code, tipBetIdle); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	if o := readOrder(t, bot); o.OrderID != a.OrderID || o.Legs[1].WindowID != "betano-1" {
		t.Fatalf("agent got %+v", o)
	}
}
