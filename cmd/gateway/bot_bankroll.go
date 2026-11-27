package main

import (
	"math"
	"slices"
	"strings"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// The bot bankroll's money logic (bankroll Phase 1 contract, 2026-11-26): which markets can push or half-settle,
// each settled leg's payout, an order's booking, settlement and pending state, the per-account / per-book / total
// figures and the curve. Pure functions over the stored rows (bot_bankroll_api.go reads them and serves the API).

// voidable: the market can push or half-settle, so an order on it cannot be booked at once. DRAW_NO_BET and every
// *_DNB; a line market (HANDICAP, TOTAL, PUCK_LINE or PLAYER_*, never ODD_EVEN) whose line is unknown or a whole or
// quarter number (2·|line| not an odd integer).
func voidable(marketType string, line *float64) bool {
	if marketType == "DRAW_NO_BET" || strings.HasSuffix(marketType, "_DNB") {
		return true
	}
	lined := (strings.Contains(marketType, "HANDICAP") || strings.Contains(marketType, "TOTAL") ||
		strings.Contains(marketType, "PUCK_LINE") || strings.HasPrefix(marketType, "PLAYER_")) && !strings.Contains(marketType, "ODD_EVEN")
	if !lined {
		return false
	}
	if line == nil {
		return true
	}
	d := 2 * math.Abs(*line)
	r := math.Round(d)
	return !(math.Abs(d-r) < 1e-9 && math.Mod(r, 2) == 1)
}

// notPlaced is the result of a leg of a check order that the owner confirms never reached the bookmaker: not a real
// leg (no stake out, no money in any figure), payout null.
const notPlaced = "not_placed"

// ledgerResults are the results a ledger leg can have (not_placed only on a check order's leg).
var ledgerResults = map[string]bool{"open": true, "won": true, "lost": true, "void": true, "half_won": true, "half_lost": true,
	"cashed_out": true, notPlaced: true}

// legPayout is what a leg of stake at odds pays back with result (Kč, 2 decimals); cashed is the amount of a cash-out.
// nil while open.
func legPayout(result string, stake, odds float64, cashed *float64) *float64 {
	var v float64
	switch result {
	case "won":
		v = stake * odds
	case "lost":
		v = 0
	case "void":
		v = stake
	case "half_won":
		v = stake/2*odds + stake/2
	case "half_lost":
		v = stake / 2
	case "cashed_out":
		if cashed == nil {
			return nil
		}
		v = *cashed
	default:
		return nil
	}
	return roundPtr(v, 2)
}

// pendingAfterMs: a leg still open this long after it was placed puts its order on the Pending list.
const pendingAfterMs = 24 * 3600 * 1000

// ledgerOrder is one order as the bankroll reads it: its stored row and info, its ledger legs (by leg index; none
// for an order that only needs a check; not_placed ones included), its stored legs and whether it needs a check
// (outcome_unknown or a stuck last flow).
type ledgerOrder struct {
	row    accounts.BotOrderRow
	info   orderInfo
	stored []storedLeg // the order's legs as bot_orders keeps them
	legs   []accounts.BotLedgerLeg
	check  bool
}

// Pending reasons, in the order they are given (the first that applies).
const (
	pendingUnknown  = "outcome_unknown" // a check order (outcome_unknown) with a leg nobody answered yet
	pendingStuck    = "stuck"           // a check order (stuck flow) with a leg nobody answered yet
	pendingVoidable = "voidable"        // a real leg open on a market that can push
	pendingOneSided = "one_sided"       // a real leg open, the order's only one
	pendingOverdue  = "overdue"         // a real leg open more than 24 h after it was placed
)

// orderEval is an order's state for the bankroll at one time.
type orderEval struct {
	voidable bool
	oneSided bool     // exactly one real leg
	settled  bool     // every leg answered and every real leg settled
	booked   *float64 // the booking of an unsettled order of two real legs on a market that cannot push
	profit   *float64 // settled: Σ payout − Σ stake of the real legs
	pending  bool
	reason   string // why it is pending ("" when not)
	status   string // settled, pending, booked or open
	open     bool   // a real leg is open
}

func legSettled(l accounts.BotLedgerLeg) bool { return l.Result != "open" && l.Payout != nil }

// realLeg: the leg reached the bookmaker (every ledger leg but a not-placed one).
func realLeg(l accounts.BotLedgerLeg) bool { return l.Result != notPlaced }

// real is o's real legs.
func (o *ledgerOrder) real() []accounts.BotLedgerLeg {
	out := make([]accounts.BotLedgerLeg, 0, len(o.legs))
	for _, l := range o.legs {
		if realLeg(l) {
			out = append(out, l)
		}
	}
	return out
}

// unresolved: a check order with a stored leg that has no ledger row yet (nobody said whether it was placed), or with
// no row at all.
func (o *ledgerOrder) unresolved() bool {
	if !o.check {
		return false
	}
	if len(o.legs) == 0 {
		return true
	}
	have := map[int]bool{}
	for _, l := range o.legs {
		have[l.Leg] = true
	}
	for i := range o.stored {
		if !have[i] {
			return true
		}
	}
	return false
}

// isVoidable: a real leg is on a voidable market, or (no real legs) the order's market is.
func (o *ledgerOrder) isVoidable() bool {
	real := o.real()
	if len(real) == 0 {
		return voidable(o.info.MarketType, o.info.Line)
	}
	return slices.ContainsFunc(real, func(l accounts.BotLedgerLeg) bool { return l.Voidable })
}

// booking is min(stake·odds) − Σ stake of two legs.
func booking(legs []accounts.BotLedgerLeg) float64 {
	return math.Min(legs[0].Stake*legs[0].Odds, legs[1].Stake*legs[1].Odds) - (legs[0].Stake + legs[1].Stake)
}

// eval is o's state at now (Unix ms), over its real legs. Pending, with the first reason that applies: a check order
// with a leg not answered yet (outcome_unknown or stuck), a real leg open on a voidable market (voidable) or on a
// one-sided order (one_sided), a real leg open more than 24 h after it was placed (overdue). Settled: every leg
// answered and every real leg settled (a check order whose legs were all not placed: profit 0). The status is
// settled, else pending, else booked, else open.
func (o *ledgerOrder) eval(now int64) orderEval {
	real := o.real()
	e := orderEval{voidable: o.isVoidable(), oneSided: len(real) == 1}
	unresolved := o.unresolved()
	overdue := false
	var paid, staked float64
	for _, l := range real {
		staked += l.Stake
		if legSettled(l) {
			paid += *l.Payout
			continue
		}
		e.open = true
		if now-l.PlacedAt > pendingAfterMs {
			overdue = true
		}
	}
	e.settled = !unresolved && !e.open && (len(real) > 0 || o.check)
	switch {
	case e.settled:
		e.profit = roundPtr(paid-staked, 2)
	case len(real) == 2 && !e.voidable:
		e.booked = roundPtr(booking(real), 2)
	}
	switch {
	case unresolved && o.row.State == "outcome_unknown":
		e.reason = pendingUnknown
	case unresolved:
		e.reason = pendingStuck
	case e.open && e.voidable:
		e.reason = pendingVoidable
	case e.open && e.oneSided:
		e.reason = pendingOneSided
	case overdue:
		e.reason = pendingOverdue
	}
	e.pending = e.reason != ""
	switch {
	case e.settled:
		e.status = "settled"
	case e.pending:
		e.status = "pending"
	case e.booked != nil:
		e.status = "booked"
	default:
		e.status = "open"
	}
	return e
}

// arbAt is o's arb profit as of t and the booked part of it: its settled profit once every real leg was placed and
// settled by t, else its booking once both legs were placed by t (a market that cannot push), else 0. An order with a
// leg nobody answered counts nothing (as on the cards).
func (o *ledgerOrder) arbAt(t int64) (profit, booked float64) {
	real := o.real()
	if len(real) == 0 || o.unresolved() {
		return 0, 0
	}
	placed, settled := true, true
	var paid, staked float64
	for _, l := range real {
		if l.PlacedAt > t {
			placed = false
		}
		if !legSettled(l) || l.SettledAt == nil || *l.SettledAt > t {
			settled = false
		} else {
			paid += *l.Payout
		}
		staked += l.Stake
	}
	switch {
	case !placed:
		return 0, 0
	case settled:
		return paid - staked, 0
	case len(real) == 2 && !o.isVoidable():
		b := booking(real)
		return b, b
	}
	return 0, 0
}

// pragueDate is the Prague date (YYYY-MM-DD) of ms.
func pragueDate(ms int64) string { return time.UnixMilli(ms).In(prague).Format("2006-01-02") }

// maxLegOf is the max leg the bankroll measures each account against: the auto settings' bankroll max_leg when its
// mode is maxleg, else the controller's limits.maxLeg when > 0, else nil.
func maxLegOf(st accounts.BotSettings) *float64 {
	if br := autoSettingsOf(st.AutoJSON).Bankroll; br != nil && br.Mode == "maxleg" && br.MaxLeg > 0 {
		v := br.MaxLeg
		return &v
	}
	if st.MaxLeg > 0 {
		v := float64(st.MaxLeg)
		return &v
	}
	return nil
}

// coverOf is how many max legs balance covers (floor; nil without a balance or a max leg) and whether it can pay one now
// (fresh and balance ≥ maxLeg; nil without a max leg).
func coverOf(balance *float64, fresh bool, maxLeg *float64) (*int64, *bool) {
	if maxLeg == nil {
		return nil, nil
	}
	can := fresh && balance != nil && *balance >= *maxLeg
	if balance == nil {
		return nil, &can
	}
	n := int64(math.Floor(*balance / *maxLeg))
	return &n, &can
}

// acctKey is an account: agent code id + window id.
type acctKey struct {
	code   int64
	window string
}

// bankrollAccount is one account in GET /api/me/bot/bankroll. Unknown figures are null.
type bankrollAccount struct {
	AgentID    int64    `json:"agentId"`
	WindowID   string   `json:"windowId"`
	Bookmaker  string   `json:"bookmaker"`
	Removed    bool     `json:"removed"`
	Balance    *float64 `json:"balance"`
	BalanceAt  *int64   `json:"balanceAt"`
	BalanceAge *int64   `json:"balanceAge"` // now − balanceAt (ms)
	Fresh      bool     `json:"fresh"`      // a balance at most balanceFreshMs old
	Start      *float64 `json:"start"`
	StartAt    *int64   `json:"startAt"`
	OpenStakes float64  `json:"openStakes"`
	ArbFlow    *float64 `json:"arbFlow"`
	Outside    *float64 `json:"outside"`
	MaxLeg     *float64 `json:"maxLeg"`
	Covers     *int64   `json:"covers"` // floor(balance / maxLeg)
	CanPay     *bool    `json:"canPay"` // fresh and balance ≥ maxLeg
}

// bankrollBook is the accounts of one bookmaker summed.
type bankrollBook struct {
	Bookmaker  string   `json:"bookmaker"`
	Balance    *float64 `json:"balance"`
	Start      *float64 `json:"start"`
	OpenStakes float64  `json:"openStakes"`
	ArbFlow    *float64 `json:"arbFlow"`
	Outside    *float64 `json:"outside"`
	ArbProfit  float64  `json:"arbProfit"`
	MaxLeg     *float64 `json:"maxLeg"`
	Covers     *int64   `json:"covers"`
	CanPay     *bool    `json:"canPay"` // every window of the book (not removed) fresh and the sum ≥ maxLeg
}

// bankrollTotal is every account summed, with the arb profit split into bookings and settled profit.
type bankrollTotal struct {
	Balance    *float64 `json:"balance"`
	Start      *float64 `json:"start"`
	OpenStakes float64  `json:"openStakes"`
	ArbFlow    *float64 `json:"arbFlow"`
	Outside    *float64 `json:"outside"`
	ArbProfit  float64  `json:"arbProfit"`
	Booked     float64  `json:"booked"`
	Settled    float64  `json:"settled"`
	Staked     float64  `json:"staked"`    // Σ stake of the real legs placed in the range
	ProfitPct  *float64 `json:"profitPct"` // arbProfit / staked × 100; null when nothing was staked
	Stale      bool     `json:"stale"`     // a window (not removed) with point 0 has no balance or an old one
	Orders     int      `json:"orders"`
	Pending    int      `json:"pending"`
}

// bankrollCounts is the number of orders per status; open = orders with a real leg still open.
type bankrollCounts struct {
	Pending int `json:"pending"`
	Booked  int `json:"booked"`
	Settled int `json:"settled"`
	Open    int `json:"open"`
}

// bankrollPoint is one point of the curve.
type bankrollPoint struct {
	At        int64    `json:"at"`
	Balance   *float64 `json:"balance"`
	ArbProfit float64  `json:"arbProfit"`
	Booked    float64  `json:"booked"` // the booked part of arbProfit
	Outside   *float64 `json:"outside"`
}

// bankrollBookPoint is one point of a bookmaker's balance curve.
type bankrollBookPoint struct {
	At      int64    `json:"at"`
	Balance *float64 `json:"balance"`
}

type bankrollCards struct {
	Accounts []bankrollAccount
	Books    []bankrollBook
	Total    bankrollTotal
	Counts   bankrollCounts
}

// sum is a nullable sum: nil until a value is added.
type sum struct{ v *float64 }

func (s *sum) add(v float64) {
	if s.v == nil {
		s.v = new(float64)
	}
	*s.v += v
}

func (s sum) out() *float64 {
	if s.v == nil {
		return nil
	}
	return roundPtr(*s.v, 2)
}

// arbFlowOf is the arb cash flow of account k with point 0 at start as of t: −stake of each of its real legs placed in
// [start, t], + payout of those settled by t.
func arbFlowOf(k acctKey, start int64, orders []*ledgerOrder, t int64) float64 {
	var flow float64
	for _, o := range orders {
		for _, l := range o.legs {
			if !realLeg(l) || (acctKey{l.CodeID, l.WindowID}) != k || l.PlacedAt < start || l.PlacedAt > t {
				continue
			}
			flow -= l.Stake
			if legSettled(l) && l.SettledAt != nil && *l.SettledAt <= t {
				flow += *l.Payout
			}
		}
	}
	return flow
}

// bankrollFigures are the cards at now: every account (point 0, a stored or live balance, or a real ledger leg) with
// its figures, summed per bookmaker and in total, and the order counts. latest is each account's newest balance;
// present the accounts the agents still have (the others are listed as removed); from the range's start (staked counts
// the real legs placed from it on; 0 = all); maxLeg the max leg of maxLegOf (nil: no cover figures). Accounts without
// point 0 have null balance figures and stay out of the balance sums; their open stakes and arb profit count. A
// not-placed leg is in no figure.
func bankrollFigures(accs []accounts.BotAccountRow, latest []accounts.BotBalanceRow, orders []*ledgerOrder, present map[acctKey]bool,
	now, from int64, maxLeg *float64) bankrollCards {
	list := map[acctKey]*bankrollAccount{}
	get := func(k acctKey, book string) *bankrollAccount {
		a := list[k]
		if a == nil {
			a = &bankrollAccount{AgentID: k.code, WindowID: k.window, Bookmaker: book, Removed: !present[k]}
			list[k] = a
		}
		return a
	}
	for _, r := range accs {
		a := get(acctKey{r.CodeID, r.WindowID}, r.Bookmaker)
		a.Start, a.StartAt = roundPtr(r.StartAmount, 2), &r.StartAt
	}
	for _, r := range latest {
		a := get(acctKey{r.CodeID, r.WindowID}, r.Bookmaker)
		at := r.At
		a.Balance, a.BalanceAt = roundPtr(r.Amount, 2), &at
	}
	bookProfit := map[string]float64{}
	var total bankrollTotal
	var counts bankrollCounts
	for _, o := range orders {
		e := o.eval(now)
		if e.pending {
			total.Pending++
		}
		switch e.status {
		case "pending":
			counts.Pending++
		case "booked":
			counts.Booked++
		case "settled":
			counts.Settled++
		}
		if e.open {
			counts.Open++
		}
		real := o.real()
		if len(real) == 0 {
			continue
		}
		total.Orders++
		switch {
		case e.settled:
			total.Settled += *e.profit
		case e.booked != nil:
			total.Booked += *e.booked
		}
		for _, l := range real {
			if l.PlacedAt >= from && l.PlacedAt <= now {
				total.Staked += l.Stake
			}
			a := get(acctKey{l.CodeID, l.WindowID}, l.Bookmaker)
			if !legSettled(l) {
				a.OpenStakes += l.Stake
			}
			switch {
			case e.settled:
				bookProfit[l.Bookmaker] += *l.Payout - l.Stake
			case e.booked != nil:
				bookProfit[l.Bookmaker] += *e.booked / 2
			}
		}
	}
	keys := make([]acctKey, 0, len(list))
	for k := range list {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y acctKey) int {
		if c := strings.Compare(list[x].Bookmaker, list[y].Bookmaker); c != 0 {
			return c
		}
		if x.code != y.code {
			return int(x.code - y.code)
		}
		return strings.Compare(x.window, y.window)
	})
	type bookSums struct {
		balance, start, flow, outside sum
		live                          int  // windows with point 0, not removed
		stale                         bool // one of them not fresh
	}
	var books []string
	sums := map[string]*bookSums{}
	open := map[string]float64{}
	var tot bookSums
	out := bankrollCards{Accounts: []bankrollAccount{}, Books: []bankrollBook{}}
	for _, k := range keys {
		a := list[k]
		if sums[a.Bookmaker] == nil {
			sums[a.Bookmaker] = &bookSums{}
			books = append(books, a.Bookmaker)
		}
		s := sums[a.Bookmaker]
		open[a.Bookmaker] += a.OpenStakes
		total.OpenStakes += a.OpenStakes
		a.OpenStakes = round(a.OpenStakes, 2)
		if a.Start == nil {
			a.Balance, a.BalanceAt = nil, nil // no point 0: listed with nulls
		} else {
			flow := arbFlowOf(k, *a.StartAt, orders, now)
			a.ArbFlow = roundPtr(flow, 2)
			s.start.add(*a.Start)
			tot.start.add(*a.Start)
			s.flow.add(flow)
			tot.flow.add(flow)
			if a.Balance != nil {
				a.Outside = roundPtr(*a.Balance-*a.Start-flow, 2)
				s.balance.add(*a.Balance)
				tot.balance.add(*a.Balance)
				s.outside.add(*a.Outside)
				tot.outside.add(*a.Outside)
			}
		}
		if a.BalanceAt != nil {
			age := now - *a.BalanceAt
			a.BalanceAge, a.Fresh = &age, age <= balanceFreshMs
		}
		if a.Start != nil && !a.Removed {
			s.live++
			if !a.Fresh {
				s.stale, tot.stale = true, true
			}
		}
		a.MaxLeg = maxLeg
		a.Covers, a.CanPay = coverOf(a.Balance, a.Fresh, maxLeg)
		out.Accounts = append(out.Accounts, *a)
	}
	for book := range bookProfit {
		if sums[book] == nil {
			sums[book] = &bookSums{}
			books = append(books, book)
		}
	}
	slices.Sort(books)
	for _, book := range books {
		s := sums[book]
		b := bankrollBook{Bookmaker: book, Balance: s.balance.out(), Start: s.start.out(), OpenStakes: round(open[book], 2), ArbFlow: s.flow.out(),
			Outside: s.outside.out(), ArbProfit: round(bookProfit[book], 2), MaxLeg: maxLeg}
		b.Covers, b.CanPay = coverOf(b.Balance, s.live > 0 && !s.stale, maxLeg)
		out.Books = append(out.Books, b)
	}
	total.Balance, total.Start, total.ArbFlow, total.Outside = tot.balance.out(), tot.start.out(), tot.flow.out(), tot.outside.out()
	total.OpenStakes, total.Booked, total.Settled = round(total.OpenStakes, 2), round(total.Booked, 2), round(total.Settled, 2)
	total.ArbProfit = round(total.Booked+total.Settled, 2)
	total.Staked = round(total.Staked, 2)
	if total.Staked > 0 {
		total.ProfitPct = roundPtr(total.ArbProfit/total.Staked*100, 2)
	}
	total.Stale = tot.stale
	out.Total, out.Counts = total, counts
	return out
}

