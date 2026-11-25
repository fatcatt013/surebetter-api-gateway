package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// autoEnv is a gateway with one user, a fake clock and (agent) one protocol-2 agent, for the automatic mode.
type autoEnv struct {
	t      *testing.T
	srv    *httptest.Server
	svc    *accounts.Service
	bots   *botHub
	db     *sql.DB
	clk    *botClock
	token  string
	userID int64
	code   string
	codeID int64
	bot    *websocket.Conn
}

func newAutoEnv(t *testing.T, phase string) *autoEnv {
	t.Helper()
	srv, svc, bots, db := newBotServerDB(t)
	bots.autoPhase = phase
	clk := &botClock{t: time.Date(2026, 11, 25, 14, 0, 0, 0, prague)}
	bots.now = clk.now
	svc.SetStateHook(bots.autoStateWritten)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	e := &autoEnv{t: t, srv: srv, svc: svc, bots: bots, db: db, clk: clk, token: token, userID: userIDOf(t, svc, token)}
	e.code, e.codeID = newBotCode(t, srv, token, "Home PC")
	return e
}

// agent connects the agent with windows (protocol 2).
func (e *autoEnv) agent(windows ...string) {
	e.t.Helper()
	e.bot = dialBot(e.t, e.srv, e.code)
	waitAgents(e.t, e.bots, e.userID, 1)
	e.status(windows...)
}

// status posts the agent's windows.
func (e *autoEnv) status(windows ...string) {
	e.t.Helper()
	if s, b := postStatus(e.t, e.srv, e.code, p2Status("", windows...)); s != 204 {
		e.t.Fatalf("status: %d %s", s, b)
	}
}

var tipFortIdle = []string{win("tipsport", 1, "idle"), win("fortuna", 1, "idle")}

// put sends PUT /api/me/bot/auto.
func (e *autoEnv) put(body string) (int, string) {
	e.t.Helper()
	return request(e.t, e.srv, "PUT", "/api/me/bot/auto", e.token, body, nil)
}

const autoFilterJSON = `{"bookmakers":["tipsport","fortuna","betano"],"sports":null,"live_state":"all","profit_min":null,"profit_max":null,` +
	`"min_age_sec":0,"three_leg":"include","blocked_markets":{}}`

// autoBody is a full settings body with mode and filter/bankroll/limits overrides ("" = the default).
func autoBody(mode, filter, bankroll, limits string) string {
	if filter == "" {
		filter = autoFilterJSON
	}
	if bankroll == "" {
		bankroll = `{"mode":"split","total":1000,"max_leg":0,"anchor_book":null,"rounding":1}`
	}
	if limits == "" {
		limits = `{"atOnce":1,"perDay":20,"stakePerDay":0,"gapSec":10,"books":[],"perBook":{}}`
	}
	return fmt.Sprintf(`{"mode":%q,"preset":{"id":"p-1","name":"Bot"},"filter":%s,"bankroll":%s,"limits":%s}`, mode, filter, bankroll, limits)
}

// setAuto stores the settings (expects 200).
func (e *autoEnv) setAuto(mode, filter, bankroll, limits string) {
	e.t.Helper()
	if s, b := e.put(autoBody(mode, filter, bankroll, limits)); s != 200 {
		e.t.Fatalf("PUT auto: %d %s", s, b)
	}
}

// controller changes the controller's settings.
func (e *autoEnv) controller(change func(*accounts.BotSettings)) {
	e.t.Helper()
	e.bots.loadSettings(e.userID)
	e.bots.mu.Lock()
	st := e.bots.settingsLocked(e.userID)
	change(&st)
	fx := &effects{}
	e.bots.saveSettingsLocked(e.userID, st, fx)
	e.bots.release(fx)
}

func (e *autoEnv) eval(opps ...ArbOpportunity) { e.bots.autoEvaluate(opps) }

// lines is the user's log, newest first.
func (e *autoEnv) lines() []autoLogLine {
	lines, _ := e.bots.auto.read(e.userID, e.bots.autoDayFrom(), "", "", maxAutoLog)
	return lines
}

// last is the newest log line (fails when there is none).
func (e *autoEnv) last() autoLogLine {
	e.t.Helper()
	l := e.lines()
	if len(l) == 0 {
		e.t.Fatal("no log line")
	}
	return l[0]
}

// expect evaluates opp and checks the newest line's reason and text.
func (e *autoEnv) expect(opp ArbOpportunity, reason, text string) autoLogLine {
	e.t.Helper()
	e.eval(opp)
	l := e.last()
	if l.Reason != reason || (text != "" && l.Text != text) {
		e.t.Fatalf("line %s %q, want %s %q", l.Reason, l.Text, reason, text)
	}
	return l
}

func (e *autoEnv) counts() map[string]int {
	return e.bots.auto.countsOf(e.userID, e.bots.autoDayFrom())
}

