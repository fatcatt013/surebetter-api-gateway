package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// fullStatus is a status as agent v1 posts it: tipsport and fortuna books, version, host
// home-pc with cpu/mem, and one window per bookmaker (tipsport in tipState, fortuna down).
func fullStatus(version string, cpu, mem int, tipState string) string {
	return fmt.Sprintf(`{"books":[{"bookmaker":"tipsport","ready":true},{"bookmaker":"fortuna","ready":false}],"dryRun":true,`+
		`"version":%q,"host":{"name":"home-pc","cpu":%d,"mem":%d},"windows":[`+
		`{"id":"tipsport-1","bookmaker":"tipsport","number":1,"device":"desktop","state":%q,"extension":true,"loggedIn":null,"proxy":null,"reason":""},`+
		`{"id":"fortuna-1","bookmaker":"fortuna","number":1,"device":"desktop","state":"down","extension":false,"loggedIn":null,"proxy":null,"reason":"Chrome not running"}]}`,
		version, cpu, mem, tipState)
}

const fullWindowsJSON = `[{"id":"tipsport-1","bookmaker":"tipsport","number":1,"device":"desktop","state":"idle","extension":true,"loggedIn":null,"proxy":null,"reason":""},` +
	`{"id":"fortuna-1","bookmaker":"fortuna","number":1,"device":"desktop","state":"down","extension":false,"loggedIn":null,"proxy":null,"reason":"Chrome not running"}]`

func TestBotStatus_Validation(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	win := func(field string) string { // one valid window with one field overridden
		return `{"windows":[{"id":"tipsport-1","bookmaker":"tipsport","number":1,"device":"desktop","state":"idle",` + field + `}]}`
	}
	bad := map[string]string{
		"not JSON":             `{"books":`,
		"empty body":           ``,
		"version over 64":      `{"version":"` + strings.Repeat("v", 65) + `"}`,
		"host name over 64":    `{"host":{"name":"` + strings.Repeat("h", 65) + `"}}`,
		"cpu over 100":         `{"host":{"name":"x","cpu":101,"mem":1}}`,
		"mem below 0":          `{"host":{"name":"x","cpu":1,"mem":-1}}`,
		"cpu not an integer":   `{"host":{"name":"x","cpu":2.5,"mem":1}}`,
		"empty bookmaker":      `{"books":[{"bookmaker":"","ready":true}]}`,
		"window id uppercase":  win(`"id":"Tipsport-1"`),
		"window id too long":   win(`"id":"` + strings.Repeat("a", 41) + `"`),
		"window number 0":      win(`"number":0`),
		"window no bookmaker":  win(`"bookmaker":""`),
		"unknown device":       win(`"device":"tablet"`),
		"unknown state":        win(`"state":"sleeping"`),
		"reason over 200":      win(`"reason":"` + strings.Repeat("r", 201) + `"`),
		"proxy over 100":       win(`"proxy":"` + strings.Repeat("p", 101) + `"`),
		"duplicate window ids": `{"windows":[{"id":"a-1","bookmaker":"a","number":1,"device":"desktop","state":"idle"},{"id":"a-1","bookmaker":"a","number":1,"device":"desktop","state":"idle"}]}`,
		"33 windows":           `{"windows":[` + strings.TrimSuffix(strings.Repeat(`{"id":"a-1","bookmaker":"a","number":1,"device":"desktop","state":"idle"},`, 33), ",") + `]}`,
	}
	// The body is checked first: these are 400 although the agent has no socket.
	for name, body := range bad {
		if s, b := postStatus(t, srv, code, body); s != 400 || !strings.Contains(b, `"bad_status"`) {
			t.Errorf("%s: %d %s, want 400 bad_status", name, s, b)
		}
	}

	dialBot(t, srv, code)
	waitBot(t, bots, userIDOf(t, svc, token), true)
	// Reserved states, a phone, a proxy and loggedIn true/false are accepted (a later window in a
	// later field order too: JSON key order does not matter).
	ok := `{"windows":[{"id":"betano-1","bookmaker":"betano","number":1,"device":"phone","state":"logged_out","extension":true,"loggedIn":false,"proxy":"socks5 · cz-1","reason":""},` +
		`{"reason":"","proxy":null,"loggedIn":true,"extension":true,"state":"blocked","device":"desktop","number":2,"bookmaker":"betano","id":"betano-2"}]}`
	if s, b := postStatus(t, srv, code, ok); s != 204 {
		t.Fatalf("valid status: %d %s", s, b)
	}
}

