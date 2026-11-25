package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// appChan adds a channel client for the rig's user (like an app socket) and returns a function that drains it: the
// message types, and the alerts among them.
func (r *loginRig) appChan() func() ([]string, []botAlert) {
	c := &Client{principal: accounts.Principal{UserID: r.uid}, send: make(chan []byte, 512), notify: true}
	r.bots.reg.add(c)
	return func() ([]string, []botAlert) {
		var types []string
		var alerts []botAlert
		for {
			select {
			case b := <-c.send:
				var m struct {
					Type  string   `json:"type"`
					Alert botAlert `json:"alert"`
				}
				json.Unmarshal(b, &m)
				types = append(types, m.Type)
				if m.Type == "alert" {
					alerts = append(alerts, m.Alert)
				}
			default:
				return types, alerts
			}
		}
	}
}

func (r *loginRig) openPrompt(window string, digits int, expiresAt int64) (int, string) {
	r.t.Helper()
	return r.agentReq("POST", "/api/me/bot/logins/"+window+"/code-prompt", `{"digits":`+i64s(int64(digits))+`,"expiresAt":`+i64s(expiresAt)+`}`)
}

// mustPrompt opens a prompt expiring in 5 min and returns its id.
func (r *loginRig) mustPrompt(window string, digits int) string {
	r.t.Helper()
	s, b := r.openPrompt(window, digits, r.clk.now().Add(5*time.Minute).UnixMilli())
	var out struct {
		PromptID  string `json:"promptId"`
		TriesLeft int    `json:"triesLeft"`
	}
	if json.Unmarshal([]byte(b), &out); s != 200 || !promptIDPattern.MatchString(out.PromptID) || out.TriesLeft != 3 {
		r.t.Fatalf("open %s: %d %s", window, s, b)
	}
	return out.PromptID
}

func (r *loginRig) submit(window, code string) (int, string) {
	r.t.Helper()
	return r.appReq("POST", "/api/me/bot/agents/"+i64s(r.agent)+"/windows/"+window+"/login-code", `{"code":"`+code+`"}`)
}

func (r *loginRig) fetch(window, id string) (int, string) {
	r.t.Helper()
	return r.agentReq("POST", "/api/me/bot/logins/"+window+"/code", `{"promptId":"`+id+`"}`)
}

func (r *loginRig) wrong(window, id string) (int, string) {
	r.t.Helper()
	return r.agentReq("POST", "/api/me/bot/logins/"+window+"/code-prompt/wrong", `{"promptId":"`+id+`"}`)
}

func (r *loginRig) prompts() []codePromptView {
	r.t.Helper()
	s, b := r.appReq("GET", "/api/me/bot/login-codes", "")
	var out struct{ Prompts []codePromptView }
	if json.Unmarshal([]byte(b), &out); s != 200 || out.Prompts == nil {
		r.t.Fatalf("GET login-codes: %d %s", s, b)
	}
	return out.Prompts
}

// codeRig is a login rig with a protocol-6 agent over wss, a login on tipsport-1 and tipsport-2, and an app channel.
func newCodeRig(t *testing.T) (*loginRig, *websocket.Conn, func() ([]string, []botAlert)) {
	r := newLoginRig(t)
	agent := r.dialAgent(true, codeProto)
	r.put("tipsport-1", "jan", "p1")
	readType(t, agent)
	r.put("tipsport-2", "jan", "p2")
	readType(t, agent)
	drain := r.appChan()
	return r, agent, drain
}