// orderCount is how many bot_orders rows the user has.
func (e *autoEnv) orderCount() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM bot_orders WHERE user_id = ?`, e.userID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// event posts an agent event (expects 204).
func (e *autoEnv) event(body string) {
	e.t.Helper()
	if s, b := postEvent(e.t, e.srv, e.code, body); s != 204 {
		e.t.Fatalf("event %s: %d %s", body, s, b)
	}
}

// sendNext evaluates opp, expects a sent line and returns the agent's order.
func (e *autoEnv) sendNext(opp ArbOpportunity) map[string]any {
	e.t.Helper()
	e.eval(opp)
	if l := e.last(); l.Kind != "sent" {
		e.t.Fatalf("line %+v, want sent", l)
	}
	return readOrderJSON(e.t, e.bot)
}

// sl is a stake with a selection id, a link and fair odds.
func sl(book, outcome string, odds float64, alts ...AltQuote) StakeAllocation {
	fair := odds - 0.05
	return StakeAllocation{Bookmaker: book, OutcomeID: outcome, Label: outcome + " " + book, DecimalOdds: odds, MatchURL: "https://" + book + "/m",
		MarketName: "Celkem " + book, SourceID: "sid-" + book + "-" + outcome, SourceMarketRef: "mref-" + book, FairOdds: &fair, Alternatives: alts}
}

// arbOf is an arb on event (sport from its first part) with the stakes, profit and stake_pct from the odds, first
// seen 60 s before now.
func (e *autoEnv) arbOf(event, market string, line float64, stakes ...StakeAllocation) ArbOpportunity {
	sport := strings.SplitN(event, ":", 2)[0]
	index := 0.0
	for _, s := range stakes {
		index += 1 / s.DecimalOdds
	}
	for i := range stakes {
		stakes[i].StakePct = (1 / stakes[i].DecimalOdds) / index * 100
	}
	first := e.clk.now().Add(-time.Minute).UnixMilli()
	return ArbOpportunity{EventID: event, Sport: sport, MarketType: market, Line: line, ProfitPct: (1 - index) * 100, Stakes: stakes,
		DetectedAt: e.clk.now().UnixMilli(), FirstDetectedAt: first, LastChangedAt: first, NameHome: "Sinner", NameAway: "Alcaraz",
		MarketName: "Celkový počet gamů"}
}

// arb2 is a tipsport OVER / fortuna UNDER arb at 2.1 / 2.1 (4.76 %).
func (e *autoEnv) arb2(event, market string, line float64) ArbOpportunity {
	return e.arbOf(event, market, line, sl("tipsport", "OVER", 2.1), sl("fortuna", "UNDER", 2.1))
}

func TestAutoOrderArbKey_AsTheAppWritesIt(t *testing.T) {
	neg0 := 0.0
	neg0 = -neg0
	for line, want := range map[float64]string{2.5: "e:1:T:2.5", 2: "e:1:T:2", -1.5: "e:1:T:-1.5", 0: "e:1:T:0", neg0: "e:1:T:0", 22.25: "e:1:T:22.25"} {
		if got := orderArbKey(ArbOpportunity{EventID: "e:1", MarketType: "T", Line: line}); got != want {
			t.Errorf("line %v: %q, want %q", line, got, want)
		}
	}
}

func TestAutoFormats(t *testing.T) {
	if fmtKc(2000) != "2 000" || fmtKc(20) != "20" || fmtKc(1234567) != "1 234 567" || fmtPct(1.6) != "1.6" || fmtPct(2) != "2" || fmtPct(1.957) != "1.96" {
		t.Fatal(fmtKc(2000), fmtKc(1234567), fmtPct(1.957))
	}
}

// Stage 1: only counted, in the contract's order; nothing in the log.
func TestAuto_Stage1CountsInOrder(t *testing.T) {
	e := newAutoEnv(t, "A")
	e.agent(tipFortIdle...)
	e.setAuto("log", `{"bookmakers":["tipsport","fortuna"],"sports":["tennis"],"live_state":"live","profit_min":3,"profit_max":10,`+
		`"min_age_sec":120,"three_leg":"include","blocked_markets":{"tennis":["GAME_HANDICAP"]}}`, "", "")
	ok := e.arb2("tennis-live:aa", "TOTAL_GAMES", 22.5) // 4.76 %, 60 s old
	three := e.arbOf("tennis-live:aa", "TOTAL_GAMES", 22.5, sl("tipsport", "A", 3.5), sl("fortuna", "B", 3.5), sl("betano", "C", 3.5))
	noBook := e.arbOf("tennis-live:aa", "T", 1, sl("tipsport", "OVER", 2.1), sl("betano", "UNDER", 2.1)) // betano: no agent window
	sameBook := e.arbOf("tennis-live:aa", "T", 1, sl("tipsport", "OVER", 2.1), sl("kingsbet", "UNDER", 2.1,
		AltQuote{Bookmaker: "tipsport", DecimalOdds: 2.05, SourceID: "x", MatchURL: "u"}))
	prematch := e.arb2("tennis:aa", "TOTAL_GAMES", 22.5)
	football := e.arb2("football-live:aa", "TOTAL_GOALS", 2.5)
	blocked := e.arb2("tennis-live:aa", "GAME_HANDICAP", 1.5)
	low := e.arbOf("tennis-live:aa", "T", 1, sl("tipsport", "OVER", 2.02), sl("fortuna", "UNDER", 2.02))
	high := e.arbOf("tennis-live:aa", "T", 1, sl("tipsport", "OVER", 3), sl("fortuna", "UNDER", 3))
	young := ok
	e.clk.advance(-0) // ok is 60 s old: under min_age 120
	for _, c := range []struct {
		opp  ArbOpportunity
		code string
	}{{three, "three_legs"}, {noBook, "books"}, {sameBook, "books"}, {prematch, "live_state"}, {football, "sport"}, {blocked, "market"},
		{low, "profit_min"}, {high, "profit_max"}, {young, "min_age"}} {
		before := e.counts()[c.code]
		e.eval(c.opp)
		if got := e.counts()[c.code]; got != before+1 {
			t.Fatalf("%s: count %d → %d (all %v)", c.code, before, got, e.counts())
		}
	}
	if len(e.lines()) != 0 {
		t.Fatalf("stage 1 logs no line: %+v", e.lines())
	}
	// The log API shows the counts.
	s, b := request(t, e.srv, "GET", "/api/me/bot/auto/log", e.token, "", nil)
	if s != 200 || !strings.Contains(b, `"min_age":1`) || !strings.Contains(b, `"books":2`) || !strings.Contains(b, `"lines":[]`) {
		t.Fatalf("log: %d %s", s, b)
	}
	e.clk.advance(time.Minute) // old enough now
	e.eval(ok)
	if l := e.last(); l.Kind != "would_send" || l.Legs[0].Stake != 500 {
		t.Fatalf("line %+v, want would_send", l)
	}
}

// Log only sends nothing and writes one would_send line per arb life.
func TestAuto_LogOnlyWouldSendOncePerLife(t *testing.T) {
	e := newAutoEnv(t, "A")
	e.agent(tipFortIdle...)
	e.setAuto("log", "", "", "")
	a := e.arb2("tennis-live:aa", "TOTAL_GAMES", 22.5)
	e.eval(a)
	e.eval(a)
	l := e.lines()
	if len(l) != 1 || l[0].Kind != "would_send" || l[0].Count != 2 || l[0].ArbKey != "tennis-live:aa:TOTAL_GAMES:22.5" || l[0].Sport != "tennis" ||
		!l[0].Live || l[0].Match != "Sinner – Alcaraz" || len(l[0].Legs) != 2 || l[0].Legs[0].Stake != 500 || l[0].Legs[1].Stake != 500 {
		t.Fatalf("lines %+v", l)
	}
	a.FirstDetectedAt += 1000 // a new life of the arb
	e.eval(a)
	if l := e.lines(); len(l) != 2 || l[0].Count != 1 {
		t.Fatalf("a new arb life is a new line: %+v", l)
	}
	if e.orderCount() != 0 {
		t.Fatal("Log only made an order")
	}
	// Log only never limits itself: no gap, no tries.
	for i := 0; i < 5; i++ {
		e.eval(e.arb2(fmt.Sprintf("tennis-live:b%d", i), "TOTAL_GAMES", 22.5))
		if l := e.last(); l.Kind != "would_send" {
			t.Fatalf("eval %d: %+v", i, l)
		}
	}
}

// Dry run: the order is the app's row order, with source auto, no rule 5, dry_run with the controller Live, the
// re-picked leg's selection ids and fair odds; its row has source auto and the match.
func TestAuto_DryRunOrderFields(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(tipFortIdle...)
	e.controller(func(st *accounts.BotSettings) {
		st.DryRun = false
		st.RulesOn = `["totals-over-first","giant-error"]`
	})
	e.setAuto("dry", "", "", "")
	fair := 1.99
	a := e.arbOf("tennis-live:aa", "TOTAL_GAMES", 22.5, sl("tipsport", "OVER", 2.1),
		sl("kingsbet", "UNDER", 2.2, AltQuote{Bookmaker: "fortuna", DecimalOdds: 2.1, Label: "Méně 22.5", MarketName: "Počet gamů", MatchURL: "https://f/m",
			SourceID: "f-77", SourceMarketRef: "fm-1", FairOdds: &fair}))
	o := e.sendNext(a)
	if o["source"] != "auto" || o["dry_run"] != true || o["arb_key"] != "tennis-live:aa:TOTAL_GAMES:22.5" || o["min_profit_pct"] != 2.0 ||
		o["sport"] != "tennis" || o["is_live"] != true || o["match"] != "Sinner – Alcaraz" || o["rounding"] != 1.0 {
		t.Fatalf("order %v", o)
	}
	if rules := o["rules_on"].([]any); len(rules) != 1 || rules[0] != "totals-over-first" {
		t.Fatalf("rules_on %v, want no giant-error", rules)
	}
	if br := o["bankroll"].(map[string]any); br["mode"] != "split" || br["total"] != 1000.0 {
		t.Fatalf("bankroll %v", br)
	}
	legs := o["legs"].([]any)
	f := legs[1].(map[string]any)
	if f["bookmaker"] != "fortuna" || f["source_id"] != "f-77" || f["source_market_ref"] != "fm-1" || f["match_url"] != "https://f/m" ||
		f["label"] != "Méně 22.5" || f["market_name"] != "Počet gamů" || f["fair_odds"] != 1.99 || f["outcome_id"] != "UNDER" || f["stake"] != 500.0 {
		t.Fatalf("re-picked leg %v", f)
	}
	var src, ev, info string
	var dry bool
	if err := e.db.QueryRow(`SELECT source, event_id, dry_run, info_json FROM bot_orders`).Scan(&src, &ev, &dry, &info); err != nil ||
		src != "auto" || ev != "tennis-live:aa" || !dry || !strings.Contains(info, `"firstDetectedAt":`) {
		t.Fatalf("row %s %s %v %s (err %v)", src, ev, dry, info, err)
	}
	// GET /api/me/bot shows it.
	if v := getView(t, e.srv, e.token); v.Auto.Mode != "dry" || !v.Auto.EffectiveDryRun || v.Auto.Today.Orders != 1 {
		t.Fatalf("auto summary %+v", v.Auto)
	}
	// GET /api/me/bot/orders tags it.
	s, b := request(t, e.srv, "GET", "/api/me/bot/orders", e.token, "", nil)
	if s != 200 || !strings.Contains(b, `"source":"auto","eventId":"tennis-live:aa"`) {
		t.Fatalf("orders %d %s", s, b)
	}
}

// The min profit is the filter's profit_min when it has one.
func TestAuto_MinProfitFromTheFilter(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(tipFortIdle...)
	e.setAuto("dry", strings.Replace(autoFilterJSON, `"profit_min":null`, `"profit_min":3.5`, 1), "", "")
	if o := e.sendNext(e.arb2("tennis-live:aa", "T", 1)); o["min_profit_pct"] != 3.5 {
		t.Fatalf("min_profit_pct %v", o["min_profit_pct"])
	}
}

// Auto orders never wait: no free window → no row, the line says which.
func TestAuto_NeverWaits(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(win("tipsport", 1, "busy"), win("fortuna", 1, "idle"))
	e.setAuto("dry", "", "", "")
	e.expect(e.arb2("tennis-live:aa", "T", 1), "no_free_window", "No free Tipsport window")
	e.status(win("tipsport", 1, "busy"), win("fortuna", 1, "logging_in"))
	e.expect(e.arb2("tennis-live:aa", "T", 1), "no_free_window", "No free Tipsport and Fortuna window")
	if e.orderCount() != 0 || len(e.bots.orders) != 0 {
		t.Fatal("an auto order that cannot go is not kept")
	}
	// No agent at all.
	e.bot.Close(websocket.StatusNormalClosure, "")
	waitAgents(t, e.bots, e.userID, 0)
	e.eval(e.arb2("tennis-live:aa", "T", 1))
	if c := e.counts()["books"]; c != 1 {
		t.Fatalf("no agent: no bot books, counted under books: %v", e.counts())
	}
}

// An app order waiting for windows takes them before an auto order does, and still waits like before.
func TestAuto_AppOrdersFirst(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(win("tipsport", 1, "busy"), win("fortuna", 1, "idle"))
	e.setAuto("dry", "", "", "")
	app := postOrderOK(t, e.srv, e.token, orderJSON("tennis-live:bb:X:1", legJSON("tipsport", "s1", 2.1, 100), legJSON("fortuna", "s2", 2.05, 102)))
	if app.State != "waiting" {
		t.Fatalf("app order %+v, want waiting", app)
	}
	// The window frees without a status (a command ends): the next evaluation lets the app order go first.
	e.bots.mu.Lock()
	for _, p := range e.bots.agents[e.userID] {
		p.windows[0].State = "idle"
	}
	e.bots.mu.Unlock()
	e.expect(e.arb2("tennis-live:aa", "T", 1), "orders_at_once", "Orders at once: 1 running")
	if o := readOrderJSON(t, e.bot); o["order_id"] != app.OrderID || o["source"] != "app" {
		t.Fatalf("agent got %v, want the app order", o)
	}
}

// Two arbs of one match evaluated together (and concurrently): one order.
func TestAuto_TwoArbsOneMatch(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(win("tipsport", 1, "idle"), win("fortuna", 1, "idle"), win("tipsport", 2, "idle"), win("fortuna", 2, "idle"))
	e.controller(func(st *accounts.BotSettings) { st.OrdersAtOnce = 5 })
	e.setAuto("dry", "", "", `{"atOnce":5,"perDay":20,"stakePerDay":0,"gapSec":0,"books":[],"perBook":{}}`)
	a, b := e.arb2("tennis-live:aa", "TOTAL_GAMES", 22.5), e.arb2("tennis-live:aa", "GAME_HANDICAP", 1.5)
	e.eval(a, b)
	if l := e.last(); l.Reason != "match_running" {
		t.Fatalf("second arb: %+v", l)
	}
	readOrderJSON(t, e.bot)
	// Concurrently, from many goroutines, on another match: still one order.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e.eval(e.arb2("tennis-live:cc", "T", float64(i)))
		}(i)
	}
	wg.Wait()
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM bot_orders WHERE event_id = 'tennis-live:cc'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d orders on one match, want 1", n)
	}
}

// injectRec adds a record of an ended order to today's day (the rebuild is tested on its own).
func (e *autoEnv) injectRec(r autoRec) {
	e.t.Helper()
	e.bots.mu.Lock()
	defer e.bots.mu.Unlock()
	fx := &effects{}
	d, err := e.bots.autoDayLocked(e.userID, fx)
	if err != nil {
		e.t.Fatal(err)
	}
	if r.created == 0 {
		r.created = e.clk.now().Add(-time.Hour).UnixMilli()
	}
	d.recs[r.id] = &r
}

func placed(v bool) *bool { return &v }

// The match rules: one placed order (live, any source; a dry auto one in Dry run) blocks every market of the match
// that day; a dry app order does not.
func TestAuto_MatchPlaced(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(tipFortIdle...)
	e.setAuto("dry", "", "", "")
	other := e.arb2("tennis-live:aa", "GAME_HANDICAP", 1.5)
	e.injectRec(autoRec{id: "dryapp00", source: "app", eventID: "tennis-live:aa", arbKey: "tennis-live:aa:T:1", dry: true, state: "placed"})
	if l := e.expect(other, "", ""); l.Kind != "sent" {
		t.Fatalf("a dry app order does not count: %+v", l)
	}
	readOrderJSON(t, e.bot)
	e.injectRec(autoRec{id: "liveapp0", source: "app", eventID: "tennis-live:bb", arbKey: "tennis-live:bb:T:1", state: "placed"})
	e.expect(e.arb2("tennis-live:bb", "GAME_HANDICAP", 1.5), "match_placed", "Already placed on this match today")
	e.injectRec(autoRec{id: "dryauto0", source: "auto", eventID: "tennis-live:cc", arbKey: "tennis-live:cc:T:1", dry: true, state: "placed"})
	e.expect(e.arb2("tennis-live:cc", "GAME_HANDICAP", 1.5), "match_placed", "")
}

func TestAuto_MatchBlocked(t *testing.T) {
	e := newAutoEnv(t, "A")
	e.agent(tipFortIdle...)
	e.setAuto("log", "", "", "")
	e.injectRec(autoRec{id: "8d10c2aa00000000", source: "app", eventID: "tennis-live:aa", state: "outcome_unknown"})
	e.expect(e.arb2("tennis-live:aa", "T", 1), "match_blocked", "Match blocked today · order 8d10c2aa")
	e.injectRec(autoRec{id: "9d10c2aa00000000", eventID: "tennis-live:bb", state: "stopped",
		legs: []storedLeg{{botLeg: botLeg{Bookmaker: "tipsport"}, Placed: placed(true)}, {botLeg: botLeg{Bookmaker: "fortuna"}, Placed: placed(false)}}})
	e.expect(e.arb2("tennis-live:bb", "T", 1), "match_blocked", "Match blocked today · order 9d10c2aa")
	e.injectRec(autoRec{id: "ad10c2aa00000000", eventID: "tennis-live:cc", state: "placed", stuck: true})
	e.expect(e.arb2("tennis-live:cc", "T", 1), "match_placed", "") // placed first
	e.injectRec(autoRec{id: "bd10c2aa00000000", eventID: "tennis-live:dd", state: "stopped", stuck: true})
	e.expect(e.arb2("tennis-live:dd", "T", 1), "match_blocked", "Match blocked today · order bd10c2aa")
	e.injectRec(autoRec{id: "cd10c2aa00000000", eventID: "tennis-live:ee", state: "outcome_unknown", dry: true})
	if l := e.expect(e.arb2("tennis-live:ee", "T", 1), "", ""); l.Kind != "would_send" {
		t.Fatalf("a dry unknown outcome does not block: %+v", l)
	}
}

// The stop rules: cool-down, same prices, 3 tries; new prices or a new detection pass.
func TestAuto_StopRules(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(tipFortIdle...)
	e.setAuto("dry", "", "", `{"atOnce":1,"perDay":20,"stakePerDay":0,"gapSec":0,"books":[],"perBook":{}}`)
	a := e.arb2("tennis-live:aa", "TOTAL_GAMES", 22.5)
	o := e.sendNext(a)
	id := o["order_id"].(string)
	e.event(`{"order_id":"` + id + `","event":"accepted"}`)
	e.event(`{"order_id":"` + id + `","event":"stopped","reason":"odds dropped"}`)
	e.clk.advance(46 * time.Second)
	e.expect(a, "match_cooldown", "Match cool-down · 74 s left")
	e.clk.advance(74 * time.Second)
	e.expect(a, "same_prices", "Same prices as order "+id[:8])
	b := a
	b.LastChangedAt = e.clk.now().UnixMilli() // new prices
	o = e.sendNext(b)
	id2 := o["order_id"].(string)
	e.event(`{"order_id":"` + id2 + `","event":"stopped","reason":"not found"}`) // before accepted: no cool-down
	c := a
	c.FirstDetectedAt = e.clk.now().UnixMilli() - 1 // a new detection
	c.LastChangedAt = c.FirstDetectedAt
	o = e.sendNext(c)
	e.event(`{"order_id":"` + o["order_id"].(string) + `","event":"stopped","reason":"not found"}`)
	d := c
	d.LastChangedAt = e.clk.now().UnixMilli() + 5
	e.clk.advance(10 * time.Millisecond)
	e.expect(d, "arb_attempts", "3 tries on this arb today")
}

// The sizing and day-limit checks, in order, with their texts (Log only with the controller Live: class live).
func TestAuto_SizingAndDayLimits(t *testing.T) {
	e := newAutoEnv(t, "A")
	e.agent(tipFortIdle...)
	e.controller(func(st *accounts.BotSettings) { st.DryRun = false })
	lim := func(perDay, stakePerDay int, perBook string) string {
		return fmt.Sprintf(`{"atOnce":1,"perDay":%d,"stakePerDay":%d,"gapSec":10,"books":[],"perBook":%s}`, perDay, stakePerDay, perBook)
	}
	// 2.5 / 1.75 (2.86 %) split 1000 by 100: 400 / 600 → 1000 back: 0 %.
	e.setAuto("log", "", `{"mode":"split","total":1000,"max_leg":0,"anchor_book":null,"rounding":100}`, "")
	l := e.expect(e.arbOf("tennis-live:aa", "T", 1, sl("tipsport", "OVER", 2.5), sl("fortuna", "UNDER", 1.75)), "rounded_below_min", "Rounded 0 % < 2 %")
	if l.Legs[0].Stake != 400 || l.Legs[1].Stake != 600 {
		t.Fatalf("legs %+v", l.Legs)
	}
	a := e.arb2("tennis-live:aa", "T", 1) // 500 / 500
	e.setAuto("log", "", "", lim(20, 0, `{"tipsport":{"minStake":600,"maxStake":0,"stakePerDay":0},"fortuna":{"minStake":0,"maxStake":400,"stakePerDay":0}}`))
	e.expect(a, "stake_under_min", "Under Tipsport's minimum stake (600 Kč)")
	e.setAuto("log", "", "", lim(20, 0, `{"fortuna":{"minStake":0,"maxStake":400,"stakePerDay":0}}`))
	e.expect(a, "stake_over_max", "Over Fortuna's maximum stake (400 Kč)")
	e.setAuto("log", "", "", lim(20, 0, `{"fortuna":{"minStake":0,"maxStake":2000,"stakePerDay":0}}`))
	e.controller(func(st *accounts.BotSettings) { st.MaxLeg = 400 })
	e.expect(a, "limit_leg", "Over the leg limit")
	e.controller(func(st *accounts.BotSettings) { st.MaxLeg, st.MaxOrder = 0, 900 })
	e.expect(a, "limit_order", "Over the order limit")
	e.controller(func(st *accounts.BotSettings) { st.MaxOrder = 0 })
	e.injectRec(autoRec{id: "auto0001", source: "auto", eventID: "tennis-live:zz", arbKey: "tennis-live:zz:T:1", state: "placed", stake: 1000,
		legs: []storedLeg{{botLeg: botLeg{Bookmaker: "tipsport", Stake: 500}, Placed: placed(true)}, {botLeg: botLeg{Bookmaker: "fortuna", Stake: 500}, Placed: placed(true)}}})
	e.injectRec(autoRec{id: "dryauto1", source: "auto", eventID: "tennis-live:yy", dry: true, state: "placed", stake: 5000})
	e.setAuto("log", "", "", lim(1, 0, `{}`))
	e.expect(a, "auto_day_count", "Auto orders today: 1")
	e.setAuto("log", "", "", lim(20, 1500, `{}`))
	e.expect(a, "auto_day_stake", "Over the auto stake per day")
	e.setAuto("log", "", "", lim(20, 0, `{"tipsport":{"minStake":0,"maxStake":0,"stakePerDay":900}}`))
	e.expect(a, "book_day_stake", "Over Tipsport's stake per day")
	e.setAuto("log", "", "", lim(20, 0, `{}`))
	e.controller(func(st *accounts.BotSettings) { st.PerDay = 1900 })
	e.expect(a, "limit_day", "Over the day limit")
	// Class dry (the controller in Dry run): the stake limits are for live orders only; the dry auto order counts.
	e.controller(func(st *accounts.BotSettings) { st.DryRun = true })
	e.setAuto("log", "", "", lim(20, 1500, `{"tipsport":{"minStake":0,"maxStake":0,"stakePerDay":900}}`))
	if l := e.expect(a, "", ""); l.Kind != "would_send" {
		t.Fatalf("dry class: %+v", l)
	}
	e.setAuto("log", "", "", lim(1, 0, `{}`))
	e.expect(a, "auto_day_count", "Auto orders today: 1")
}

// At once, orders at once and the gap, with real orders.
func TestAuto_AtOnceAndGap(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(win("tipsport", 1, "idle"), win("fortuna", 1, "idle"), win("tipsport", 2, "idle"), win("fortuna", 2, "idle"))
	e.setAuto("dry", "", "", "")
	e.sendNext(e.arb2("tennis-live:aa", "T", 1))
	b := e.arb2("tennis-live:bb", "T", 1)
	e.expect(b, "auto_at_once", "Auto orders at once: 1 running")
	e.setAuto("dry", "", "", `{"atOnce":2,"perDay":20,"stakePerDay":0,"gapSec":10,"books":[],"perBook":{}}`)
	e.expect(b, "orders_at_once", "Orders at once: 1 running")
	e.controller(func(st *accounts.BotSettings) { st.OrdersAtOnce = 2 })
	e.clk.advance(4 * time.Second)
	e.expect(b, "gap", "Gap · 6 s left")
	e.clk.advance(6 * time.Second)
	e.sendNext(b)
}

// The checks before the match rules: hidden, selection ids, Pause all, the auto pause, an order on the arb.
func TestAuto_FirstChecks(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(tipFortIdle...)
	e.setAuto("dry", "", "", "")
	a := e.arb2("tennis-live:aa", "TOTAL_GAMES", 22.5)
	put := func(v string) {
		t.Helper()
		if s, b := request(t, e.srv, "PUT", "/api/me/state/hidden", e.token, `{"value":`+v+`}`, nil); s != 204 && s != 200 {
			t.Fatalf("hidden: %d %s", s, b)
		}
	}
	put(`[{"kind":"arb","arbKey":"tennis-live:aa:TOTAL_GAMES:22.5"}]`)
	e.expect(a, "hidden", "Hidden")
	put(`[{"kind":"match","eventId":"tennis-live:aa"}]`)
	e.expect(e.arb2("tennis-live:aa", "T", 3), "hidden", "Hidden")
	put(`[]`)
	noID := a
	noID.Stakes = []StakeAllocation{sl("tipsport", "OVER", 2.1), sl("fortuna", "UNDER", 2.1)}
	noID.Stakes[0].SourceID = ""
	e.expect(noID, "no_selection_id", "No selection id at Tipsport")
	e.controller(func(st *accounts.BotSettings) { st.Paused, st.PausedReason = true, "Paused" })
	e.expect(a, "bots_paused", "Bots paused")
	e.controller(func(st *accounts.BotSettings) {
		st.Paused, st.PausedReason = false, ""
		st.AutoPausedReason = "Stuck order · order 12345678"
	})
	e.expect(a, "auto_paused", "Auto paused")
	e.controller(func(st *accounts.BotSettings) { st.AutoPausedReason = "" })
	app := postOrderOK(t, e.srv, e.token, orderJSON("tennis-live:aa:TOTAL_GAMES:22.5", legJSON("tipsport", "s1", 2.1, 100), legJSON("fortuna", "s2", 2.05, 102)))
	for m := readJSON(t, e.bot); m["type"] != "order"; m = readJSON(t, e.bot) { // the state notices first
	}
	e.expect(a, "order_open", "Order open on this arb")
	e.expect(e.arb2("tennis-live:aa", "GAME_HANDICAP", 1.5), "match_running", "Order running on this match")
	var ev string
	e.db.QueryRow(`SELECT event_id FROM bot_orders WHERE id = ?`, app.OrderID).Scan(&ev)
	if ev != "tennis-live:aa" {
		t.Fatalf("app order event_id from the arb key: %q", ev)
	}
}

// App orders: event_id from the body, else from an arb key of four parts; at most 128 characters.
func TestAuto_AppOrderEventID(t *testing.T) {
	e := newAutoEnv(t, "A")
	e.agent(win("tipsport", 1, "idle"), win("fortuna", 1, "idle"), win("tipsport", 2, "idle"), win("fortuna", 2, "idle"))
	e.controller(func(st *accounts.BotSettings) { st.OrdersAtOnce = 5 })
	body := func(key, ev string) string {
		return fmt.Sprintf(`{"arb_key":%q,"event_id":%q,"legs":[%s,%s]}`, key, ev, legJSON("tipsport", "s1", 2.1, 100), legJSON("fortuna", "s2", 2.05, 102))
	}
	a := postOrderOK(t, e.srv, e.token, body("x:y:T:1", "football-live:abc"))
	b := postOrderOK(t, e.srv, e.token, body("short:key", ""))
	if s, r := postOrder(t, e.srv, e.token, body("k:k:k:1", strings.Repeat("e", 129))); s != 400 || !strings.Contains(r, "bad_order") {
		t.Fatalf("long event_id: %d %s", s, r)
	}
	for id, want := range map[string]string{a.OrderID: "football-live:abc", b.OrderID: ""} {
		var ev, src string
		e.db.QueryRow(`SELECT event_id, source FROM bot_orders WHERE id = ?`, id).Scan(&ev, &src)
		if ev != want || src != "app" {
			t.Fatalf("order %s: event %q source %q, want %q app", id, ev, src, want)
		}
	}
}

func TestAuto_PutChecks(t *testing.T) {
	e := newAutoEnv(t, "A")
	s, b := request(t, e.srv, "GET", "/api/me/bot/auto", e.token, "", nil)
	if s != 200 || !strings.HasPrefix(b, `{"settings":{"mode":"off","preset":null,"filter":null,"bankroll":null,"limits":{"atOnce":1,"perDay":20,`+
		`"stakePerDay":0,"gapSec":10,"books":[],"perBook":{}},"updatedAt":0},"effectiveDryRun":true,"paused":false,"pausedReason":"","pausedOrders":[],`+
		`"today":{"orders":0,"placed":0,"stake":0},"counts":{},"botBooks":[]}`) {
		t.Fatalf("defaults: %d %s", s, b)
	}
	for name, body := range map[string]string{
		"not JSON":           `{"mode":`,
		"mode":               `{"mode":"auto"}`,
		"preset alone":       `{"preset":{"id":"p","name":"n"}}`,
		"filter alone":       `{"filter":` + autoFilterJSON + `}`,
		"preset id":          `{"preset":{"id":"","name":"n"},"filter":` + autoFilterJSON + `}`,
		"no bookmakers":      `{"preset":{"id":"p","name":"n"},"filter":` + strings.Replace(autoFilterJSON, `["tipsport","fortuna","betano"]`, `[]`, 1) + `}`,
		"null bookmakers":    `{"preset":{"id":"p","name":"n"},"filter":` + strings.Replace(autoFilterJSON, `["tipsport","fortuna","betano"]`, `null`, 1) + `}`,
		"bad book":           `{"preset":{"id":"p","name":"n"},"filter":` + strings.Replace(autoFilterJSON, `"betano"`, `"Betano"`, 1) + `}`,
		"live_state":         `{"preset":{"id":"p","name":"n"},"filter":` + strings.Replace(autoFilterJSON, `"all"`, `"any"`, 1) + `}`,
		"profit_max":         `{"preset":{"id":"p","name":"n"},"filter":` + strings.Replace(autoFilterJSON, `"profit_max":null`, `"profit_max":101`, 1) + `}`,
		"min_age":            `{"preset":{"id":"p","name":"n"},"filter":` + strings.Replace(autoFilterJSON, `"min_age_sec":0`, `"min_age_sec":3601`, 1) + `}`,
		"three_leg":          `{"preset":{"id":"p","name":"n"},"filter":` + strings.Replace(autoFilterJSON, `"include"`, `"all"`, 1) + `}`,
		"bankroll whole":     `{"bankroll":{"mode":"split","total":100.5,"max_leg":0,"anchor_book":null,"rounding":1}}`,
		"bankroll split 0":   `{"bankroll":{"mode":"split","total":0,"max_leg":500,"anchor_book":null,"rounding":1}}`,
		"bankroll maxleg 0":  `{"bankroll":{"mode":"maxleg","total":500,"max_leg":0,"anchor_book":null,"rounding":1}}`,
		"bankroll rounding":  `{"bankroll":{"mode":"split","total":500,"max_leg":0,"anchor_book":null,"rounding":5}}`,
		"atOnce":             `{"limits":{"atOnce":0}}`,
		"perDay":             `{"limits":{"perDay":1001}}`,
		"gap":                `{"limits":{"gapSec":3601}}`,
		"books":              `{"limits":{"books":["Tipsport"]}}`,
		"max under min":      `{"limits":{"perBook":{"tipsport":{"minStake":100,"maxStake":50,"stakePerDay":0}}}}`,
		"per-book stake":     `{"limits":{"perBook":{"tipsport":{"minStake":0,"maxStake":0,"stakePerDay":10000001}}}}`,
		"per-book fractions": `{"limits":{"perBook":{"tipsport":{"minStake":1.5,"maxStake":0,"stakePerDay":0}}}}`,
	} {
		if s, b := e.put(body); s != 400 || !strings.Contains(b, `"bad_auto"`) {
			t.Errorf("%s: %d %s, want 400 bad_auto", name, s, b)
		}
	}
	if s, b := e.put(`{"mode":"dry"}`); s != 409 || !strings.Contains(b, `"mode_unavailable"`) {
		t.Fatalf("dry in phase A: %d %s", s, b)
	}
	if s, b := e.put(`{"mode":"log"}`); s != 409 || !strings.Contains(b, `"auto_incomplete"`) {
		t.Fatalf("log without a preset: %d %s", s, b)
	}
	if s, b := e.put(`{"mode":"off","preset":{"id":"p","name":"n"},"filter":` + autoFilterJSON + `}`); s != 200 {
		t.Fatalf("preset with mode off: %d %s", s, b)
	}
	if s, b := e.put(`{"mode":"log"}`); s != 409 || !strings.Contains(b, `"auto_incomplete"`) {
		t.Fatalf("log without a bankroll: %d %s", s, b)
	}
	if s, b := e.put(`{"mode":"log","bankroll":{"mode":"maxleg","total":0,"max_leg":500,"anchor_book":"tipsport","rounding":10},` +
		`"limits":{"atOnce":1,"perDay":10,"stakePerDay":5000,"gapSec":10,"books":["tipsport","fortuna"],"perBook":{"tipsport":{"minStake":20,"maxStake":2000,"stakePerDay":0}}}}`); s != 200 ||
		!strings.Contains(b, `"mode":"log"`) || !strings.Contains(b, `"books":["tipsport","fortuna"]`) || !strings.Contains(b, `"anchor_book":"tipsport"`) {
		t.Fatalf("log: %d %s", s, b)
	}
	// Limits replace the stored object whole; a field left out takes its default.
	if s, b := e.put(`{"limits":{"atOnce":2}}`); s != 200 || !strings.Contains(b, `"limits":{"atOnce":2,"perDay":20,"stakePerDay":0,"gapSec":10,"books":[],"perBook":{}}`) {
		t.Fatalf("limits: %d %s", s, b)
	}
	var stored string
	e.db.QueryRow(`SELECT auto_json FROM bot_settings`).Scan(&stored)
	if !strings.Contains(stored, `"preset":{"id":"p","name":"n"}`) || !strings.Contains(stored, `"updatedAt":`) {
		t.Fatalf("stored %s", stored)
	}
	// Live needs phase C and the controller Live.
	e.bots.autoPhase = "C"
	if s, b := e.put(`{"mode":"live"}`); s != 409 || !strings.Contains(b, `"controller_dry_run"`) {
		t.Fatalf("live with the controller in dry run: %d %s", s, b)
	}
	e.controller(func(st *accounts.BotSettings) { st.DryRun = false })
	if s, b := e.put(`{"mode":"live"}`); s != 200 || !strings.Contains(b, `"effectiveDryRun":false`) {
		t.Fatalf("live: %d %s", s, b)
	}
	// A later switch of the controller to Dry run is allowed: auto orders go dry.
	e.controller(func(st *accounts.BotSettings) { st.DryRun = true })
	if v := getView(t, e.srv, e.token); v.Auto.Mode != "live" || !v.Auto.EffectiveDryRun {
		t.Fatalf("summary %+v", v.Auto)
	}
	// Back to phase A: the stored live mode does not run.
	e.bots.autoPhase = "A"
	if v := getView(t, e.srv, e.token); v.Auto.Mode != "off" {
		t.Fatalf("live under phase A: %+v", v.Auto)
	}
}

func TestAuto_LogQuery(t *testing.T) {
	e := newAutoEnv(t, "A")
	for _, q := range []string{"?limit=0", "?limit=2001", "?limit=x", "?reason=nope", "?kind=maybe"} {
		if s, b := request(t, e.srv, "GET", "/api/me/bot/auto/log"+q, e.token, "", nil); s != 400 || !strings.Contains(b, `"bad_query"`) {
			t.Errorf("%s: %d %s", q, s, b)
		}
	}
	e.agent(tipFortIdle...)
	e.setAuto("log", "", "", "")
	e.eval(e.arb2("tennis-live:aa", "T", 1))
	e.controller(func(st *accounts.BotSettings) { st.Paused = true })
	e.eval(e.arb2("tennis-live:bb", "T", 1))
	s, b := request(t, e.srv, "GET", "/api/me/bot/auto/log?kind=skip&reason=bots_paused&limit=5", e.token, "", nil)
	var got struct {
		Lines  []autoLogLine  `json:"lines"`
		Counts map[string]int `json:"counts"`
	}
	if json.Unmarshal([]byte(b), &got); s != 200 || len(got.Lines) != 1 || got.Lines[0].EventID != "tennis-live:bb" || got.Counts["bots_paused"] != 1 {
		t.Fatalf("log: %d %s", s, b)
	}
}

// The hub's listener never blocks: a full queue drops the key and counts it.
func TestAuto_FullQueueNeverBlocks(t *testing.T) {
	bots := newBotHub(newConnRegistry(), nil)
	bots.auto.active.Store(1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < autoQueueSize+100; i++ {
			bots.autoObserve(fmt.Sprintf("k%d", i), ArbOpportunity{}, false, false)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener blocked")
	}
	if len(bots.auto.queue) != autoQueueSize {
		t.Fatalf("queue %d", len(bots.auto.queue))
	}
	// Nobody with auto on: nothing is queued.
	bots.auto.active.Store(0)
	<-bots.auto.queue
	bots.autoObserve("x", ArbOpportunity{}, false, false)
	if len(bots.auto.queue) != autoQueueSize-1 {
		t.Fatal("queued with auto off everywhere")
	}
}

// The picker reads queued keys from the hub again: a removed arb is not evaluated.
func TestAuto_RunReadsTheHub(t *testing.T) {
	e := newAutoEnv(t, "A")
	e.agent(tipFortIdle...)
	e.setAuto("log", "", "", "")
	hub := NewHub(Config{})
	hub.listen(e.bots.autoObserve)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.bots.startAuto(ctx, hub)
	gone := e.arb2("tennis-live:gone", "T", 1)
	hub.ApplyOpportunity(gone)
	hub.RemoveOpportunity(gone)
	kept := e.arb2("tennis-live:kept", "T", 1)
	hub.ApplyOpportunity(kept)
	deadline := time.Now().Add(3 * time.Second)
	for len(e.lines()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	e.bots.tick() // and a pass over the hub
	time.Sleep(50 * time.Millisecond)
	for _, l := range e.lines() {
		if l.EventID != "tennis-live:kept" {
			t.Fatalf("evaluated a removed arb: %+v", l)
		}
	}
	if len(e.lines()) != 1 || e.lines()[0].Count < 1 {
		t.Fatalf("lines %+v", e.lines())
	}
}

// autoPause reads the stored auto pause.
func (e *autoEnv) autoPause() (string, []string) {
	e.t.Helper()
	var reason, ids string
	if err := e.db.QueryRow(`SELECT auto_paused_reason, auto_paused_orders FROM bot_settings WHERE user_id = ?`, e.userID).Scan(&reason, &ids); err != nil {
		e.t.Fatal(err)
	}
	return reason, pausedOrdersOf(ids)
}

// alertRows is the user's alerts titled title: "id|orderId|detail|resolved".
func (e *autoEnv) alertRows(title string) []string {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT order_id, detail, kind, resolved_at > 0 FROM bot_alerts WHERE user_id = ? AND title = ? ORDER BY created_at, id`, e.userID, title)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var order, detail, kind string
		var resolved bool
		rows.Scan(&order, &detail, &kind, &resolved)
		out = append(out, fmt.Sprintf("%s|%s|%s|%v", kind, order, detail, resolved))
	}
	return out
}

