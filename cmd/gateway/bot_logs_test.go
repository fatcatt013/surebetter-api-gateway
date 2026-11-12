package main

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func postLogs(t *testing.T, srv *httptest.Server, code, body string) (int, string) {
	t.Helper()
	return request(t, srv, "POST", "/api/me/bot/logs", "", body, bearer(code))
}

// logLine is one valid log line JSON object with the given fields.
func logLine(ts int64, level, window, order, step, msg string) string {
	return fmt.Sprintf(`{"ts":%d,"level":%q,"windowId":%q,"orderId":%q,"step":%q,"msg":%q}`, ts, level, window, order, step, msg)
}

// infoLines is a {"lines":[…]} body of n valid info lines.
func infoLines(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = logLine(1789999999999, "info", "", "", "agent", fmt.Sprintf("line %d", i))
	}
	return `{"lines":[` + strings.Join(lines, ",") + `]}`
}

func TestBotLogs_PostValidation(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	ok := logLine(1789999999999, "info", "", "", "", "agent connected")
	bad := map[string]string{
		"not JSON":               `{"lines":`,
		"no lines":               `{"lines":[]}`,
		"201 lines":              infoLines(201),
		"ts 0":                   `{"lines":[` + logLine(0, "info", "", "", "", "x") + `]}`,
		"level debug":            `{"lines":[` + logLine(1, "debug", "", "", "", "x") + `]}`,
		"empty msg":              `{"lines":[` + logLine(1, "info", "", "", "", "") + `]}`,
		"msg over 500":           `{"lines":[` + logLine(1, "info", "", "", "", strings.Repeat("m", 501)) + `]}`,
		"step over 40":           `{"lines":[` + logLine(1, "info", "", "", strings.Repeat("s", 41), "x") + `]}`,
		"window over 40":         `{"lines":[` + logLine(1, "info", strings.Repeat("w", 41), "", "", "x") + `]}`,
		"order over 32":          `{"lines":[` + logLine(1, "info", "", strings.Repeat("o", 33), "", "x") + `]}`,
		"detail string":          `{"lines":[{"ts":1,"level":"info","msg":"x","detail":"odds"}]}`,
		"detail wrong type":      `{"lines":[{"ts":1,"level":"info","msg":"x","detail":{"ordered":"2.10"}}]}`,
		"detail over 2000 bytes": `{"lines":[{"ts":1,"level":"info","msg":"x","detail":{"partner":"` + strings.Repeat("p", 2000) + `"}}]}`,
	}
	for name, body := range bad {
		if s, b := postLogs(t, srv, code, body); s != 400 || !strings.Contains(b, `"bad_logs"`) {
			t.Errorf("%s: %d %s, want 400 bad_logs", name, s, b)
		}
	}
	// 200 lines and 500-char messages are fine, and no bot socket is needed.
	if s, b := postLogs(t, srv, code, infoLines(200)); s != 204 {
		t.Fatalf("200 lines: %d %s", s, b)
	}
	if s, b := postLogs(t, srv, code, `{"lines":[`+logLine(1, "error", "", "", "", strings.Repeat("m", 500))+`]}`); s != 204 {
		t.Fatalf("500-char msg: %d %s", s, b)
	}
	if s, b := request(t, srv, "POST", "/api/me/bot/logs", token, `{"lines":[`+ok+`]}`, nil); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("logs with a session: %d %s", s, b)
	}
	if s, b := postLogs(t, srv, newCode(t, srv, token, "extension"), `{"lines":[`+ok+`]}`); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("logs with an extension code: %d %s", s, b)
	}
}

func TestBotLogs_DetailKeepsOnlyKnownKeys(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	body := `{"lines":[` +
		`{"ts":1789999999999,"level":"warn","windowId":"fortuna-1","orderId":"c81e55a0deadbeef","step":"checkOdds","msg":"odds dropped 2.10 → 2.05 · pair stopped",` +
		`"detail":{"ordered":2.10,"onSlip":2.05,"partner":"Tipsport 1 · stopped, nothing pressed","nothingBet":true,"shot":"a1b2c3d4e5f60718",` +
		`"match":"Lyon – Lille","market":"Corners 10.5","password":"hunter2"}},` +
		`{"ts":1789999999999,"level":"info","msg":"empty detail","detail":{}}]}`
	if s, b := postLogs(t, srv, code, body); s != 204 {
		t.Fatalf("post: %d %s", s, b)
	}
	lines, err := svc.QueryBotLogs(userIDOf(t, svc, token), accounts.BotLogFilter{})
	if err != nil || len(lines) != 2 {
		t.Fatalf("lines: %+v %v", lines, err)
	}
	if lines[0].Msg != "empty detail" || lines[0].Detail != nil {
		t.Fatalf("an empty detail is none: %+v", lines[0])
	}
	want := `{"ordered":2.1,"onSlip":2.05,"partner":"Tipsport 1 · stopped, nothing pressed","nothingBet":true,"shot":"a1b2c3d4e5f60718","match":"Lyon – Lille","market":"Corners 10.5"}`
	if got := string(lines[1].Detail); got != want {
		t.Fatalf("detail:\n got %s\nwant %s", got, want)
	}
	if l := lines[1]; l.Level != "warn" || l.WindowID != "fortuna-1" || l.OrderID != "c81e55a0deadbeef" || l.Step != "checkOdds" || l.TS != 1789999999999 {
		t.Fatalf("line: %+v", l)
	}
}

func TestBotLogs_RateLimitPerCode(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	other := newCode(t, srv, token, "bot")

	for i := 0; i < 10; i++ { // 2 000 lines in the minute
		if s, b := postLogs(t, srv, code, infoLines(200)); s != 204 {
			t.Fatalf("batch %d: %d %s", i, s, b)
		}
	}
	clk.advance(59 * time.Second)
	if s, b := postLogs(t, srv, code, infoLines(1)); s != 429 || !strings.Contains(b, `"too_many_logs"`) {
		t.Fatalf("line 2 001: %d %s", s, b)
	}
	if s, b := postLogs(t, srv, other, infoLines(1)); s != 204 {
		t.Fatalf("another code has its own budget: %d %s", s, b)
	}
	clk.advance(time.Second)
	if s, b := postLogs(t, srv, code, infoLines(200)); s != 204 {
		t.Fatalf("next minute: %d %s", s, b)
	}
}
