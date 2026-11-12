package main

import (
	"fmt"
	"strings"
	"testing"

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
