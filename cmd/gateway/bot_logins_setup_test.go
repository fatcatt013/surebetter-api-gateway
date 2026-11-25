package main

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// syncBuffer is a log sink handler goroutines may write to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLogs sends every log line (debug and up) to the returned buffer until the test ends.
func captureLogs(t *testing.T) *syncBuffer {
	buf := &syncBuffer{}
	prev, level := log.Logger, zerolog.GlobalLevel()
	log.Logger = zerolog.New(buf)
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = prev; zerolog.SetGlobalLevel(level) })
	return buf
}

func TestConfig_BotLogins(t *testing.T) {
	for _, k := range []string{"BOT_LOGINS_KEY", "BOT_LOGINS_KEY_PREVIOUS", "BOT_LOGINS_ALLOW_HTTP"} {
		t.Setenv(k, "")
	}
	if c := configFromEnv(); c.BotLoginsKey != "" || c.BotLoginsKeyPrevious != "" || c.BotLoginsAllowHTTP {
		t.Fatalf("defaults: %+v", c)
	}
	t.Setenv("BOT_LOGINS_KEY", testLoginKey(0))
	t.Setenv("BOT_LOGINS_KEY_PREVIOUS", testLoginKey(1))
	t.Setenv("BOT_LOGINS_ALLOW_HTTP", "1")
	if c := configFromEnv(); c.BotLoginsKey != testLoginKey(0) || c.BotLoginsKeyPrevious != testLoginKey(1) || !c.BotLoginsAllowHTTP {
		t.Fatalf("set: %+v", c)
	}
	t.Setenv("BOT_LOGINS_ALLOW_HTTP", "true") // only 1
	if configFromEnv().BotLoginsAllowHTTP {
		t.Fatal("BOT_LOGINS_ALLOW_HTTP=true allowed http")
	}
}

func TestSetupBotLogins_OffAndWarnings(t *testing.T) {
	buf := captureLogs(t)
	short := strings.Repeat("A", 40) + "=="
	for _, key := range []string{"", "not base64", short} {
		if c := setupBotLogins(Config{BotLoginsKey: key}, nil); c != nil {
			t.Fatalf("key %q: on", key)
		}
	}
	if n := strings.Count(buf.String(), "bot logins off: BOT_LOGINS_KEY missing or not 32 bytes"); n != 3 {
		t.Fatalf("off lines: %d\n%s", n, buf)
	}
	if strings.Contains(buf.String(), short) {
		t.Fatal("the bad key was logged")
	}
	c := setupBotLogins(Config{BotLoginsKey: testLoginKey(0), BotLoginsKeyPrevious: "nope", BotLoginsAllowHTTP: true}, nil)
	if c == nil || c.prev != nil {
		t.Fatalf("bad previous: %+v", c)
	}
	out := buf.String()
	if !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, "BOT_LOGINS_KEY_PREVIOUS ignored") ||
		!strings.Contains(out, "BOT_LOGINS_ALLOW_HTTP=1") || strings.Contains(out, testLoginKey(0)) {
		t.Fatalf("warnings:\n%s", out)
	}
}

func TestSetupBotLogins_Rotation(t *testing.T) {
	_, svc, _, _ := newBotServerDB(t)
	uid := func() int64 {
		inv, _ := svc.CreateInvite(0, accounts.RoleUser)
		p, _, err := svc.Register("10.0.0.1", inv.Code, "u@example.com", "correct horse battery")
		if err != nil {
			t.Fatal(err)
		}
		return p.UserID
	}()
	code, _ := svc.CreateCode(uid, "A", accounts.KindBot)
	old := mustCipher(t, testLoginKey(1), "")
	lost := mustCipher(t, testLoginKey(2), "")
	save := func(c *loginCipher, w, u, p string) {
		secret, keyID, tag, _ := c.seal(uid, code.ID, w, u, p)
		if err := svc.SaveBotLogin(accounts.BotLoginRow{UserID: uid, CodeID: code.ID, WindowID: w, Bookmaker: "tipsport", Secret: secret,
			KeyID: keyID, UserTag: tag}); err != nil {
			t.Fatal(err)
		}
	}
	save(old, "tipsport-1", "Jan", "p1")
	save(old, "tipsport-2", "jan", "p2")
	save(lost, "tipsport-3", "eva", "p3")
	buf := captureLogs(t)
	cfg := Config{BotLoginsKey: testLoginKey(0), BotLoginsKeyPrevious: testLoginKey(1)}
	c := setupBotLogins(cfg, svc)
	if c == nil || c.prev == nil {
		t.Fatal("setup")
	}
	if !strings.Contains(buf.String(), `"count":2,"message":"bot logins re-encrypted"`) {
		t.Fatalf("log:\n%s", buf)
	}
	cur := mustCipher(t, testLoginKey(0), "")
	rows, _ := svc.ListBotLoginsOfCode(code.ID)
	for i, want := range [][2]string{{"Jan", "p1"}, {"jan", "p2"}} {
		r := rows[i]
		u, p, err := cur.open(r.Secret, r.KeyID, r.UserID, r.CodeID, r.WindowID)
		if err != nil || u != want[0] || p != want[1] || r.KeyID != cur.cur.id || r.UserTag != cur.tag("jan") {
			t.Fatalf("row %d: %q %q %v %+v", i, u, p, err, r)
		}
	}
	if rows[2].KeyID != lost.cur.id {
		t.Fatal("the unknown key's row was touched")
	}
	// Run twice: nothing left to re-encrypt.
	setupBotLogins(cfg, svc)
	if !strings.Contains(buf.String(), `"count":0,"message":"bot logins re-encrypted"`) {
		t.Fatalf("second run:\n%s", buf)
	}
	for _, s := range []string{"Jan", "jan", "eva", "p1", "p2", "p3", testLoginKey(0), testLoginKey(1)} {
		if strings.Contains(buf.String(), s) {
			t.Fatalf("%q in the log:\n%s", s, buf)
		}
	}
}