func (e *autoEnv) resume() (int, string) {
	e.t.Helper()
	return request(e.t, e.srv, "POST", "/api/me/bot/auto/resume", e.token, `{}`, nil)
}

// An unknown outcome (any order, dry too) pauses auto with one alert; later triggers add their orders; Resume needs
// the real checkable orders checked, then resolves the alert.
func TestAuto_PauseOnUnknownAndResume(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.bots.alerts = newAlertEngine(e.clk.now())
	e.agent(win("tipsport", 1, "idle"), win("fortuna", 1, "idle"), win("tipsport", 2, "idle"), win("fortuna", 2, "idle"))
	e.controller(func(st *accounts.BotSettings) { st.OrdersAtOnce, st.PauseOnUnknown = 3, false })
	e.setAuto("dry", "", "", `{"atOnce":3,"perDay":20,"stakePerDay":0,"gapSec":0,"books":[],"perBook":{}}`)
	if s, b := e.resume(); s != 409 || !strings.Contains(b, `"not_paused"`) {
		t.Fatalf("resume when not paused: %d %s", s, b)
	}
	o := e.sendNext(e.arb2("tennis-live:aa", "T", 1))
	id := o["order_id"].(string)
	e.event(`{"order_id":"` + id + `","event":"outcome_unknown","reason":"slip vanished"}`)
	reason, ids := e.autoPause()
	if reason != "Outcome unknown · order "+id[:8] || len(ids) != 1 || ids[0] != id {
		t.Fatalf("pause %q %v", reason, ids)
	}
	if a := e.alertRows(titleAutoPaused); len(a) != 1 || a[0] != "attention|"+id+"|Outcome unknown · order "+id[:8]+" · Dry run|false" {
		t.Fatalf("alerts %v", a)
	}
	if v := getView(t, e.srv, e.token); !v.Auto.Paused || v.Auto.PausedReason != reason || v.Paused {
		t.Fatalf("view %+v: auto paused, the controller not", v)
	}
	e.expect(e.arb2("tennis-live:bb", "T", 1), "auto_paused", "Auto paused")
	// An app order (live) that ends one-sided adds its id; the reason stays; no second alert.
	e.controller(func(st *accounts.BotSettings) { st.DryRun = false })
	app := postOrderOK(t, e.srv, e.token, orderJSON("tennis-live:cc:T:1", legJSON("tipsport", "s1", 2.1, 100), legJSON("fortuna", "s2", 2.05, 102)))
	e.event(`{"order_id":"` + app.OrderID + `","event":"stopped","reason":"second leg refused","legs":[{"bookmaker":"tipsport","stake":100,"odds":2.1,"placed":true},{"bookmaker":"fortuna","stake":0,"odds":0,"placed":false}]}`)
	if r, ids := e.autoPause(); r != reason || len(ids) != 2 || ids[1] != app.OrderID {
		t.Fatalf("second trigger: %q %v", r, ids)
	}
	if a := e.alertRows(titleAutoPaused); len(a) != 1 {
		t.Fatalf("one alert per pause: %v", a)
	}
	// A live order goes stuck: while it runs it holds Resume; ended outcome_unknown it is checkable until checked.
	live := postOrderOK(t, e.srv, e.token, orderJSON("tennis-live:dd:T:1", legJSON("tipsport", "s1", 2.1, 100), legJSON("fortuna", "s2", 2.05, 102)))
	e.event(`{"order_id":"` + live.OrderID + `","event":"flow","flow":{"rule":"r1","playbook":"p1","phase":3,"stuck":{"since":1}}}`)
	if _, ids := e.autoPause(); len(ids) != 3 || ids[2] != live.OrderID {
		t.Fatalf("stuck live order not added: %v", ids)
	}
	if s, b := e.resume(); s != 409 || !strings.Contains(b, `"needs_check"`) {
		t.Fatalf("resume while a paused live order runs: %d %s", s, b)
	}
	e.event(`{"order_id":"` + live.OrderID + `","event":"outcome_unknown","reason":"no answer"}`)
	if s, b := e.resume(); s != 409 || !strings.Contains(b, `"needs_check"`) {
		t.Fatalf("resume with an unchecked order: %d %s", s, b)
	}
	if s, b := request(t, e.srv, "POST", "/api/me/bot/orders/"+live.OrderID+"/checked", e.token, `{}`, nil); s != 200 {
		t.Fatalf("checked: %d %s", s, b)
	}
	// The stopped one-sided app order is not checkable (not unknown, no stuck flow): it does not hold Resume.
	if s, b := e.resume(); s != 204 {
		t.Fatalf("resume: %d %s", s, b)
	}
	if r, ids := e.autoPause(); r != "" || len(ids) != 0 {
		t.Fatalf("after resume: %q %v", r, ids)
	}
	if a := e.alertRows(titleAutoPaused); len(a) != 1 || !strings.HasSuffix(a[0], "|true") {
		t.Fatalf("alert after resume %v", a)
	}
	// Mode off: no trigger pauses.
	e.setAuto("off", "", "", "")
	e.controller(func(st *accounts.BotSettings) { st.DryRun = true })
	x := postOrderOK(t, e.srv, e.token, orderJSON("tennis-live:ee:T:1", legJSON("tipsport", "s1", 2.1, 100), legJSON("fortuna", "s2", 2.05, 102)))
	e.event(`{"order_id":"` + x.OrderID + `","event":"outcome_unknown"}`)
	if r, _ := e.autoPause(); r != "" {
		t.Fatalf("mode off paused: %q", r)
	}
}

