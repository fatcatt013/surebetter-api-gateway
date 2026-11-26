package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func TestCodeCipher_RoundTripBindingAndSeparation(t *testing.T) {
	c := mustCipher(t, testLoginKey(0), "")
	const code = "SBX-AAAA-BBBB-CCCC-DDDD-EEEE"
	sealed, err := c.SealCode(1, 2, code)
	if err != nil || strings.Contains(string(sealed), code) {
		t.Fatalf("seal: %v", err)
	}
	if got, err := c.OpenCode(1, 2, sealed); err != nil || got != code {
		t.Fatalf("open: %q %v", got, err)
	}
	if again, _ := c.SealCode(1, 2, code); string(again) == string(sealed) {
		t.Fatal("two seals are equal: the nonce is not random")
	}
	for _, ids := range [][2]int64{{1, 3}, {2, 2}} {
		if _, err := c.OpenCode(ids[0], ids[1], sealed); err == nil {
			t.Fatalf("opened as user %d code %d", ids[0], ids[1])
		}
	}
	if _, err := c.OpenCode(1, 2, sealed[:5]); err == nil {
		t.Fatal("a short value opened")
	}
	if _, err := mustCipher(t, testLoginKey(1), "").OpenCode(1, 2, sealed); err == nil {
		t.Fatal("opened with another key")
	}
	// Key separation: a bot login's secret never opens as a code, nor a code as a login, whatever the ids.
	secret, keyID, _, _ := c.seal(1, 2, "", "jan", code)
	if _, err := c.OpenCode(1, 2, secret); err == nil {
		t.Fatal("a login secret opened as a code")
	}
	if _, _, err := c.open(sealed, keyID, 1, 2, ""); err == nil {
		t.Fatal("a code opened as a login")
	}
	// The previous key still opens it after a rotation.
	rotated := mustCipher(t, testLoginKey(1), testLoginKey(0))
	if got, err := rotated.OpenCode(1, 2, sealed); err != nil || got != code {
		t.Fatalf("previous key: %q %v", got, err)
	}
}

func TestSetupBotLogins_ResealsCodes(t *testing.T) {
	_, svc, _, _ := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	old := mustCipher(t, testLoginKey(1), "")
	svc.SetCodeCipher(old, false)
	a, _ := svc.CreateCode(uid, "A", accounts.KindBot)
	b, _ := svc.CreateCode(uid, "B", accounts.KindExtension)
	svc.SetCodeCipher(mustCipher(t, testLoginKey(2), ""), false)
	lost, _ := svc.CreateCode(uid, "Lost", accounts.KindExtension)
	buf := captureLogs(t)
	c := setupBotLogins(Config{BotLoginsKey: testLoginKey(0), BotLoginsKeyPrevious: testLoginKey(1)}, svc)
	if c == nil || c.prev == nil {
		t.Fatal("setup")
	}
	if !strings.Contains(buf.String(), `"count":2,"message":"extension codes re-encrypted"`) {
		t.Fatalf("log:\n%s", buf)
	}
	svc.SetCodeCipher(mustCipher(t, testLoginKey(0), ""), false) // the previous key gone
	for _, want := range []accounts.NewExtCode{a, b} {
		if got, err := svc.RevealExtensionCode(uid, want.ID); err != nil || got != want.Code {
			t.Fatalf("code %d after re-encryption: %v", want.ID, err)
		}
	}
	if _, err := svc.RevealExtensionCode(uid, lost.ID); err != accounts.ErrNotViewable {
		t.Fatalf("a code of an unknown key: %v", err)
	}
	setupBotLogins(Config{BotLoginsKey: testLoginKey(0), BotLoginsKeyPrevious: testLoginKey(1)}, svc)
	if !strings.Contains(buf.String(), `"count":0,"message":"extension codes re-encrypted"`) {
		t.Fatalf("second run:\n%s", buf)
	}
	for _, s := range []string{a.Code, b.Code, lost.Code} {
		if strings.Contains(buf.String(), s) {
			t.Fatalf("a code in the log:\n%s", buf)
		}
	}
}