// bankrollRanges are the curve's ranges (ms back from now; 0 = all).
var bankrollRanges = map[string]int64{"7d": 7 * 24 * 3600 * 1000, "30d": 30 * 24 * 3600 * 1000, "all": 0}

// seriesMax is the most points a curve has.
const seriesMax = 400

// seriesTimes are the curve's times: every stored balance read and every real leg's placing and settling in [from, now]
// (from 0 = no bound), plus now; sorted, each once, downsampled to seriesMax.
func seriesTimes(hist []accounts.BotBalanceRow, orders []*ledgerOrder, from, now int64) []int64 {
	var ts []int64
	add := func(t int64) {
		if t >= from && t <= now {
			ts = append(ts, t)
		}
	}
	for _, r := range hist {
		add(r.At)
	}
	for _, o := range orders {
		for _, l := range o.real() {
			add(l.PlacedAt)
			if legSettled(l) && l.SettledAt != nil {
				add(*l.SettledAt)
			}
		}
	}
	ts = append(ts, now)
	slices.Sort(ts)
	return downsample(slices.Compact(ts), seriesMax)
}

// downsample keeps at most max of the sorted, distinct times ts: the first, and the last time of each of max−1
// equal-width buckets over [first, last] (so the last time too).
func downsample(ts []int64, max int) []int64 {
	if len(ts) <= max {
		return ts
	}
	first, span, n := ts[0], ts[len(ts)-1]-ts[0], int64(max-1)
	bucket := func(t int64) int64 { return min((t-first)*n/span, n-1) }
	out := []int64{first}
	for i := 1; i < len(ts); i++ {
		if i == len(ts)-1 || bucket(ts[i+1]) != bucket(ts[i]) {
			out = append(out, ts[i])
		}
	}
	return out
}

