package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func postLogs(t *testing.T, srv *httptest.Server, code, body string) (int, string) {
	t.Helper()
	return request(t, srv, "POST", "/api/me/bot/logs", "", body, bearer(code))
}

// logTestNow is the bot clock of the log tests that post lines, and logTS a valid line ts at it.
var (
	logTestNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	logTS      = logTestNow.UnixMilli()
)

// fixClock sets the hub's clock to logTestNow and returns it.
func fixClock(bots *botHub) *botClock {
	clk := &botClock{t: logTestNow}
	bots.now = clk.now
	return clk
}

// logLine is one valid log line JSON object with the given fields.
func logLine(ts int64, level, window, order, step, msg string) string {
	return fmt.Sprintf(`{"ts":%d,"level":%q,"windowId":%q,"orderId":%q,"step":%q,"msg":%q}`, ts, level, window, order, step, msg)
}

// infoLines is a {"lines":[…]} body of n valid info lines.
func infoLines(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = logLine(logTS, "info", "", "", "agent", fmt.Sprintf("line %d", i))
	}
	return `{"lines":[` + strings.Join(lines, ",") + `]}`
}

func TestBotLogs_PostValidation(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	fixClock(bots)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	ok := logLine(logTS, "info", "", "", "", "agent connected")
	week, hour := botRetention.Milliseconds(), time.Hour.Milliseconds()
	bad := map[string]string{
		"not JSON":               `{"lines":`,
		"no lines":               `{"lines":[]}`,
		"201 lines":              infoLines(201),
		"ts 0":                   `{"lines":[` + logLine(0, "info", "", "", "", "x") + `]}`,
		"level debug":            `{"lines":[` + logLine(logTS, "debug", "", "", "", "x") + `]}`,
		"empty msg":              `{"lines":[` + logLine(logTS, "info", "", "", "", "") + `]}`,
		"msg over 500":           `{"lines":[` + logLine(logTS, "info", "", "", "", strings.Repeat("m", 501)) + `]}`,
		"step over 40":           `{"lines":[` + logLine(logTS, "info", "", "", strings.Repeat("s", 41), "x") + `]}`,
		"window over 40":         `{"lines":[` + logLine(logTS, "info", strings.Repeat("w", 41), "", "", "x") + `]}`,
		"order over 32":          `{"lines":[` + logLine(logTS, "info", "", strings.Repeat("o", 33), "", "x") + `]}`,
		"detail string":          fmt.Sprintf(`{"lines":[{"ts":%d,"level":"info","msg":"x","detail":"odds"}]}`, logTS),
		"detail wrong type":      fmt.Sprintf(`{"lines":[{"ts":%d,"level":"info","msg":"x","detail":{"ordered":"2.10"}}]}`, logTS),
		"detail over 2000 bytes": fmt.Sprintf(`{"lines":[{"ts":%d,"level":"info","msg":"x","detail":{"partner":"%s"}}]}`, logTS, strings.Repeat("p", 2000)),
		"ts over 7 days old":     `{"lines":[` + ok + "," + logLine(logTS-week-1, "info", "", "", "", "x") + `]}`,
		"ts over 1 h ahead":      `{"lines":[` + ok + "," + logLine(logTS+hour+1, "info", "", "", "", "x") + `]}`,
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
	if s, b := postLogs(t, srv, code, `{"lines":[`+logLine(logTS, "error", "", "", "", strings.Repeat("m", 500))+`]}`); s != 204 {
		t.Fatalf("500-char msg: %d %s", s, b)
	}
	edges := `{"lines":[` + logLine(logTS-week, "info", "", "", "", "7 days old") + "," + logLine(logTS+hour, "info", "", "", "", "1 h ahead") + `]}`
	if s, b := postLogs(t, srv, code, edges); s != 204 {
		t.Fatalf("ts at the window's edges: %d %s", s, b)
	}
	if s, b := request(t, srv, "POST", "/api/me/bot/logs", token, `{"lines":[`+ok+`]}`, nil); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("logs with a session: %d %s", s, b)
	}
	if s, b := postLogs(t, srv, newCode(t, srv, token, "extension"), `{"lines":[`+ok+`]}`); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("logs with an extension code: %d %s", s, b)
	}
}

func TestBotLogs_DetailKeepsOnlyKnownKeys(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	fixClock(bots)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	body := `{"lines":[` +
		`{"ts":` + fmt.Sprint(logTS) + `,"level":"warn","windowId":"fortuna-1","orderId":"c81e55a0deadbeef","step":"checkOdds","msg":"odds dropped 2.10 → 2.05 · pair stopped",` +
		`"detail":{"ordered":2.10,"onSlip":2.05,"partner":"Tipsport 1 · stopped, nothing pressed","nothingBet":true,"shot":"a1b2c3d4e5f60718",` +
		`"match":"Lyon – Lille","market":"Corners 10.5","password":"hunter2"}},` +
		`{"ts":` + fmt.Sprint(logTS) + `,"level":"info","msg":"empty detail","detail":{}}]}`
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
	if l := lines[1]; l.Level != "warn" || l.WindowID != "fortuna-1" || l.OrderID != "c81e55a0deadbeef" || l.Step != "checkOdds" || l.TS != logTS {
		t.Fatalf("line: %+v", l)
	}
}