// The stuck trigger (a flow with stuck), dry runs too; and a mode change never clears the pause.
func TestAuto_PauseOnStuck(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.bots.alerts = newAlertEngine(e.clk.now())
	e.agent(tipFortIdle...)
	noGap := `{"atOnce":1,"perDay":20,"stakePerDay":0,"gapSec":0,"books":[],"perBook":{}}`
	e.setAuto("dry", "", "", noGap)
	o := e.sendNext(e.arb2("tennis-live:aa", "T", 1))
	id := o["order_id"].(string)
	e.event(`{"order_id":"` + id + `","event":"flow","flow":{"rule":"r1","playbook":"p1","phase":2,"stuck":null}}`)
	if r, _ := e.autoPause(); r != "" {
		t.Fatalf("a flow without stuck paused: %q", r)
	}
	e.event(`{"order_id":"` + id + `","event":"flow","flow":{"rule":"r1","playbook":"p1","phase":3,"stuck":{"since":1}}}`)
	if r, ids := e.autoPause(); r != "Stuck order · order "+id[:8] || len(ids) != 1 {
		t.Fatalf("stuck: %q %v", r, ids)
	}
	e.setAuto("log", "", "", noGap)
	if r, _ := e.autoPause(); r == "" {
		t.Fatal("a mode change cleared the pause")
	}
	// A dry stuck order never needs a check: Resume works once it is over.
	if s, b := e.resume(); s != 204 {
		t.Fatalf("resume: %d %s", s, b)
	}
	// The stuck order's match is blocked only by a live order.
	e.event(`{"order_id":"` + id + `","event":"stopped"}`)
	if l := e.expect(e.arb2("tennis-live:aa", "GAME_HANDICAP", 1), "", ""); l.Kind != "would_send" {
		t.Fatalf("a dry stuck order blocks nothing: %+v", l)
	}
}