// bankrollSeries is the curve at the sorted times: the total balance (each account with point 0 by then: its last
// stored read ≤ T; hist oldest first), the arb profit as of T with its booked part, and the outside money of those
// accounts as of T (null without any); and per bookmaker of accs, its accounts' balance at the same times.
func bankrollSeries(accs []accounts.BotAccountRow, hist []accounts.BotBalanceRow, orders []*ledgerOrder,
	times []int64) ([]bankrollPoint, map[string][]bankrollBookPoint) {
	out := make([]bankrollPoint, 0, len(times))
	books := map[string][]bankrollBookPoint{}
	last := map[acctKey]float64{}
	j := 0
	for _, t := range times {
		for ; j < len(hist) && hist[j].At <= t; j++ {
			last[acctKey{hist[j].CodeID, hist[j].WindowID}] = hist[j].Amount
		}
		p := bankrollPoint{At: t}
		var balance, outside sum
		bookBal := map[string]*sum{}
		for _, a := range accs {
			if bookBal[a.Bookmaker] == nil {
				bookBal[a.Bookmaker] = &sum{}
			}
			k := acctKey{a.CodeID, a.WindowID}
			amount, ok := last[k]
			if a.StartAt > t || !ok {
				continue
			}
			balance.add(amount)
			bookBal[a.Bookmaker].add(amount)
			outside.add(amount - a.StartAmount - arbFlowOf(k, a.StartAt, orders, t))
		}
		var profit, booked float64
		for _, o := range orders {
			pr, bk := o.arbAt(t)
			profit += pr
			booked += bk
		}
		p.Balance, p.Outside, p.ArbProfit, p.Booked = balance.out(), outside.out(), round(profit, 2), round(booked, 2)
		out = append(out, p)
		for book, s := range bookBal {
			books[book] = append(books[book], bankrollBookPoint{At: t, Balance: s.out()})
		}
	}
	return out, books
}
