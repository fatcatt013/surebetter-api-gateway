package main

import (
	"strings"
	"testing"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// humanWin is a protocol-4 window in state (reason) with an extra field (e.g. `,"human":{…}`; "" = none).
func humanWin(id, state, reason, extra string) string {
	book, num, _ := strings.Cut(id, "-")
	return `{"id":"` + id + `","bookmaker":"` + book + `","number":` + num + `,"device":"desktop","state":"` + state +
		`","mode":"bot","extension":true,"loggedIn":null,"proxy":null,"reason":"` + reason + `"` + extra + `}`
}

// p4Status is a protocol-4 status with humanActivities (JSON, "" = not sent) and windows.
func p4Status(acts string, windows ...string) string {
	a := ""
	if acts != "" {
		a = `,"humanActivities":` + acts
	}
	return `{"proto":4,"books":[],"dryRun":true` + a + `,"windows":[` + strings.Join(windows, ",") + `]}`
}

const fortunaMybets = `{"fortuna":["mybets"],"tipsport":[],"betano":[]}`

func TestBotStatus_HumanValidation(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	code := newCode(t, srv, token, "bot")
	human := func(fields string) string {
		return p4Status("", humanWin("fortuna-1", "idle", "", `,"human":{`+fields+`}`))
	}
	acts := func(js string) string { return p4Status(js, humanWin("fortuna-1", "idle", "", "")) }
	many := func(n int) string {
		return `{` + repeatJSON(n, func(i int) string { return `"b` + strings.Repeat("x", i) + `":[]` }) + `}`
	}
	bad := map[string]string{
		"activity empty":         human(`"activity":"","since":1`),
		"activity over 16":       human(`"activity":"` + strings.Repeat("a", 17) + `","since":1`),
		"sport over 40":          human(`"activity":"live","sport":"` + strings.Repeat("s", 41) + `","since":1`),
		"detail over 120":        human(`"activity":"check","detail":"` + strings.Repeat("d", 121) + `","since":1`),
		"since negative":         human(`"activity":"mybets","since":-1`),
		"since a string":         human(`"activity":"mybets","since":"1"`),
		"human a string":         p4Status("", humanWin("fortuna-1", "idle", "", `,"human":"mybets"`)),
		"acts bookmaker upper":   acts(`{"Fortuna":["mybets"]}`),
		"acts bookmaker over 40": acts(`{"` + strings.Repeat("b", 41) + `":[]}`),
		"acts 21 bookmakers":     acts(many(21)),
		"acts unknown activity":  acts(`{"fortuna":["bet"]}`),
		"acts not an array":      acts(`{"fortuna":"mybets"}`),
		"acts an array":          acts(`["mybets"]`),
	}
	for name, body := range bad {
		if s, b := postStatus(t, srv, code, body); s != 400 || !strings.Contains(b, `"bad_status"`) {
			t.Errorf("%s: %d %s, want 400 bad_status", name, s, b)
		}
	}

	dialBot(t, srv, code)
	waitBot(t, bots, uid, true)
	ok := map[string]string{
		"off sleep":           p4Status("", humanWin("fortuna-1", "off", "sleep", "")),
		"off inactive":        p4Status("", humanWin("fortuna-1", "off", "inactive", "")),
		"human at the limits": human(`"activity":"` + strings.Repeat("a", 16) + `","sport":"` + strings.Repeat("s", 40) + `","detail":"` + strings.Repeat("ž", 120) + `","since":0`),
		"human without sport": human(`"activity":"mybets","since":1763100000000`),
		"acts 20 bookmakers":  acts(many(20)),
		"acts every name":     acts(`{"fortuna":["prematch","live","community","mybets","open","inactive"],"betano":null}`),
		"acts empty":          acts(`{}`),
	}
	for name, body := range ok {
		if s, b := postStatus(t, srv, code, body); s != 204 {
			t.Errorf("%s: %d %s, want 204", name, s, b)
		}
	}
	// human is stored with its window.
	body := p4Status(fortunaMybets, humanWin("fortuna-1", "idle", "", `,"human":{"activity":"mybets","detail":"ticket 18:02","since":5}`),
		humanWin("tipsport-1", "off", "sleep", ""))
	if s, b := postStatus(t, srv, code, body); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	rows, _ := svc.ListBotAgents(uid)
	if len(rows) != 1 || !strings.Contains(rows[0].WindowsJSON, `"id":"fortuna-1"`) ||
		!strings.Contains(rows[0].WindowsJSON, `"human":{"activity":"mybets","detail":"ticket 18:02","since":5}`) ||
		!strings.Contains(rows[0].WindowsJSON, `"state":"off","extension":true,"loggedIn":null,"proxy":null,"reason":"sleep"`) ||
		strings.Count(rows[0].WindowsJSON, `"human"`) != 1 {
		t.Fatalf("stored windows: %+v", rows)
	}
}

// A change of human alone sends no {"type":"bot"}; a state or humanActivities change does.
func TestBotStatus_HumanChangeAloneDoesNotNotify(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	h := func(activity, detail string, since int) string {
		return `,"human":{"activity":"` + activity + `","detail":"` + detail + `","since":` + string(rune('0'+since)) + `}`
	}
	startAgentWith(t, srv, code, p4Status(fortunaMybets, humanWin("fortuna-1", "idle", "", "")), app)

	steps := []struct {
		name, body string
		notify     bool
	}{
		{"human appears", p4Status(fortunaMybets, humanWin("fortuna-1", "idle", "", h("mybets", "list", 1))), false},
		{"human detail and since", p4Status(fortunaMybets, humanWin("fortuna-1", "idle", "", h("mybets", "ticket", 2))), false},
		{"human activity", p4Status(fortunaMybets, humanWin("fortuna-1", "idle", "", h("check", "abc", 3))), false},
		{"human gone", p4Status(fortunaMybets, humanWin("fortuna-1", "idle", "", "")), false},
		{"state off", p4Status(fortunaMybets, humanWin("fortuna-1", "off", "sleep", "")), true},
		{"off reason", p4Status(fortunaMybets, humanWin("fortuna-1", "off", "inactive", "")), true},
		{"humanActivities", p4Status(`{"fortuna":[]}`, humanWin("fortuna-1", "off", "inactive", "")), true},
		{"humanActivities key order only", p4Status(`{"fortuna":[]}`, humanWin("fortuna-1", "off", "inactive", "")), false},
		{"humanActivities dropped", p4Status("", humanWin("fortuna-1", "off", "inactive", "")), true},
		{"back to idle with human", p4Status("", humanWin("fortuna-1", "idle", "", h("mybets", "", 4))), true},
	}
	for _, st := range steps {
		if s, b := postStatus(t, srv, code, st.body); s != 204 {
			t.Fatalf("%s: %d %s", st.name, s, b)
		}
		if st.notify {
			expectBot(t, app, st.name)
		}
	}
	expectNoMessage(t, app, "app after the last step") // the last read on app
}

// An off window is neither ready nor usable and never gets an order.
func TestBotSched_OffWindowsNeverPicked(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code := newCode(t, srv, token, "bot")
	bot := startAgentWith(t, srv, code, p4Status("", humanWin("tipsport-1", "idle", "", ""), humanWin("betano-1", "off", "sleep", "")), app)

	v := getView(t, srv, token)
	for _, bk := range v.Books {
		if bk.Bookmaker == "betano" && (bk.Ready || bk.Usable) {
			t.Fatalf("betano with only an off window: %+v", bk)
		}
	}
	if s, b := postOrder(t, srv, token, tipBetOrder("k")); s != 409 || !strings.Contains(b, `"book_not_ready"`) {
		t.Fatalf("betano window off: %d %s", s, b)
	}
	// betano-1 off, betano-2 busy: the order waits for betano-2, never takes betano-1.
	off2 := p4Status("", humanWin("tipsport-1", "idle", "", ""), humanWin("betano-1", "off", "inactive", ""), humanWin("betano-2", "busy", "", ""))
	if s, b := postStatus(t, srv, code, off2); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	expectBot(t, app, "on status")
	a := postOrderOK(t, srv, token, tipBetOrder("k"))
	if a.State != orderWaiting {
		t.Fatalf("order: %+v, want waiting", a)
	}
	expectEvent(t, app, a.OrderID, "waiting", "")
	expectBot(t, app, "on the waiting order")
	if v := getView(t, srv, token); v.Waiting != 1 || v.Running != 0 {
		t.Fatalf("view: %+v", v)
	}
	free := p4Status("", humanWin("tipsport-1", "idle", "", ""), humanWin("betano-1", "off", "inactive", ""), humanWin("betano-2", "idle", "", ""))
	if s, b := postStatus(t, srv, code, free); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	o := readOrder(t, bot)
	if o.OrderID != a.OrderID || o.Legs[1].WindowID != "betano-2" {
		t.Fatalf("order sent: %+v, want betano-2", o)
	}
}

// The Fleet shows each agent's protocol, human-mode switch and humanActivities, and each window's
// human; an offline agent has protocol 0, no humanActivities and its windows down without human.
func TestBotAgents_FleetHumanFields(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	bot := startAgentWith(t, srv, code, p4Status(fortunaMybets,
		humanWin("fortuna-1", "idle", "", `,"human":{"activity":"mybets","detail":"ticket 18:02","since":5}`),
		humanWin("tipsport-1", "off", "sleep", "")), app)
	if s, b := request(t, srv, "PUT", routinePath(id), token, `{"humanMode":true,"routine":null}`, nil); s != 200 {
		t.Fatalf("PUT routine: %d %s", s, b)
	}
	readType(t, bot) // {"type":"routine"}

	_, body := request(t, srv, "GET", "/api/me/bot/agents", token, "", nil)
	for _, part := range []string{`"proto":4`, `"humanMode":true`, `"humanActivities":{"betano":[],"fortuna":["mybets"],"tipsport":[]}`,
		`"human":{"activity":"mybets","detail":"ticket 18:02","since":5}`, `"state":"off"`} {
		if !strings.Contains(body, part) {
			t.Fatalf("online fleet has no %s: %s", part, body)
		}
	}
	if strings.Count(body, `"human":`) != 1 {
		t.Fatalf("a window without human shows one: %s", body)
	}

	bot.Close(1000, "")
	expectBot(t, app, "on disconnect")
	a := getFleet(t, srv, token)
	if len(a) != 1 || a[0].Online || a[0].Proto != 0 || !a[0].HumanMode || a[0].HumanActivities == nil || len(a[0].HumanActivities) != 0 {
		t.Fatalf("offline agent: %+v", a)
	}
	for _, w := range a[0].Windows {
		if w.Human != nil || w.State != "down" {
			t.Fatalf("offline window: %+v", w)
		}
	}
	if _, body := request(t, srv, "GET", "/api/me/bot/agents", token, "", nil); !strings.Contains(body, `"proto":0,"humanMode":true,"humanActivities":{}`) {
		t.Fatalf("offline JSON: %s", body)
	}
}