func TestBotLogins_RemoveCommandDeletesTheLogin(t *testing.T) {
	r := newLoginRig(t)
	agent := r.dialAgent(true, 5)
	r.put("tipsport-1", "jan", "p1")
	readType(t, agent)
	expectBot(t, r.app, "after PUT 1")
	r.put("tipsport-2", "jan", "p2")
	readType(t, agent)
	expectBot(t, r.app, "after PUT 2")
	remove := func(ok bool) {
		t.Helper()
		done := commandAsync(t, r.srv, r.token, fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/remove", r.agent), ``)
		cmd := readCommand(t, agent)
		ack := `{"id":"` + cmd.ID + `","ok":true,"windowId":"tipsport-1"}`
		if !ok {
			ack = `{"id":"` + cmd.ID + `","ok":false,"error":"bad_state"}`
		}
		if s, b := postAck(t, r.srv, r.code, ack); s != 204 {
			t.Fatalf("ack: %d %s", s, b)
		}
		<-done
	}
	remove(false)
	if l := r.list(); len(l) != 2 {
		t.Fatalf("refused remove deleted: %+v", l)
	}
	remove(true)
	if got := readType(t, agent); got != "logins" {
		t.Fatalf("agent after remove: %s", got)
	}
	if l := r.list(); len(l) != 1 || l[0].WindowID != "tipsport-2" {
		t.Fatalf("after remove: %+v", l)
	}
}

// Neither the user name nor the password reaches the log, whatever route handles them (contract §6 tests).
func TestBotLogins_NeverLogged(t *testing.T) {
	r := newLoginRig(t)
	buf := captureLogs(t)
	const user, pass = "Uzivatel.Tajny", "Heslo-Tajne-42"
	agent := r.dialAgent(true, 5)
	r.put("tipsport-1", user, pass)
	r.put("tipsport-2", user, pass)
	r.appReq("PUT", r.loginPath("tipsport-3"), `{"username":"`+user+`","password":"`+pass+`","x":1}`) // refused
	r.appReq("PUT", r.loginPath("bad"), `{"username":"`+user+`","password":"`+pass+`"}`)              // refused
	r.list()
	r.agentReq("GET", "/api/me/bot/logins/mine", "")
	if s, b := r.agentReq("POST", "/api/me/bot/logins/tipsport-1/reveal", `{}`); s != 200 || !strings.Contains(b, pass) {
		t.Fatalf("reveal: %d %s", s, b)
	}
	r.agentReq("POST", "/api/me/bot/logins/tipsport-1/result", `{"result":"code","detail":"code page"}`)
	r.agentReq("POST", "/api/me/bot/logins/tipsport-1/result", `{"result":"wrong_password"}`)
	r.agentReq("POST", "/api/me/bot/logins/tipsport-1/reveal", `{}`) // failing
	r.agentReq("POST", "/api/me/bot/logins/tipsport-1/result", `{"result":"bogus","detail":"`+pass+`"}`)
	r.appReq("DELETE", r.loginPath("tipsport-2"), "")
	agent.Close(websocket.StatusNormalClosure, "")
	time.Sleep(50 * time.Millisecond)
	out := buf.String()
	if !strings.Contains(out, "bot login revealed") || !strings.Contains(out, "bot login saved") || !strings.Contains(out, "bot login alert") {
		t.Fatalf("the expected lines are missing:\n%s", out)
	}
	for _, s := range []string{user, strings.ToLower(user), pass} {
		if strings.Contains(out, s) {
			t.Fatalf("%q in the log:\n%s", s, out)
		}
	}
}
