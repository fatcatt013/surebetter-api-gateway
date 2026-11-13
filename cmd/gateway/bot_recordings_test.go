package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// mousePaths is a mouse recording of n paths of 3 points each.
func mousePaths(n int) string {
	paths := make([]string, n)
	for i := range paths {
		paths[i] = `{"points":[[0,0,0],[10,5,16.5],[20,9,33]]}`
	}
	return `{"paths":[` + strings.Join(paths, ",") + `]}`
}

// typingSessions is a typing recording of n sessions, each "1" down and up.
func typingSessions(n int) string {
	s := make([]string, n)
	for i := range s {
		s[i] = `{"focusAt":100,"events":[{"type":"down","key":"1","t":350},{"type":"up","key":"1","t":420}]}`
	}
	return `{"sessions":[` + strings.Join(s, ",") + `]}`
}

func TestBotRecordings_Validation(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	_, id := newBotCode(t, srv, token, "Home PC")
	put := func(kind, body string) (int, string) {
		return request(t, srv, "PUT", fmt.Sprintf("/api/me/bot/agents/%d/recordings/%s", id, kind), token, body, nil)
	}
	point := `[1,2,3]`
	bad := map[string][2]string{
		"not JSON":               {"mouse", `{"paths":`},
		"no paths":               {"mouse", `{"paths":[]}`},
		"201 paths":              {"mouse", mousePaths(201)},
		"path without points":    {"mouse", `{"paths":[{"points":[]}]}`},
		"5001 points":            {"mouse", `{"paths":[{"points":[` + strings.TrimSuffix(strings.Repeat(point+",", 5001), ",") + `]}]}`},
		"two numbers":            {"mouse", `{"paths":[{"points":[[1,2]]}]}`},
		"a string coordinate":    {"mouse", `{"paths":[{"points":[[1,"2",3]]}]}`},
		"a null coordinate":      {"mouse", `{"paths":[{"points":[[1,null,3]]}]}`},
		"a null time":            {"mouse", `{"paths":[{"points":[[1,2,null]]}]}`},
		"a null point":           {"mouse", `{"paths":[{"points":[null]}]}`},
		"no sessions":            {"typing", `{"sessions":[]}`},
		"201 sessions":           {"typing", typingSessions(201)},
		"session without events": {"typing", `{"sessions":[{"focusAt":1,"events":[]}]}`},
		"no focusAt":             {"typing", `{"sessions":[{"events":[{"type":"down","key":"1","t":1}]}]}`},
		"event type press":       {"typing", `{"sessions":[{"focusAt":1,"events":[{"type":"press","key":"1","t":1}]}]}`},
		"event without key":      {"typing", `{"sessions":[{"focusAt":1,"events":[{"type":"down","key":"","t":1}]}]}`},
		"event without t":        {"typing", `{"sessions":[{"focusAt":1,"events":[{"type":"down","key":"1"}]}]}`},
		"501 events": {"typing", `{"sessions":[{"focusAt":1,"events":[` +
			strings.TrimSuffix(strings.Repeat(`{"type":"down","key":"1","t":1},`, 501), ",") + `]}]}`},
	}
	for name, c := range bad {
		if s, b := put(c[0], c[1]); s != 400 || !strings.Contains(b, `"bad_recording"`) {
			t.Errorf("%s: %d %s, want 400 bad_recording", name, s, b)
		}
	}
	big := `{"paths":[{"points":[` + strings.TrimSuffix(strings.Repeat(`[123456.25,654321.5,99999.125],`, 40000), ",") + `]}]}`
	if s, b := put("mouse", big); s != 413 || !strings.Contains(b, `"too_large"`) {
		t.Fatalf("over 1 MiB: %d %s", s, b)
	}
	if s, b := put("voice", mousePaths(1)); s != 404 {
		t.Fatalf("unknown kind: %d %s", s, b)
	}
	if s, b := put("mouse", mousePaths(200)); s != 200 || !strings.Contains(b, `"count":200`) {
		t.Fatalf("200 paths: %d %s", s, b)
	}
	if s, b := put("typing", typingSessions(1)); s != 200 || !strings.Contains(b, `"count":1`) {
		t.Fatalf("one session: %d %s", s, b)
	}
}

