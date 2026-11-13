package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func limitIndexPath(agent int64) string {
	return fmt.Sprintf("/api/me/bot/agents/%d/limit-index", agent)
}

// limitIndexRequest is request with the body's trailing newline trimmed.
func limitIndexRequest(t *testing.T, srv *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()
	s, b := request(t, srv, method, path, token, body, nil)
	return s, strings.TrimSpace(b)
}

// putLimitIndex replaces agent's limit index with entries (a JSON array) and expects 200.
func putLimitIndex(t *testing.T, srv *httptest.Server, token string, agent int64, entries string) {
	t.Helper()
	if s, b := request(t, srv, "PUT", limitIndexPath(agent), token, `{"entries":`+entries+`}`, nil); s != 200 {
		t.Fatalf("PUT limit index: %d %s", s, b)
	}
}

// betanoStrictCorners is a limit index with betano Strict on football corners totals.
const betanoStrictCorners = `[{"bookmaker":"betano","sport":"football","marketTypes":["TOTAL_CORNERS","TOTAL_CORNERS_HALF_1"],"level":2}]`

func TestBotLimitIndex_GetPutAndValidation(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	_, neverID := newBotCode(t, srv, token, "Never connected")
	_, otherID := newBotCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "Other")
	startBot(t, srv, code, app)

	if s, b := limitIndexRequest(t, srv, "GET", limitIndexPath(id), token, ""); s != 200 || b != `{"entries":[]}` {
		t.Fatalf("new agent: %d %s", s, b)
	}
	// Stored in a fixed shape: marketTypes never null, level always present.
	in := `[{"bookmaker":"betano","sport":"tennis","marketTypes":["GAME_HANDICAP","GAME_HANDICAP_SET_1"],"level":2},` +
		`{"bookmaker":"tipsport","sport":"tennis","marketTypes":null,"level":1},{"bookmaker":"fortuna","sport":"football"}]`
	want := `{"entries":[{"bookmaker":"betano","sport":"tennis","marketTypes":["GAME_HANDICAP","GAME_HANDICAP_SET_1"],"level":2},` +
		`{"bookmaker":"tipsport","sport":"tennis","marketTypes":[],"level":1},{"bookmaker":"fortuna","sport":"football","marketTypes":[],"level":0}]}`
	if s, b := limitIndexRequest(t, srv, "PUT", limitIndexPath(id), token, `{"entries":`+in+`}`); s != 200 || b != want {
		t.Fatalf("PUT: %d %s", s, b)
	}
	if s, b := limitIndexRequest(t, srv, "GET", limitIndexPath(id), token, ""); s != 200 || b != want {
		t.Fatalf("GET after PUT: %d %s", s, b)
	}
	expectNoMessage(t, app, "app (a limit index change sends nothing)")

	entry := func(fields string) string { return `{"entries":[{` + fields + `}]}` }
	types := func(n, size int) string {
		ts := make([]string, n)
		for i := range ts {
			ts[i] = `"` + strings.Repeat("M", size) + `"`
		}
		return `[` + strings.Join(ts, ",") + `]`
	}
	many := func(n int) string {
		es := make([]string, n)
		for i := range es {
			es[i] = `{"bookmaker":"betano","sport":"tennis","marketTypes":["GAME_HANDICAP"],"level":1}`
		}
		return `{"entries":[` + strings.Join(es, ",") + `]}`
	}
	for name, body := range map[string]string{
		"not JSON":            `{"entries":`,
		"no entries":          `{}`,
		"entries null":        `{"entries":null}`,
		"entries an object":   `{"entries":{}}`,
		"over 1000 entries":   many(1001),
		"bookmaker empty":     entry(`"bookmaker":"","sport":"tennis","level":1`),
		"bookmaker uppercase": entry(`"bookmaker":"Betano","sport":"tennis","level":1`),
		"bookmaker over 40":   entry(`"bookmaker":"` + strings.Repeat("b", 41) + `","sport":"tennis","level":1`),
		"sport over 40":       entry(`"bookmaker":"betano","sport":"` + strings.Repeat("s", 41) + `","level":1`),
		"over 50 types":       entry(`"bookmaker":"betano","sport":"tennis","marketTypes":` + types(51, 10) + `,"level":1`),
		"type over 64":        entry(`"bookmaker":"betano","sport":"tennis","marketTypes":` + types(1, 65) + `,"level":1`),
		"level 3":             entry(`"bookmaker":"betano","sport":"tennis","level":3`),
		"level -1":            entry(`"bookmaker":"betano","sport":"tennis","level":-1`),
		"level 1.5":           entry(`"bookmaker":"betano","sport":"tennis","level":1.5`),
		"level a string":      entry(`"bookmaker":"betano","sport":"tennis","level":"2"`),
	} {
		if s, b := request(t, srv, "PUT", limitIndexPath(id), token, body, nil); s != 400 || !strings.Contains(b, `"bad_limit_index"`) {
			t.Errorf("%s: %d %s, want 400 bad_limit_index", name, s, b)
		}
	}
	// At the limits: valid.
	if s, b := request(t, srv, "PUT", limitIndexPath(id), token, many(1000), nil); s != 200 {
		t.Fatalf("1000 entries: %d %.80s", s, b)
	}
	limits := entry(`"bookmaker":"` + strings.Repeat("b", 40) + `","sport":"` + strings.Repeat("s", 40) + `","marketTypes":` + types(50, 64) + `,"level":2`)
	if s, b := request(t, srv, "PUT", limitIndexPath(id), token, limits, nil); s != 200 {
		t.Fatalf("at the limits: %d %.80s", s, b)
	}
	// An empty list clears it.
	putLimitIndex(t, srv, token, id, `[]`)
	if _, b := limitIndexRequest(t, srv, "GET", limitIndexPath(id), token, ""); b != `{"entries":[]}` {
		t.Fatalf("cleared: %s", b)
	}

	// Not the user's agent, a code that never connected, or no number: 404 unknown_agent.
	for _, path := range []string{limitIndexPath(otherID), limitIndexPath(neverID), "/api/me/bot/agents/home/limit-index"} {
		for _, method := range []string{"GET", "PUT"} {
			if s, b := request(t, srv, method, path, token, `{"entries":[]}`, nil); s != 404 || !strings.Contains(b, `"unknown_agent"`) {
				t.Errorf("%s %s: %d %s, want 404 unknown_agent", method, path, s, b)
			}
		}
	}
	// A session only; PUT behind CSRF.
	if s, b := request(t, srv, "GET", limitIndexPath(id), "", "", bearer(code)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("GET with a bot code: %d %s", s, b)
	}
	if s, b := request(t, srv, "PUT", limitIndexPath(id), token, `{"entries":[]}`, map[string]string{"Origin": "http://evil.example"}); s != 403 ||
		!strings.Contains(b, `"bad_origin"`) {
		t.Fatalf("cross-site PUT: %d %s", s, b)
	}
}

