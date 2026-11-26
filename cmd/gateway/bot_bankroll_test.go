package main

import (
	"math"
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
		status  string
		booked  *float64
		profit  *float64
		pending bool
	}{
		{"two open legs: booked", []accounts.BotLedgerLeg{a, b}, false, "booked", fp(math.Min(210, 209.1) - 202), nil, false},
		{"one settled, one open: still booked", []accounts.BotLedgerLeg{settle(a, "won", now), b}, false, "booked", fp(7.1), nil, false},
		{"both settled", []accounts.BotLedgerLeg{settle(a, "won", now), settle(b, "lost", now)}, false, "settled", nil, fp(8), false},
		{"half results", []accounts.BotLedgerLeg{settle(voidA, "half_won", now), settle(voidB, "half_lost", now)}, false, "settled", nil,
			fp(155 + 51 - 202), false},
		{"voidable open: pending", []accounts.BotLedgerLeg{voidA, voidB}, false, "pending", nil, nil, true},
		{"one-sided open: pending", []accounts.BotLedgerLeg{a}, false, "pending", nil, nil, true},
		{"one-sided settled", []accounts.BotLedgerLeg{settle(a, "void", now)}, false, "settled", nil, fp(0), false},
		{"booked but open > 24 h", []accounts.BotLedgerLeg{func() accounts.BotLedgerLeg { l := a; l.PlacedAt = now - 25*h; return l }(), b},
			false, "pending", fp(7.1), nil, true},
		{"check order without rows", nil, true, "pending", nil, nil, true},
	}
	for _, c := range cases {
		o := &ledgerOrder{legs: c.legs, check: c.check}
		e := o.eval(now)
		if e.status != c.status || e.pending != c.pending || !sameMoney(e.booked, c.booked) || !sameMoney(e.profit, c.profit) {
			t.Errorf("%s: status %s pending %v booked %v profit %v, want %s %v %v %v", c.name, e.status, e.pending, deref(e.booked),
				deref(e.profit), c.status, c.pending, deref(c.booked), deref(c.profit))
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
	f := bankrollFigures(accs, latest, []*ledgerOrder{o0, o1, o2, o3}, present, now)
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
}

// The curve: one point at each Prague day end in the range plus now; balance and outside from the last stored read ≤ T,
// arb profit counting a booking once both legs are placed and a settlement once its last leg settled.
func TestBankrollSeries(t *testing.T) {
	day := func(d, hour int) int64 { return time.Date(2026, 11, d, hour, 0, 0, 0, prague).UnixMilli() }
	now := day(26, 18)
	pts := seriesPoints("7d", now, 0)
	if len(pts) != 8 || pts[0] != day(20, 0) || pts[6] != day(26, 0) || pts[7] != now {
		t.Fatalf("7d points: %v", pts)
	}
	if pts := seriesPoints("all", now, day(25, 10)); len(pts) != 2 || pts[0] != day(26, 0) {
		t.Fatalf("all points: %v", pts)
	}
	if pts := seriesPoints("all", now, 0); len(pts) != 1 || pts[0] != now {
		t.Fatalf("all without data: %v", pts)
	}
	if pts := seriesPoints("30d", now, 0); len(pts) != 31 {
		t.Fatalf("30d points: %d", len(pts))
	}
	accs := []accounts.BotAccountRow{{CodeID: 1, WindowID: "tipsport-1", Bookmaker: "tipsport", StartAmount: 1000, StartAt: day(24, 10)}}
	hist := []accounts.BotBalanceRow{
		{CodeID: 1, WindowID: "tipsport-1", Amount: 1000, At: day(24, 10)},
		{CodeID: 1, WindowID: "tipsport-1", Amount: 900, At: day(24, 12)},  // a leg of 100 placed at 11
		{CodeID: 1, WindowID: "tipsport-1", Amount: 1410, At: day(25, 12)}, // won 210, deposit 300
	}
	a := lleg("o", 0, "tipsport", 2.1, 100, day(24, 11))
	b := lleg("o", 1, "betano", 2.05, 102, day(25, 1)) // second leg after midnight: booked only from the 25th
	o := &ledgerOrder{legs: []accounts.BotLedgerLeg{settle(a, "won", day(25, 11)), settle(b, "lost", day(26, 9))}}
	got := bankrollSeries(accs, hist, []*ledgerOrder{o}, []int64{day(24, 0), day(25, 0), day(26, 0), now})
	want := []struct {
		balance, outside *float64
		profit           float64
	}{
		{nil, nil, 0},                  // before point 0
		{fp(900), fp(0), 0},            // one leg placed: no booking yet
		{fp(1410), fp(300), 7.1},       // booked (min(210, 209.1) − 202)
		{fp(1410), fp(300), 210 - 202}, // settled
	}
	for i, w := range want {
		p := got[i]
		if !sameMoney(p.Balance, w.balance) || !sameMoney(p.Outside, w.outside) || math.Abs(p.ArbProfit-w.profit) > 0.005 {
			t.Errorf("point %d: balance %v outside %v profit %v, want %v %v %v", i, deref(p.Balance), deref(p.Outside), p.ArbProfit,
				deref(w.balance), deref(w.outside), w.profit)
		}
	}
}