func TestBotRecordings_PerAgentUploadAndAgentReads(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	home, homeID := newBotCode(t, srv, token, "Home PC")
	vps, vpsID := newBotCode(t, srv, token, "VPS Praha")
	other, otherID := newBotCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "Other")
	homeSock := dialBot(t, srv, home)
	vpsSock := dialBot(t, srv, vps)
	waitAgents(t, bots, userIDOf(t, svc, token), 2)

	if s, b := request(t, srv, "GET", "/api/me/bot/recordings", "", "", bearer(home)); s != 200 || strings.TrimSpace(b) != `{"mouse":null,"typing":null}` {
		t.Fatalf("summary before any upload: %d %s", s, b)
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/recordings/mouse", "", "", bearer(home)); s != 404 || !strings.Contains(b, `"no_recording"`) {
		t.Fatalf("no mouse recording: %d %s", s, b)
	}
	s, b := request(t, srv, "PUT", fmt.Sprintf("/api/me/bot/agents/%d/recordings/mouse", homeID), token, mousePaths(2), nil)
	var res struct {
		Count     int   `json:"count"`
		UpdatedAt int64 `json:"updatedAt"`
	}
	if json.Unmarshal([]byte(b), &res); s != 200 || res.Count != 2 || res.UpdatedAt <= 0 {
		t.Fatalf("upload: %d %s", s, b)
	}
	// Only that agent's socket is told.
	if m := readJSON(t, homeSock); m["type"] != "recordings" {
		t.Fatalf("home agent got %v", m)
	}
	expectNoMessage(t, vpsSock, "the other agent's socket") // the last read on vpsSock

	want := fmt.Sprintf(`{"mouse":{"count":2,"updatedAt":%d},"typing":null}`, res.UpdatedAt)
	if s, b := request(t, srv, "GET", "/api/me/bot/recordings", "", "", bearer(home)); s != 200 || strings.TrimSpace(b) != want {
		t.Fatalf("agent summary: %d %s, want %s", s, b, want)
	}
	if s, b := request(t, srv, "GET", fmt.Sprintf("/api/me/bot/agents/%d/recordings", homeID), token, "", nil); s != 200 || strings.TrimSpace(b) != want {
		t.Fatalf("app summary: %d %s", s, b)
	}
	wantBody := fmt.Sprintf(`{"updatedAt":%d,"paths":[{"points":[[0,0,0],[10,5,16.5],[20,9,33]]},{"points":[[0,0,0],[10,5,16.5],[20,9,33]]}]}`, res.UpdatedAt)
	if s, b := request(t, srv, "GET", "/api/me/bot/recordings/mouse", "", "", bearer(home)); s != 200 || strings.TrimSpace(b) != wantBody {
		t.Fatalf("agent download: %d %s", s, b)
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/recordings", "", "", bearer(vps)); s != 200 || strings.TrimSpace(b) != `{"mouse":null,"typing":null}` {
		t.Fatalf("the other agent has none: %d %s", s, b)
	}
	// An offline agent may get a recording; another user's or a revoked code may not.
	if s, b := request(t, srv, "PUT", fmt.Sprintf("/api/me/bot/agents/%d/recordings/typing", vpsID), token, typingSessions(3), nil); s != 200 {
		t.Fatalf("upload for the vps: %d %s", s, b)
	}
	if s, b := request(t, srv, "PUT", fmt.Sprintf("/api/me/bot/agents/%d/recordings/typing", otherID), token, typingSessions(1), nil); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
		t.Fatalf("another user's agent: %d %s", s, b)
	}
	if s, b := request(t, srv, "GET", fmt.Sprintf("/api/me/bot/agents/%d/recordings", otherID), token, "", nil); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
		t.Fatalf("another user's agent summary: %d %s", s, b)
	}
	if s, b := request(t, srv, "DELETE", fmt.Sprintf("/api/me/extension-codes/%d", vpsID), token, "", nil); s != 204 {
		t.Fatalf("revoke: %d %s", s, b)
	}
	if s, b := request(t, srv, "PUT", fmt.Sprintf("/api/me/bot/agents/%d/recordings/typing", vpsID), token, typingSessions(1), nil); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
		t.Fatalf("revoked agent: %d %s", s, b)
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/recordings/typing", "", "", bearer(other)); s != 404 || !strings.Contains(b, `"no_recording"`) {
		t.Fatalf("another user's agent download: %d %s", s, b)
	}
	// Credentials.
	if s, b := request(t, srv, "GET", "/api/me/bot/recordings", token, "", nil); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("agent summary with a session: %d %s", s, b)
	}
	if s, b := request(t, srv, "PUT", fmt.Sprintf("/api/me/bot/agents/%d/recordings/mouse", homeID), "", mousePaths(1), bearer(home)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("upload with a bot code: %d %s", s, b)
	}
	if s, b := request(t, srv, "PUT", fmt.Sprintf("/api/me/bot/agents/%d/recordings/mouse", homeID), token, mousePaths(1), map[string]string{"Origin": "http://evil.example"}); s != 403 || !strings.Contains(b, `"bad_origin"`) {
		t.Fatalf("cross-site upload: %d %s", s, b)
	}
}