func TestLoginCodes_Open(t *testing.T) {
	r := newLoginRig(t)
	exp := r.clk.now().Add(5 * time.Minute).UnixMilli()
	if s, b := r.openPrompt("tipsport-1", 6, exp); s != 404 || !strings.Contains(b, `"no_login"`) {
		t.Fatalf("no socket, no login: %d %s", s, b)
	}
	old := r.dialAgent(true, 5)
	if s, b := r.openPrompt("tipsport-1", 6, exp); s != 409 || !strings.Contains(b, `"agent_update"`) {
		t.Fatalf("proto 5: %d %s", s, b)
	}
	old.Close(websocket.StatusNormalClosure, "")
	waitBot(t, r.bots, r.uid, false)
	expectBot(t, r.app, "on disconnect")
	r.dialAgent(true, codeProto)
	now := r.clk.now()
	for name, body := range map[string]string{
		"digits 3":       `{"digits":3,"expiresAt":` + i64s(exp) + `}`,
		"digits 9":       `{"digits":9,"expiresAt":` + i64s(exp) + `}`,
		"no digits":      `{"expiresAt":` + i64s(exp) + `}`,
		"no expiresAt":   `{"digits":6}`,
		"expired":        `{"digits":6,"expiresAt":` + i64s(now.UnixMilli()) + `}`,
		"too far":        `{"digits":6,"expiresAt":` + i64s(now.Add(codePromptAhead).UnixMilli()+1) + `}`,
		"not JSON":       `x`,
		"string digits":  `{"digits":"6","expiresAt":` + i64s(exp) + `}`,
		"body over 1KiB": `{"digits":6,"expiresAt":` + i64s(exp) + `,"x":"` + strings.Repeat("x", 1100) + `"}`,
	} {
		if s, b := r.agentReq("POST", "/api/me/bot/logins/tipsport-1/code-prompt", body); s != 400 || !strings.Contains(b, `"bad_prompt"`) {
			t.Fatalf("%s: %d %s", name, s, b)
		}
	}
	if s, b := r.openPrompt("tipsport-1", 6, now.Add(codePromptAhead).UnixMilli()); s != 404 || !strings.Contains(b, `"no_login"`) {
		t.Fatalf("no login (5 min 30 s is fine): %d %s", s, b)
	}
	r.put("tipsport-1", "jan", "p1")
	drain := r.appChan()
	first := r.mustPrompt("tipsport-1", 6)
	types, alerts := drain()
	if len(alerts) != 1 || alerts[0].Title != "SMS code needed" || alerts[0].Detail != "Tipsport 1 (Test)" || alerts[0].Kind != alertAttention ||
		alerts[0].WindowID != "tipsport-1" || alerts[0].AgentID != r.agent || alerts[0].OrderID != "" || !strings.Contains(strings.Join(types, ","), "bot") {
		t.Fatalf("open: %v %+v", types, alerts)
	}
	if p := r.prompts(); len(p) != 1 || p[0] != (codePromptView{AgentID: r.agent, WindowID: "tipsport-1", Bookmaker: "tipsport", Digits: 6,
		ExpiresAt: exp, State: "waiting", TriesLeft: 3}) {
		t.Fatalf("list: %+v", p)
	}
	// A newer prompt replaces it; the alert stays the one open alert.
	second := r.mustPrompt("tipsport-1", 4)
	if second == first {
		t.Fatal("same prompt id")
	}
	if _, alerts := drain(); len(alerts) != 0 {
		t.Fatalf("replace alerted: %+v", alerts)
	}
	if s, b := r.fetch("tipsport-1", first); s != 404 || !strings.Contains(b, `"no_prompt"`) {
		t.Fatalf("replaced prompt: %d %s", s, b)
	}
	if p := r.prompts(); len(p) != 1 || p[0].Digits != 4 {
		t.Fatalf("after replace: %+v", p)
	}
}