// A bot code deleted for good through the gateway: the agent's socket closes, the agent leaves the Fleet, its logins
// go, its orders stay; and no code ever reaches the log (shown, refused, revoked or deleted).
func TestExtensionCodes_DeleteAgentAndNeverLogged(t *testing.T) {
	r := newLoginRig(t)
	buf := captureLogs(t)
	r.svc.SetCodeCipher(r.bots.logins, false)
	agent := r.dialAgent(true, 5)
	r.put("tipsport-1", "jan", "p1")
	readType(t, agent) // logins
	expectBot(t, r.app, "after PUT")
	if err := r.svc.InsertBotOrder(accounts.BotOrderRow{ID: "o1", UserID: r.uid, ArbKey: "k", CodeID: r.agent, State: "placed",
		InfoJSON: "{}", LegsJSON: "[]", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	s, body := r.appReq("POST", "/api/me/extension-codes", `{"name":"Laptop"}`)
	var created struct {
		ID   int64
		Code string
	}
	if json.Unmarshal([]byte(body), &created); s != 201 || created.Code == "" {
		t.Fatalf("create: %d %s", s, body)
	}
	path := "/api/me/extension-codes/" + i64s(created.ID)
	if s, b := r.appReq("GET", "/api/me/extension-codes", ""); s != 200 || !strings.Contains(b, `"viewable":true`) || strings.Contains(b, created.Code) {
		t.Fatalf("list: %d %s", s, b)
	}
	if s, b := r.appReq("GET", path+"/code", ""); s != 200 || !strings.Contains(b, created.Code) {
		t.Fatalf("reveal: %d %s", s, b)
	}
	if s, b, _ := r.do("GET", path+"/code", r.token, "", "", nil); s != 403 || !strings.Contains(b, "https_required") {
		t.Fatalf("plain http: %d %s", s, b)
	}
	if s, b := r.agentReq("GET", path+"/code", ""); s != 403 || !strings.Contains(b, "session_required") {
		t.Fatalf("bot code: %d %s", s, b)
	}
	if s, _ := r.appReq("DELETE", path, ""); s != 204 {
		t.Fatalf("revoke: %d", s)
	}
	if s, b := r.appReq("GET", path+"/code", ""); s != 409 || !strings.Contains(b, "revoked") {
		t.Fatalf("revoked: %d %s", s, b)
	}
	if s, _ := r.appReq("DELETE", path+"?permanent=1", ""); s != 204 {
		t.Fatalf("delete: %d", s)
	}

	// The agent's bot code, active, deleted for good.
	agentKey := `"id":` + i64s(r.agent) + `,"name"`
	if s, b := r.appReq("GET", "/api/me/bot/agents", ""); s != 200 || !strings.Contains(b, agentKey) {
		t.Fatalf("agents before delete: %d %s", s, b)
	}
	if s, _ := r.appReq("DELETE", "/api/me/extension-codes/"+i64s(r.agent)+"?permanent=1", ""); s != 204 {
		t.Fatalf("delete the agent's code: %d", s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := agent.Read(ctx); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("agent socket: %v", err)
			}
			break
		}
	}
	if s, b := r.appReq("GET", "/api/me/bot/agents", ""); s != 200 || strings.Contains(b, agentKey) {
		t.Fatalf("agents after delete: %d %s", s, b)
	}
	if rows, _ := r.svc.ListBotLoginsOfCode(r.agent); len(rows) != 0 {
		t.Fatalf("logins after delete: %+v", rows)
	}
	if o, ok, _ := r.svc.BotOrderByID(r.uid, "o1"); !ok || o.CodeID != r.agent {
		t.Fatalf("order after delete: %+v %v", o, ok)
	}
	if s, _ := r.agentReq("GET", "/api/me/bot/logins/mine", ""); s != 401 {
		t.Fatalf("the deleted code still authenticates: %d", s)
	}
	for _, s := range []string{created.Code, r.code} {
		if strings.Contains(buf.String(), s) {
			t.Fatalf("a code in the log:\n%s", buf)
		}
	}
}
