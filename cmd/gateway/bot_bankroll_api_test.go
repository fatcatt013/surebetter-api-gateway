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
	// o4 now has a real leg, settled: no longer pending (one-sided, settled); its other leg is still listed without a row.
	if got := getBankrollOrders(t, srv, token, "?status=pending"); orderIDs(got) != "o3" {
		t.Fatalf("pending after o4: %s", orderIDs(got))
	}
	got := getBankrollOrders(t, srv, token, "?status=settled")
	if orderIDs(got) != "o4,o1" || *got.Orders[0].Profit != -60 || len(got.Orders[0].Legs) != 2 || got.Orders[0].Legs[0].PlacedAt != nil ||
		*got.Orders[0].Legs[0].Result != "open" {
		t.Fatalf("settled after o4: %s %s", orderIDs(got), mustJSON(got.Orders))
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
	dialBot(t, srv, code)
	waitBot(t, bots, userID, true)
	for _, at := range []int64{now - hour, now - 50*60000} { // the second read: same amount, not stored
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
	if tip.AgentID != codeID || tip.Removed || *tip.Balance != 6060 || *tip.BalanceAt != now-50*60000 || *tip.Start != 5000 ||
		*tip.StartAt != t0 || tip.OpenStakes != 50 || *tip.ArbFlow != 60 || *tip.Outside != 1000 {
		t.Fatalf("tipsport-1: %s", mustJSON(tip))
	}
	// betano-1: no longer in the agent's windows; o3's open 40 is in its flow and open stakes.
	if !bet.Removed || *bet.ArbFlow != -142 || bet.OpenStakes != 40 || *bet.Outside != -10 {
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
	if len(v.Series) != 8 || v.Series[7].At != now || *v.Series[7].Balance != 7908 || v.Series[7].ArbProfit != 8 || *v.Series[7].Outside != 990 {
		t.Fatalf("series: %s", mustJSON(v.Series))
	}
	if v.Series[0].Balance != nil || v.Series[0].ArbProfit != 0 {
		t.Fatalf("series before point 0: %s", mustJSON(v.Series[0]))
	}
	for rng, n := range map[string]int{"": 8, "30d": 31, "all": 3} {
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
