package main

import (
	"math"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

func TestVoidable(t *testing.T) {
	cases := []struct {
		market string
		line   *float64
		want   bool
	}{
		{"DRAW_NO_BET", nil, true},
		{"HALF_1_DNB", nil, true},
		{"REGULATION_DNB", fp(0), true},
		{"ASIAN_HANDICAP", fp(0), true},
		{"ASIAN_HANDICAP", fp(-1), true},
		{"ASIAN_HANDICAP", fp(0.25), true},
		{"ASIAN_HANDICAP", fp(-1.75), true},
		{"ASIAN_HANDICAP", fp(1.5), false},
		{"HANDICAP", fp(-2.5), false},
		{"TOTAL_GOALS", fp(2.5), false},
		{"TOTAL_GOALS", fp(3), true},
		{"TOTAL_GOALS", fp(2.75), true},
		{"TOTAL_GOALS", fp(2.5000000001), false}, // within the tolerance
		{"ODD_EVEN_TOTAL", fp(2), false},
		{"ODD_EVEN", nil, false},
		{"MATCH_WINNER", nil, false},
		{"BTTS", nil, false},
		{"DOUBLE_CHANCE", nil, false},
		{"GAME_HANDICAP", nil, true}, // a line market without its line
		{"PUCK_LINE", fp(-1.5), false},
		{"PUCK_LINE", fp(-1), true},
		{"PLAYER_POINTS", fp(20), true},
		{"PLAYER_POINTS", fp(20.5), false},
		{"TEAM_TOTAL_HOME", fp(1.5), false},
	}
	for _, c := range cases {
		line := "nil"
		if c.line != nil {
			line = strconv.FormatFloat(*c.line, 'g', -1, 64)
		}
		if got := voidable(c.market, c.line); got != c.want {
			t.Errorf("voidable(%s, %s) = %v, want %v", c.market, line, got, c.want)
		}
	}
}

func TestLegPayout(t *testing.T) {
	cases := []struct {
		result string
		stake  float64
		odds   float64
		cashed *float64
		want   *float64
	}{
		{"open", 100, 2.1, nil, nil},
		{"won", 100, 2.1, nil, fp(210)},
		{"lost", 100, 2.1, nil, fp(0)},
		{"void", 100, 2.1, nil, fp(100)},
		{"half_won", 100, 2.1, nil, fp(155)},
		{"half_lost", 100, 2.1, nil, fp(50)},
		{"cashed_out", 100, 2.1, fp(87.5), fp(87.5)},
		{"won", 33.33, 1.333, nil, fp(44.43)}, // 44.42889 → 2 decimals
	}
	for _, c := range cases {
		got := legPayout(c.result, c.stake, c.odds, c.cashed)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("legPayout(%s, %v, %v) = %v, want %v", c.result, c.stake, c.odds, deref(got), deref(c.want))
		}
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// lleg is a ledger leg of order at book (window <book>-1, code 1) placed at placedAt.
func lleg(order string, leg int, book string, odds, stake float64, placedAt int64) accounts.BotLedgerLeg {
	return accounts.BotLedgerLeg{OrderID: order, Leg: leg, CodeID: 1, WindowID: book + "-1", Bookmaker: book, Source: "app",
		Odds: odds, Stake: stake, PlacedAt: placedAt, Result: "open", MarketType: "MATCH_WINNER"}
}

// settle sets l's result, its payout per legPayout and settledAt.
func settle(l accounts.BotLedgerLeg, result string, at int64) accounts.BotLedgerLeg {
	l.Result, l.SettledAt = result, ip(at)
	l.Payout = legPayout(result, l.Stake, l.Odds, fp(0))
	return l
}

// np marks l not placed (the owner's answer for a leg of a check order): no payout.
func np(l accounts.BotLedgerLeg, at int64) accounts.BotLedgerLeg {
	l.Result, l.SettledAt, l.Payout = notPlaced, ip(at), nil
	return l
}

func TestLedgerOrderEval(t *testing.T) {
	const h = int64(3600000)
	now := 100 * h
	a := lleg("o", 0, "tipsport", 2.1, 100, now-h)
	b := lleg("o", 1, "betano", 2.05, 102, now-h)
	voidA, voidB := a, b
	voidA.Voidable, voidB.Voidable = true, true
	cases := []struct {
		name    string
		legs    []accounts.BotLedgerLeg
		check   bool
		state   string // the order's state
		stored  int    // its stored legs
		status  string
		booked  *float64
		profit  *float64
		pending bool
		reason  string
	}{
		{"two open legs: booked", []accounts.BotLedgerLeg{a, b}, false, "placed", 2, "booked", fp(math.Min(210, 209.1) - 202), nil, false, ""},
		{"one settled, one open: still booked", []accounts.BotLedgerLeg{settle(a, "won", now), b}, false, "placed", 2, "booked", fp(7.1), nil, false, ""},
		{"both settled", []accounts.BotLedgerLeg{settle(a, "won", now), settle(b, "lost", now)}, false, "placed", 2, "settled", nil, fp(8), false, ""},
		{"half results", []accounts.BotLedgerLeg{settle(voidA, "half_won", now), settle(voidB, "half_lost", now)}, false, "placed", 2, "settled", nil,
			fp(155 + 51 - 202), false, ""},
		{"voidable open: pending", []accounts.BotLedgerLeg{voidA, voidB}, false, "placed", 2, "pending", nil, nil, true, "voidable"},
		{"one-sided open: pending", []accounts.BotLedgerLeg{a}, false, "stopped", 2, "pending", nil, nil, true, "one_sided"},
		{"voidable before one-sided", []accounts.BotLedgerLeg{voidA}, false, "stopped", 2, "pending", nil, nil, true, "voidable"},
		{"one-sided before overdue", []accounts.BotLedgerLeg{func() accounts.BotLedgerLeg { l := a; l.PlacedAt = now - 25*h; return l }()},
			false, "stopped", 2, "pending", nil, nil, true, "one_sided"},
		{"one-sided settled", []accounts.BotLedgerLeg{settle(a, "void", now)}, false, "stopped", 2, "settled", nil, fp(0), false, ""},
		{"booked but open > 24 h", []accounts.BotLedgerLeg{func() accounts.BotLedgerLeg { l := a; l.PlacedAt = now - 25*h; return l }(), b},
			false, "placed", 2, "pending", fp(7.1), nil, true, "overdue"},
		{"check order without rows", nil, true, "outcome_unknown", 2, "pending", nil, nil, true, "outcome_unknown"},
		{"stuck order without rows", nil, true, "placed", 2, "pending", nil, nil, true, "stuck"},
		{"check order, a leg without a row", []accounts.BotLedgerLeg{settle(b, "lost", now)}, true, "outcome_unknown", 2, "pending", nil, nil, true,
			"outcome_unknown"},
		{"stuck order, a leg without a row", []accounts.BotLedgerLeg{settle(b, "lost", now)}, true, "stopped", 2, "pending", nil, nil, true, "stuck"},
		{"check order, the other leg not placed", []accounts.BotLedgerLeg{np(a, now), settle(b, "lost", now)}, true, "outcome_unknown", 2, "settled",
			nil, fp(-102), false, ""},
		{"check order, both legs not placed", []accounts.BotLedgerLeg{np(a, now), np(b, now)}, true, "outcome_unknown", 2, "settled", nil, fp(0),
			false, ""},
		{"check order, one not placed, one open", []accounts.BotLedgerLeg{np(a, now), b}, true, "outcome_unknown", 2, "pending", nil, nil, true,
			"one_sided"},
		{"check order resolved, voidable open", []accounts.BotLedgerLeg{voidA, voidB}, true, "outcome_unknown", 2, "pending", nil, nil, true,
			"voidable"},
		{"check order resolved, booked", []accounts.BotLedgerLeg{a, b}, true, "placed", 2, "booked", fp(7.1), nil, false, ""},
	}
	for _, c := range cases {
		o := &ledgerOrder{row: accounts.BotOrderRow{State: c.state}, stored: make([]storedLeg, c.stored), legs: c.legs, check: c.check}
		e := o.eval(now)
		if e.status != c.status || e.pending != c.pending || e.reason != c.reason || !sameMoney(e.booked, c.booked) || !sameMoney(e.profit, c.profit) {
			t.Errorf("%s: status %s pending %v reason %q booked %v profit %v, want %s %v %q %v %v", c.name, e.status, e.pending, e.reason,
				deref(e.booked), deref(e.profit), c.status, c.pending, c.reason, deref(c.booked), deref(c.profit))
		}
	}
}

// An order's day is the Prague date of its creation.
func TestPragueDate(t *testing.T) {
	for at, want := range map[time.Time]string{
		time.Date(2026, 11, 26, 22, 59, 59, 0, time.UTC): "2026-11-26",
		time.Date(2026, 11, 26, 23, 0, 0, 0, time.UTC):   "2026-11-27",
		time.Date(2026, 7, 1, 21, 59, 0, 0, time.UTC):    "2026-07-01", // summer time: UTC+2
		time.Date(2026, 7, 1, 22, 0, 0, 0, time.UTC):     "2026-07-02",
	} {
		if got := pragueDate(at.UnixMilli()); got != want {
			t.Errorf("pragueDate(%s) = %s, want %s", at, got, want)
		}
	}
}

func sameMoney(a, b *float64) bool {
	return (a == nil) == (b == nil) && (a == nil || math.Abs(*a-*b) < 0.005)
}

// Two Tipsport accounts and one Betano account: an account's balance, point 0, open stakes, arb flow and outside money;
// a deposit between two reads is outside money; an account without point 0 is listed with nulls and left out of sums.
func TestBankrollFigures(t *testing.T) {
	const h = int64(3600000)
	now := 61 * h
	accs := []accounts.BotAccountRow{
		{CodeID: 1, WindowID: "tipsport-1", Bookmaker: "tipsport", StartAmount: 5000, StartAt: 10 * h},
		{CodeID: 1, WindowID: "betano-1", Bookmaker: "betano", StartAmount: 2000, StartAt: 10 * h},
		{CodeID: 2, WindowID: "tipsport-1", Bookmaker: "tipsport", StartAmount: 1000, StartAt: 10 * h},
	}
	// Order 1 (settled): tipsport-1 won 100 @ 2.1, betano-1 lost 102 @ 2.05. Then the owner deposits 1000 at Tipsport.
	// Order 2 (booked): tipsport-1 50 @ 2.0, betano-1 50 @ 2.1. A leg before point 0 (order 0) stays out of the flow.
	o1 := &ledgerOrder{legs: []accounts.BotLedgerLeg{settle(lleg("o1", 0, "tipsport", 2.1, 100, 20*h), "won", 30*h),
		settle(lleg("o1", 1, "betano", 2.05, 102, 20*h), "lost", 30*h)}}
	o2 := &ledgerOrder{legs: []accounts.BotLedgerLeg{lleg("o2", 0, "tipsport", 2.0, 50, 40*h), lleg("o2", 1, "betano", 2.1, 50, 40*h)}}
	old := lleg("o0", 0, "tipsport", 3, 10, 5*h)
	old.Leg = 0
	o0 := &ledgerOrder{legs: []accounts.BotLedgerLeg{settle(old, "lost", 6*h), settle(lleg("o0", 1, "betano", 1.5, 20, 5*h), "won", 6*h)}}
	// A third window with legs but no balance (an old agent): no point 0.
	o3 := &ledgerOrder{legs: []accounts.BotLedgerLeg{func() accounts.BotLedgerLeg {
		l := lleg("o3", 0, "fortuna", 2, 30, 50*h)
		l.CodeID = 3
		return l
	}()}}
	// Balances: tipsport-1 = 5000 + 110 (order 1) + 1000 (deposit) − 50 (order 2) = 6060; betano-1 = 2000 − 102 − 50.
	latest := []accounts.BotBalanceRow{
		{CodeID: 1, WindowID: "tipsport-1", Bookmaker: "tipsport", Amount: 6060, At: 60 * h},
		{CodeID: 1, WindowID: "betano-1", Bookmaker: "betano", Amount: 1848, At: 60 * h},
		{CodeID: 2, WindowID: "tipsport-1", Bookmaker: "tipsport", Amount: 1000, At: 10 * h},
	}
	present := map[acctKey]bool{{1, "tipsport-1"}: true, {1, "betano-1"}: true}
	f := bankrollFigures(accs, latest, []*ledgerOrder{o0, o1, o2, o3}, present, now, nil)
	byKey := map[acctKey]bankrollAccount{}
	for _, a := range f.Accounts {
		byKey[acctKey{a.AgentID, a.WindowID}] = a
	}
	tip := byKey[acctKey{1, "tipsport-1"}]
	if *tip.Balance != 6060 || *tip.Start != 5000 || tip.OpenStakes != 50 || *tip.ArbFlow != 60 || *tip.Outside != 1000 || tip.Removed {
		t.Fatalf("tipsport-1: %+v", tip)
	}
	bet := byKey[acctKey{1, "betano-1"}]
	if *bet.ArbFlow != -152 || *bet.Outside != 0 || bet.OpenStakes != 50 {
		t.Fatalf("betano-1: %+v", bet)
	}
	if tip2 := byKey[acctKey{2, "tipsport-1"}]; !tip2.Removed || *tip2.Outside != 0 {
		t.Fatalf("code 2 tipsport-1: %+v", tip2)
	}
	fort := byKey[acctKey{3, "fortuna-1"}]
	if fort.Balance != nil || fort.Start != nil || fort.ArbFlow != nil || fort.Outside != nil || fort.OpenStakes != 30 || fort.Bookmaker != "fortuna" {
		t.Fatalf("fortuna-1 without point 0: %+v", fort)
	}
	if len(f.Accounts) != 4 {
		t.Fatalf("accounts: %+v", f.Accounts)
	}
	books := map[string]bankrollBook{}
	for _, b := range f.Books {
		books[b.Bookmaker] = b
	}
	// Per book: order 1's legs count payout − stake (+110 / −102), order 2's booking (100 − 100 = 0 → 0/2 each), order 0
	// (before point 0) still counts as arb profit (−10 / +10).
	if b := books["tipsport"]; *b.Balance != 7060 || *b.Start != 6000 || *b.Outside != 1000 || b.ArbProfit != 100 || b.OpenStakes != 50 {
		t.Fatalf("tipsport book: %+v", b)
	}
	if b := books["fortuna"]; b.Balance != nil || b.Start != nil || b.Outside != nil || b.OpenStakes != 30 {
		t.Fatalf("fortuna book: %+v", b)
	}
	tot := f.Total
	if *tot.Balance != 8908 || *tot.Start != 8000 || *tot.ArbFlow != -92 || *tot.Outside != 1000 || tot.OpenStakes != 130 ||
		tot.Settled != 8 || tot.Booked != 0 || tot.ArbProfit != 8 || tot.Orders != 4 || tot.Pending != 1 {
		t.Fatalf("total: %+v", tot)
	}
	// Staked: every real leg placed, order 0 (before point 0) too, the same base as arb profit: 30 + 202 + 100 + 30; arb
	// profit 8 of it. Every read is an hour old: the total is stale.
	if tot.Staked != 362 || tot.ProfitPct == nil || *tot.ProfitPct != 2.21 || !tot.Stale {
		t.Fatalf("staked / profit %% / stale: %+v %v", tot, deref(tot.ProfitPct))
	}
	if f.Counts != (bankrollCounts{Pending: 1, Booked: 1, Settled: 2, Open: 2}) {
		t.Fatalf("counts: %+v", f.Counts)
	}
	if f := bankrollFigures(accs, latest, nil, present, now, nil); f.Total.Staked != 0 || f.Total.ProfitPct != nil {
		t.Fatalf("nothing staked: %+v", f.Total)
	}
}

// Freshness (a read at most 12 min old), the read's age, the max leg's cover and whether the account can pay it, per
// account and per book; a removed window never makes the total stale; a not-placed leg is in no figure.
func TestBankrollFreshCover(t *testing.T) {
	const h, fresh = int64(3600000), balanceFreshMs
	now := 100 * h
	accs := []accounts.BotAccountRow{
		{CodeID: 1, WindowID: "tipsport-1", Bookmaker: "tipsport", StartAmount: 5000, StartAt: h},
		{CodeID: 1, WindowID: "tipsport-2", Bookmaker: "tipsport", StartAmount: 1000, StartAt: h},
		{CodeID: 1, WindowID: "betano-1", Bookmaker: "betano", StartAmount: 2000, StartAt: h},
		{CodeID: 2, WindowID: "fortuna-1", Bookmaker: "fortuna", StartAmount: 500, StartAt: h},
	}
	latest := func(tip2At int64) []accounts.BotBalanceRow {
		return []accounts.BotBalanceRow{
			{CodeID: 1, WindowID: "tipsport-1", Bookmaker: "tipsport", Amount: 6060, At: now - fresh},
			{CodeID: 1, WindowID: "tipsport-2", Bookmaker: "tipsport", Amount: 1000, At: tip2At},
			{CodeID: 1, WindowID: "betano-1", Bookmaker: "betano", Amount: 2400, At: now - 1000},
			{CodeID: 2, WindowID: "fortuna-1", Bookmaker: "fortuna", Amount: 300, At: now - 10*h},
		}
	}
	present := map[acctKey]bool{{1, "tipsport-1"}: true, {1, "tipsport-2"}: true, {1, "betano-1"}: true} // fortuna-1 removed
	// A check order: its tipsport-1 leg never reached the book (not placed), its betano-1 leg is open.
	chk := &ledgerOrder{row: accounts.BotOrderRow{State: "outcome_unknown"}, check: true, stored: make([]storedLeg, 2),
		legs: []accounts.BotLedgerLeg{np(lleg("c", 0, "tipsport", 2, 999, 50*h), 60*h), lleg("c", 1, "betano", 2, 100, 50*h)}}
	f := bankrollFigures(accs, latest(now-fresh-1), []*ledgerOrder{chk}, present, now, fp(2500))
	by := map[string]bankrollAccount{}
	for _, a := range f.Accounts {
		by[a.WindowID] = a
	}
	type want struct {
		fresh  bool
		age    int64
		covers int64
		canPay bool
	}
	for w, x := range map[string]want{"tipsport-1": {true, fresh, 2, true}, "tipsport-2": {false, fresh + 1, 0, false},
		"betano-1": {true, 1000, 0, false}, "fortuna-1": {false, 10 * h, 0, false}} {
		a := by[w]
		if a.Fresh != x.fresh || a.BalanceAge == nil || *a.BalanceAge != x.age || a.MaxLeg == nil || *a.MaxLeg != 2500 || a.Covers == nil ||
			*a.Covers != x.covers || a.CanPay == nil || *a.CanPay != x.canPay {
			t.Errorf("%s: %s, want %+v", w, mustJSON(a), x)
		}
	}
	if t1 := by["tipsport-1"]; t1.OpenStakes != 0 || *t1.ArbFlow != 0 || *t1.Outside != 1060 {
		t.Errorf("tipsport-1 with a not-placed leg: %s", mustJSON(t1))
	}
	if b := by["betano-1"]; b.OpenStakes != 100 || *b.ArbFlow != -100 {
		t.Errorf("betano-1: %s", mustJSON(b))
	}
	tot := f.Total
	if !tot.Stale || tot.Staked != 100 || tot.OpenStakes != 100 || tot.Orders != 1 || tot.Pending != 1 {
		t.Fatalf("total: %s", mustJSON(tot))
	}
	if f.Counts != (bankrollCounts{Pending: 1, Open: 1}) {
		t.Fatalf("counts: %+v", f.Counts)
	}
	books := map[string]bankrollBook{}
	for _, b := range f.Books {
		books[b.Bookmaker] = b
	}
	// tipsport: 7060 covers 2 legs but tipsport-2's read is stale; betano 2400 < 2500; fortuna: only a removed window.
	for book, x := range map[string]want{"tipsport": {covers: 2}, "betano": {covers: 0}, "fortuna": {covers: 0}} {
		b := books[book]
		if b.MaxLeg == nil || *b.MaxLeg != 2500 || b.Covers == nil || *b.Covers != x.covers || b.CanPay == nil || *b.CanPay {
			t.Errorf("book %s: %s", book, mustJSON(b))
		}
	}

	// tipsport-2 read fresh: the total is no longer stale (the removed fortuna-1 does not count), tipsport can pay.
	f = bankrollFigures(accs, latest(now), []*ledgerOrder{chk}, present, now, fp(2500))
	if f.Total.Stale {
		t.Fatalf("stale with only a removed window old: %s", mustJSON(f.Total))
	}
	for _, b := range f.Books {
		if b.Bookmaker == "tipsport" && (b.CanPay == nil || !*b.CanPay) {
			t.Fatalf("tipsport fresh: %s", mustJSON(b))
		}
	}
	// No max leg: no cover, no can-pay.
	f = bankrollFigures(accs, latest(now), nil, present, now, nil)
	for _, a := range f.Accounts {
		if a.MaxLeg != nil || a.Covers != nil || a.CanPay != nil {
			t.Fatalf("no max leg: %s", mustJSON(a))
		}
	}
	// An account without a balance: not fresh, age null, no cover, cannot pay.
	f = bankrollFigures(accs[:1], nil, nil, present, now, fp(2500))
	if a := f.Accounts[0]; a.Fresh || a.BalanceAge != nil || a.Covers != nil || a.CanPay == nil || *a.CanPay || !f.Total.Stale {
		t.Fatalf("no balance: %s %s", mustJSON(a), mustJSON(f.Total))
	}
}

// The max leg: the auto bankroll's in maxleg mode, else the controller's limit, else none.
func TestMaxLegOf(t *testing.T) {
	auto := func(mode string, maxLeg float64) string {
		return `{"mode":"off","bankroll":{"mode":"` + mode + `","total":10000,"max_leg":` + strconv.FormatFloat(maxLeg, 'f', -1, 64) +
			`,"anchor_book":null,"rounding":1}}`
	}
	for _, c := range []struct {
		auto   string
		maxLeg int64
		want   *float64
	}{
		{auto("maxleg", 3000), 2000, fp(3000)},
		{auto("split", 3000), 2000, fp(2000)},
		{"", 2000, fp(2000)},
		{auto("maxleg", 3000), 0, fp(3000)},
		{auto("split", 0), 0, nil},
		{"", 0, nil},
	} {
		got := maxLegOf(accounts.BotSettings{AutoJSON: c.auto, MaxLeg: c.maxLeg})
		if !sameMoney(got, c.want) {
			t.Errorf("maxLegOf(%s, %d) = %v, want %v", c.auto, c.maxLeg, deref(got), deref(c.want))
		}
	}
}

// The curve's times: every balance read and every real leg's placing and settling in the range, plus now; sorted, once.
func TestSeriesTimes(t *testing.T) {
	day := func(d, hour int) int64 { return time.Date(2026, 11, d, hour, 0, 0, 0, prague).UnixMilli() }
	now := day(26, 18)
	hist := []accounts.BotBalanceRow{{At: day(24, 10)}, {At: day(24, 12)}, {At: day(25, 1)}, {At: day(25, 12)}}
	a := lleg("o", 0, "tipsport", 2.1, 100, day(24, 11))
	b := lleg("o", 1, "betano", 2.05, 102, day(25, 1)) // the same time as a read
	o := &ledgerOrder{legs: []accounts.BotLedgerLeg{settle(a, "won", day(25, 11)), b}}
	n := &ledgerOrder{check: true, legs: []accounts.BotLedgerLeg{np(lleg("n", 0, "fortuna", 2, 50, day(25, 5)), day(25, 6))}} // no event
	got := seriesTimes(hist, []*ledgerOrder{o, n}, day(25, 0), now)
	want := []int64{day(25, 1), day(25, 11), day(25, 12), now}
	if !slices.Equal(got, want) {
		t.Fatalf("from the 25th: %v, want %v", got, want)
	}
	got = seriesTimes(hist, []*ledgerOrder{o, n}, 0, now)
	want = []int64{day(24, 10), day(24, 11), day(24, 12), day(25, 1), day(25, 11), day(25, 12), now}
	if !slices.Equal(got, want) {
		t.Fatalf("all: %v, want %v", got, want)
	}
	if got := seriesTimes(nil, nil, 0, now); !slices.Equal(got, []int64{now}) {
		t.Fatalf("no data: %v", got)
	}
}

// Downsampling keeps the first time and the last time of each of 399 equal-width buckets: at most 400, the last kept.
func TestDownsample(t *testing.T) {
	even := make([]int64, 1000)
	for i := range even {
		even[i] = int64(i) * 1000
	}
	got := downsample(even, seriesMax)
	if len(got) != 400 || got[0] != 0 || got[399] != 999000 || !slices.IsSorted(got) {
		t.Fatalf("even: %d points, first %d, last %d", len(got), got[0], got[len(got)-1])
	}
	// Each kept time but the first is the last of its bucket: the next time of the input falls in a later bucket.
	width := float64(999000) / 399
	for _, v := range got[1 : len(got)-1] {
		i := slices.Index(even, v)
		if int(float64(even[i+1])/width) == int(float64(v)/width) {
			t.Fatalf("%d is not the last of its bucket", v)
		}
	}
	// Skewed: 990 times in the first second, 10 spread over a day.
	var skew []int64
	for i := range 990 {
		skew = append(skew, int64(i))
	}
	for i := 1; i <= 10; i++ {
		skew = append(skew, int64(i)*8640000)
	}
	got = downsample(skew, seriesMax)
	if len(got) > 400 || got[0] != 0 || got[len(got)-1] != 86400000 || len(got) != 12 {
		t.Fatalf("skewed: %v", got)
	}
	short := []int64{1, 2, 3}
	if got := downsample(short, seriesMax); !slices.Equal(got, short) {
		t.Fatalf("short: %v", got)
	}
}

// The curve at given times: balance and outside from each account's last read ≤ T, the arb profit with its booked part
// (a booking once both legs are placed, the settlement once its last leg settled), and each book's balance.
func TestBankrollSeries(t *testing.T) {
	day := func(d, hour int) int64 { return time.Date(2026, 11, d, hour, 0, 0, 0, prague).UnixMilli() }
	now := day(26, 18)
	accs := []accounts.BotAccountRow{
		{CodeID: 1, WindowID: "tipsport-1", Bookmaker: "tipsport", StartAmount: 1000, StartAt: day(24, 10)},
		{CodeID: 1, WindowID: "betano-1", Bookmaker: "betano", StartAmount: 300, StartAt: day(25, 6)},
	}
	hist := []accounts.BotBalanceRow{
		{CodeID: 1, WindowID: "tipsport-1", Amount: 1000, At: day(24, 10)},
		{CodeID: 1, WindowID: "tipsport-1", Amount: 900, At: day(24, 12)}, // a leg of 100 placed at 11
		{CodeID: 1, WindowID: "betano-1", Amount: 300, At: day(25, 6)},
		{CodeID: 1, WindowID: "tipsport-1", Amount: 1410, At: day(25, 12)}, // won 210, deposit 300
	}
	a := lleg("o", 0, "tipsport", 2.1, 100, day(24, 11))
	b := lleg("o", 1, "betano", 2.05, 102, day(25, 1)) // second leg after midnight (before betano's point 0): booked from the 25th
	o := &ledgerOrder{legs: []accounts.BotLedgerLeg{settle(a, "won", day(25, 11)), settle(b, "lost", day(26, 9))}}
	got, books := bankrollSeries(accs, hist, []*ledgerOrder{o}, []int64{day(24, 0), day(25, 0), day(26, 0), now})
	want := []struct {
		balance, outside *float64
		profit, booked   float64
	}{
		{nil, nil, 0, 0},                  // before point 0
		{fp(900), fp(0), 0, 0},            // one leg placed: no booking yet
		{fp(1710), fp(300), 7.1, 7.1},     // booked (min(210, 209.1) − 202)
		{fp(1710), fp(300), 210 - 202, 0}, // settled
	}
	if len(got) != len(want) {
		t.Fatalf("points: %d", len(got))
	}
	for i, w := range want {
		p := got[i]
		if !sameMoney(p.Balance, w.balance) || !sameMoney(p.Outside, w.outside) || math.Abs(p.ArbProfit-w.profit) > 0.005 ||
			math.Abs(p.Booked-w.booked) > 0.005 {
			t.Errorf("point %d: balance %v outside %v profit %v booked %v, want %v %v %v %v", i, deref(p.Balance), deref(p.Outside),
				p.ArbProfit, p.Booked, deref(w.balance), deref(w.outside), w.profit, w.booked)
		}
	}
	for book, w := range map[string][]*float64{"tipsport": {nil, fp(900), fp(1410), fp(1410)}, "betano": {nil, nil, fp(300), fp(300)}} {
		pts := books[book]
		if len(pts) != len(w) {
			t.Fatalf("book %s: %s", book, mustJSON(pts))
		}
		for i := range w {
			if pts[i].At != got[i].At || !sameMoney(pts[i].Balance, w[i]) {
				t.Errorf("book %s point %d: %s, want %v", book, i, mustJSON(pts[i]), deref(w[i]))
			}
		}
	}
	if len(books) != 2 {
		t.Fatalf("books: %s", mustJSON(books))
	}
}