func TestLoginCodes_SubmitFetchWrong(t *testing.T) {
	r, agent, drain := newCodeRig(t)
	other := newUser(t, r.svc, "o@example.com", accounts.RoleUser)
	otherCode, _, _ := r.svc.AuthenticateCode(newCode(t, r.srv, other, "bot"))
	if s, b := r.appReq("POST", "/api/me/bot/agents/"+i64s(otherCode.CodeID)+"/windows/tipsport-1/login-code", `{"code":"123456"}`); s != 404 ||
		!strings.Contains(b, `"unknown_agent"`) {
		t.Fatalf("another user's agent: %d %s", s, b)
	}
	for name, body := range map[string]string{"letters": `{"code":"12a456"}`, "3 digits": `{"code":"123"}`, "9 digits": `{"code":"123456789"}`,
		"number": `{"code":123456}`, "missing": `{}`, "big": `{"code":"123456","x":"` + strings.Repeat("x", 300) + `"}`} {
		if s, b := r.appReq("POST", "/api/me/bot/agents/"+i64s(r.agent)+"/windows/tipsport-1/login-code", body); s != 400 || !strings.Contains(b, `"bad_code"`) {
			t.Fatalf("%s: %d %s", name, s, b)
		}
	}
	if s, b := r.submit("tipsport-1", "123456"); s != 404 || !strings.Contains(b, `"no_prompt"`) {
		t.Fatalf("no prompt: %d %s", s, b)
	}
	id := r.mustPrompt("tipsport-1", 6)
	drain()
	if s, b := r.submit("tipsport-1", "1234"); s != 400 || !strings.Contains(b, `"bad_code"`) {
		t.Fatalf("4 digits for a 6-digit prompt: %d %s", s, b)
	}
	if s, b := r.fetch("tipsport-1", id); s != 204 {
		t.Fatalf("fetch before a code: %d %s", s, b)
	}
	if s, b := r.submit("tipsport-1", "482913"); s != 200 || strings.TrimSpace(b) != `{"state":"sent","triesLeft":2}` {
		t.Fatalf("submit: %d %s", s, b)
	}
	if m := readJSON(t, agent); m["type"] != "loginCode" || m["windowId"] != "tipsport-1" || len(m) != 2 {
		t.Fatalf("agent got %v", m)
	}
	if types, _ := drain(); len(types) != 1 || types[0] != "bot" {
		t.Fatalf("app on submit: %v", types)
	}
	if p := r.prompts(); p[0].State != "sent" || p[0].TriesLeft != 2 {
		t.Fatalf("sent: %+v", p)
	}
	if s, b := r.submit("tipsport-1", "482914"); s != 409 || !strings.Contains(b, `"code_pending"`) {
		t.Fatalf("second code while one is pending: %d %s", s, b)
	}
	if s, b := r.fetch("tipsport-1", "0123456789abcdef"); s != 404 {
		t.Fatalf("wrong prompt id: %d %s", s, b)
	}
	s, b, hdr := r.do("POST", "/api/me/bot/logins/tipsport-1/code", "", r.code, `{"promptId":"`+id+`"}`, httpsHeader)
	if s != 200 || strings.TrimSpace(b) != `{"code":"482913"}` || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("fetch: %d %s %v", s, b, hdr)
	}
	r.bots.mu.Lock()
	kept := r.bots.codes[codeKey{r.agent, "tipsport-1"}].code
	r.bots.mu.Unlock()
	if kept != "" {
		t.Fatal("the code stayed in memory after the fetch")
	}
	if s, _ := r.fetch("tipsport-1", id); s != 204 {
		t.Fatalf("second fetch: %d", s)
	}
	if p := r.prompts(); p[0].State != "checking" {
		t.Fatalf("checking: %+v", p)
	}
	if s, b := r.submit("tipsport-1", "482914"); s != 409 || !strings.Contains(b, `"code_pending"`) {
		t.Fatalf("code while checking: %d %s", s, b)
	}
	// Wrong: back to waiting, two more tries, then too many.
	for i, left := range []int{2, 1, 0} {
		if s, b := r.wrong("tipsport-1", id); s != 200 || strings.TrimSpace(b) != `{"triesLeft":`+i64s(int64(left))+`}` {
			t.Fatalf("wrong %d: %d %s", i, s, b)
		}
		if p := r.prompts(); p[0].State != "waiting" || !p[0].Wrong || p[0].TriesLeft != left {
			t.Fatalf("after wrong %d: %+v", i, p)
		}
		if left == 0 {
			break
		}
		if s, b := r.submit("tipsport-1", "11111"+i64s(int64(i))); s != 200 {
			t.Fatalf("submit %d: %d %s", i, s, b)
		}
		readJSON(t, agent)
		if p := r.prompts(); p[0].Wrong {
			t.Fatalf("wrong after a new code: %+v", p)
		}
		r.fetch("tipsport-1", id)
	}
	if s, b := r.submit("tipsport-1", "999999"); s != 429 || !strings.Contains(b, `"too_many_codes"`) {
		t.Fatalf("fourth code: %d %s", s, b)
	}
	if s, b := r.wrong("tipsport-1", "x"); s != 400 {
		t.Fatalf("bad prompt id: %d %s", s, b)
	}
	// The agent offline, or on plain ws: agent_offline (after every other check).
	r.mustPrompt("tipsport-2", 6)
	agent.Close(websocket.StatusNormalClosure, "")
	waitBot(t, r.bots, r.uid, false)
	if s, b := r.submit("tipsport-2", "123456"); s != 409 || !strings.Contains(b, `"agent_offline"`) {
		t.Fatalf("offline: %d %s", s, b)
	}
	r.dialAgent(false, codeProto)
	if s, b := r.submit("tipsport-2", "123456"); s != 409 || !strings.Contains(b, `"agent_offline"`) {
		t.Fatalf("plain ws: %d %s", s, b)
	}
	if p := r.prompts(); p[1].TriesLeft != 3 || p[1].State != "waiting" {
		t.Fatalf("a refused code counted: %+v", p)
	}
}