// Five auto orders in a row stopped after accepted pause auto; a placed one resets the count.
func TestAuto_StopStreak(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.bots.alerts = newAlertEngine(e.clk.now())
	e.agent(tipFortIdle...)
	e.setAuto("dry", "", "", `{"atOnce":1,"perDay":100,"stakePerDay":0,"gapSec":0,"books":[],"perBook":{}}`)
	run := func(i int, final string) {
		t.Helper()
		o := e.sendNext(e.arb2(fmt.Sprintf("tennis-live:m%d", i), "T", 1))
		id := o["order_id"].(string)
		e.event(`{"order_id":"` + id + `","event":"accepted"}`)
		e.event(`{"order_id":"` + id + `","event":"` + final + `"}`)
	}
	for i := 0; i < 4; i++ {
		run(i, "stopped")
	}
	run(4, "placed")
	for i := 5; i < 9; i++ {
		run(i, "stopped")
	}
	if r, _ := e.autoPause(); r != "" {
		t.Fatalf("paused after a placed one reset the streak: %q", r)
	}
	run(9, "stopped")
	if r, ids := e.autoPause(); r != "5 orders in a row stopped" || len(ids) != 0 {
		t.Fatalf("streak: %q %v", r, ids)
	}
	if a := e.alertRows(titleAutoPaused); len(a) != 1 || a[0] != "attention||5 orders in a row stopped · Dry run|false" {
		t.Fatalf("alert %v", a)
	}
	if s, b := e.resume(); s != 204 {
		t.Fatalf("resume: %d %s", s, b)
	}
}