func TestBotLogs_RateLimitPerUser(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := fixClock(bots)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	sameUser := newCode(t, srv, token, "bot")
	otherUser := newCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "bot")

	for i := 0; i < 10; i++ { // 2 000 lines in the minute, over two codes of one user
		c := code
		if i%2 == 1 {
			c = sameUser
		}
		if s, b := postLogs(t, srv, c, infoLines(200)); s != 204 {
			t.Fatalf("batch %d: %d %s", i, s, b)
		}
	}
	clk.advance(59 * time.Second)
	if s, b := postLogs(t, srv, code, infoLines(1)); s != 429 || !strings.Contains(b, `"too_many_logs"`) {
		t.Fatalf("line 2 001: %d %s", s, b)
	}
	if s, b := postLogs(t, srv, newCode(t, srv, token, "bot"), infoLines(1)); s != 429 || !strings.Contains(b, `"too_many_logs"`) {
		t.Fatalf("a new code of the same user shares its budget: %d %s", s, b)
	}
	if s, b := postLogs(t, srv, otherUser, infoLines(1)); s != 204 {
		t.Fatalf("another user has its own budget: %d %s", s, b)
	}
	clk.advance(time.Second)
	if s, b := postLogs(t, srv, code, infoLines(200)); s != 204 {
		t.Fatalf("next minute: %d %s", s, b)
	}
}

// Lines that were not stored do not count against the minute's budget.
func TestBotLogs_FailedInsertIsNotCounted(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	fixClock(bots)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	if _, err := db.Exec(`CREATE TRIGGER fail_logs BEFORE INSERT ON bot_logs BEGIN SELECT RAISE(ABORT, 'test'); END`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if s, b := postLogs(t, srv, code, infoLines(200)); s != 500 {
			t.Fatalf("failing insert %d: %d %s", i, s, b)
		}
	}
	if _, err := db.Exec(`DROP TRIGGER fail_logs`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if s, b := postLogs(t, srv, code, infoLines(200)); s != 204 {
			t.Fatalf("batch %d after the failures: %d %s", i, s, b)
		}
	}
}

// getLogs returns the GET /api/me/bot/logs lines for a session and query string.
func getLogs(t *testing.T, srv *httptest.Server, token, query string) []accounts.BotLog {
	t.Helper()
	s, body := request(t, srv, "GET", "/api/me/bot/logs"+query, token, "", nil)
	var res struct {
		Lines []accounts.BotLog `json:"lines"`
	}
	if err := json.Unmarshal([]byte(body), &res); s != 200 || err != nil || res.Lines == nil {
		t.Fatalf("GET logs%s: %d %s", query, s, body)
	}
	return res.Lines
}

func msgs(lines []accounts.BotLog) []string {
	out := []string{}
	for _, l := range lines {
		out = append(out, l.Msg)
	}
	return out
}

