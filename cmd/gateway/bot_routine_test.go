package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// routineVectors is testdata/routine-vectors.json, a byte-for-byte copy of the shared vectors
// (surebetter-common/docs/superpowers/plans/2026-11-14-routine-vectors.json).
type routineVectors struct {
	Base    json.RawMessage `json:"base"`
	Resolve []struct {
		Name      string          `json:"name"`
		HumanMode bool            `json:"humanMode"`
		Routine   json.RawMessage `json:"routine"`
		Window    string          `json:"window"`
		At        string          `json:"at"`
		Expect    resolved        `json:"expect"`
	} `json:"resolve"`
	Valid   []routineCase `json:"valid"`
	Invalid []routineCase `json:"invalid"`
}

type routineCase struct {
	Name    string          `json:"name"`
	Routine json.RawMessage `json:"routine"`
}

// resolved is the resolution of a routine for one window at one instant (contract §2).
type resolved struct {
	Activity string `json:"activity"`
	Sport    string `json:"sport,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func loadRoutineVectors(t *testing.T) routineVectors {
	t.Helper()
	b, err := os.ReadFile("testdata/routine-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v routineVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// routineJSON resolves a vector's routine: "base" means the base object.
func (v routineVectors) routineJSON(raw json.RawMessage) json.RawMessage {
	if string(raw) == `"base"` {
		return v.Base
	}
	return raw
}

// resolveRoutine is contract §2's resolution. The gateway never resolves a routine (the agent does);
// this test-only copy runs the vectors' resolve cases against the routine the gateway parses.
func resolveRoutine(humanMode bool, r *routine, windowID string, t time.Time) resolved {
	if !humanMode || r == nil {
		return resolved{Activity: "open"}
	}
	wall := t.In(prague)
	day := int(wall.Weekday()+6)%7 + 1 // 1 Mon … 7 Sun
	prev := (day+5)%7 + 1
	now := wall.Hour()*60 + wall.Minute()
	in := func(days []int, d int) bool {
		for _, x := range days {
			if x == d {
				return true
			}
		}
		return false
	}
	for _, s := range r.Sleep {
		from, _ := clockMinutes(s.From)
		to, _ := clockMinutes(s.To)
		if to > from {
			if in(s.Days, day) && now >= from && now < to {
				return resolved{Activity: "inactive", Reason: "sleep"}
			}
			continue
		}
		if (in(s.Days, day) && now >= from) || (in(s.Days, prev) && now < to) {
			return resolved{Activity: "inactive", Reason: "sleep"}
		}
	}
	for _, b := range r.Windows[windowID].Blocks {
		from, _ := clockMinutes(b.From)
		to, _ := clockMinutes(b.To)
		if in(b.Days, day) && now >= from && now < to {
			out := resolved{Activity: b.Activity}
			if b.Sport != nil {
				out.Sport = *b.Sport
			}
			if b.Activity == "inactive" {
				out.Reason = "inactive"
			}
			return out
		}
	}
	return resolved{Activity: "open"}
}

func TestRoutine_Vectors(t *testing.T) {
	v := loadRoutineVectors(t)
	if len(v.Valid) == 0 || len(v.Invalid) == 0 || len(v.Resolve) == 0 {
		t.Fatal("empty vectors")
	}
	for _, c := range v.Valid {
		if _, ok := routineOf(v.routineJSON(c.Routine)); !ok {
			t.Errorf("valid %q refused", c.Name)
		}
	}
	for _, c := range v.Invalid {
		if _, ok := routineOf(v.routineJSON(c.Routine)); ok {
			t.Errorf("invalid %q accepted", c.Name)
		}
	}
	for _, c := range v.Resolve {
		var r *routine
		if string(c.Routine) != "null" {
			var ok bool
			if r, ok = routineOf(v.routineJSON(c.Routine)); !ok {
				t.Fatalf("%s: routine refused", c.Name)
			}
		}
		at, err := time.Parse(time.RFC3339, c.At)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got := resolveRoutine(c.HumanMode, r, c.Window, at); got != c.Expect {
			t.Errorf("resolve %q: %+v, want %+v", c.Name, got, c.Expect)
		}
	}
}

// minimalRoutine is a valid routine with sleep, windows and the trigger replaced by the given JSON
// (each "" = the default: [], {}, on 2–5).
func minimalRoutine(sleep, windows, trigger string) string {
	if sleep == "" {
		sleep = `[]`
	}
	if windows == "" {
		windows = `{}`
	}
	if trigger == "" {
		trigger = `{"on":true,"minMin":2,"maxMin":5}`
	}
	return `{"version":1,"tz":"Europe/Prague","sleep":` + sleep + `,"windows":` + windows + `,"triggers":{"afterBetCheckMyBets":` + trigger + `}}`
}

// blocks is a windows object with one window w1 holding the given blocks.
func blocks(bs ...string) string { return `{"w1":{"blocks":[` + strings.Join(bs, ",") + `]}}` }

func repeatJSON(n int, f func(i int) string) string {
	out := make([]string, n)
	for i := range out {
		out[i] = f(i)
	}
	return strings.Join(out, ",")
}

func TestRoutine_Limits(t *testing.T) {
	block := func(fields string) string {
		return `{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"mybets"` + fields + `}`
	}
	sleep := func(from, to string) string { return `[{"days":[1],"from":"` + from + `","to":"` + to + `"}]` }
	manyBlocks := func(n int) string {
		return `{"w1":{"blocks":[` + repeatJSON(n, func(i int) string {
			return fmt.Sprintf(`{"id":"b%d","days":[%d],"from":"%02d:%02d","to":"%02d:%02d","activity":"open"}`,
				i, i%7+1, i/7/2, i/7%2*30, i/7/2, i/7%2*30+15)
		}) + `]}}`
	}
	manyWindows := func(n int) string {
		return `{` + repeatJSON(n, func(i int) string { return fmt.Sprintf(`"w-%d":{"blocks":[]}`, i) }) + `}`
	}
	manySleep := func(n int) string {
		return `[` + repeatJSON(n, func(int) string { return `{"days":[1],"from":"23:00","to":"07:00"}` }) + `]`
	}
	valid := map[string]string{
		"sleep to 24:00":        minimalRoutine(sleep("22:00", "24:00"), "", ""),
		"sleep from 00:00":      minimalRoutine(sleep("00:00", "06:00"), "", ""),
		"sleep all day":         minimalRoutine(sleep("00:00", "24:00"), "", ""),
		"block 00:00–24:00":     minimalRoutine("", `{"w1":{"blocks":[{"id":"a","days":[7],"from":"00:00","to":"24:00","activity":"open"}]}}`, ""),
		"20 sleep periods":      minimalRoutine(manySleep(20), "", ""),
		"32 windows":            minimalRoutine("", manyWindows(32), ""),
		"200 blocks":            minimalRoutine("", manyBlocks(200), ""),
		"sport at 40":           minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":"`+strings.Repeat("s", 40)+`"}`), ""),
		"sport with _ and -":    minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"prematch","sport":"ice_hockey-2"}`), ""),
		"block id 16":           minimalRoutine("", blocks(`{"id":"`+strings.Repeat("a", 16)+`","days":[1],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"window id 40":          minimalRoutine("", `{"`+strings.Repeat("w", 40)+`":{"blocks":[]}}`, ""),
		"trigger 60–60":         minimalRoutine("", "", `{"on":false,"minMin":60,"maxMin":60}`),
		"days 1–7 in any order": minimalRoutine("", blocks(`{"id":"a","days":[7,1,3,2,6,5,4],"from":"08:00","to":"09:00","activity":"open"}`), ""),
	}
	for name, body := range valid {
		if _, ok := routineOf([]byte(body)); !ok {
			t.Errorf("%s: refused", name)
		}
	}
	invalid := map[string]string{
		"not JSON":              `{"version":1`,
		"an array":              `[]`,
		"null":                  `null`,
		"21 sleep periods":      minimalRoutine(manySleep(21), "", ""),
		"33 windows":            minimalRoutine("", manyWindows(33), ""),
		"201 blocks":            minimalRoutine("", manyBlocks(201), ""),
		"sport over 40":         minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":"`+strings.Repeat("s", 41)+`"}`), ""),
		"sport uppercase":       minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":"Football"}`), ""),
		"sport empty":           minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":""}`), ""),
		"sport null on live":    minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":null}`), ""),
		"sport on open":         minimalRoutine("", blocks(block(`,"sport":"any"`)), ""),
		"block id 17":           minimalRoutine("", blocks(`{"id":"`+strings.Repeat("a", 17)+`","days":[1],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"block id with dash":    minimalRoutine("", blocks(`{"id":"a-1","days":[1],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"block from 24:00":      minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"24:00","to":"24:00","activity":"open"}`), ""),
		"minutes 60":            minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:60","to":"09:00","activity":"open"}`), ""),
		"hour 25":               minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"25:00","activity":"open"}`), ""),
		"24:01":                 minimalRoutine("", blocks(`{"id":"a","days":[1],"from":"08:00","to":"24:01","activity":"open"}`), ""),
		"day 8":                 minimalRoutine("", blocks(`{"id":"a","days":[8],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"day 1.5":               minimalRoutine("", blocks(`{"id":"a","days":[1.5],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"block without days":    minimalRoutine("", blocks(`{"id":"a","from":"08:00","to":"09:00","activity":"open"}`), ""),
		"block without id":      minimalRoutine("", blocks(`{"days":[1],"from":"08:00","to":"09:00","activity":"open"}`), ""),
		"blocks null":           minimalRoutine("", `{"w1":{"blocks":null}}`, ""),
		"window without blocks": minimalRoutine("", `{"w1":{}}`, ""),
		"window id 41":          minimalRoutine("", `{"`+strings.Repeat("w", 41)+`":{"blocks":[]}}`, ""),
		"sleep from 24:00":      minimalRoutine(sleep("24:00", "06:00"), "", ""),
		"sleep 00:00 to 00:00":  minimalRoutine(sleep("00:00", "00:00"), "", ""),
		"sleep without days":    minimalRoutine(`[{"from":"23:00","to":"07:00"}]`, "", ""),
		"sleep day repeated":    minimalRoutine(`[{"days":[2,2],"from":"23:00","to":"07:00"}]`, "", ""),
		"sleep null":            strings.Replace(minimalRoutine("", "", ""), `"sleep":[]`, `"sleep":null`, 1),
		"no sleep":              strings.Replace(minimalRoutine("", "", ""), `"sleep":[],`, ``, 1),
		"no windows":            strings.Replace(minimalRoutine("", "", ""), `"windows":{},`, ``, 1),
		"windows an array":      minimalRoutine("", `[]`, ""),
		"no version":            strings.Replace(minimalRoutine("", "", ""), `"version":1,`, ``, 1),
		"no tz":                 strings.Replace(minimalRoutine("", "", ""), `"tz":"Europe/Prague",`, ``, 1),
		"trigger without on":    minimalRoutine("", "", `{"minMin":2,"maxMin":5}`),
		"trigger without min":   minimalRoutine("", "", `{"on":true,"maxMin":5}`),
		"trigger without max":   minimalRoutine("", "", `{"on":true,"minMin":2}`),
		"minMin 0":              minimalRoutine("", "", `{"on":true,"minMin":0,"maxMin":5}`),
		"minMin 2.5":            minimalRoutine("", "", `{"on":true,"minMin":2.5,"maxMin":5}`),
		"trigger null":          strings.Replace(minimalRoutine("", "", ""), `"afterBetCheckMyBets":{"on":true,"minMin":2,"maxMin":5}`, `"afterBetCheckMyBets":null`, 1),
		"triggers empty":        strings.Replace(minimalRoutine("", "", ""), `{"afterBetCheckMyBets":{"on":true,"minMin":2,"maxMin":5}}`, `{}`, 1),
		"overlap across days":   minimalRoutine("", blocks(block(""), `{"id":"b","days":[3,1],"from":"07:00","to":"08:01","activity":"open"}`), ""),
		"overlap contained":     minimalRoutine("", blocks(block(""), `{"id":"b","days":[1],"from":"08:15","to":"08:30","activity":"open"}`), ""),
	}
	for name, body := range invalid {
		if _, ok := routineOf([]byte(body)); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The stored routine is the parsed one re-encoded: only the known keys, sport only where it is set.
func TestRoutine_Encoding(t *testing.T) {
	in := `{"tz":"Europe/Prague","version":1,"extra":true,"sleep":[{"days":[1],"from":"23:00","to":"07:00","x":1}],` +
		`"windows":{"w2":{"blocks":[]},"w1":{"blocks":[{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":"any"},` +
		`{"id":"b","days":[2],"from":"08:00","to":"09:00","activity":"open"}]}},"triggers":{"afterBetCheckMyBets":{"on":true,"minMin":2,"maxMin":5}}}`
	r, ok := routineOf([]byte(in))
	if !ok {
		t.Fatal("refused")
	}
	want := `{"version":1,"tz":"Europe/Prague","sleep":[{"days":[1],"from":"23:00","to":"07:00"}],` +
		`"windows":{"w1":{"blocks":[{"id":"a","days":[1],"from":"08:00","to":"09:00","activity":"live","sport":"any"},` +
		`{"id":"b","days":[2],"from":"08:00","to":"09:00","activity":"open"}]},"w2":{"blocks":[]}},"triggers":{"afterBetCheckMyBets":{"on":true,"minMin":2,"maxMin":5}}}`
	if got := mustJSON(r); got != want {
		t.Fatalf("encoded:\n got %s\nwant %s", got, want)
	}
}

func routinePath(agent int64) string { return fmt.Sprintf("/api/me/bot/agents/%d/routine", agent) }

// routineRequest is request with the body's trailing newline trimmed.
func routineRequest(t *testing.T, srv *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()
	s, b := request(t, srv, method, path, token, body, nil)
	return s, strings.TrimSpace(b)
}

// tipBetIdleP4 is a protocol-4 status with tipsport-1 and betano-1 idle.
var tipBetIdleP4 = strings.Replace(tipBetIdle, `"proto":2`, `"proto":4`, 1)

func TestBotRoutine_GetPut(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	_, neverID := newBotCode(t, srv, token, "Never connected")
	_, otherID := newBotCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "Other")
	startBot(t, srv, code, app)
	v := loadRoutineVectors(t)

	if s, b := routineRequest(t, srv, "GET", routinePath(id), token, ""); s != 200 || b != `{"humanMode":false,"routine":null}` {
		t.Fatalf("new agent: %d %s", s, b)
	}
	r, _ := routineOf(v.Base)
	want := `{"humanMode":true,"routine":` + mustJSON(r) + `}`
	if s, b := routineRequest(t, srv, "PUT", routinePath(id), token, `{"humanMode":true,"routine":`+string(v.Base)+`}`); s != 200 || b != want {
		t.Fatalf("PUT base: %d %s", s, b)
	}
	if s, b := routineRequest(t, srv, "GET", routinePath(id), token, ""); s != 200 || b != want {
		t.Fatalf("GET after PUT: %d %s", s, b)
	}
	expectBot(t, app, "after a routine PUT (another open Fleet re-reads humanMode)")
	for _, c := range v.Valid {
		if s, b := routineRequest(t, srv, "PUT", routinePath(id), token, `{"humanMode":false,"routine":`+string(v.routineJSON(c.Routine))+`}`); s != 200 {
			t.Errorf("valid %q: %d %s", c.Name, s, b)
		}
	}
	// A null routine is stored as none; the switch stays as sent.
	if s, b := routineRequest(t, srv, "PUT", routinePath(id), token, `{"humanMode":true,"routine":null}`); s != 200 || b != `{"humanMode":true,"routine":null}` {
		t.Fatalf("PUT null: %d %s", s, b)
	}
	if s, b := routineRequest(t, srv, "GET", routinePath(id), token, ""); b != `{"humanMode":true,"routine":null}` {
		t.Fatalf("GET after null: %d %s", s, b)
	}

	bad := map[string]string{
		"not JSON":                            `{"humanMode":true`,
		"no humanMode":                        `{"routine":null}`,
		"humanMode null":                      `{"humanMode":null,"routine":null}`,
		"humanMode string":                    `{"humanMode":"true","routine":null}`,
		"no routine":                          `{"humanMode":true}`,
		"routine an array":                    `{"humanMode":true,"routine":[]}`,
		"over 256 KiB":                        `{"humanMode":true,"routine":null,"pad":"` + strings.Repeat("x", 256<<10) + `"}`,
		"trailing garbage":                    `{"humanMode":true,"routine":null}garbage`,
		"a second value":                      `{"humanMode":true,"routine":null} {}`,
		"padded over 256 KiB with whitespace": `{"humanMode":true,"routine":null}` + strings.Repeat(" ", 256<<10),
	}
	for _, c := range v.Invalid {
		bad["invalid vector "+c.Name] = `{"humanMode":true,"routine":` + string(c.Routine) + `}`
	}
	for name, body := range bad {
		if s, b := request(t, srv, "PUT", routinePath(id), token, body, nil); s != 400 || !strings.Contains(b, `"bad_routine"`) {
			t.Errorf("%s: %d %.100s, want 400 bad_routine", name, s, b)
		}
	}
	if _, b := routineRequest(t, srv, "GET", routinePath(id), token, ""); b != `{"humanMode":true,"routine":null}` {
		t.Fatalf("changed by a refused PUT: %s", b)
	}
	// At 256 KiB: accepted (the limit is the whole body).
	pad := 256<<10 - len(`{"humanMode":true,"routine":null,"pad":""}`)
	if s, b := request(t, srv, "PUT", routinePath(id), token, `{"humanMode":true,"routine":null,"pad":"`+strings.Repeat("x", pad)+`"}`, nil); s != 200 {
		t.Fatalf("256 KiB: %d %.100s", s, b)
	}

	// Not the user's agent, a code that never connected, or no number: 404 unknown_agent, also with
	// a bad body (the agent is checked first).
	for _, path := range []string{routinePath(otherID), routinePath(neverID), "/api/me/bot/agents/home/routine"} {
		for _, method := range []string{"GET", "PUT"} {
			if s, b := request(t, srv, method, path, token, `{"humanMode":false,"routine":null}`, nil); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
				t.Errorf("%s %s: %d %s, want 404 unknown_agent", method, path, s, b)
			}
		}
		if s, b := request(t, srv, "PUT", path, token, `{"humanMode":"x"}`, nil); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
			t.Errorf("PUT %s with a bad body: %d %s, want 404 unknown_agent", path, s, b)
		}
	}
	// A session only; PUT behind CSRF.
	if s, b := request(t, srv, "GET", routinePath(id), "", "", bearer(code)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("GET with a bot code: %d %s", s, b)
	}
	if s, b := request(t, srv, "PUT", routinePath(id), token, `{"humanMode":false,"routine":null}`, map[string]string{"Origin": "http://evil.example"}); s != 403 ||
		!strings.Contains(b, `"bad_origin"`) {
		t.Fatalf("cross-site PUT: %d %s", s, b)
	}
}

// {"type":"routine"} goes to the agent's bot socket after a PUT, only when it speaks protocol 4.
func TestBotRoutine_MessageOnlyToProto4(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	oldCode, oldID := newBotCode(t, srv, token, "Old PC")
	code, id := newBotCode(t, srv, token, "Home PC")
	old := startAgentWith(t, srv, oldCode, tipBetIdleP3, app)
	bot := startAgentWith(t, srv, code, tipBetIdleP4, app)

	body := `{"humanMode":true,"routine":null}`
	if s, b := request(t, srv, "PUT", routinePath(oldID), token, body, nil); s != 200 {
		t.Fatalf("PUT old: %d %s", s, b)
	}
	if s, b := request(t, srv, "PUT", routinePath(id), token, body, nil); s != 200 {
		t.Fatalf("PUT: %d %s", s, b)
	}
	expectBot(t, app, "after the first PUT")
	expectBot(t, app, "after the second PUT")
	if got := readType(t, bot); got != "routine" {
		t.Fatalf("protocol-4 agent got %s, want routine", got)
	}
	// expectNoMessage closes the socket it waited on (nhooyr closes a connection whose read is
	// cancelled), and an agent's close tells the app: the app goes first.
	expectNoMessage(t, app, "app")
	expectNoMessage(t, old, "protocol-3 agent")
	expectNoMessage(t, bot, "protocol-4 agent after its one message")
}

// GET /api/me/bot/routine is the calling agent's own routine plus the ids of its orders the user
// marked checked that ended within the last 30 days.
func TestBotRoutine_Own(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := &botClock{t: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, svc, token)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	vps, vpsID := newBotCode(t, srv, token, "VPS")
	startAgentWith(t, srv, code, tipBetIdleP4, app)
	startAgentWith(t, srv, vps, tipBetIdleP4, app)

	own := func(code string) (int, string) {
		t.Helper()
		s, b := request(t, srv, "GET", "/api/me/bot/routine", "", "", bearer(code))
		return s, strings.TrimSpace(b)
	}
	if s, b := own(code); s != 200 || b != `{"humanMode":false,"routine":null,"checked":[]}` {
		t.Fatalf("new agent: %d %s", s, b)
	}
	v := loadRoutineVectors(t)
	r, _ := routineOf(v.Base)
	if s, b := request(t, srv, "PUT", routinePath(id), token, `{"humanMode":true,"routine":`+string(v.Base)+`}`, nil); s != 200 {
		t.Fatalf("PUT: %d %s", s, b)
	}
	now := clk.now().UnixMilli()
	day := int64(24 * 60 * 60 * 1000)
	ended := func(orderID string, codeID, endedAt int64, checked bool) {
		t.Helper()
		if err := svc.InsertBotOrder(accounts.BotOrderRow{ID: orderID, UserID: uid, ArbKey: orderID, State: "waiting", InfoJSON: "{}", LegsJSON: "[]",
			CreatedAt: endedAt - 1000}); err != nil {
			t.Fatal(err)
		}
		if err := svc.MarkBotOrderSent(orderID, codeID, false, "[]", endedAt-500); err != nil {
			t.Fatal(err)
		}
		if err := svc.EndBotOrder(orderID, "outcome_unknown", "", "[]", 0, endedAt); err != nil {
			t.Fatal(err)
		}
		if checked {
			if _, err := svc.MarkBotOrderChecked(uid, orderID, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	ended("00000000000000a1", id, now-day, true)
	ended("00000000000000a2", id, now-30*day, true)   // the 30-day edge: in
	ended("00000000000000a3", id, now-30*day-1, true) // just older: out
	ended("00000000000000a4", id, now-day, false)     // not checked
	ended("00000000000000a5", vpsID, now-day, true)   // another agent's
	want := `{"humanMode":true,"routine":` + mustJSON(r) + `,"checked":["00000000000000a1","00000000000000a2"]}`
	if s, b := own(code); s != 200 || b != want {
		t.Fatalf("own:\n got %d %s\nwant %s", s, b, want)
	}
	if s, b := own(vps); s != 200 || b != `{"humanMode":false,"routine":null,"checked":["00000000000000a5"]}` {
		t.Fatalf("vps: %d %s", s, b)
	}
	// A bot code only.
	if s, b := request(t, srv, "GET", "/api/me/bot/routine", token, "", nil); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("with a session: %d %s", s, b)
	}
}