// At a restart: open orders end (Gateway restarted) and pause auto; today's rows rebuild the match and arb memory.
func TestAuto_RestartRebuildsFromRows(t *testing.T) {
	e := newAutoEnv(t, "B")
	e.agent(win("tipsport", 1, "idle"), win("fortuna", 1, "idle"), win("tipsport", 2, "idle"), win("fortuna", 2, "idle"))
	e.controller(func(st *accounts.BotSettings) { st.OrdersAtOnce, st.PauseOnUnknown = 3, false })
	e.setAuto("dry", "", "", `{"atOnce":3,"perDay":20,"stakePerDay":0,"gapSec":0,"books":[],"perBook":{}}`)
	placedArb := e.arb2("tennis-live:aa", "T", 1)
	o := e.sendNext(placedArb)
	e.event(`{"order_id":"` + o["order_id"].(string) + `","event":"placed"}`)
	stopped := e.arb2("tennis-live:bb", "T", 1)
	o = e.sendNext(stopped)
	e.event(`{"order_id":"` + o["order_id"].(string) + `","event":"accepted"}`)
	e.event(`{"order_id":"` + o["order_id"].(string) + `","event":"leg_step","bookmaker":"tipsport","step":"checkOdds"}`)
	e.event(`{"order_id":"` + o["order_id"].(string) + `","event":"stopped","reason":"odds dropped"}`)
	open := e.sendNext(e.arb2("tennis-live:cc", "T", 1))

	// A new gateway process on the same database.
	svc := accounts.NewService(e.db, nil)
	reg := newConnRegistry()
	svc.SetConnections(reg)
	b := newBotHub(reg, svc)
	b.autoPhase, b.now = "B", e.clk.now
	b.alerts = newAlertEngine(e.clk.now())
	// Its agent (a presence without a socket: nothing is sent in this test).
	b.agents[e.userID] = map[int64]*botPresence{e.codeID: {client: &Client{send: make(chan []byte, 8)}, proto: 2, idleSince: map[string]time.Time{},
		windows: []botWindow{{ID: "tipsport-1", Bookmaker: "tipsport", Number: 1, State: "idle"}, {ID: "fortuna-1", Bookmaker: "fortuna", Number: 1, State: "idle"}}}}
	b.recoverOrders()
	b.loadSettings(e.userID)
	b.mu.Lock()
	st := b.settingsLocked(e.userID)
	b.mu.Unlock()
	if st.AutoPausedReason != "Outcome unknown · order "+open["order_id"].(string)[:8] {
		t.Fatalf("restart pause %q", st.AutoPausedReason)
	}
	b.mu.Lock()
	fx := &effects{}
	day, err := b.autoDayLocked(e.userID, fx)
	b.release(fx)
	if err != nil || len(day.recs) != 3 {
		t.Fatalf("rebuilt %d records (err %v)", len(day.recs), err)
	}
	b.mu.Lock()
	var stoppedRec *autoRec
	for _, r := range day.recs {
		if r.eventID == "tennis-live:bb" {
			stoppedRec = r
		}
	}
	b.mu.Unlock()
	if stoppedRec == nil || !stoppedRec.accepted || stoppedRec.state != "stopped" || stoppedRec.firstSeen != stopped.FirstDetectedAt || stoppedRec.source != "auto" {
		t.Fatalf("stopped record %+v", stoppedRec)
	}
	// Resume, then the rules read the rebuilt day.
	b.mu.Lock()
	st.AutoPausedReason, st.AutoPausedOrders = "", "[]"
	fx = &effects{}
	b.saveSettingsLocked(e.userID, st, fx)
	b.autoActivateLocked(e.userID)
	b.release(fx)
	hidden := hiddenOf(nil)
	check := func(opp ArbOpportunity, reason string) {
		t.Helper()
		b.autoEvalOne(e.userID, opp, hidden)
		lines, _ := b.auto.read(e.userID, b.autoDayFrom(), "", "", 1)
		if len(lines) != 1 || lines[0].Reason != reason {
			t.Fatalf("%s: %+v, want %s", opp.EventID, lines, reason)
		}
	}
	check(e.arb2("tennis-live:aa", "GAME_HANDICAP", 1), "match_placed")
	check(e.arb2("tennis-live:bb", "GAME_HANDICAP", 1), "match_cooldown")
	e.clk.advance(3 * time.Minute)
	check(stopped, "same_prices")
}

// The Telegram text of an auto pause links the Auto tab.
func TestAuto_AlertLinksTheAutoTab(t *testing.T) {
	a := botAlert{Kind: alertAttention, Severity: severityAttention, OrderID: "8d10c2aa00000000", Title: titleAutoPaused, Detail: "Outcome unknown · order 8d10c2aa",
		CreatedAt: time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC).UnixMilli()}
	if got := alertText(a, "https://x", prague); got != "⚠️ Auto paused · Outcome unknown · order 8d10c2aa · 14:00\nhttps://x/app/bots/auto" {
		t.Fatalf("text %q", got)
	}
}