func TestBotLogs_GetFilters(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	home, homeID := newBotCode(t, srv, token, "Home PC")
	vps, _ := newBotCode(t, srv, token, "VPS Praha")
	otherToken := newUser(t, svc, "o@example.com", accounts.RoleUser)
	otherCode, _ := newBotCode(t, srv, otherToken, "Other")

	post := func(code string, lines ...string) {
		t.Helper()
		if s, b := postLogs(t, srv, code, `{"lines":[`+strings.Join(lines, ",")+`]}`); s != 204 {
			t.Fatalf("post: %d %s", s, b)
		}
	}
	base := time.Now().Add(-time.Hour).UnixMilli()
	post(home,
		logLine(base+1000, "info", "", "", "agent", "agent connected"),
		logLine(base+2000, "info", "fortuna-1", "c81e55a0deadbeef", "navigate", "leg started"),
		logLine(base+3000, "warn", "fortuna-1", "c81e55a0deadbeef", "checkOdds", "Odds dropped"))
	post(vps, logLine(base+4000, "error", "tipsport-1", "", "placeBet", "outcome unknown"))
	post(otherCode, logLine(base+5000, "error", "", "", "", "not yours"))

	all := getLogs(t, srv, token, "")
	if got := msgs(all); !slices.Equal(got, []string{"outcome unknown", "Odds dropped", "leg started", "agent connected"}) {
		t.Fatalf("all, newest first: %v", got)
	}
	if l := all[1]; l.AgentID != homeID || l.Agent != "Home PC" || l.TS != base+3000 || l.Level != "warn" || l.WindowID != "fortuna-1" ||
		l.OrderID != "c81e55a0deadbeef" || l.Step != "checkOdds" {
		t.Fatalf("line fields: %+v", l)
	}
	first, third := all[3].ID, all[1].ID
	cases := map[string][]string{
		fmt.Sprintf("?agent=%d", homeID):        {"Odds dropped", "leg started", "agent connected"},
		"?window=fortuna-1":                     {"Odds dropped", "leg started"},
		"?order=c81e55a0deadbeef":               {"Odds dropped", "leg started"},
		"?problems=1":                           {"outcome unknown", "Odds dropped"},
		"?q=odds%20DROPPED":                     {"Odds dropped"},
		"?q=placebet":                           {"outcome unknown"},
		fmt.Sprintf("?after=%d", first):         {"leg started", "Odds dropped", "outcome unknown"},
		fmt.Sprintf("?after=%d&limit=1", first): {"leg started"},
		fmt.Sprintf("?before=%d", third):        {"leg started", "agent connected"},
		"?limit=2":                              {"outcome unknown", "Odds dropped"},
		"?problems=1&window=fortuna-1":          {"Odds dropped"},
	}
	for q, want := range cases {
		if got := msgs(getLogs(t, srv, token, q)); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", q, got, want)
		}
	}
	for _, q := range []string{"?agent=x", "?agent=0", "?after=-1", "?before=abc", "?after=1&before=9", "?limit=0", "?limit=501",
		"?q=" + strings.Repeat("q", 101), "?window=" + strings.Repeat("w", 41), "?order=" + strings.Repeat("o", 33)} {
		if s, b := request(t, srv, "GET", "/api/me/bot/logs"+q, token, "", nil); s != 400 || !strings.Contains(b, `"bad_query"`) {
			t.Errorf("%s: %d %s, want 400 bad_query", q, s, b)
		}
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/logs", "", "", bearer(home)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("logs with a bot code: %d %s", s, b)
	}
	if _, raw := request(t, srv, "GET", "/api/me/bot/logs", token, "", nil); strings.Contains(raw, `"detail"`) {
		t.Fatalf("lines without detail must not carry the key: %s", raw)
	}
	if got := msgs(getLogs(t, srv, otherToken, "")); !slices.Equal(got, []string{"not yours"}) {
		t.Fatalf("other user sees: %v", got)
	}
}

