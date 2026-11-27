package main

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// balanceRows is every bot_balances row of window as "amount@at", oldest first.
func balanceRows(t *testing.T, db *sql.DB, window string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT amount, at FROM bot_balances WHERE window_id = ? ORDER BY at, rowid`, window)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var amount float64
		var at int64
		rows.Scan(&amount, &at)
		out = append(out, strconv.FormatFloat(amount, 'f', -1, 64)+"@"+strconv.FormatInt(at, 10))
	}
	return out
}

// A balance read is stored when the window has none yet, when the amount changed (at 2 decimals), or when the last
// stored row is at least an hour older than the read; null and protocol-6 balances are never stored. The first stored
// read is the account's point 0, never changed. A new hub over the same database does not store the last read again.
func TestBankroll_BalanceRows(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	userID := userIDOf(t, svc, token)
	code, codeID := newBotCode(t, srv, token, "PC")
	dialBot(t, srv, code)
	waitBot(t, bots, userID, true)
	const t0, minute, hour = int64(1795710000000), int64(60000), int64(3600000)
	post := func(proto int, balance string) {
		t.Helper()
		if s, b := postStatus(t, srv, code, protoStatus(proto, winBal("tipsport", 1, "idle", balance), winBal("betano", 1, "idle", "null"))); s != 204 {
			t.Fatalf("status: %d %s", s, b)
		}
	}
	steps := []struct {
		proto   int
		balance string
		want    int // rows of tipsport-1 after it
	}{
		{7, bal(5000, t0), 1},                  // first
		{7, bal(5000, t0+10*minute), 1},        // same, within the hour
		{7, bal(5000.004, t0+20*minute), 1},    // same at 2 decimals
		{7, "null", 1},                         // unknown
		{6, bal(9999, t0+30*minute), 1},        // protocol 6: no balance
		{7, bal(4800, t0+40*minute), 2},        // changed
		{7, bal(4800, t0+40*minute+hour-1), 2}, // same, 1 h − 1 ms after the last row
		{7, bal(4800, t0+40*minute+hour), 3},   // same, 1 h after
		{7, bal(4800, t0+40*minute+hour), 3},   // the same read again
	}
	for i, s := range steps {
		post(s.proto, s.balance)
		if got := balanceRows(t, db, "tipsport-1"); len(got) != s.want {
			t.Fatalf("step %d (%s): rows %v, want %d", i, s.balance, got, s.want)
		}
	}
	if got := balanceRows(t, db, "betano-1"); len(got) != 0 {
		t.Fatalf("betano-1 (always null): %v", got)
	}
	accs, err := svc.BotAccounts(userID)
	if err != nil || len(accs) != 1 || accs[0].CodeID != codeID || accs[0].WindowID != "tipsport-1" || accs[0].Bookmaker != "tipsport" ||
		accs[0].StartAmount != 5000 || accs[0].StartAt != t0 {
		t.Fatalf("point 0: %+v %v", accs, err)
	}

	// A new process: the marks come from the database (no row for the same amount within the hour).
	b2 := newBotHub(newConnRegistry(), svc)
	ws := []botWindow{{ID: "tipsport-1", Bookmaker: "tipsport", Balance: &botBalance{Amount: 4800, At: t0 + 40*minute + hour + 5*minute}}}
	record := func() {
		b2.loadBalanceMarks(userID, codeID)
		b2.mu.Lock()
		fx := &effects{}
		b2.recordBalancesLocked(userID, codeID, 7, ws, fx)
		b2.release(fx)
	}
	record()
	if got := balanceRows(t, db, "tipsport-1"); len(got) != 3 {
		t.Fatalf("after a restart: %v", got)
	}
	ws[0].Balance = &botBalance{Amount: 4700, At: t0 + 2*hour}
	record()
	if got := balanceRows(t, db, "tipsport-1"); len(got) != 4 || got[3] != "4700@"+strconv.FormatInt(t0+2*hour, 10) {
		t.Fatalf("a change after a restart: %v", got)
	}
	if accs, _ := svc.BotAccounts(userID); len(accs) != 1 || accs[0].StartAmount != 5000 {
		t.Fatalf("point 0 changed: %+v", accs)
	}
}

// ledgerOf is userID's ledger legs as "order/leg book stake@odds placedAt".
func ledgerOf(t *testing.T, svc *accounts.Service, userID int64) []accounts.BotLedgerLeg {
	t.Helper()
	legs, err := svc.BotLedgerLegs(userID)
	if err != nil {
		t.Fatal(err)
	}
	return legs
}

// liveAgent connects a bot code of the user's with tipsport-1 and betano-1 idle, switches the user to Live and sets
// the hub's clock; it returns the code.
func liveAgent(t *testing.T, srv *httptest.Server, svc *accounts.Service, bots *botHub, token string, clk *botClock) (string, int64) {
	t.Helper()
	userID := userIDOf(t, svc, token)
	bots.now = clk.now
	code, codeID := newBotCode(t, srv, token, "PC")
	dialBot(t, srv, code)
	waitBot(t, bots, userID, true)
	if s, b := postStatus(t, srv, code, tipBetIdle); s != 204 {
		t.Fatalf("status: %d %s", s, b)
	}
	setSettings(t, svc, bots, userID, func(st *accounts.BotSettings) { st.DryRun = false })
	return code, codeID
}

// sendTotals posts the totals order (tipsport Over 1000 @ 2.05, betano Under 1025 @ 2) and expects it sent.
func sendTotals(t *testing.T, srv *httptest.Server, token, arbKey string) string {
	t.Helper()
	a := postOrderOK(t, srv, token, totalsOrder(arbKey))
	if a.State != "sent" {
		t.Fatalf("order %s: %+v", arbKey, a)
	}
	return a.OrderID
}

// A live order's legs reported placed become ledger rows with what the agent reported (stake, odds; odds 0 keeps the
// ordered odds) and the event's time; the same event again adds nothing; a one-sided stop adds its placed leg only; a
// dry order adds nothing.
func TestBankroll_LedgerOnPlaced(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	userID := userIDOf(t, svc, token)
	clk := &botClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	code, codeID := liveAgent(t, srv, svc, bots, token, clk)

	id := sendTotals(t, srv, token, "arb-1")
	clk.advance(30 * time.Second)
	placedAt := clk.now().UnixMilli()
	placed := `{"order_id":"` + id + `","event":"placed","legs":[{"bookmaker":"tipsport","stake":1000,"odds":2.08,"placed":true},` +
		`{"bookmaker":"betano","stake":1040,"odds":0,"placed":true}]}`
	if s, b := postEvent(t, srv, code, placed); s != 204 {
		t.Fatalf("placed: %d %s", s, b)
	}
	legs := ledgerOf(t, svc, userID)
	if len(legs) != 2 {
		t.Fatalf("ledger: %+v", legs)
	}
	l0, l1 := legs[0], legs[1]
	if l0.OrderID != id || l0.Leg != 0 || l0.Bookmaker != "tipsport" || l0.WindowID != "tipsport-1" || l0.CodeID != codeID || l0.Stake != 1000 ||
		l0.Odds != 2.08 || l0.PlacedAt != placedAt || l0.Source != "app" || l0.Match != "Lyon – Lille" || l0.Market != "Corners 9.5" ||
		l0.MarketType != "TOTAL_CORNERS" || l0.Line == nil || *l0.Line != 9.5 || l0.Voidable || l0.Result != "open" || l0.UpdatedAt != placedAt {
		t.Fatalf("leg 0: %+v", l0)
	}
	if l1.Leg != 1 || l1.Bookmaker != "betano" || l1.Stake != 1040 || l1.Odds != 2 {
		t.Fatalf("leg 1: %+v", l1)
	}
	if s, _ := postEvent(t, srv, code, placed); s != 404 {
		t.Fatalf("the same event again: %d", s)
	}
	if legs := ledgerOf(t, svc, userID); len(legs) != 2 || legs[0].Stake != 1000 {
		t.Fatalf("after the same event again: %+v", legs)
	}

	// One-sided: stopped with the tipsport leg placed.
	id2 := sendTotals(t, srv, token, "arb-2")
	if s, b := postEvent(t, srv, code, `{"order_id":"`+id2+`","event":"stopped","reason":"odds","legs":[`+
		`{"bookmaker":"tipsport","stake":1000,"odds":2.05,"placed":true},{"bookmaker":"betano","stake":1025,"odds":2,"placed":false}]}`); s != 204 {
		t.Fatalf("stopped: %d %s", s, b)
	}
	legs = ledgerOf(t, svc, userID)
	if n := slices.IndexFunc(legs, func(l accounts.BotLedgerLeg) bool { return l.OrderID == id2 }); len(legs) != 3 || n < 0 || legs[n].Leg != 0 {
		t.Fatalf("one-sided: %+v", legs)
	}

	// Dry run: nothing.
	setSettings(t, svc, bots, userID, func(st *accounts.BotSettings) { st.DryRun = true })
	id3 := sendTotals(t, srv, token, "arb-3")
	if s, b := postEvent(t, srv, code, `{"order_id":"`+id3+`","event":"placed","legs":[`+
		`{"bookmaker":"tipsport","stake":1000,"odds":2.05,"placed":true},{"bookmaker":"betano","stake":1025,"odds":2,"placed":true}]}`); s != 204 {
		t.Fatalf("dry placed: %d %s", s, b)
	}
	if legs := ledgerOf(t, svc, userID); len(legs) != 3 {
		t.Fatalf("dry run: %+v", legs)
	}
}

// The backfill at start: every non-dry stored order with a placed leg gets its rows once (placed_at = ended_at, else
// sent_at); dry orders and legs not placed get none.
func TestBankroll_Backfill(t *testing.T) {
	_, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	userID := userIDOf(t, svc, token)
	info := `{"match":"A – B","market":"DNB","marketType":"DRAW_NO_BET","source":"x"}`
	legs := `[{"bookmaker":"tipsport","label":"A","odds":2,"stake":100,"windowId":"tipsport-1","placed":true},` +
		`{"bookmaker":"betano","label":"B","odds":2.1,"stake":95,"windowId":"betano-1","placed":false}]`
	for _, r := range []accounts.BotOrderRow{
		{ID: "a", State: "stopped", LegsJSON: legs, SentAt: 100, EndedAt: 200, Source: "auto", CodeID: 4},
		{ID: "b", State: "running", LegsJSON: legs, SentAt: 300},
		{ID: "c", State: "placed", LegsJSON: legs, DryRun: true, SentAt: 100, EndedAt: 200},
		{ID: "d", State: "stopped", LegsJSON: `[{"bookmaker":"tipsport","placed":false}]`, SentAt: 100, EndedAt: 200},
	} {
		r.UserID, r.ArbKey, r.InfoJSON, r.CreatedAt, r.OddsDrop = userID, "k", info, 50, "strict"
		if err := svc.InsertBotOrder(r); err != nil {
			t.Fatal(err)
		}
	}
	bots.backfillLedger()
	bots.backfillLedger()
	got := ledgerOf(t, svc, userID)
	if len(got) != 2 {
		t.Fatalf("backfill: %+v", got)
	}
	a, b := got[0], got[1]
	if a.OrderID != "a" || a.Leg != 0 || a.PlacedAt != 200 || a.Source != "auto" || a.CodeID != 4 || !a.Voidable || a.Label != "A" ||
		a.Stake != 100 || a.Odds != 2 || a.MarketType != "DRAW_NO_BET" || a.WindowID != "tipsport-1" {
		t.Fatalf("order a: %+v", a)
	}
	if b.OrderID != "b" || b.PlacedAt != 300 || b.Source != "app" {
		t.Fatalf("order b: %+v", b)
	}
}

// ledgerFixture stores, for the user, orders created at 1000…4000 (relative to base) with their ledger legs:
// o1 app, settled (tipsport won 100 @ 2.1, betano lost 102 @ 2.05); o2 auto, booked (tipsport 50 @ 2, fortuna 50 @ 2.1);
// o3 app, one-sided (betano 40 @ 2.5 open; its tipsport leg not placed); o4 app, outcome_unknown without rows.
func ledgerFixture(t *testing.T, svc *accounts.Service, userID, codeID, base int64) {
	t.Helper()
	legsOf := func(a, b string) string { return "[" + a + "," + b + "]" }
	leg := func(book string, odds, stake float64, placed string) string {
		return `{"bookmaker":"` + book + `","label":"` + book + ` side","odds":` + strconv.FormatFloat(odds, 'f', -1, 64) + `,"stake":` +
			strconv.FormatFloat(stake, 'f', -1, 64) + `,"windowId":"` + book + `-1","placed":` + placed + `}`
	}
	info := `{"match":"A – B","market":"Winner","marketType":"MATCH_WINNER","sport":"tennis","isLive":true}`
	orders := []accounts.BotOrderRow{
		{ID: "o1", State: "placed", Source: "app", LegsJSON: legsOf(leg("tipsport", 2.1, 100, "true"), leg("betano", 2.05, 102, "true"))},
		{ID: "o2", State: "placed", Source: "auto", LegsJSON: legsOf(leg("tipsport", 2, 50, "true"), leg("fortuna", 2.1, 50, "true"))},
		{ID: "o3", State: "stopped", Source: "app", LegsJSON: legsOf(leg("tipsport", 2, 60, "false"), leg("betano", 2.5, 40, "true"))},
		{ID: "o4", State: "outcome_unknown", Source: "app", LegsJSON: legsOf(leg("tipsport", 1.9, 70, "null"), leg("betano", 2.2, 60, "null"))},
	}
	for i := range orders {
		o := &orders[i]
		o.UserID, o.CodeID, o.ArbKey, o.InfoJSON, o.OddsDrop = userID, codeID, "k", info, "strict"
		o.CreatedAt, o.SentAt, o.EndedAt = base+int64(i+1)*1000, base+int64(i+1)*1000+10, base+int64(i+1)*1000+20
		if err := svc.InsertBotOrder(*o); err != nil {
			t.Fatal(err)
		}
	}
	// The ledger rows as the backfill makes them, then o1's results.
	b := newBotHub(newConnRegistry(), svc)
	b.backfillLedger()
	won, lost, at := 210.0, 0.0, base+5000
	if ok, err := svc.SetBotLedgerResult(userID, "o1", 0, "won", &won, &at, "manual", at); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := svc.SetBotLedgerResult(userID, "o1", 1, "lost", &lost, &at, "manual", at); !ok || err != nil {
		t.Fatal(ok, err)
	}
}

type bankrollOrdersAnswer struct {
	Orders []bankrollOrderView `json:"orders"`
	Next   *string             `json:"next"`
}

func getBankrollOrders(t *testing.T, srv *httptest.Server, token, query string) bankrollOrdersAnswer {
	t.Helper()
	s, body := request(t, srv, "GET", "/api/me/bot/bankroll/orders"+query, token, "", nil)
	var a bankrollOrdersAnswer
	if err := json.Unmarshal([]byte(body), &a); s != 200 || err != nil {
		t.Fatalf("orders%s: %d %s", query, s, body)
	}
	return a
}

func orderIDs(a bankrollOrdersAnswer) string {
	var ids []string
	for _, o := range a.Orders {
		ids = append(ids, o.OrderID)
	}
	return strings.Join(ids, ",")
}

// The orders list: newest first, each filter, the cursor; legs of an order that needs a check are listed without a row
// (placedAt null, result open).
func TestBankroll_OrdersList(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	userID := userIDOf(t, svc, token)
	_, codeID := newBotCode(t, srv, token, "PC")
	clk := &botClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	base := clk.now().UnixMilli() - 3600000
	ledgerFixture(t, svc, userID, codeID, base)
	// Another user's order is never listed.
	other := newUser(t, svc, "o@example.com", accounts.RoleUser)
	if err := svc.InsertBotOrder(accounts.BotOrderRow{ID: "x1", UserID: userIDOf(t, svc, other), ArbKey: "k", State: "outcome_unknown",
		InfoJSON: `{}`, LegsJSON: `[]`, CreatedAt: base + 1500, OddsDrop: "strict"}); err != nil {
		t.Fatal(err)
	}

	all := getBankrollOrders(t, srv, token, "")
	if orderIDs(all) != "o4,o3,o2,o1" || all.Next != nil {
		t.Fatalf("all: %s %v", orderIDs(all), all.Next)
	}
	o1, o2, o3, o4 := all.Orders[3], all.Orders[2], all.Orders[1], all.Orders[0]
	if o1.Status != "settled" || o1.Profit == nil || *o1.Profit != 8 || o1.Booked != nil || len(o1.Legs) != 2 || *o1.Legs[0].Result != "won" ||
		*o1.Legs[0].Payout != 210 || o1.Legs[0].ResultSource != "manual" || o1.Source != "app" || o1.Sport != "tennis" || !o1.Live ||
		o1.MarketType != "MATCH_WINNER" || o1.CreatedAt != base+1000 || o1.State != "placed" || *o1.Legs[0].PlacedAt != base+1020 {
		t.Fatalf("o1: %s", mustJSON(o1))
	}
	if o2.Status != "booked" || o2.Booked == nil || *o2.Booked != 0 || o2.Source != "auto" || o2.Voidable {
		t.Fatalf("o2: %s", mustJSON(o2))
	}
	if o3.Status != "pending" || len(o3.Legs) != 1 || o3.Legs[0].Leg != 1 || o3.Legs[0].Bookmaker != "betano" {
		t.Fatalf("o3: %s", mustJSON(o3))
	}
	// Each order's agent, Prague day (created 11:00 UTC = 12:00 Prague) and, when pending, why.
	for _, o := range all.Orders {
		reason := ""
		if o.PendingReason != nil {
			reason = *o.PendingReason
		}
		want := map[string]string{"o3": "one_sided", "o4": "outcome_unknown"}[o.OrderID]
		if o.AgentID != codeID || o.AgentName != "PC" || o.Day != "2026-11-26" || reason != want {
			t.Errorf("%s: agent %d %q day %s reason %q, want %q", o.OrderID, o.AgentID, o.AgentName, o.Day, reason, want)
		}
	}
	if o4.Status != "pending" || len(o4.Legs) != 2 || o4.Legs[0].Result == nil || *o4.Legs[0].Result != "open" || o4.Legs[0].PlacedAt != nil || o4.Legs[1].Stake != 60 {
		t.Fatalf("o4: %s", mustJSON(o4))
	}
	for query, want := range map[string]string{
		"?source=auto":       "o2",
		"?source=app":        "o4,o3,o1",
		"?bookmaker=fortuna": "o2",
		"?bookmaker=betano":  "o4,o3,o1",
		"?status=pending":    "o4,o3",
		"?status=booked":     "o2",
		"?status=settled":    "o1",
		"?status=open":       "o3,o2",
		"?from=" + strconv.FormatInt(base+2000, 10) + "&to=" + strconv.FormatInt(base+4000, 10): "o3,o2",
	} {
		if got := orderIDs(getBankrollOrders(t, srv, token, query)); got != want {
			t.Errorf("%s: %s, want %s", query, got, want)
		}
	}
	page := getBankrollOrders(t, srv, token, "?limit=2")
	if orderIDs(page) != "o4,o3" || page.Next == nil || *page.Next != strconv.FormatInt(base+3000, 10)+".o3" {
		t.Fatalf("page 1: %s %v", orderIDs(page), page.Next)
	}
	page = getBankrollOrders(t, srv, token, "?limit=2&before="+*page.Next)
	if orderIDs(page) != "o2,o1" || page.Next != nil {
		t.Fatalf("page 2: %s %v", orderIDs(page), page.Next)
	}
	for _, q := range []string{"?limit=0", "?limit=201", "?status=won", "?source=x", "?before=abc", "?before=12", "?from=-1", "?to=x"} {
		if s, body := request(t, srv, "GET", "/api/me/bot/bankroll/orders"+q, token, "", nil); s != 400 || !strings.Contains(body, "bad_query") {
			t.Errorf("%s: %d %s", q, s, body)
		}
	}
}

func putResult(t *testing.T, srv *httptest.Server, token, order string, leg int, body string) (int, string) {
	t.Helper()
	return request(t, srv, "PUT", "/api/me/bot/bankroll/orders/"+order+"/legs/"+strconv.Itoa(leg), token, body, nil)
}

// Setting a leg's result: each result's payout, cashed out with its amount, the bad bodies, reset to open, another
// user's order, a leg without a row (created for an outcome_unknown order only); the app is told.
func TestBankroll_PutResult(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	userID := userIDOf(t, svc, token)
	_, codeID := newBotCode(t, srv, token, "PC")
	clk := &botClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	now := clk.now().UnixMilli()
	base := now - 3600000
	ledgerFixture(t, svc, userID, codeID, base)
	app := appSocket(t, srv, token)
	leg := func(order string, i int) accounts.BotLedgerLeg {
		t.Helper()
		l, ok, err := svc.BotLedgerLegOf(userID, order, i)
		if !ok || err != nil {
			t.Fatalf("leg %s/%d: %v %v", order, i, ok, err)
		}
		return l
	}
	// o2 leg 0: tipsport 50 @ 2.
	for result, want := range map[string]float64{"won": 100, "lost": 0, "void": 50, "half_won": 75, "half_lost": 25} {
		if s, b := putResult(t, srv, token, "o2", 0, `{"result":"`+result+`","payout":999}`); s != 204 {
			t.Fatalf("%s: %d %s", result, s, b)
		}
		expectBot(t, app, "after "+result)
		if l := leg("o2", 0); l.Result != result || *l.Payout != want || *l.SettledAt != now || l.ResultSource != "manual" || l.UpdatedAt != now {
			t.Fatalf("%s: %+v", result, l)
		}
	}
	if s, b := putResult(t, srv, token, "o2", 0, `{"result":"cashed_out","payout":87.5,"settledAt":`+strconv.FormatInt(now-60000, 10)+`}`); s != 204 {
		t.Fatalf("cashed_out: %d %s", s, b)
	}
	expectBot(t, app, "after cashed_out")
	if l := leg("o2", 0); l.Result != "cashed_out" || *l.Payout != 87.5 || *l.SettledAt != now-60000 {
		t.Fatalf("cashed_out: %+v", l)
	}
	for _, body := range []string{`{"result":"cashed_out"}`, `{"result":"cashed_out","payout":-1}`, `{"result":"x"}`, `{}`, `nope`,
		`{"result":"won","settledAt":` + strconv.FormatInt(now+5*60000+1, 10) + `}`, `{"result":"won","settledAt":0}`} {
		if s, b := putResult(t, srv, token, "o2", 0, body); s != 400 || !strings.Contains(b, "bad_result") {
			t.Fatalf("%s: %d %s", body, s, b)
		}
	}
	if s, _ := putResult(t, srv, token, "o2", 0, `{"result":"won","settledAt":`+strconv.FormatInt(now+5*60000, 10)+`}`); s != 204 {
		t.Fatalf("settledAt 5 min ahead: %d", s)
	}
	expectBot(t, app, "after won")
	if s, _ := putResult(t, srv, token, "o2", 0, `{"result":"open"}`); s != 204 {
		t.Fatalf("open: %d", s)
	}
	expectBot(t, app, "after open")
	if l := leg("o2", 0); l.Result != "open" || l.Payout != nil || l.SettledAt != nil || l.ResultSource != "" {
		t.Fatalf("reset: %+v", l)
	}
	// Not found: another user's order, a leg out of range, o3's leg that was not placed (not an order that needs a check).
	other := newUser(t, svc, "o@example.com", accounts.RoleUser)
	for _, c := range []struct {
		token, order string
		leg          int
	}{{other, "o2", 0}, {token, "o2", 2}, {token, "o3", 0}, {token, "nope", 0}} {
		if s, b := putResult(t, srv, c.token, c.order, c.leg, `{"result":"won"}`); s != 404 || !strings.Contains(b, "not_found") {
			t.Fatalf("%s/%d: %d %s", c.order, c.leg, s, b)
		}
	}
	// o4 (outcome_unknown, no rows): the row is made from the stored leg.
	if s, b := putResult(t, srv, token, "o4", 1, `{"result":"lost"}`); s != 204 {
		t.Fatalf("o4 leg 1: %d %s", s, b)
	}
	expectBot(t, app, "after o4")
	if l := leg("o4", 1); l.Bookmaker != "betano" || l.Stake != 60 || l.Odds != 2.2 || l.PlacedAt != base+4020 || l.Result != "lost" ||
		*l.Payout != 0 || l.WindowID != "betano-1" || l.CodeID != codeID || l.Source != "app" {
		t.Fatalf("o4 leg 1: %+v", l)
	}
	// o4's leg 0 is still unanswered: o4 stays pending (outcome_unknown); its leg 0 is listed without a row.
	got := getBankrollOrders(t, srv, token, "?status=pending")
	if orderIDs(got) != "o4,o3" || *got.Orders[0].PendingReason != "outcome_unknown" || got.Orders[0].Legs[0].PlacedAt != nil ||
		*got.Orders[0].Legs[0].Result != "open" {
		t.Fatalf("pending after o4 leg 1: %s %s", orderIDs(got), mustJSON(got.Orders))
	}
	// Not placed: only on a leg of an order that needs a check.
	if s, b := putResult(t, srv, token, "o2", 0, `{"result":"not_placed"}`); s != 400 || !strings.Contains(b, "bad_result") {
		t.Fatalf("not_placed on o2: %d %s", s, b)
	}
	if s, b := putResult(t, srv, token, "nope", 0, `{"result":"not_placed"}`); s != 404 {
		t.Fatalf("not_placed on an unknown order: %d %s", s, b)
	}
	if l := leg("o2", 0); l.Result != "open" {
		t.Fatalf("o2 leg 0 after a refused not_placed: %+v", l)
	}
	if s, b := putResult(t, srv, token, "o4", 0, `{"result":"not_placed","payout":5}`); s != 204 {
		t.Fatalf("not_placed on o4 leg 0: %d %s", s, b)
	}
	expectBot(t, app, "after not_placed")
	if l := leg("o4", 0); l.Result != "not_placed" || l.Payout != nil || l.SettledAt == nil || *l.SettledAt != now || l.ResultSource != "manual" ||
		l.Stake != 70 {
		t.Fatalf("o4 leg 0 not placed: %+v", l)
	}
	// Every leg answered: o4 leaves Pending, settled at −60 (its not-placed 70 is in no figure).
	if got := getBankrollOrders(t, srv, token, "?status=pending"); orderIDs(got) != "o3" {
		t.Fatalf("pending after not_placed: %s", orderIDs(got))
	}
	got = getBankrollOrders(t, srv, token, "?status=settled")
	if orderIDs(got) != "o4,o1" || *got.Orders[0].Profit != -60 || len(got.Orders[0].Legs) != 2 || *got.Orders[0].Legs[0].Result != "not_placed" ||
		got.Orders[0].Legs[0].Payout != nil || got.Orders[0].PendingReason != nil {
		t.Fatalf("settled after not_placed: %s %s", orderIDs(got), mustJSON(got.Orders))
	}
	if got := getBankrollOrders(t, srv, token, "?status=open"); orderIDs(got) != "o3,o2" {
		t.Fatalf("open after not_placed: %s", orderIDs(got))
	}
	s, body := request(t, srv, "GET", "/api/me/bot/bankroll?range=all", token, "", nil)
	var v bankrollView
	if err := json.Unmarshal([]byte(body), &v); s != 200 || err != nil {
		t.Fatalf("bankroll: %d %s", s, body)
	}
	// Real legs: o1 100 + 102, o3 40, o4 60 (o2's leg 0 is open again: 50 + 50); o4's 70 nowhere.
	if v.Total.Staked != 402 || v.Total.Orders != 4 || v.Total.OpenStakes != 140 || v.Counts != (bankrollCounts{Pending: 1, Booked: 1, Settled: 2, Open: 2}) {
		t.Fatalf("figures after not_placed: %s %s", mustJSON(v.Total), mustJSON(v.Counts))
	}
	for _, a := range v.Accounts {
		if a.WindowID == "tipsport-1" && a.OpenStakes != 50 {
			t.Fatalf("tipsport-1 holds the not-placed leg: %s", mustJSON(a))
		}
	}
	// Reset to open: the leg is a real open leg again.
	if s, _ := putResult(t, srv, token, "o4", 0, `{"result":"open"}`); s != 204 {
		t.Fatalf("o4 leg 0 open: %d", s)
	}
	expectBot(t, app, "after o4 open")
	if l := leg("o4", 0); l.Result != "open" || l.Payout != nil || l.SettledAt != nil || l.ResultSource != "" {
		t.Fatalf("o4 leg 0 reset: %+v", l)
	}
	if got := getBankrollOrders(t, srv, token, "?status=open"); orderIDs(got) != "o4,o3,o2" {
		t.Fatalf("open after the reset: %s", orderIDs(got))
	}
}

// The cards and the curve: point 0, the newest balance (the agent's status when it is newer than the stored row),
// outside money with a deposit between two reads, a removed window, the totals and the range.
func TestBankroll_Get(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	userID := userIDOf(t, svc, token)
	code, codeID := newBotCode(t, srv, token, "PC")
	clk := &botClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	now := clk.now().UnixMilli()
	const hour = int64(3600000)
	t0 := now - 48*hour
	for _, r := range []accounts.BotBalanceRow{
		{CodeID: codeID, WindowID: "tipsport-1", Bookmaker: "tipsport", Amount: 5000, At: t0},
		{CodeID: codeID, WindowID: "betano-1", Bookmaker: "betano", Amount: 2000, At: t0},
		{CodeID: codeID, WindowID: "betano-1", Bookmaker: "betano", Amount: 1848, At: now - 2*hour},
	} {
		r.UserID = userID
		if err := svc.InsertBotBalance(r); err != nil {
			t.Fatal(err)
		}
	}
	// Orders o1–o4 an hour ago (see ledgerFixture): tipsport-1 = 5000 + 110 − 50 + a 1000 deposit = 6060.
	ledgerFixture(t, svc, userID, codeID, now-3*hour)
	setSettings(t, svc, bots, userID, func(st *accounts.BotSettings) { st.MaxLeg = 2000 })
	dialBot(t, srv, code)
	waitBot(t, bots, userID, true)
	for _, at := range []int64{now - hour, now - 5*60000} { // the second read: same amount, not stored
		if s, b := postStatus(t, srv, code, protoStatus(7, winBal("tipsport", 1, "idle", bal(6060, at)))); s != 204 {
			t.Fatalf("status: %d %s", s, b)
		}
	}
	s, body := request(t, srv, "GET", "/api/me/bot/bankroll?range=7d", token, "", nil)
	var v bankrollView
	if err := json.Unmarshal([]byte(body), &v); s != 200 || err != nil {
		t.Fatalf("bankroll: %d %s", s, body)
	}
	accs := map[string]bankrollAccount{}
	for _, a := range v.Accounts {
		accs[a.WindowID] = a
	}
	tip, bet, fort := accs["tipsport-1"], accs["betano-1"], accs["fortuna-1"]
	if tip.AgentID != codeID || tip.Removed || *tip.Balance != 6060 || *tip.BalanceAt != now-5*60000 || *tip.Start != 5000 ||
		*tip.StartAt != t0 || tip.OpenStakes != 50 || *tip.ArbFlow != 60 || *tip.Outside != 1000 || !tip.Fresh || *tip.BalanceAge != 5*60000 ||
		*tip.MaxLeg != 2000 || *tip.Covers != 3 || !*tip.CanPay {
		t.Fatalf("tipsport-1: %s", mustJSON(tip))
	}
	// betano-1: no longer in the agent's windows; o3's open 40 is in its flow and open stakes.
	if !bet.Removed || *bet.ArbFlow != -142 || bet.OpenStakes != 40 || *bet.Outside != -10 || bet.Fresh || *bet.Covers != 0 || *bet.CanPay {
		t.Fatalf("betano-1: %s", mustJSON(bet))
	}
	if fort.Start != nil || fort.Balance != nil || fort.OpenStakes != 50 {
		t.Fatalf("fortuna-1: %s", mustJSON(fort))
	}
	tot := v.Total
	if *tot.Balance != 7908 || *tot.Start != 7000 || tot.OpenStakes != 140 || *tot.Outside != 990 || tot.ArbProfit != 8 || tot.Settled != 8 ||
		tot.Booked != 0 || tot.Orders != 3 || tot.Pending != 2 {
		t.Fatalf("total: %s", mustJSON(tot))
	}
	if len(v.Books) != 3 || v.Books[0].Bookmaker != "betano" || v.Books[1].Bookmaker != "fortuna" || v.Books[2].ArbProfit != 110 {
		t.Fatalf("books: %s", mustJSON(v.Books))
	}
	// Staked in the range: o1 202, o2 100, o3 40; 8 of arb profit. Only the removed betano-1 is old: not stale.
	if tot.Staked != 342 || tot.ProfitPct == nil || *tot.ProfitPct != 2.34 || tot.Stale {
		t.Fatalf("staked / profit %% / stale: %s", mustJSON(tot))
	}
	if v.Counts != (bankrollCounts{Pending: 2, Booked: 1, Settled: 1, Open: 2}) {
		t.Fatalf("counts: %s", mustJSON(v.Counts))
	}
	if len(v.Agents) != 1 || v.Agents[0] != (bankrollAgent{AgentID: codeID, Name: "PC", Online: true, Accounts: 2}) {
		t.Fatalf("agents: %s", mustJSON(v.Agents))
	}
	// Event points: point 0 (both accounts), the three placings, o1's settling, betano's and tipsport's reads, now.
	ats := []int64{t0, now - 3*hour + 1020, now - 3*hour + 2020, now - 3*hour + 3020, now - 3*hour + 5000, now - 2*hour, now - hour, now}
	if len(v.Series) != len(ats) {
		t.Fatalf("series: %s", mustJSON(v.Series))
	}
	for i, at := range ats {
		if v.Series[i].At != at {
			t.Fatalf("series point %d at %d, want %d", i, v.Series[i].At, at)
		}
	}
	if p := v.Series[0]; *p.Balance != 7000 || p.ArbProfit != 0 || *p.Outside != 0 {
		t.Fatalf("series at point 0: %s", mustJSON(p))
	}
	if p := v.Series[7]; *p.Balance != 7908 || p.ArbProfit != 8 || p.Booked != 0 || *p.Outside != 990 {
		t.Fatalf("series now: %s", mustJSON(p))
	}
	// o1 booked (min(210, 209.1) − 202) and o2 booked at 0 (min(100, 105) − 100) until o1 settles at 8.
	if p := v.Series[3]; p.ArbProfit != 7.1 || p.Booked != 7.1 {
		t.Fatalf("series before o1 settled: %s", mustJSON(p))
	}
	if p := v.Series[4]; p.ArbProfit != 8 || p.Booked != 0 {
		t.Fatalf("series after o1 settled: %s", mustJSON(p))
	}
	if tip, bet := v.BookSeries["tipsport"], v.BookSeries["betano"]; len(v.BookSeries) != 2 || len(tip) != 8 || *tip[7].Balance != 6060 ||
		*tip[0].Balance != 5000 || len(bet) != 8 || *bet[7].Balance != 1848 || tip[7].At != now {
		t.Fatalf("bookSeries: %s", mustJSON(v.BookSeries))
	}
	for rng, n := range map[string]int{"": 8, "30d": 8, "all": 8} {
		s, body := request(t, srv, "GET", "/api/me/bot/bankroll?range="+rng, token, "", nil)
		var v bankrollView
		if json.Unmarshal([]byte(body), &v); s != 200 || len(v.Series) != n {
			t.Errorf("range %q: %d points (%d), want %d", rng, len(v.Series), s, n)
		}
	}
	if s, body := request(t, srv, "GET", "/api/me/bot/bankroll?range=1y", token, "", nil); s != 400 || !strings.Contains(body, "bad_query") {
		t.Fatalf("bad range: %d %s", s, body)
	}
}

// Agent scope: the agents list (agents with an account or an order, by name), ?agent= limiting every figure, count,
// curve and order to that agent, an agent without data, and 404 unknown_agent for another user's code, an unknown id
// and a bad value, on both GETs.
func TestBankroll_AgentScope(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	userID := userIDOf(t, svc, token)
	_, codeB := newBotCode(t, srv, token, "beta")
	_, codeA := newBotCode(t, srv, token, "Alpha")
	_, codeC := newBotCode(t, srv, token, "Gamma") // no data
	other := newUser(t, svc, "o@example.com", accounts.RoleUser)
	_, codeX := newBotCode(t, srv, other, "Other")
	clk := &botClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	bots.now = clk.now
	now := clk.now().UnixMilli()
	base := now - 3600000
	for _, r := range []accounts.BotBalanceRow{
		{CodeID: codeA, WindowID: "tipsport-1", Bookmaker: "tipsport", Amount: 1000, At: base},
		{CodeID: codeB, WindowID: "betano-1", Bookmaker: "betano", Amount: 2000, At: base},
	} {
		r.UserID = userID
		if err := svc.InsertBotBalance(r); err != nil {
			t.Fatal(err)
		}
	}
	// b1 on agent B (booked: min(200, 199.5) − 195), then o1–o4 on agent A.
	if err := svc.InsertBotOrder(accounts.BotOrderRow{ID: "b1", UserID: userID, CodeID: codeB, ArbKey: "k", State: "placed", Source: "app",
		InfoJSON: `{"match":"C – D","market":"Winner","marketType":"MATCH_WINNER"}`, OddsDrop: "strict", CreatedAt: base + 500, SentAt: base + 510,
		EndedAt: base + 520, LegsJSON: `[{"bookmaker":"tipsport","label":"C","odds":2,"stake":100,"windowId":"tipsport-1","placed":true},` +
			`{"bookmaker":"betano","label":"D","odds":2.1,"stake":95,"windowId":"betano-1","placed":true}]`}); err != nil {
		t.Fatal(err)
	}
	ledgerFixture(t, svc, userID, codeA, base)
	get := func(query string) bankrollView {
		t.Helper()
		s, body := request(t, srv, "GET", "/api/me/bot/bankroll"+query, token, "", nil)
		var v bankrollView
		if err := json.Unmarshal([]byte(body), &v); s != 200 || err != nil {
			t.Fatalf("bankroll%s: %d %s", query, s, body)
		}
		return v
	}
	all := get("?range=all")
	want := []bankrollAgent{{AgentID: codeA, Name: "Alpha", Accounts: 1}, {AgentID: codeB, Name: "beta", Accounts: 1}}
	if !slices.Equal(all.Agents, want) {
		t.Fatalf("agents: %s", mustJSON(all.Agents))
	}
	if all.Total.Orders != 4 || all.Counts.Booked != 2 {
		t.Fatalf("all agents: %s %s", mustJSON(all.Total), mustJSON(all.Counts))
	}

	a := get("?range=all&agent=" + strconv.FormatInt(codeA, 10))
	if !slices.Equal(a.Agents, want) {
		t.Fatalf("agents with agent=A: %s", mustJSON(a.Agents))
	}
	for _, acc := range a.Accounts {
		if acc.AgentID != codeA {
			t.Fatalf("agent A lists %s", mustJSON(acc))
		}
	}
	if a.Total.Orders != 3 || *a.Total.Balance != 1000 || a.Total.Staked != 342 || a.Counts != (bankrollCounts{Pending: 2, Booked: 1, Settled: 1, Open: 2}) ||
		len(a.BookSeries) != 1 || a.BookSeries["tipsport"] == nil || *a.Series[len(a.Series)-1].Balance != 1000 {
		t.Fatalf("agent A: %s %s %s", mustJSON(a.Total), mustJSON(a.Counts), mustJSON(a.BookSeries))
	}
	b := get("?range=all&agent=" + strconv.FormatInt(codeB, 10))
	for _, acc := range b.Accounts {
		if acc.AgentID != codeB {
			t.Fatalf("agent B lists %s", mustJSON(acc))
		}
	}
	if b.Total.Orders != 1 || *b.Total.Balance != 2000 || b.Total.Booked != 4.5 || b.Counts != (bankrollCounts{Booked: 1, Open: 1}) ||
		len(b.BookSeries) != 1 || b.BookSeries["betano"] == nil {
		t.Fatalf("agent B: %s %s %s", mustJSON(b.Total), mustJSON(b.Counts), mustJSON(b.BookSeries))
	}
	c := get("?agent=" + strconv.FormatInt(codeC, 10))
	if len(c.Accounts) != 0 || len(c.Books) != 0 || c.Total.Orders != 0 || c.Total.Balance != nil || len(c.Series) != 1 || len(c.Agents) != 2 {
		t.Fatalf("agent C (no data): %s", mustJSON(c))
	}

	if got := orderIDs(getBankrollOrders(t, srv, token, "?agent="+strconv.FormatInt(codeA, 10))); got != "o4,o3,o2,o1" {
		t.Fatalf("orders of A: %s", got)
	}
	ob := getBankrollOrders(t, srv, token, "?agent="+strconv.FormatInt(codeB, 10))
	if orderIDs(ob) != "b1" || ob.Orders[0].AgentID != codeB || ob.Orders[0].AgentName != "beta" || ob.Orders[0].Day != "2026-11-26" {
		t.Fatalf("orders of B: %s", mustJSON(ob.Orders))
	}
	if got := getBankrollOrders(t, srv, token, "?agent="+strconv.FormatInt(codeC, 10)); len(got.Orders) != 0 {
		t.Fatalf("orders of C: %s", orderIDs(got))
	}
	if got := getBankrollOrders(t, srv, token, ""); orderIDs(got) != "o4,o3,o2,o1,b1" {
		t.Fatalf("orders of all: %s", orderIDs(got))
	}
	for _, q := range []string{strconv.FormatInt(codeX, 10), "999", "abc", "0", "-1"} {
		for _, path := range []string{"/api/me/bot/bankroll?agent=", "/api/me/bot/bankroll/orders?agent="} {
			if s, body := request(t, srv, "GET", path+q, token, "", nil); s != 404 || !strings.Contains(body, "unknown_agent") {
				t.Errorf("%s%s: %d %s", path, q, s, body)
			}
		}
	}
}

// Retention never touches the bankroll tables.
func TestBankroll_PruneKeepsTables(t *testing.T) {
	_, svc, bots, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	userID := userIDOf(t, svc, token)
	ledgerFixture(t, svc, userID, 0, 1000)
	if err := svc.InsertBotBalance(accounts.BotBalanceRow{UserID: userID, WindowID: "tipsport-1", Bookmaker: "tipsport", Amount: 1, At: 1}); err != nil {
		t.Fatal(err)
	}
	bots.now = func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }
	bots.prune()
	for table, want := range map[string]int{"bot_balances": 1, "bot_accounts": 1, "bot_ledger_legs": 5} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows (%v), want %d", table, n, err, want)
		}
	}
}