// legLevels is the limit_level of each leg of an order message, as JSON (absent → "absent").
func legLevels(o map[string]any) string {
	var out []string
	legs, _ := o["legs"].([]any)
	for _, l := range legs {
		v, ok := l.(map[string]any)["limit_level"]
		if !ok {
			out = append(out, "absent")
			continue
		}
		js, _ := json.Marshal(v)
		out = append(out, string(js))
	}
	return strings.Join(out, ",")
}

// Each leg's limit_level comes from the index of the agent the order is sent to, as it is when the
// order is sent; the body's own limit_level is ignored. legs_json keeps it and the Orders list shows it.
func TestBotSched_LimitLevelFromTheChosenAgentsIndexAtSend(t *testing.T) {
	srv, svc, _, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	busyCode, busyID := newBotCode(t, srv, token, "Busy PC")
	code, id := newBotCode(t, srv, token, "Home PC")
	// The busy agent rates betano Strict but has no free betano window: the order goes to Home PC.
	startAgentWith(t, srv, busyCode, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "busy")), app)
	putLimitIndex(t, srv, token, busyID, betanoStrictCorners)
	bot := startAgentWith(t, srv, code, p2Status("", win("tipsport", 1, "idle"), win("betano", 1, "busy")), app)
	putLimitIndex(t, srv, token, id, `[{"bookmaker":"betano","sport":"football","marketTypes":["TOTAL_CORNERS"],"level":1}]`)

	// The body's limit_level is not the order's.
	body := strings.Replace(totalsOrder("arb-1"), `"outcome_id":"OVER"`, `"outcome_id":"OVER","limit_level":2`, 1)
	a := postOrderOK(t, srv, token, body)
	expectEvent(t, app, a.OrderID, "waiting", "Needs a free Betano window")
	expectBot(t, app, "on the waiting order")
	if w := getOrders(t, srv, token, "").Waiting; len(w) != 1 || w[0].Legs[0].LimitLevel != nil || w[0].Legs[1].LimitLevel != nil {
		t.Fatalf("a waiting order has no levels yet: %+v", w)
	}
	// Changed while the order waits: the order carries the index as it is when it is sent.
	putLimitIndex(t, srv, token, id, `[{"bookmaker":"tipsport","sport":"football","marketTypes":["TOTAL_CORNERS"],"level":1},`+
		`{"bookmaker":"betano","sport":"tennis","marketTypes":["TOTAL_CORNERS"],"level":1},`+
		`{"bookmaker":"betano","sport":"football","marketTypes":["TOTAL_GOALS"],"level":1},`+
		`{"bookmaker":"betano","sport":"football","marketTypes":["TOTAL_GOALS","TOTAL_CORNERS"],"level":2},`+
		`{"bookmaker":"betano","sport":"football","marketTypes":["TOTAL_CORNERS"],"level":0}]`)
	if s, b := postStatus(t, srv, code, tipBetIdle); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	o := readOrderJSON(t, bot)
	if got := legLevels(o); got != "1,2" { // the first matching entry wins
		t.Fatalf("limit_level = %s, want 1,2", got)
	}
	expectEvent(t, app, a.OrderID, "sent", "")
	if _, _, _, _, legs := orderRow(t, db, a.OrderID); len(legs) != 2 || legs[0].LimitLevel == nil || *legs[0].LimitLevel != 1 ||
		legs[1].LimitLevel == nil || *legs[1].LimitLevel != 2 {
		t.Fatalf("legs_json: %+v", legs)
	}
	var js string
	db.QueryRow(`SELECT legs_json FROM bot_orders WHERE id = ?`, a.OrderID).Scan(&js)
	if !strings.Contains(js, `"limit_level":1`) || !strings.Contains(js, `"limit_level":2`) {
		t.Fatalf("legs_json: %s", js)
	}
	if r := getOrders(t, srv, token, "").Running; len(r) != 1 || r[0].Legs[0].LimitLevel == nil || *r[0].Legs[0].LimitLevel != 1 ||
		r[0].Legs[1].LimitLevel == nil || *r[0].Legs[1].LimitLevel != 2 {
		t.Fatalf("Orders running: %+v", r)
	}
	ordersJSON := func() string { _, b := request(t, srv, "GET", "/api/me/bot/orders", token, "", nil); return b }
	if b := ordersJSON(); !strings.Contains(b, `"limitLevel":1`) || !strings.Contains(b, `"limitLevel":2`) {
		t.Fatalf("Orders JSON: %s", b)
	}
}

