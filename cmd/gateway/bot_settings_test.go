package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func putSettings(t *testing.T, srv *httptest.Server, token, body string) (int, string) {
	t.Helper()
	return request(t, srv, "PUT", "/api/me/bot/settings", token, body, nil)
}

const defaultSettingsJSON = `{"dryRun":true,"paused":false,"pausedReason":"","limits":{"maxOrder":0,"maxLeg":0,"perDay":0,"ordersAtOnce":1,"pauseOnUnknown":true}}`

func TestBotSettings_GetPutAndValidation(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	app := appSocket(t, srv, token)
	if s, b := request(t, srv, "GET", "/api/me/bot/settings", token, "", nil); s != 200 || strings.TrimSpace(b) != defaultSettingsJSON {
		t.Fatalf("defaults: %d %s", s, b)
	}
	bad := map[string]string{
		"not JSON":           `{"dryRun":`,
		"an array":           `[]`,
		"dryRun a string":    `{"dryRun":"no"}`,
		"maxOrder negative":  `{"limits":{"maxOrder":-1}}`,
		"maxLeg over 10M":    `{"limits":{"maxLeg":10000001}}`,
		"perDay a fraction":  `{"limits":{"perDay":100.5}}`,
		"ordersAtOnce 0":     `{"limits":{"ordersAtOnce":0}}`,
		"ordersAtOnce 21":    `{"limits":{"ordersAtOnce":21}}`,
		"limits not object":  `{"limits":5}`,
		"pauseOnUnknown int": `{"limits":{"pauseOnUnknown":1}}`,
	}
	for name, body := range bad {
		if s, b := putSettings(t, srv, token, body); s != 400 || !strings.Contains(b, `"bad_settings"`) {
			t.Errorf("%s: %d %s, want 400 bad_settings", name, s, b)
		}
	}
	// A subset changes only what it names and answers the full settings.
	s, b := putSettings(t, srv, token, `{"dryRun":false,"limits":{"maxOrder":10000000,"ordersAtOnce":20,"pauseOnUnknown":false}}`)
	want := `{"dryRun":false,"paused":false,"pausedReason":"","limits":{"maxOrder":10000000,"maxLeg":0,"perDay":0,"ordersAtOnce":20,"pauseOnUnknown":false}}`
	if s != 200 || strings.TrimSpace(b) != want {
		t.Fatalf("put: %d %s", s, b)
	}
	expectBot(t, app, "on a settings change")
	if st, _ := svc.BotSettings(uid); st.DryRun || st.MaxOrder != 10_000_000 || st.OrdersAtOnce != 20 || st.PauseOnUnknown {
		t.Fatalf("stored: %+v", st)
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/settings", token, "", nil); s != 200 || strings.TrimSpace(b) != want {
		t.Fatalf("get after put: %d %s", s, b)
	}
	// Nothing changed: no message.
	if s, b := putSettings(t, srv, token, `{"limits":{"maxLeg":0}}`); s != 200 || strings.TrimSpace(b) != want {
		t.Fatalf("no-op put: %d %s", s, b)
	}
	if s, b := request(t, srv, "PUT", "/api/me/bot/settings", token, `{"dryRun":true}`, map[string]string{"Origin": "http://evil.example"}); s != 403 || !strings.Contains(b, `"bad_origin"`) {
		t.Fatalf("cross-site put: %d %s", s, b)
	}
	code := newCode(t, srv, token, "bot")
	if s, b := request(t, srv, "GET", "/api/me/bot/settings", "", "", bearer(code)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("get with a bot code: %d %s", s, b)
	}
	expectNoMessage(t, app, "app after a put that changed nothing")
}

