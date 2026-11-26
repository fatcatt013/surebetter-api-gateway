package main

import (
	"database/sql"
	"net/http/httptest"
	"strconv"
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
	if legs := ledgerOf(t, svc, userID); len(legs) != 3 || legs[2].OrderID != id2 || legs[2].Leg != 0 {
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
