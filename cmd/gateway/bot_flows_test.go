package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func putFlows(t *testing.T, srv *httptest.Server, code, body string) (int, string) {
	t.Helper()
	return request(t, srv, "PUT", "/api/me/bot/flows", "", body, bearer(code))
}

// flowsBody is a FlowsView with one step named step.
func flowsBody(step string) string {
	return `{"steps":[{"id":"` + step + `"}],"cases":[],"playbooks":[],"rules":[],"ranAt":null,"codeChanged":false}`
}

type flowsAnswer struct {
	AgentID   int64           `json:"agentId"`
	Agent     string          `json:"agent"`
	UpdatedAt int64           `json:"updatedAt"`
	Flows     json.RawMessage `json:"flows"`
}

func getFlows(t *testing.T, srv *httptest.Server, token, query string) (int, flowsAnswer, string) {
	t.Helper()
	s, body := request(t, srv, "GET", "/api/me/bot/flows"+query, token, "", nil)
	var a flowsAnswer
	json.Unmarshal([]byte(body), &a)
	return s, a, body
}

func TestBotFlows_UploadValidation(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	bad := map[string]string{
		"not JSON":        `{"steps":`,
		"an array":        `[]`,
		"no rules":        `{"steps":[],"cases":[],"playbooks":[]}`,
		"steps null":      `{"steps":null,"cases":[],"playbooks":[],"rules":[]}`,
		"cases an object": `{"steps":[],"cases":{},"playbooks":[],"rules":[]}`,
		"trailing data":   flowsBody("a") + `{}`,
	}
	for name, body := range bad {
		if s, b := putFlows(t, srv, code, body); s != 400 || !strings.Contains(b, `"bad_flows"`) {
			t.Errorf("%s: %d %s, want 400 bad_flows", name, s, b)
		}
	}
	big := `{"steps":[],"cases":[],"playbooks":[],"rules":[],"pad":"` + strings.Repeat("x", maxFlowsBody) + `"}`
	if s, b := putFlows(t, srv, code, big); s != 413 || !strings.Contains(b, `"too_large"`) {
		t.Fatalf("over 256 KiB: %d %s", s, b)
	}
	if s, b := request(t, srv, "PUT", "/api/me/bot/flows", token, flowsBody("a"), nil); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("upload with a session: %d %s", s, b)
	}
	// No bot socket is needed.
	if s, b := putFlows(t, srv, code, flowsBody("a")); s != 204 {
		t.Fatalf("valid upload: %d %s", s, b)
	}
}

func TestBotFlows_UploadNotifiesOnChangeAndIsServedAsIs(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, codeID := newBotCode(t, srv, token, "Home PC")

	body := flowsBody("navigate")
	if s, b := putFlows(t, srv, code, body); s != 204 {
		t.Fatalf("upload: %d %s", s, b)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on new flows: %s", got)
	}
	s, a, raw := getFlows(t, srv, token, "")
	if s != 200 || a.AgentID != codeID || a.Agent != "Home PC" || a.UpdatedAt <= 0 || string(a.Flows) != body {
		t.Fatalf("GET: %d %s", s, raw)
	}
	if s, b := putFlows(t, srv, code, body); s != 204 {
		t.Fatalf("same upload: %d %s", s, b)
	}
	expectNoMessage(t, app, "app after an unchanged upload")
}

func TestBotFlows_ChoosingTheAgent(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	if s, b := request(t, srv, "GET", "/api/me/bot/flows", token, "", nil); s != 404 || !strings.Contains(b, `"no_flows"`) {
		t.Fatalf("no flows yet: %d %s", s, b)
	}
	zeta, zetaID := newBotCode(t, srv, token, "Zeta")
	alpha, alphaID := newBotCode(t, srv, token, "Alpha")
	beta, betaID := newBotCode(t, srv, token, "Beta")
	_, emptyID := newBotCode(t, srv, token, "Empty")
	otherCode, otherID := newBotCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "Other")
	if s, _ := putFlows(t, srv, otherCode, flowsBody("other")); s != 204 {
		t.Fatal("other user's upload")
	}
	for _, c := range []string{alpha, beta, zeta} {
		if s, b := putFlows(t, srv, c, flowsBody(c)); s != 204 {
			t.Fatalf("upload: %d %s", s, b)
		}
		time.Sleep(5 * time.Millisecond) // distinct flows_at
	}
	// None online: the most recently updated (zeta).
	if s, a, raw := getFlows(t, srv, token, ""); s != 200 || a.AgentID != zetaID {
		t.Fatalf("none online: %d %s", s, raw)
	}
	// Online agents with flows first, by name: beta and zeta online → Beta.
	dialBot(t, srv, zeta)
	dialBot(t, srv, beta)
	waitAgents(t, bots, uid, 2)
	if s, a, raw := getFlows(t, srv, token, ""); s != 200 || a.AgentID != betaID || a.Agent != "Beta" {
		t.Fatalf("online first, by name: %d %s", s, raw)
	}
	if s, a, raw := getFlows(t, srv, token, fmt.Sprintf("?agent=%d", alphaID)); s != 200 || a.AgentID != alphaID ||
		string(a.Flows) != flowsBody(alpha) {
		t.Fatalf("?agent=alpha: %d %s", s, raw)
	}
	if s, b := request(t, srv, "GET", fmt.Sprintf("/api/me/bot/flows?agent=%d", emptyID), token, "", nil); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
		t.Fatalf("a code that never connected: %d %s", s, b)
	}
	if s, b := request(t, srv, "GET", fmt.Sprintf("/api/me/bot/flows?agent=%d", otherID), token, "", nil); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
		t.Fatalf("another user's agent: %d %s", s, b)
	}
	for _, q := range []string{"?agent=x", "?agent=0", "?agent=-1"} {
		if s, b := request(t, srv, "GET", "/api/me/bot/flows"+q, token, "", nil); s != 400 || !strings.Contains(b, `"bad_query"`) {
			t.Errorf("%s: %d %s", q, s, b)
		}
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/flows", "", "", bearer(alpha)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("GET with a bot code: %d %s", s, b)
	}
}

func TestBotFlows_AgentWithoutFlows(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code, codeID := newBotCode(t, srv, token, "Home PC")
	dialBot(t, srv, code)
	waitBot(t, bots, userIDOf(t, svc, token), true)
	if s, b := request(t, srv, "GET", fmt.Sprintf("/api/me/bot/flows?agent=%d", codeID), token, "", nil); s != 404 || !strings.Contains(b, `"no_flows"`) {
		t.Fatalf("agent without flows: %d %s", s, b)
	}
}