func TestBotSettings_PauseEndsWaitingOrdersAndTellsAgents(t *testing.T) {
	srv, svc, _, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startBot(t, srv, code, app)
	running := placeOrder(t, srv, token, "arb-1", bot, app)
	waiting := postOrderOK(t, srv, token, tipBetOrder("arb-2"))
	expectEvent(t, app, waiting.OrderID, "waiting", "Orders at once: 1 running")
	expectBot(t, app, "on the waiting order")

	s, b := putSettings(t, srv, token, `{"paused":true}`)
	if s != 200 || !strings.Contains(b, `"paused":true,"pausedReason":"Paused"`) {
		t.Fatalf("pause: %d %s", s, b)
	}
	expectEvent(t, app, waiting.OrderID, "stopped", "Paused")
	expectBot(t, app, "on pause")
	if m := readJSON(t, bot); m["type"] != "pause" {
		t.Fatalf("agent got %v, want pause", m)
	}
	if state, reason, _, _, _ := orderRow(t, db, waiting.OrderID); state != "stopped" || reason != "Paused" {
		t.Fatalf("waiting row: %s %s", state, reason)
	}
	if v := getView(t, srv, token); !v.Paused || v.PausedReason != "Paused" || v.Running != 1 || v.Waiting != 0 {
		t.Fatalf("view: %+v (the running order runs on)", v)
	}
	if s, b := postOrder(t, srv, token, tipBetOrder("arb-3")); s != 409 || !strings.Contains(b, `"bots_paused"`) {
		t.Fatalf("order while paused: %d %s", s, b)
	}
	// The running order still reports normally.
	if s, b := postEvent(t, srv, code, `{"order_id":"`+running.OrderID+`","event":"stopped","reason":"Paused"}`); s != 204 {
		t.Fatalf("final event while paused: %d %s", s, b)
	}
	expectEvent(t, app, running.OrderID, "stopped", "Paused")
	expectBot(t, app, "after the final event")
	// Paused again from the app: no change, reason kept.
	if s, b := putSettings(t, srv, token, `{"paused":true}`); s != 200 || !strings.Contains(b, `"pausedReason":"Paused"`) {
		t.Fatalf("pause again: %d %s", s, b)
	}
	if s, b := putSettings(t, srv, token, `{"paused":false}`); s != 200 || !strings.Contains(b, `"paused":false,"pausedReason":""`) {
		t.Fatalf("resume: %d %s", s, b)
	}
	expectBot(t, app, "on resume")
	placeOrder(t, srv, token, "arb-3", bot, app)
}

func TestBotSettings_ResumeKeepsAnAutomaticReasonUntilCleared(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	setSettings(t, svc, bots, uid, func(st *accounts.BotSettings) { st.Paused, st.PausedReason = true, "Outcome unknown · order 0123abcd" })
	if s, b := putSettings(t, srv, token, `{"paused":true,"limits":{"maxLeg":500}}`); s != 200 ||
		!strings.Contains(b, `"pausedReason":"Outcome unknown · order 0123abcd"`) || !strings.Contains(b, `"maxLeg":500`) {
		t.Fatalf("pause while paused: %d %s", s, b)
	}
}

// Raising ordersAtOnce is a settings change: the waiting orders are tried at once.
func TestBotSettings_ChangeRetriesWaitingOrders(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	bot := startAgentWith(t, srv, newCode(t, srv, token, "bot"), p2Status("", win("tipsport", 1, "idle"), win("tipsport", 2, "idle"),
		win("betano", 1, "idle"), win("betano", 2, "idle")), app)
	placeOrder(t, srv, token, "arb-1", bot, app)
	waiting := postOrderOK(t, srv, token, tipBetOrder("arb-2"))
	expectEvent(t, app, waiting.OrderID, "waiting", "")
	expectBot(t, app, "on the waiting order")
	if s, b := putSettings(t, srv, token, `{"limits":{"ordersAtOnce":2}}`); s != 200 {
		t.Fatalf("put: %d %s", s, b)
	}
	expectEvent(t, app, waiting.OrderID, "sent", "")
	expectBot(t, app, "on the settings change")
	if o := readOrder(t, bot); o.OrderID != waiting.OrderID || o.Legs[0].WindowID != "tipsport-2" {
		t.Fatalf("agent got %+v", o)
	}
}

// An agent that was disconnected when the user paused learns of it from its first protocol-2
// status on the new socket (its running order may still be before the barrier), and only once.
func TestBotSettings_PauseReachesAnAgentThatReconnects(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	app := appSocket(t, srv, token)
	withBoot := p2Status(bootA, win("tipsport", 1, "idle"), win("betano", 1, "idle"))
	bot := startAgentWith(t, srv, code, withBoot, app)
	placeOrder(t, srv, token, "arb-1", bot, app)

	bot.Close(websocket.StatusNormalClosure, "")
	expectBot(t, app, "on agent disconnect")
	if s, b := putSettings(t, srv, token, `{"paused":true}`); s != 200 {
		t.Fatalf("pause: %d %s", s, b)
	}
	expectBot(t, app, "on pause")
	bot = startAgentWith(t, srv, code, withBoot, app) // same process, the order still runs
	if m := readJSON(t, bot); m["type"] != "pause" {
		t.Fatalf("agent got %v, want pause", m)
	}
	if s, b := postStatus(t, srv, code, withBoot); s != 204 {
		t.Fatalf("second status: %d %s", s, b)
	}
	expectNoMessage(t, bot, "the agent on its second status")
}