// No matching entry (another sport or market, or an agent without an index): every leg is 0.
func TestBotSched_LimitLevelZeroWithoutAMatch(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	setSettings(t, svc, bots, userIDOf(t, svc, token), func(st *accounts.BotSettings) { st.OrdersAtOnce = 3 })
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	var wins []string
	for n := 1; n <= 3; n++ {
		wins = append(wins, win("tipsport", n, "idle"), win("betano", n, "idle"))
	}
	bot := startAgentWith(t, srv, code, p2Status("", wins...), app)
	postOrderOK(t, srv, token, totalsOrder("arb-1"))
	if got := legLevels(readOrderJSON(t, bot)); got != "0,0" {
		t.Fatalf("no index: limit_level = %s, want 0,0", got)
	}
	putLimitIndex(t, srv, token, id, `[{"bookmaker":"betano","sport":"tennis","marketTypes":["TOTAL_CORNERS"],"level":2},`+
		`{"bookmaker":"betano","sport":"football","marketTypes":["TOTAL_GOALS"],"level":2}]`)
	postOrderOK(t, srv, token, totalsOrder("arb-2"))
	if got := legLevels(readOrderJSON(t, bot)); got != "0,0" {
		t.Fatalf("another sport or market: limit_level = %s, want 0,0", got)
	}
	postOrderOK(t, srv, token, strings.Replace(totalsOrder("arb-3"), `"market_type":"TOTAL_CORNERS"`, `"market_type":"TOTAL_GOALS"`, 1))
	if got := legLevels(readOrderJSON(t, bot)); got != "0,2" {
		t.Fatalf("matching entry: limit_level = %s, want 0,2", got)
	}
}