func TestBotStatus_StoredInTheRegistry(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	app := appSocket(t, srv, token)
	code, codeID := newBotCode(t, srv, token, "Home PC")
	dialBot(t, srv, code)
	readType(t, app)

	if s, b := postStatus(t, srv, code, fullStatus("0.9.0+e9ed9d2", 23, 41, "idle")); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	rows, err := svc.ListBotAgents(uid)
	if err != nil || len(rows) != 1 {
		t.Fatalf("registry: %+v %v", rows, err)
	}
	if r := rows[0]; r.CodeID != codeID || r.Version != "0.9.0+e9ed9d2" || r.HostName != "home-pc" || r.WindowsJSON != fullWindowsJSON {
		t.Fatalf("stored: %+v", r)
	}
	// An older agent (no version, host or windows) still works; the stored fields become empty.
	if s, b := postStatus(t, srv, code, `{"books":[{"bookmaker":"tipsport","ready":true}],"dryRun":true}`); s != 204 {
		t.Fatalf("old-style status: %d %s", s, b)
	}
	rows, _ = svc.ListBotAgents(uid)
	if r := rows[0]; r.Version != "" || r.HostName != "" || r.WindowsJSON != "[]" {
		t.Fatalf("after an old-style status: %+v", r)
	}
}

func TestBotStatus_NotifiesOnChangesButNotForCPUAndMemory(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	dialBot(t, srv, code)
	readType(t, app)

	steps := []struct {
		name, body string
		notify     bool
	}{
		{"first full status", fullStatus("0.9.0", 10, 20, "idle"), true},
		{"window state", fullStatus("0.9.0", 10, 20, "busy"), true},
		{"version", fullStatus("0.9.1", 10, 20, "busy"), true},
		{"host name", strings.Replace(fullStatus("0.9.1", 10, 20, "busy"), `"home-pc"`, `"vps-praha"`, 1), true},
		{"back to home-pc", fullStatus("0.9.1", 10, 20, "busy"), true},
	}
	for _, st := range steps {
		if s, b := postStatus(t, srv, code, st.body); s != 204 {
			t.Fatalf("%s: %d %s", st.name, s, b)
		}
		if got := readType(t, app); got != "bot" {
			t.Fatalf("%s: app got %s, want bot", st.name, got)
		}
	}
	// cpu/mem only: no message (expectNoMessage must be the last read on app).
	if s, b := postStatus(t, srv, code, fullStatus("0.9.1", 77, 88, "busy")); s != 204 {
		t.Fatalf("cpu/mem status: %d %s", s, b)
	}
	expectNoMessage(t, app, "app socket after a cpu/mem-only status")
}

