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

// tipBetIdleP3 is tipBetIdle from a Phase 3 agent (protocol 3: it takes a Try).
var tipBetIdleP3 = strings.Replace(tipBetIdle, `"proto":2`, `"proto":3`, 1)

// tryBody is a Try body as the app prefills it from a recent order: no arb key, no selection ids.
const tryBody = `{"legs":[{"bookmaker":"tipsport","odds":2.05,"stake":1000,"outcome_id":"OVER"},{"bookmaker":"betano","odds":2,"stake":1025,"outcome_id":"UNDER"}],` +
	`"market_type":"TOTAL_CORNERS","line":9.5,"is_live":true,"sport":"football","rounding":10}`

// tryResult is an agent's answer to a Try.
const tryResult = `{"rule":"totals-over-first","ruleOn":true,"playbook":"overFirst","wouldMatch":"totals-over-first",` +
	`"legs":[{"bookmaker":"tipsport","side":"OVER","role":"Over"},{"bookmaker":"betano","side":"UNDER","role":"Under"}],` +
	`"phases":["Both ready","Over confirmed"],"underStake":{"exact":1025,"stake":1030,"resultPct":0.24}}`

// readTry reads the next message on the agent's socket as a try command and returns its id and order.
func readTry(t *testing.T, bot *websocket.Conn) (string, map[string]any) {
	t.Helper()
	m := readJSON(t, bot)
	cmd, _ := m["command"].(map[string]any)
	order, _ := cmd["order"].(map[string]any)
	id, _ := cmd["id"].(string)
	if m["type"] != "command" || cmd["action"] != "try" || !bootPattern.MatchString(id) || order == nil {
		t.Fatalf("agent got %v, want a try command", m)
	}
	return id, order
}

func TestBotTry_RoundTrip(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) {
		st.MaxLeg, st.RulesOn, st.Paused, st.PausedReason = 3000, `["totals-over-first"]`, true, "Paused"
	})
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	bot := startAgentWith(t, srv, code, tipBetIdleP3, app)
	if m := readJSON(t, bot); m["type"] != "pause" { // paused: the agent's first protocol-2 status gets the pause
		t.Fatalf("agent got %v, want pause", m)
	}
	path := fmt.Sprintf("/api/me/bot/agents/%d/try", id)

	// A Try is not an order: it goes while paused, carries the settings and the order message, no windows.
	done := commandAsync(t, srv, token, path, tryBody)
	cmdID, o := readTry(t, bot)
	want := map[string]any{"order_id": "", "arb_key": "", "dry_run": true, "market_type": "TOTAL_CORNERS", "line": 9.5, "is_live": true,
		"sport": "football", "match": "", "rounding": float64(10), "max_leg": float64(3000)}
	for k, v := range want {
		if o[k] != v {
			t.Errorf("try order %s = %v, want %v", k, o[k], v)
		}
	}
	if rules, _ := json.Marshal(o["rules_on"]); string(rules) != `["totals-over-first"]` {
		t.Errorf("rules_on = %s", rules)
	}
	legs, _ := o["legs"].([]any)
	if len(legs) != 2 || legs[0].(map[string]any)["outcome_id"] != "OVER" || legs[1].(map[string]any)["window_id"] != "" ||
		legs[1].(map[string]any)["stake"] != float64(1025) {
		t.Fatalf("try legs: %v", legs)
	}
	// ok without a result is refused (the app keeps waiting); then the result is the app's answer.
	if s, b := postAck(t, srv, code, `{"id":"`+cmdID+`","ok":true}`); s != 400 || !strings.Contains(b, `"bad_command"`) {
		t.Fatalf("try ack without a result: %d %s", s, b)
	}
	if s, b := postAck(t, srv, code, `{"id":"`+cmdID+`","ok":true,"result":`+tryResult+`}`); s != 204 {
		t.Fatalf("ack: %d %s", s, b)
	}
	if a := <-done; a.status != 200 || a.body != tryResult {
		t.Fatalf("app answer: %+v", a)
	}
	if v := getView(t, srv, token); v.Running != 0 || v.Waiting != 0 {
		t.Fatalf("a Try starts nothing: %+v", v)
	}

	// The agent's refusal.
	done = commandAsync(t, srv, token, path, tryBody)
	cmdID, _ = readTry(t, bot)
	if s, b := postAck(t, srv, code, `{"id":"`+cmdID+`","ok":false,"error":"failed","message":"no playbook"}`); s != 204 {
		t.Fatalf("ack: %d %s", s, b)
	}
	if a := <-done; a.status != 409 || a.body != `{"error":"failed","message":"no playbook"}` {
		t.Fatalf("app answer: %+v", a)
	}

	// The result's shape: an object of at most 8 KiB.
	done = commandAsync(t, srv, token, path, tryBody)
	cmdID, _ = readTry(t, bot)
	for name, result := range map[string]string{
		"an array":   `[]`,
		"a string":   `"overFirst"`,
		"over 8 KiB": `{"note":"` + strings.Repeat("n", 8<<10) + `"}`,
	} {
		if s, b := postAck(t, srv, code, `{"id":"`+cmdID+`","ok":true,"result":`+result+`}`); s != 400 || !strings.Contains(b, `"bad_command"`) {
			t.Errorf("%s: %d %s, want 400 bad_command", name, s, b)
		}
	}
	big := `{"note":"` + strings.Repeat("n", 8<<10-12) + `"}` // 8 KiB once compact
	if s, b := postAck(t, srv, code, `{"id":"`+cmdID+`","ok":true,"result": `+big+`}`); s != 204 {
		t.Fatalf("8 KiB result: %d %s", s, b)
	}
	if a := <-done; a.status != 200 || a.body != big {
		t.Fatalf("app answer: %d %d bytes", a.status, len(a.body))
	}
	expectNoMessage(t, app, "app (a Try sends no botEvent)")
}