// A Try's order message carries each leg's level from the target agent's index; a leg's
// limit_level in the Try body (the app's Try form) is used as given.
func TestBotTry_LimitLevel(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	app := appSocket(t, srv, token)
	code, id := newBotCode(t, srv, token, "Home PC")
	otherCode, otherID := newBotCode(t, srv, token, "Other PC")
	bot := startAgentWith(t, srv, code, tipBetIdleP3, app)
	startAgentWith(t, srv, otherCode, tipBetIdleP3, app)
	putLimitIndex(t, srv, token, id, betanoStrictCorners)
	putLimitIndex(t, srv, token, otherID, `[{"bookmaker":"tipsport","sport":"football","marketTypes":["TOTAL_CORNERS"],"level":2}]`)
	path := fmt.Sprintf("/api/me/bot/agents/%d/try", id)

	try := func(body string) map[string]any {
		t.Helper()
		done := commandAsync(t, srv, token, path, body)
		cmdID, o := readTry(t, bot)
		if s, b := postAck(t, srv, code, `{"id":"`+cmdID+`","ok":true,"result":`+tryResult+`}`); s != 204 {
			t.Fatalf("ack: %d %s", s, b)
		}
		if a := <-done; a.status != 200 {
			t.Fatalf("app answer: %+v", a)
		}
		return o
	}
	if got := legLevels(try(tryBody)); got != "0,2" {
		t.Fatalf("try limit_level = %s, want 0,2 (the target agent's index)", got)
	}
	given := strings.Replace(tryBody, `"outcome_id":"OVER"`, `"outcome_id":"OVER","limit_level":1`, 1)
	given = strings.Replace(given, `"outcome_id":"UNDER"`, `"outcome_id":"UNDER","limit_level":0`, 1)
	if got := legLevels(try(given)); got != "1,0" {
		t.Fatalf("try limit_level = %s, want 1,0 (as given)", got)
	}
	nullGiven := strings.Replace(tryBody, `"outcome_id":"UNDER"`, `"outcome_id":"UNDER","limit_level":null`, 1)
	if got := legLevels(try(nullGiven)); got != "0,2" {
		t.Fatalf("try limit_level null = %s, want 0,2", got)
	}
	for name, level := range map[string]string{"3": "3", "-1": "-1", "1.5": "1.5", "a string": `"2"`} {
		body := strings.Replace(tryBody, `"outcome_id":"OVER"`, `"outcome_id":"OVER","limit_level":`+level, 1)
		if s, b := request(t, srv, "POST", path, token, body, nil); s != 400 || !strings.Contains(b, `"bad_order"`) {
			t.Errorf("limit_level %s: %d %s, want 400 bad_order", name, s, b)
		}
	}
}
