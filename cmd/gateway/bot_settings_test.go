package main

import (
	"database/sql"
	"fmt"
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

const defaultSettingsJSON = `{"dryRun":true,"paused":false,"pausedReason":"","limits":{"maxOrder":0,"maxLeg":0,"perDay":0,"ordersAtOnce":1,"pauseOnUnknown":true},"rulesOn":[],"oddsDrop":"strict"}`

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
	want := `{"dryRun":false,"paused":false,"pausedReason":"","limits":{"maxOrder":10000000,"maxLeg":0,"perDay":0,"ordersAtOnce":20,"pauseOnUnknown":false},"rulesOn":[],"oddsDrop":"strict"}`
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

// breakSettingsRead stores userID's settings with maxLeg 500 and then a dry_run value the read
// cannot scan: reading fails while an UPSERT would still overwrite the row.
func breakSettingsRead(t *testing.T, svc *accounts.Service, db *sql.DB, userID int64, paused bool) {
	t.Helper()
	st := accounts.DefaultBotSettings()
	st.MaxLeg, st.Paused = 500, paused
	if paused {
		st.PausedReason = "Paused"
	}
	if err := svc.SaveBotSettings(userID, st); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE bot_settings SET dry_run = 'abc' WHERE user_id = ?`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BotSettings(userID); err == nil {
		t.Fatal("the read must fail")
	}
}

// settingsRow reads the stored row as text: dry_run, paused, paused_reason, max_leg.
func settingsRow(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()
	var dryRun, reason string
	var paused, maxLeg int64
	if err := db.QueryRow(`SELECT CAST(dry_run AS TEXT), paused, paused_reason, max_leg FROM bot_settings WHERE user_id = ?`, userID).
		Scan(&dryRun, &paused, &reason, &maxLeg); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s/%d/%s/%d", dryRun, paused, reason, maxLeg)
}

// A settings read that fails is never replaced by the defaults: GET and PUT answer 500 and the
// stored row stays as it was; a new order is refused too.
func TestBotSettings_ReadFailureNeverSavesDefaults(t *testing.T) {
	srv, svc, _, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	breakSettingsRead(t, svc, db, uid, true)
	for _, body := range []string{`{"limits":{"maxOrder":100}}`, `{"paused":false}`} {
		if s, b := putSettings(t, srv, token, body); s != 500 || !strings.Contains(b, `"internal"`) {
			t.Fatalf("PUT %s: %d %s, want 500 internal", body, s, b)
		}
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/settings", token, "", nil); s != 500 || !strings.Contains(b, `"internal"`) {
		t.Fatalf("GET: %d %s, want 500 internal", s, b)
	}
	if s, b := postOrder(t, srv, token, tipBetOrder("arb-1")); s != 500 || !strings.Contains(b, `"internal"`) {
		t.Fatalf("order: %d %s, want 500 internal", s, b)
	}
	if got := settingsRow(t, db, uid); got != "abc/1/Paused/500" {
		t.Fatalf("stored row changed: %s", got)
	}
}

// An automatic pause while the settings cannot be read pauses in memory only; once the row reads
// again, the pause is applied to the real settings (not the defaults) and stored.
func TestBotSettings_AutoPauseWithUnreadableSettingsKeepsTheRow(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	breakSettingsRead(t, svc, db, uid, false)
	if err := svc.InsertBotOrder(accounts.BotOrderRow{ID: "00000000000000b2", UserID: uid, ArbKey: "k", State: "waiting", DryRun: true, Stake: 1,
		InfoJSON: "{}", LegsJSON: "[]", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkBotOrderSent("00000000000000b2", 0, true, "strict", "[]", 2); err != nil {
		t.Fatal(err)
	}
	bots.recoverOrders()
	if got := settingsRow(t, db, uid); got != "abc/0//500" {
		t.Fatalf("stored row overwritten: %s", got)
	}
	if v := getView(t, srv, token); !v.Paused || v.PausedReason != "Outcome unknown · order 00000000" {
		t.Fatalf("in memory: %+v, want paused", v)
	}
	if _, err := db.Exec(`UPDATE bot_settings SET dry_run = 1 WHERE user_id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	s, b := request(t, srv, "GET", "/api/me/bot/settings", token, "", nil)
	if s != 200 || !strings.Contains(b, `"paused":true,"pausedReason":"Outcome unknown · order 00000000"`) || !strings.Contains(b, `"maxLeg":500`) {
		t.Fatalf("GET after the row reads again: %d %s", s, b)
	}
	if got := settingsRow(t, db, uid); got != "1/1/Outcome unknown · order 00000000/500" {
		t.Fatalf("stored row: %s", got)
	}
}

func TestBotSettings_RulesOn(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	app := appSocket(t, srv, token)
	ids := make([]string, 21)
	for i := range ids {
		ids[i] = fmt.Sprintf(`"rule-%d"`, i)
	}
	bad := map[string]string{
		"not an array":     `{"rulesOn":"totals-over-first"}`,
		"a number":         `{"rulesOn":[1]}`,
		"uppercase":        `{"rulesOn":["Totals-over-first"]}`,
		"empty id":         `{"rulesOn":[""]}`,
		"space":            `{"rulesOn":["totals over first"]}`,
		"id over 40 chars": `{"rulesOn":["` + strings.Repeat("r", 41) + `"]}`,
		"21 ids":           `{"rulesOn":[` + strings.Join(ids, ",") + `]}`,
	}
	for name, body := range bad {
		if s, b := putSettings(t, srv, token, body); s != 400 || !strings.Contains(b, `"bad_settings"`) {
			t.Errorf("%s: %d %s, want 400 bad_settings", name, s, b)
		}
	}
	// PUT replaces the list; an unknown id is kept (an agent may know it); a repeated id is kept once.
	s, b := putSettings(t, srv, token, `{"rulesOn":["totals-over-first","unknown-rule","totals-over-first"]}`)
	if s != 200 || !strings.HasSuffix(strings.TrimSpace(b), `"rulesOn":["totals-over-first","unknown-rule"],"oddsDrop":"strict"}`) {
		t.Fatalf("put: %d %s", s, b)
	}
	expectBot(t, app, "on switching rules on")
	if st, _ := svc.BotSettings(uid); st.RulesOn != `["totals-over-first","unknown-rule"]` {
		t.Fatalf("stored: %+v", st)
	}
	// null and absent leave it; the same list again is no change.
	for _, body := range []string{`{"rulesOn":null}`, `{"dryRun":true}`, `{"rulesOn":["totals-over-first","unknown-rule"]}`} {
		if s, b := putSettings(t, srv, token, body); s != 200 || !strings.Contains(b, `"rulesOn":["totals-over-first","unknown-rule"]`) {
			t.Fatalf("put %s: %d %s", body, s, b)
		}
	}
	if s, b := putSettings(t, srv, token, `{"rulesOn":[]}`); s != 200 || !strings.HasSuffix(strings.TrimSpace(b), `"rulesOn":[],"oddsDrop":"strict"}`) {
		t.Fatalf("every rule off: %d %s", s, b)
	}
	expectBot(t, app, "on switching every rule off")
	if st, _ := svc.BotSettings(uid); st.RulesOn != "[]" {
		t.Fatalf("stored after off: %+v", st)
	}
	expectNoMessage(t, app, "app after the puts that changed nothing")
}

func TestBotSettings_OddsDrop(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	app := appSocket(t, srv, token)
	for name, body := range map[string]string{
		"unknown mode": `{"oddsDrop":"fast"}`,
		"empty":        `{"oddsDrop":""}`,
		"upper case":   `{"oddsDrop":"Rearb"}`,
		"a number":     `{"oddsDrop":1}`,
	} {
		if s, b := putSettings(t, srv, token, body); s != 400 || !strings.Contains(b, `"bad_settings"`) {
			t.Errorf("%s: %d %s, want 400 bad_settings", name, s, b)
		}
	}
	s, b := putSettings(t, srv, token, `{"oddsDrop":"rearb"}`)
	if s != 200 || !strings.HasSuffix(strings.TrimSpace(b), `"rulesOn":[],"oddsDrop":"rearb"}`) {
		t.Fatalf("put: %d %s", s, b)
	}
	expectBot(t, app, "on the odds drop change")
	if st, _ := svc.BotSettings(uid); st.OddsDrop != "rearb" {
		t.Fatalf("stored: %+v", st)
	}
	if s, b := putSettings(t, srv, token, `{"oddsDrop":"strict"}`); s != 200 || !strings.Contains(b, `"oddsDrop":"strict"`) {
		t.Fatalf("back to strict: %d %s", s, b)
	}
	expectBot(t, app, "on the switch back")
	if s, b := putSettings(t, srv, token, `{"oddsDrop":"strict","dryRun":true}`); s != 200 || !strings.Contains(b, `"oddsDrop":"strict"`) {
		t.Fatalf("same again: %d %s", s, b)
	}
	if s, b := putSettings(t, srv, token, `{"oddsDrop":null}`); s != 200 || !strings.Contains(b, `"oddsDrop":"strict"`) {
		t.Fatalf("null = unchanged: %d %s", s, b)
	}
	expectNoMessage(t, app, "app after puts that changed nothing")
}