func TestBotLogs_Text(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	bots.now = (&botClock{t: time.Date(2026, 11, 12, 18, 10, 0, 0, time.UTC)}).now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	home, _ := newBotCode(t, srv, token, "Home PC")
	ts := time.Date(2026, 11, 12, 18, 4, 5, 123_000_000, time.UTC).UnixMilli()
	body := `{"lines":[` +
		logLine(ts, "info", "", "", "agent", "agent connected") + "," +
		logLine(ts+1, "warn", "fortuna-1", "c81e55a0deadbeef", "checkOdds", "odds dropped\nsecond line") + "]}"
	if s, b := postLogs(t, srv, home, body); s != 204 {
		t.Fatalf("post: %d %s", s, b)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/me/bot/logs.txt?problems=0&limit=1", nil)
	req.Header.Set("Cookie", accounts.CookieName+"="+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="surebetter-bot-logs.txt"` {
		t.Fatalf("logs.txt: %d %v", resp.StatusCode, resp.Header)
	}
	// Oldest first, limit ignored.
	want := "2026-11-12T18:04:05.123Z  Home PC  -  agent  info  agent connected\n" +
		"2026-11-12T18:04:05.124Z  Home PC · fortuna-1  c81e55a0deadbeef  checkOdds  warn  odds dropped second line\n"
	if string(text) != want {
		t.Fatalf("logs.txt:\n%s\nwant:\n%s", text, want)
	}
	// Paged reads give the same file.
	defer func(n int) { logsTextPage = n }(logsTextPage)
	logsTextPage = 1
	if s, b := request(t, srv, "GET", "/api/me/bot/logs.txt", token, "", nil); s != 200 || b != want {
		t.Fatalf("logs.txt read one row per query: %d %q", s, b)
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/logs.txt?problems=1", token, "", nil); s != 200 ||
		b != "2026-11-12T18:04:05.124Z  Home PC · fortuna-1  c81e55a0deadbeef  checkOdds  warn  odds dropped second line\n" {
		t.Fatalf("logs.txt?problems=1: %d %q", s, b)
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/logs.txt?agent=x", token, "", nil); s != 400 || !strings.Contains(b, `"bad_query"`) {
		t.Fatalf("bad query: %d %s", s, b)
	}
}

// A q search reads only the last 7 days, so lines the hourly prune has not reached yet are not scanned.
func TestBotLogs_QueryReadsOnlySevenDays(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	_, codeID := newBotCode(t, srv, token, "Home PC")
	uid := userIDOf(t, svc, token)
	if err := svc.AddBotLogs(uid, codeID, []accounts.BotLogLine{
		{TS: clk.now().Add(-botRetention - time.Minute).UnixMilli(), Level: "info", Msg: "odds old"},
		{TS: clk.now().Add(-botRetention + time.Minute).UnixMilli(), Level: "info", Msg: "odds fresh"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := msgs(getLogs(t, srv, token, "?q=odds")); !slices.Equal(got, []string{"odds fresh"}) {
		t.Fatalf("q: %v", got)
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/logs.txt?q=odds", token, "", nil); s != 200 || strings.Contains(b, "odds old") || !strings.Contains(b, "odds fresh") {
		t.Fatalf("logs.txt?q: %d %q", s, b)
	}
	if got := msgs(getLogs(t, srv, token, "")); len(got) != 2 {
		t.Fatalf("without q every stored line is listed: %v", got)
	}
}

// Every logs.txt field is one line: a line break in any of them becomes a space.
func TestBotLogs_TextFieldsHaveNoLineBreaks(t *testing.T) {
	l := accounts.BotLog{TS: logTS, Agent: "Home\nPC", WindowID: "fortuna\r1", OrderID: "c81e\r\n55a0", Step: "check\nOdds",
		Level: "warn\n", Msg: "odds\ndropped"}
	want := "2026-11-12T12:00:00.000Z  Home PC · fortuna 1  c81e 55a0  check Odds  warn   odds dropped"
	if got := logText(l); got != want {
		t.Fatalf("logText:\n got %q\nwant %q", got, want)
	}
}

// A full batch can exceed 1 MiB as JSON (escaped multi-byte messages and details), so the body cap is 2 MiB.
func TestBotLogs_BodyUpTo2MiB(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	fixClock(bots)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	line := logLine(logTS, "info", "", "", "", "x")
	padded := func(size int) string { // whitespace inside the object, so the decoder must read all of it
		return `{"lines":[` + strings.Repeat(" ", size-len(line)-len(`{"lines":[]}`)) + line + `]}`
	}
	if s, b := postLogs(t, srv, code, padded(2<<20)); s != 204 {
		t.Fatalf("2 MiB body: %d %s", s, b)
	}
	if s, b := postLogs(t, srv, code, padded(2<<20+1)); s != 400 || !strings.Contains(b, `"bad_logs"`) {
		t.Fatalf("2 MiB + 1 byte: %d %s", s, b)
	}
}

// Rule 1's stuck-mode lines (contract §4: detail with A, B and the choice) keep their numbers and
// the choice; 0 is a value (a cashout of 0 Kč), not "absent".
func TestBotLogs_DetailKeepsTheStuckDecision(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	fixClock(bots)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	body := `{"lines":[{"ts":` + fmt.Sprint(logTS) + `,"level":"warn","windowId":"betano-1","orderId":"c81e55a0deadbeef","step":"stuck",` +
		`"msg":"stuck · A cash out 955 Kč (loss 45) · B Under 1 030 Kč at 1.90 (loss 63) · cash out",` +
		`"detail":{"cashout":955,"underOdds":1.9,"underStake":1030,"lossA":45,"lossB":63.5,"choice":"cashout","other":1}},` +
		`{"ts":` + fmt.Sprint(logTS) + `,"level":"info","step":"stuck","msg":"stuck · nothing yet","detail":{"cashout":0,"lossA":0,"choice":"wait"}}]}`
	if s, b := postLogs(t, srv, code, body); s != 204 {
		t.Fatalf("post: %d %s", s, b)
	}
	lines, err := svc.QueryBotLogs(userIDOf(t, svc, token), accounts.BotLogFilter{})
	if err != nil || len(lines) != 2 {
		t.Fatalf("lines: %+v %v", lines, err)
	}
	if got := string(lines[1].Detail); got != `{"cashout":955,"underOdds":1.9,"underStake":1030,"lossA":45,"lossB":63.5,"choice":"cashout"}` {
		t.Fatalf("stuck detail: %s", got)
	}
	if got := string(lines[0].Detail); got != `{"cashout":0,"lossA":0,"choice":"wait"}` {
		t.Fatalf("zero values: %s", got)
	}
	if s, b := postLogs(t, srv, code, `{"lines":[{"ts":`+fmt.Sprint(logTS)+`,"level":"info","msg":"x","detail":{"lossB":"63"}}]}`); s != 400 || !strings.Contains(b, `"bad_logs"`) {
		t.Fatalf("a number as a string: %d %s", s, b)
	}
}