func TestLoginCodes_LoginCodeOnlyToProtocol6(t *testing.T) {
	r, agent, _ := newCodeRig(t)
	r.mustPrompt("tipsport-1", 6)
	st := strings.Replace(p2Status("", win("tipsport", 1, "logging_in"), win("tipsport", 2, "idle")), `"proto":2`, `"proto":5`, 1)
	if s, b := postStatus(t, r.srv, r.code, st); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	if s, b := r.submit("tipsport-1", "123456"); s != 200 {
		t.Fatalf("submit: %d %s", s, b)
	}
	expectNothing(t, agent, 300*time.Millisecond, "a protocol-5 agent")
}

func TestLoginCodes_PromptEnds(t *testing.T) {
	r, agent, drain := newCodeRig(t)
	ended := func(what, window, id string) {
		t.Helper()
		if s, b := r.fetch(window, id); s != 404 || !strings.Contains(b, `"no_prompt"`) {
			t.Fatalf("%s: fetch %d %s", what, s, b)
		}
		for _, p := range r.prompts() {
			if p.WindowID == window {
				t.Fatalf("%s: still listed %+v", what, p)
			}
		}
	}
	// A result: the code alert resolves first, then the result's own alert is raised.
	id := r.mustPrompt("tipsport-1", 6)
	r.submit("tipsport-1", "123456")
	readJSON(t, agent)
	drain()
	if s, _ := r.agentReq("POST", "/api/me/bot/logins/tipsport-1/result", `{"result":"code"}`); s != 204 {
		t.Fatal("result")
	}
	ended("result", "tipsport-1", id)
	_, alerts := drain()
	if len(alerts) != 2 || alerts[0].Title != "SMS code needed" || alerts[0].ResolvedAt == 0 || alerts[1].Title != "Log in needed" || alerts[1].ResolvedAt != 0 {
		t.Fatalf("alerts: %+v", alerts)
	}
	// An ignored (stale) result ends it too.
	r.put("tipsport-1", "jan", "p1") // resolves the Log in needed alert
	readType(t, agent)
	id = r.mustPrompt("tipsport-1", 6)
	if s, _ := r.agentReq("POST", "/api/me/bot/logins/tipsport-1/result", `{"result":"ok","updatedAt":1}`); s != 204 {
		t.Fatal("stale result")
	}
	ended("stale result", "tipsport-1", id)
	// Expiry + 30 s (the tick), not before; a code submitted then is refused.
	exp := r.clk.now().Add(time.Minute)
	s, b := r.openPrompt("tipsport-1", 6, exp.UnixMilli())
	var out struct{ PromptID string }
	json.Unmarshal([]byte(b), &out)
	if s != 200 {
		t.Fatalf("open: %d %s", s, b)
	}
	r.clk.advance(time.Minute + codePromptGrace - time.Millisecond)
	r.bots.tick()
	if p := r.prompts(); len(p) != 1 {
		t.Fatalf("ended early: %+v", p)
	}
	r.clk.advance(time.Millisecond)
	r.bots.tick()
	ended("expiry", "tipsport-1", out.PromptID)
	if s, _ := r.submit("tipsport-1", "123456"); s != 404 {
		t.Fatalf("submit after expiry: %d", s)
	}
	drain()
	// The login deleted.
	id = r.mustPrompt("tipsport-1", 6)
	r.appReq("DELETE", r.loginPath("tipsport-1"), "")
	readType(t, agent)
	ended("delete", "tipsport-1", id)
	// The window removed (the agent's ok to a remove command).
	id = r.mustPrompt("tipsport-2", 6)
	done := commandAsync(t, r.srv, r.token, "/api/me/bot/agents/"+i64s(r.agent)+"/windows/tipsport-2/remove", ``)
	cmd := readCommand(t, agent)
	postAck(t, r.srv, r.code, `{"id":"`+cmd.ID+`","ok":true,"windowId":"tipsport-2"}`)
	<-done
	ended("remove", "tipsport-2", id)
	_, alerts = drain()
	last := map[string]botAlert{}
	for _, a := range alerts {
		last[a.ID] = a
	}
	for _, a := range last {
		if a.Title == "SMS code needed" && a.ResolvedAt == 0 {
			t.Fatalf("a code alert left open: %+v", alerts)
		}
	}
}