func TestBotTry_Refusals(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	defer func(d time.Duration) { commandWait = d }(commandWait)
	commandWait = 200 * time.Millisecond
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	oldCode, oldID := newBotCode(t, srv, token, "Old PC")
	p2Code, p2ID := newBotCode(t, srv, token, "Phase 2 PC")
	_, offlineID := newBotCode(t, srv, token, "Offline PC")
	_, otherID := newBotCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "Other")
	bot := startAgentWith(t, srv, code, tipBetIdleP3, app)
	startAgentWith(t, srv, oldCode, `{"books":[{"bookmaker":"tipsport","ready":true}],"dryRun":true}`, app)
	p2 := startAgentWith(t, srv, p2Code, p2Status(""), app) // protocol 2, no windows: takes orders, not a Try
	try := func(agent int64, body string) (int, string) {
		return request(t, srv, "POST", fmt.Sprintf("/api/me/bot/agents/%d/try", agent), token, body, nil)
	}
	for name, body := range map[string]string{
		"not JSON":       `{"legs":`,
		"one leg":        `{"legs":[{"bookmaker":"tipsport","odds":2.05,"stake":1000}]}`,
		"same bookmaker": `{"legs":[{"bookmaker":"tipsport","odds":2.05,"stake":1000},{"bookmaker":"tipsport","odds":2,"stake":1025}]}`,
		"odds 1":         `{"legs":[{"bookmaker":"tipsport","odds":1,"stake":1000},{"bookmaker":"betano","odds":2,"stake":1025}]}`,
		"stake 0":        `{"legs":[{"bookmaker":"tipsport","odds":2.05,"stake":0},{"bookmaker":"betano","odds":2,"stake":1025}]}`,
		"rounding 5":     strings.Replace(tryBody, `"rounding":10`, `"rounding":5`, 1),
		"sport over 40":  strings.Replace(tryBody, `"sport":"football"`, `"sport":"`+strings.Repeat("s", 41)+`"`, 1),
	} {
		if s, b := try(id, body); s != 400 || !strings.Contains(b, `"bad_order"`) {
			t.Errorf("%s: %d %s, want 400 bad_order", name, s, b)
		}
	}
	if s, b := request(t, srv, "POST", "/api/me/bot/agents/home/try", token, tryBody, nil); s != 400 || !strings.Contains(b, `"bad_order"`) {
		t.Fatalf("agent id not numeric: %d %s", s, b)
	}
	if s, b := try(otherID, tryBody); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
		t.Fatalf("another user's agent: %d %s", s, b)
	}
	if s, b := try(offlineID, tryBody); s != 409 || !strings.Contains(b, `"agent_offline"`) {
		t.Fatalf("offline agent: %d %s", s, b)
	}
	// An online agent older than protocol 3 cannot take a Try: "Update the agent".
	if s, b := try(oldID, tryBody); s != 409 || !strings.Contains(b, `"agent_update"`) {
		t.Fatalf("Phase 1 agent: %d %s", s, b)
	}
	if s, b := try(p2ID, tryBody); s != 409 || !strings.Contains(b, `"agent_update"`) {
		t.Fatalf("Phase 2 agent: %d %s", s, b)
	}
	if s, b := request(t, srv, "POST", fmt.Sprintf("/api/me/bot/agents/%d/try", id), "", tryBody, bearer(code)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("try with a bot code: %d %s", s, b)
	}
	if s, b := request(t, srv, "POST", fmt.Sprintf("/api/me/bot/agents/%d/try", id), token, tryBody, map[string]string{"Origin": "http://evil.example"}); s != 403 || !strings.Contains(b, `"bad_origin"`) {
		t.Fatalf("cross-site try: %d %s", s, b)
	}
	// While the windows are reserved for an order, a Try still goes (it presses nothing); no answer → 504.
	placeOrder(t, srv, token, "arb-1", bot, app)
	done := commandAsync(t, srv, token, fmt.Sprintf("/api/me/bot/agents/%d/try", id), tryBody)
	cmdID, _ := readTry(t, bot)
	if a := <-done; a.status != 504 || !strings.Contains(a.body, `"agent_timeout"`) {
		t.Fatalf("no answer: %+v", a)
	}
	if s, b := postAck(t, srv, code, `{"id":"`+cmdID+`","ok":true,"result":`+tryResult+`}`); s != 404 || !strings.Contains(b, `"unknown_command"`) {
		t.Fatalf("late ack: %d %s", s, b)
	}
	// Window commands still go to a protocol-2 agent.
	done = commandAsync(t, srv, token, fmt.Sprintf("/api/me/bot/agents/%d/windows", p2ID), `{"bookmaker":"tipsport"}`)
	cmd := readCommand(t, p2)
	if s, b := postAck(t, srv, p2Code, `{"id":"`+cmd.ID+`","ok":true,"windowId":"tipsport-1"}`); s != 204 || cmd.Action != "add" {
		t.Fatalf("protocol-2 agent: %+v, ack %d %s", cmd, s, b)
	}
	if a := <-done; a.status != 200 {
		t.Fatalf("window command to a protocol-2 agent: %+v", a)
	}
}

// A window command's answer may not carry a result; one that does is answered as before.
func TestBotCommands_ResultOnAWindowCommandIsIgnored(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	bot := startBot(t, srv, code, app)
	done := commandAsync(t, srv, token, fmt.Sprintf("/api/me/bot/agents/%d/windows/tipsport-1/restart", id), ``)
	cmd := readCommand(t, bot)
	if s, b := postAck(t, srv, code, `{"id":"`+cmd.ID+`","ok":true,"windowId":"tipsport-1","result":{"x":1}}`); s != 204 {
		t.Fatalf("ack: %d %s", s, b)
	}
	if a := <-done; a.status != 200 || a.body != `{"ok":true,"windowId":"tipsport-1"}` {
		t.Fatalf("app answer: %+v", a)
	}
}
