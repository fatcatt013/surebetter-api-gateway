package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// readCommand reads the next message on the agent's socket as a window command.
func readCommand(t *testing.T, bot *websocket.Conn) botCommand {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := bot.Read(ctx)
	if err != nil {
		t.Fatalf("agent read: %v", err)
	}
	var m struct {
		Type    string     `json:"type"`
		Command botCommand `json:"command"`
	}
	if json.Unmarshal(b, &m); m.Type != "command" || !bootPattern.MatchString(m.Command.ID) {
		t.Fatalf("agent got %s, want a command", b)
	}
	return m.Command
}

type httpAnswer struct {
	status int
	body   string
}

// commandAsync sends an app command request in the background.
func commandAsync(t *testing.T, srv *httptest.Server, token, path, body string) chan httpAnswer {
	done := make(chan httpAnswer, 1)
	go func() {
		s, b := request(t, srv, "POST", path, token, body, nil)
		done <- httpAnswer{s, strings.TrimSpace(b)}
	}()
	return done
}

func postAck(t *testing.T, srv *httptest.Server, code, body string) (int, string) {
	t.Helper()
	return request(t, srv, "POST", "/api/me/bot/commands", "", body, bearer(code))
}

func TestBotCommands_RoundTrip(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	bot := startBot(t, srv, code, app)

	// Add a window: the command has no window id and names the bookmaker.
	done := commandAsync(t, srv, token, fmt.Sprintf("/api/me/bot/agents/%d/windows", id), `{"bookmaker":"tipsport"}`)
	cmd := readCommand(t, bot)
	if cmd.Action != "add" || cmd.WindowID != "" || cmd.Bookmaker != "tipsport" {
		t.Fatalf("add command: %+v", cmd)
	}
	if s, b := postAck(t, srv, code, `{"id":"`+cmd.ID+`","ok":true,"windowId":"tipsport-2"}`); s != 204 {
		t.Fatalf("ack: %d %s", s, b)
	}
	if a := <-done; a.status != 200 || a.body != `{"ok":true,"windowId":"tipsport-2"}` {
		t.Fatalf("app answer: %+v", a)
	}
	// The same id again is unknown.
	if s, b := postAck(t, srv, code, `{"id":"`+cmd.ID+`","ok":true,"windowId":"tipsport-2"}`); s != 404 || !strings.Contains(b, `"unknown_command"`) {
		t.Fatalf("second ack: %d %s", s, b)
	}

	// An action on a window; the agent refuses it.
	done = commandAsync(t, srv, token, fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-2/remove", id), ``)
	cmd = readCommand(t, bot)
	if cmd.Action != "remove" || cmd.WindowID != "tipsport-2" || cmd.Bookmaker != "" {
		t.Fatalf("remove command: %+v", cmd)
	}
	if s, b := postAck(t, srv, code, `{"id":"`+cmd.ID+`","ok":false,"error":"bad_state","message":"tipsport 2 is not stopped"}`); s != 204 {
		t.Fatalf("ack: %d %s", s, b)
	}
	if a := <-done; a.status != 409 || a.body != `{"error":"bad_state","message":"tipsport 2 is not stopped"}` {
		t.Fatalf("app answer: %+v", a)
	}
	// Every action reaches the agent.
	for _, action := range []string{"start", "stop", "restart", "login", "bot"} {
		done = commandAsync(t, srv, token, fmt.Sprintf("/api/me/bot/agents/%d/windows/betano-1/%s", id, action), ``)
		if cmd = readCommand(t, bot); cmd.Action != action || cmd.WindowID != "betano-1" {
			t.Fatalf("%s: %+v", action, cmd)
		}
		postAck(t, srv, code, `{"id":"`+cmd.ID+`","ok":true,"windowId":"betano-1"}`)
		if a := <-done; a.status != 200 {
			t.Fatalf("%s: %+v", action, a)
		}
	}
}

func TestBotCommands_GatewayRefusals(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	oldCode, oldID := newBotCode(t, srv, token, "Old PC")
	_, offlineID := newBotCode(t, srv, token, "Offline PC")
	_, otherID := newBotCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "Other")
	bot := startBot(t, srv, code, app)
	startAgentWith(t, srv, oldCode, `{"books":[{"bookmaker":"tipsport","ready":true}],"dryRun":true}`, app)
	call := func(path, body string) (int, string) {
		return request(t, srv, "POST", path, token, body, nil)
	}
	for name, c := range map[string][2]string{
		"unknown action":       {fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/explode", id), ""},
		"window id uppercase":  {fmt.Sprintf("/api/me/bot/agents/%d/windows/Tipsport-1/stop", id), ""},
		"agent id not numeric": {"/api/me/bot/agents/home/windows/tipsport-1/stop", ""},
		"add without body":     {fmt.Sprintf("/api/me/bot/agents/%d/windows", id), ""},
		"add bad bookmaker":    {fmt.Sprintf("/api/me/bot/agents/%d/windows", id), `{"bookmaker":"Tip sport"}`},
		"add as an action":     {fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/add", id), ""},
	} {
		if s, b := call(c[0], c[1]); s != 400 || !strings.Contains(b, `"bad_command"`) {
			t.Errorf("%s: %d %s, want 400 bad_command", name, s, b)
		}
	}
	if s, b := call(fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/stop", otherID), ""); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
		t.Fatalf("another user's agent: %d %s", s, b)
	}
	if s, b := call(fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/stop", offlineID), ""); s != 409 || !strings.Contains(b, `"agent_offline"`) {
		t.Fatalf("offline agent: %d %s", s, b)
	}
	if s, b := call(fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/stop", oldID), ""); s != 409 || !strings.Contains(b, `"agent_offline"`) {
		t.Fatalf("Phase 1 agent: %d %s", s, b)
	}
	// A window reserved for an order is refused without asking the agent; another is not.
	o := placeOrder(t, srv, token, "arb-1", bot, app)
	for _, action := range []string{"stop", "restart", "login", "remove"} {
		if s, b := call(fmt.Sprintf("/api/me/bot/agents/%d/windows/%s/%s", id, o.Legs[1].WindowID, action), ""); s != 409 || !strings.Contains(b, `"window_busy"`) {
			t.Fatalf("%s on a reserved window: %d %s", action, s, b)
		}
	}
	if s, b := request(t, srv, "POST", fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/stop", id), "", "", bearer(code)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("command with a bot code: %d %s", s, b)
	}
	if s, b := request(t, srv, "POST", fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/stop", id), token, "", map[string]string{"Origin": "http://evil.example"}); s != 403 || !strings.Contains(b, `"bad_origin"`) {
		t.Fatalf("cross-site command: %d %s", s, b)
	}
	// Revoked: unknown.
	if s, b := request(t, srv, "DELETE", fmt.Sprintf("/api/me/extension-codes/%d", oldID), token, "", nil); s != 204 {
		t.Fatalf("revoke: %d %s", s, b)
	}
	if s, b := call(fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/stop", oldID), ""); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
		t.Fatalf("revoked agent: %d %s", s, b)
	}
	expectNoMessage(t, bot, "the agent (no command was sent)")
}

func TestBotCommands_TimeoutAndAckValidation(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	defer func(d time.Duration) { commandWait = d }(commandWait)
	commandWait = 200 * time.Millisecond
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	other, _ := newBotCode(t, srv, token, "Other PC")
	bot := startBot(t, srv, code, app)

	done := commandAsync(t, srv, token, fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/restart", id), "")
	cmd := readCommand(t, bot)
	if a := <-done; a.status != 504 || !strings.Contains(a.body, `"agent_timeout"`) {
		t.Fatalf("no answer: %+v", a)
	}
	if s, b := postAck(t, srv, code, `{"id":"`+cmd.ID+`","ok":true,"windowId":"tipsport-1"}`); s != 404 || !strings.Contains(b, `"unknown_command"`) {
		t.Fatalf("late ack: %d %s", s, b)
	}

	commandWait = 5 * time.Second
	done = commandAsync(t, srv, token, fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/stop", id), "")
	cmd = readCommand(t, bot)
	for name, body := range map[string]string{
		"not JSON":         `{"id":`,
		"bad id":           `{"id":"xyz","ok":true}`,
		"no ok":            `{"id":"` + cmd.ID + `"}`,
		"error uppercase":  `{"id":"` + cmd.ID + `","ok":false,"error":"Failed"}`,
		"no error":         `{"id":"` + cmd.ID + `","ok":false}`,
		"message over 200": `{"id":"` + cmd.ID + `","ok":false,"error":"failed","message":"` + strings.Repeat("m", 201) + `"}`,
		"bad window id":    `{"id":"` + cmd.ID + `","ok":true,"windowId":"Tipsport 1"}`,
	} {
		if s, b := postAck(t, srv, code, body); s != 400 || !strings.Contains(b, `"bad_command"`) {
			t.Errorf("%s: %d %s, want 400 bad_command", name, s, b)
		}
	}
	if s, b := postAck(t, srv, other, `{"id":"`+cmd.ID+`","ok":true}`); s != 404 || !strings.Contains(b, `"unknown_command"`) {
		t.Fatalf("another code's command: %d %s", s, b)
	}
	if s, b := request(t, srv, "POST", "/api/me/bot/commands", token, `{"id":"`+cmd.ID+`","ok":true}`, nil); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("ack with a session: %d %s", s, b)
	}
	if s, b := postAck(t, srv, code, `{"id":"`+cmd.ID+`","ok":false,"error":"window_busy"}`); s != 204 {
		t.Fatalf("ack: %d %s", s, b)
	}
	if a := <-done; a.status != 409 || a.body != `{"error":"window_busy","message":""}` {
		t.Fatalf("app answer: %+v", a)
	}
}

// The scheduler does not pick a window while a command for it waits for the agent's answer.
func TestBotCommands_PendingCommandKeepsTheWindowFromOrders(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	bot := startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("tipsport", 2, "idle"), win("betano", 1, "idle")), app)

	// tipsport-1 (idle as long, lowest number) would be picked; its pending restart leaves tipsport-2.
	done := commandAsync(t, srv, token, fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/restart", id), ``)
	cmd := readCommand(t, bot)
	a := postOrderOK(t, srv, token, tipBetOrder("arb-1"))
	if o := readOrder(t, bot); a.State != "sent" || o.Legs[0].WindowID != "tipsport-2" {
		t.Fatalf("order %+v went to %+v, want tipsport-2", a, o.Legs)
	}
	expectEvent(t, app, a.OrderID, "sent", "")
	expectBot(t, app, "on the sent order")
	postAck(t, srv, code, `{"id":"`+cmd.ID+`","ok":true,"windowId":"tipsport-1"}`)
	if r := <-done; r.status != 200 {
		t.Fatalf("command: %+v", r)
	}

}

// With the only Tipsport window under a pending command, an order waits; once the command is
// answered, the next trigger sends it there.
func TestBotCommands_PendingCommandMakesAnOrderWait(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	bot := startBot(t, srv, code, app)
	done := commandAsync(t, srv, token, fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/login", id), ``)
	cmd := readCommand(t, bot)
	a := postOrderOK(t, srv, token, tipBetOrder("arb-1"))
	if a.State != "waiting" || a.Reason != "Needs a free Tipsport window" {
		t.Fatalf("order: %+v, want waiting for Tipsport", a)
	}
	expectEvent(t, app, a.OrderID, "waiting", "Needs a free Tipsport window")
	expectBot(t, app, "on the waiting order")
	postAck(t, srv, code, `{"id":"`+cmd.ID+`","ok":true,"windowId":"tipsport-1"}`)
	if r := <-done; r.status != 200 {
		t.Fatalf("command: %+v", r)
	}
	if s, b := postStatus(t, srv, code, tipBetIdle); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	if o := readOrder(t, bot); o.OrderID != a.OrderID || o.Legs[0].WindowID != "tipsport-1" {
		t.Fatalf("agent got %+v", o)
	}
}