func TestLoginCodes_RevokeAndDisableEndPrompts(t *testing.T) {
	r, _, drain := newCodeRig(t)
	r.mustPrompt("tipsport-1", 6)
	drain()
	if err := r.svc.RevokeExtensionCode(r.uid, r.agent); err != nil {
		t.Fatal(err)
	}
	r.bots.mu.Lock()
	n := len(r.bots.codes)
	r.bots.mu.Unlock()
	types, alerts := drain()
	if n != 0 || len(alerts) != 1 || alerts[0].Title != titleCodeNeeded || alerts[0].ResolvedAt == 0 || !strings.Contains(strings.Join(types, ","), "bot") {
		t.Fatalf("after revoke: %d prompts, %v %+v", n, types, alerts)
	}
	// A disabled user's prompts end too.
	other, _ := r.svc.CreateCode(r.uid, "Other", accounts.KindBot)
	r.bots.mu.Lock()
	r.bots.codes[codeKey{other.ID, "betano-1"}] = &codePrompt{id: "cccccccccccccccc", userID: r.uid, codeID: other.ID, windowID: "betano-1",
		digits: 4, expiresAt: r.clk.now().Add(time.Minute), state: codeStateWaiting}
	r.bots.mu.Unlock()
	inv, _ := r.svc.CreateInvite(0, accounts.RoleAdmin)
	admin, _, err := r.svc.Register("10.0.0.1", inv.Code, "admin@example.com", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	r.bots.reg.mu.Lock()
	for c := range r.bots.reg.clients {
		if c.conn == nil { // the app channel: CloseUser would close it as a socket
			delete(r.bots.reg.clients, c)
		}
	}
	r.bots.reg.mu.Unlock()
	if err := r.svc.SetDisabled(admin.UserID, r.uid, true); err != nil {
		t.Fatal(err)
	}
	r.bots.mu.Lock()
	n = len(r.bots.codes)
	r.bots.mu.Unlock()
	if n != 0 {
		t.Fatalf("prompts after disable: %d", n)
	}
}

// After a gateway restart the prompts are gone and the code alert read back from the DB stays the window's one alert
// when the agent opens its prompt again; the prompt's end resolves it.
func TestLoginCodes_AfterARestart(t *testing.T) {
	r := newAlertRig(t)
	if err := r.svc.InsertBotAlert(accounts.BotAlertRow{ID: "0123456789abcdef", UserID: r.uid, Kind: alertAttention, Severity: severityAttention,
		AgentCodeID: r.code.ID, WindowID: "fortuna-1", Title: titleCodeNeeded, Detail: "Fortuna 1 (Home PC)", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	r.b.loadLoginAlerts()
	r.attach()
	pr := &codePrompt{id: "aaaaaaaaaaaaaaaa", userID: r.uid, codeID: r.code.ID, windowID: "fortuna-1", digits: 6,
		expiresAt: r.clk.now().Add(time.Minute), state: codeStateWaiting}
	r.b.mu.Lock()
	fx := &effects{}
	r.b.codes[codeKey{r.code.ID, "fortuna-1"}] = pr
	r.b.alertCodeLocked(pr, fx)
	r.b.release(fx)
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("a second code alert: %+v", got)
	}
	r.b.mu.Lock()
	fx = &effects{}
	r.b.endPromptLocked(codeKey{r.code.ID, "fortuna-1"}, fx)
	r.b.release(fx)
	if got := r.alerts(); len(got) != 1 || got[0].ID != "0123456789abcdef" || got[0].ResolvedAt == 0 {
		t.Fatalf("resolution: %+v", got)
	}
	// An earlier login alert of another kind is superseded by the code alert.
	r.loginResult("fortuna-2", "captcha")
	r.alerts()
	pr2 := &codePrompt{id: "bbbbbbbbbbbbbbbb", userID: r.uid, codeID: r.code.ID, windowID: "fortuna-2", digits: 6,
		expiresAt: r.clk.now().Add(time.Minute), state: codeStateWaiting}
	r.b.mu.Lock()
	fx = &effects{}
	r.b.alertCodeLocked(pr2, fx)
	r.b.release(fx)
	got := r.alerts()
	if len(got) != 2 || got[0].Title != "Log in needed" || got[0].ResolvedAt == 0 || got[1].Title != titleCodeNeeded {
		t.Fatalf("superseded: %+v", got)
	}
	// Telegram: the line and the link to the window's Fleet row.
	want := "⚠️ SMS code needed · Fortuna 2 (Home PC) · " + time.UnixMilli(got[1].CreatedAt).In(prague).Format("15:04") +
		"\nhttps://sb.example/app/bots/fleet?agent=" + i64s(r.code.ID) + "&window=fortuna-2"
	if txt := alertText(got[1], "https://sb.example", prague); txt != want {
		t.Fatalf("telegram text:\n%s\nwant\n%s", txt, want)
	}
	if p := alertPath(botAlert{Title: "Log in needed", AgentID: 3, WindowID: "fortuna-2"}); p != "/app/bots/fleet" {
		t.Fatalf("other login alert link: %s", p)
	}
}

func TestLoginCodes_NeverLogged(t *testing.T) {
	r, agent, _ := newCodeRig(t)
	buf := captureLogs(t)
	const code = "482913"
	id := r.mustPrompt("tipsport-1", 6)
	r.submit("tipsport-1", "48291")                                                                                // refused: wrong length
	r.appReq("POST", "/api/me/bot/agents/"+i64s(r.agent)+"/windows/tipsport-1/login-code", `{"code":"`+code+`9"}`) // refused: 7 digits
	if s, _ := r.submit("tipsport-1", code); s != 200 {
		t.Fatal("submit")
	}
	readJSON(t, agent)
	if s, b := r.fetch("tipsport-1", id); s != 200 || !strings.Contains(b, code) {
		t.Fatalf("fetch: %d %s", s, b)
	}
	r.wrong("tipsport-1", id)
	r.agentReq("POST", "/api/me/bot/logins/tipsport-1/result", `{"result":"ok"}`)
	out := buf.String()
	if !strings.Contains(out, "bot login code submitted") || !strings.Contains(out, "bot login code fetched") {
		t.Fatalf("expected lines missing:\n%s", out)
	}
	if strings.Contains(out, code) {
		t.Fatalf("the code in the log:\n%s", out)
	}
}

// An agent with no bot socket, or whose socket has posted no status yet (right after a gateway restart), may open its
// prompt again; a status of protocol 1–5 may not.
func TestLoginCodes_OpenWithoutAStatus(t *testing.T) {
	r := newLoginRig(t)
	r.put("tipsport-1", "jan", "p1")
	if s, b := r.openPrompt("tipsport-1", 6, r.clk.now().Add(time.Minute).UnixMilli()); s != 200 {
		t.Fatalf("no socket: %d %s", s, b)
	}
	conn, _, err := dial(t, r.srv, "/ws?ticket="+wsTicket(t, r.srv, r.code)+"&feed=0&bot=1", http.Header{"X-Forwarded-Proto": {"https"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	waitBot(t, r.bots, r.uid, true)
	drain := r.appChan()
	if s, b := r.openPrompt("tipsport-1", 6, r.clk.now().Add(time.Minute).UnixMilli()); s != 200 {
		t.Fatalf("socket without a status: %d %s", s, b)
	}
	if _, alerts := drain(); len(alerts) != 0 {
		t.Fatalf("the reopened prompt alerted again: %+v", alerts)
	}
}

func TestLoginCodes_SubmitCheckOrder(t *testing.T) {
	r, agent, _ := newCodeRig(t)
	id := r.mustPrompt("tipsport-1", 6)
	r.submit("tipsport-1", "123456")
	readJSON(t, agent)
	agent.Close(websocket.StatusNormalClosure, "")
	waitBot(t, r.bots, r.uid, false)
	// Offline, yet the earlier checks answer first: digits before code_pending before agent_offline.
	if s, b := r.submit("tipsport-1", "1234"); s != 400 || !strings.Contains(b, `"bad_code"`) {
		t.Fatalf("digits first: %d %s", s, b)
	}
	if s, b := r.submit("tipsport-1", "123456"); s != 409 || !strings.Contains(b, `"code_pending"`) {
		t.Fatalf("code_pending before agent_offline: %d %s", s, b)
	}
	// A late wrong report while a code waits (not checked) changes nothing.
	if s, b := r.wrong("tipsport-1", id); s != 200 || strings.TrimSpace(b) != `{"triesLeft":2}` {
		t.Fatalf("wrong while sent: %d %s", s, b)
	}
	if s, b := r.fetch("tipsport-1", id); s != 200 || !strings.Contains(b, "123456") {
		t.Fatalf("the waiting code was dropped: %d %s", s, b)
	}
	r.bots.mu.Lock()
	r.bots.codes[codeKey{r.agent, "tipsport-1"}].tries, r.bots.codes[codeKey{r.agent, "tipsport-1"}].state = 3, codeStateWaiting
	r.bots.mu.Unlock()
	if s, b := r.submit("tipsport-1", "123456"); s != 429 || !strings.Contains(b, `"too_many_codes"`) {
		t.Fatalf("too_many_codes before agent_offline: %d %s", s, b)
	}
}

func TestLoginCodes_SubmitAfterExpiresAt(t *testing.T) {
	r, _, _ := newCodeRig(t)
	s, b := r.openPrompt("tipsport-1", 6, r.clk.now().Add(time.Minute).UnixMilli())
	if s != 200 {
		t.Fatalf("open: %d %s", s, b)
	}
	r.clk.advance(time.Minute)
	if s, b := r.submit("tipsport-1", "123456"); s != 404 || !strings.Contains(b, `"no_prompt"`) {
		t.Fatalf("after expiresAt: %d %s", s, b)
	}
	if p := r.prompts(); len(p) != 1 {
		t.Fatalf("listed until expiresAt + 30 s: %+v", p)
	}
}

func TestLoginCodes_EveryChangeTellsTheApp(t *testing.T) {
	r, agent, drain := newCodeRig(t)
	bots := func(what string) {
		t.Helper()
		types, _ := drain()
		if !strings.Contains(strings.Join(types, ","), "bot") {
			t.Fatalf("%s: %v", what, types)
		}
	}
	id := r.mustPrompt("tipsport-1", 6)
	bots("open")
	r.submit("tipsport-1", "123456")
	readJSON(t, agent)
	bots("sent")
	r.fetch("tipsport-1", id)
	bots("checking")
	r.wrong("tipsport-1", id)
	bots("wrong")
	r.agentReq("POST", "/api/me/bot/logins/tipsport-1/result", `{"result":"ok"}`)
	bots("end")
}

// After a gateway restart: the code alert read back, no prompt in memory, the agent posts its result: the code alert
// resolves and the result's own alert is raised. A read-back code alert nobody reopens resolves on its own after 6 min.
func TestLoginCodes_ResultAfterARestart(t *testing.T) {
	r, _, drain := newCodeRig(t)
	insert := func(id, window string, at int64) {
		if err := r.svc.InsertBotAlert(accounts.BotAlertRow{ID: id, UserID: r.uid, Kind: alertAttention, Severity: severityAttention,
			AgentCodeID: r.agent, WindowID: window, Title: titleCodeNeeded, Detail: "x", CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	insert("aaaaaaaaaaaaaaaa", "tipsport-1", r.clk.now().UnixMilli())
	insert("bbbbbbbbbbbbbbbb", "tipsport-2", r.clk.now().UnixMilli())
	r.bots.loadLoginAlerts()
	if s, _ := r.agentReq("POST", "/api/me/bot/logins/tipsport-1/result", `{"result":"code"}`); s != 204 {
		t.Fatal("result")
	}
	_, alerts := drain()
	if len(alerts) != 2 || alerts[0].ID != "aaaaaaaaaaaaaaaa" || alerts[0].ResolvedAt == 0 || alerts[1].Title != "Log in needed" ||
		alerts[1].Detail != "Tipsport 1 (Test) · code asked" {
		t.Fatalf("alerts: %+v", alerts)
	}
	r.clk.advance(codePromptAhead + codePromptGrace - time.Millisecond)
	r.bots.tick()
	if _, alerts := drain(); len(alerts) != 0 {
		t.Fatalf("resolved early: %+v", alerts)
	}
	r.clk.advance(time.Millisecond)
	r.bots.tick()
	if _, alerts := drain(); len(alerts) != 1 || alerts[0].ID != "bbbbbbbbbbbbbbbb" || alerts[0].ResolvedAt == 0 {
		t.Fatalf("stale code alert: %+v", alerts)
	}
}