// getFleet returns the GET /api/me/bot/agents list for a session.
func getFleet(t *testing.T, srv *httptest.Server, token string) []agentView {
	t.Helper()
	s, body := request(t, srv, "GET", "/api/me/bot/agents", token, "", nil)
	var res struct {
		Agents []agentView `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &res); s != 200 || err != nil || res.Agents == nil {
		t.Fatalf("GET /api/me/bot/agents: %d %s", s, body)
	}
	return res.Agents
}

func intp(n int) *int { return &n }

func TestBotAgents_Fleet(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)

	vps, vpsID := newBotCode(t, srv, token, "VPS Praha")
	home, homeID := newBotCode(t, srv, token, "Home PC")
	attic, atticID := newBotCode(t, srv, token, "attic")
	newBotCode(t, srv, token, "Never connected")
	otherToken := newUser(t, svc, "o@example.com", accounts.RoleUser)
	otherCode, _ := newBotCode(t, srv, otherToken, "Other")
	dialBot(t, srv, otherCode)

	// VPS Praha: connected, reported two windows, went offline.
	vpsSock := dialBot(t, srv, vps)
	readType(t, app)
	if s, b := postStatus(t, srv, vps, fullStatus("0.8.0", 5, 6, "idle")); s != 204 {
		t.Fatalf("vps status: %d %s", s, b)
	}
	readType(t, app)
	vpsSock.Close(websocket.StatusNormalClosure, "")
	readType(t, app)
	// attic: online, no status yet. Home PC: online, live, busy-free, with host numbers.
	dialBot(t, srv, attic)
	readType(t, app)
	homeSock := dialBot(t, srv, home)
	readType(t, app)
	live := strings.Replace(fullStatus("0.9.0+e9ed9d2", 23, 41, "idle"), `"dryRun":true`, `"dryRun":false`, 1)
	if s, b := postStatus(t, srv, home, live); s != 204 {
		t.Fatalf("home status: %d %s", s, b)
	}
	readType(t, app)

	got := getFleet(t, srv, token)
	for i := range got {
		if got[i].LastSeen <= 0 {
			t.Fatalf("agent %d lastSeen = %d", got[i].ID, got[i].LastSeen)
		}
		got[i].LastSeen = 0
	}
	down := windowsOf(fullWindowsJSON)
	for i := range down {
		down[i].State, down[i].Reason = "down", "Agent offline"
	}
	want := []agentView{
		// Online first, then by name ignoring case: attic, Home PC; then the offline VPS Praha.
		// Both online agents speak protocol 1 (no "proto" in their status): they need an update.
		{ID: atticID, Name: "attic", Online: true, Host: botHost{}, DryRun: true, Update: true, Windows: []botWindow{}},
		{ID: homeID, Name: "Home PC", Online: true, Version: "0.9.0+e9ed9d2", Host: botHost{Name: "home-pc", CPU: intp(23), Mem: intp(41)},
			DryRun: false, Update: true, Windows: windowsOf(fullWindowsJSON)},
		{ID: vpsID, Name: "VPS Praha", Online: false, Version: "0.8.0", Host: botHost{Name: "home-pc"}, DryRun: true, Windows: down},
	}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Fatalf("fleet:\n got %s\nwant %s", gj, wj)
	}
	// Offline cpu/mem are null on the wire.
	if _, body := request(t, srv, "GET", "/api/me/bot/agents", token, "", nil); !strings.Contains(body, `"host":{"name":"home-pc","cpu":null,"mem":null}`) {
		t.Fatalf("offline host: %s", body)
	}

	// Revoking a code closes its agent's socket and drops it from the Fleet.
	if s, b := request(t, srv, "DELETE", fmt.Sprintf("/api/me/extension-codes/%d", homeID), token, "", nil); s != 204 {
		t.Fatalf("revoke: %d %s", s, b)
	}
	expectClosedBy(t, homeSock, websocket.StatusPolicyViolation, "the revoked agent's socket")
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on revoke: %s", got)
	}
	for _, a := range getFleet(t, srv, token) {
		if a.ID == homeID {
			t.Fatalf("revoked agent still listed: %+v", a)
		}
	}
}

func TestBotAgents_SessionOnly(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	if s, b := request(t, srv, "GET", "/api/me/bot/agents", "", "", bearer(code)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("agents with a bot code: %d %s", s, b)
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/agents", token, "", nil); s != 200 || strings.TrimSpace(b) != `{"agents":[]}` {
		t.Fatalf("no agents: %d %s", s, b)
	}
}

func TestBotStatus_ProtoAndWindowMode(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, codeID := newBotCode(t, srv, token, "Home PC")
	win := `{"id":"tipsport-1","bookmaker":"tipsport","number":1,"device":"desktop","state":"down","reason":"Stopped","mode":%q}`
	bad := map[string]string{
		"proto -1":       `{"proto":-1}`,
		"proto 100":      `{"proto":100}`,
		"proto a string": `{"proto":"2"}`,
		"proto 2.5":      `{"proto":2.5}`,
		"mode sleeping":  `{"windows":[` + fmt.Sprintf(win, "sleeping") + `]}`,
	}
	for name, body := range bad {
		if s, b := postStatus(t, srv, code, body); s != 400 || !strings.Contains(b, `"bad_status"`) {
			t.Errorf("%s: %d %s, want 400 bad_status", name, s, b)
		}
	}
	dialBot(t, srv, code)
	readType(t, app)
	waitBot(t, bots, userIDOf(t, svc, token), true)

	// Before any status (proto 0): the Fleet says the agent needs an update.
	if a := getFleet(t, srv, token); len(a) != 1 || a[0].ID != codeID || !a[0].Update {
		t.Fatalf("before a status: %+v", a)
	}
	for _, mode := range []string{"bot", "login", "stopped"} {
		if s, b := postStatus(t, srv, code, `{"proto":2,"windows":[`+fmt.Sprintf(win, mode)+`]}`); s != 204 {
			t.Fatalf("mode %s: %d %s", mode, s, b)
		}
		if got := readType(t, app); got != "bot" {
			t.Fatalf("app on mode %s: %s", mode, got)
		}
		a := getFleet(t, srv, token)
		if a[0].Update || len(a[0].Windows) != 1 || a[0].Windows[0].Mode != mode {
			t.Fatalf("mode %s: %+v", mode, a[0])
		}
	}
	// Going back to an older agent (no proto) is a change and needs an update; no mode → no "mode" key.
	if s, b := postStatus(t, srv, code, `{"windows":[{"id":"tipsport-1","bookmaker":"tipsport","number":1,"device":"desktop","state":"down","reason":"Stopped"}]}`); s != 204 {
		t.Fatalf("older status: %d %s", s, b)
	}
	if got := readType(t, app); got != "bot" {
		t.Fatalf("app on proto change: %s", got)
	}
	_, body := request(t, srv, "GET", "/api/me/bot/agents", token, "", nil)
	if !strings.Contains(body, `"update":true`) || strings.Contains(body, `"mode"`) {
		t.Fatalf("older agent: %s", body)
	}
}

func TestBotAgents_ReservedWindowsShowTheirOrder(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, codeID := newBotCode(t, srv, token, "Home PC")
	// The agent cannot set the overlay itself: orderId and step in a status are dropped.
	status := p2Status("", strings.Replace(win("tipsport", 1, "idle"), `"reason":""`, `"reason":"","orderId":"x","step":"y"`, 1),
		win("betano", 1, "idle"), win("betano", 2, "idle"))
	bot := startAgentWith(t, srv, code, status, app)
	if a := getFleet(t, srv, token); a[0].Busy || a[0].Windows[0].OrderID != "" || a[0].Windows[0].Step != "" {
		t.Fatalf("before an order: %+v", a[0])
	}
	o := placeOrder(t, srv, token, "arb-1", bot, app)
	if s, b := postEvent(t, srv, code, `{"order_id":"`+o.OrderID+`","event":"leg_step","bookmaker":"betano","step":"enterStake"}`); s != 204 {
		t.Fatalf("leg_step: %d %s", s, b)
	}
	a := getFleet(t, srv, token)
	if len(a) != 1 || a[0].ID != codeID || !a[0].Busy || a[0].Update {
		t.Fatalf("agent: %+v", a)
	}
	ws := a[0].Windows
	// The agent's status still says idle; the gateway shows the reserved windows busy.
	if ws[0].State != "busy" || ws[0].OrderID != o.OrderID || ws[0].Step != "" ||
		ws[1].State != "busy" || ws[1].OrderID != o.OrderID || ws[1].Step != "enterStake" ||
		ws[2].State != "idle" || ws[2].OrderID != "" || ws[2].Mode != "bot" {
		t.Fatalf("windows: %+v", ws)
	}
	_, body := request(t, srv, "GET", "/api/me/bot/agents", token, "", nil)
	if !strings.Contains(body, `"orderId":"`+o.OrderID+`","step":"enterStake"`) || strings.Count(body, `"orderId"`) != 2 {
		t.Fatalf("wire: %s", body)
	}
}

func TestBotAgents_FlowsAt(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code, codeID := newBotCode(t, srv, token, "Home PC")
	dialBot(t, srv, code)
	waitBot(t, bots, userIDOf(t, svc, token), true)
	if a := getFleet(t, srv, token); len(a) != 1 || a[0].FlowsAt != 0 {
		t.Fatalf("before any Flows: %+v", a)
	}
	if s, b := request(t, srv, "PUT", "/api/me/bot/flows", "", `{"steps":[],"cases":[],"playbooks":[],"rules":[]}`, bearer(code)); s != 204 {
		t.Fatalf("upload: %d %s", s, b)
	}
	var flows struct {
		UpdatedAt int64 `json:"updatedAt"`
	}
	_, body := request(t, srv, "GET", fmt.Sprintf("/api/me/bot/flows?agent=%d", codeID), token, "", nil)
	json.Unmarshal([]byte(body), &flows)
	if a := getFleet(t, srv, token); a[0].FlowsAt <= 0 || a[0].FlowsAt != flows.UpdatedAt {
		t.Fatalf("flowsAt %d, flows updatedAt %d", a[0].FlowsAt, flows.UpdatedAt)
	}
}
