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

// ledgerResults are the results a ledger leg can have.
var ledgerResults = map[string]bool{"open": true, "won": true, "lost": true, "void": true, "half_won": true, "half_lost": true,
	"cashed_out": true}

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
// for an order that only needs a check), its stored legs and whether it needs a check (outcome_unknown or a stuck last flow).
type ledgerOrder struct {
	row    accounts.BotOrderRow
	info   orderInfo
	stored []storedLeg // the order's legs as bot_orders keeps them
	legs   []accounts.BotLedgerLeg
	check  bool
}

// orderEval is an order's state for the bankroll at one time.
type orderEval struct {
	voidable bool
	oneSided bool     // exactly one real leg
	settled  bool     // every real leg settled
	booked   *float64 // the booking of an unsettled order of two real legs on a market that cannot push
	profit   *float64 // settled: Σ payout − Σ stake
	pending  bool
	status   string // settled, pending, booked or open
}

func legSettled(l accounts.BotLedgerLeg) bool { return l.Result != "open" && l.Payout != nil }

// isVoidable: a leg is on a voidable market, or (no legs) the order's market is.
func (o *ledgerOrder) isVoidable() bool {
	if len(o.legs) == 0 {
		return voidable(o.info.MarketType, o.info.Line)
	}
	return slices.ContainsFunc(o.legs, func(l accounts.BotLedgerLeg) bool { return l.Voidable })
}

// booking is min(stake·odds) − Σ stake of two legs.
func booking(legs []accounts.BotLedgerLeg) float64 {
	return math.Min(legs[0].Stake*legs[0].Odds, legs[1].Stake*legs[1].Odds) - (legs[0].Stake + legs[1].Stake)
}

// eval is o's state at now (Unix ms). Pending: a real leg open on a voidable or one-sided order, a check order without
// real legs, or a real leg open more than 24 h after it was placed. The status is settled, else pending, else booked,
// else open.
func (o *ledgerOrder) eval(now int64) orderEval {
	e := orderEval{voidable: o.isVoidable(), oneSided: len(o.legs) == 1}
	open := false
	var paid, staked float64
	for _, l := range o.legs {
		staked += l.Stake
		if legSettled(l) {
			paid += *l.Payout
			continue
		}
		open = true
		if now-l.PlacedAt > pendingAfterMs {
			e.pending = true
		}
	}
	e.settled = len(o.legs) > 0 && !open
	switch {
	case e.settled:
		e.profit = roundPtr(paid-staked, 2)
	case len(o.legs) == 2 && !e.voidable:
		e.booked = roundPtr(booking(o.legs), 2)
	}
	if (open && (e.voidable || e.oneSided)) || (o.check && len(o.legs) == 0) {
		e.pending = true
	}
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

// arbProfitAt is o's arb profit as of t: its settled profit once every real leg was placed and settled by t, else its
// booking once both legs were placed by t (a market that cannot push), else 0.
func (o *ledgerOrder) arbProfitAt(t int64) float64 {
	if len(o.legs) == 0 {
		return 0
	}
	placed, settled := true, true
	var paid, staked float64
	for _, l := range o.legs {
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
		return 0
	case settled:
		return paid - staked
	case len(o.legs) == 2 && !o.isVoidable():
		return booking(o.legs)
	}
	return 0
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
	Start      *float64 `json:"start"`
	StartAt    *int64   `json:"startAt"`
	OpenStakes float64  `json:"openStakes"`
	ArbFlow    *float64 `json:"arbFlow"`
	Outside    *float64 `json:"outside"`
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
	Orders     int      `json:"orders"`
	Pending    int      `json:"pending"`
}

// bankrollPoint is one point of the curve.
type bankrollPoint struct {
	At        int64    `json:"at"`
	Balance   *float64 `json:"balance"`
	ArbProfit float64  `json:"arbProfit"`
	Outside   *float64 `json:"outside"`
}

type bankrollCards struct {
	Accounts []bankrollAccount
	Books    []bankrollBook
	Total    bankrollTotal
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
			if (acctKey{l.CodeID, l.WindowID}) != k || l.PlacedAt < start || l.PlacedAt > t {
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

// bankrollFigures are the cards at now: every account (point 0, a stored or live balance, or a ledger leg) with its
// figures, summed per bookmaker and in total. latest is each account's newest balance; present the accounts the
// agents still have (the others are listed as removed). Accounts without point 0 have null balance figures and stay
// out of the balance sums; their open stakes and arb profit count.
func bankrollFigures(accs []accounts.BotAccountRow, latest []accounts.BotBalanceRow, orders []*ledgerOrder, present map[acctKey]bool,
	now int64) bankrollCards {
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
	for _, o := range orders {
		e := o.eval(now)
		if e.pending {
			total.Pending++
		}
		if len(o.legs) == 0 {
			continue
		}
		total.Orders++
		switch {
		case e.settled:
			total.Settled += *e.profit
		case e.booked != nil:
			total.Booked += *e.booked
		}
		for _, l := range o.legs {
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
	type bookSums struct{ balance, start, flow, outside sum }
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
		out.Books = append(out.Books, bankrollBook{Bookmaker: book, Balance: s.balance.out(), Start: s.start.out(),
			OpenStakes: round(open[book], 2), ArbFlow: s.flow.out(), Outside: s.outside.out(), ArbProfit: round(bookProfit[book], 2)})
	}
	total.Balance, total.Start, total.ArbFlow, total.Outside = tot.balance.out(), tot.start.out(), tot.flow.out(), tot.outside.out()
	total.OpenStakes, total.Booked, total.Settled = round(total.OpenStakes, 2), round(total.Booked, 2), round(total.Settled, 2)
	total.ArbProfit = round(total.Booked+total.Settled, 2)
	out.Total = total
	return out
}

// bankrollRanges are the curve's ranges (ms back from now; 0 = all).
var bankrollRanges = map[string]int64{"7d": 7 * 24 * 3600 * 1000, "30d": 30 * 24 * 3600 * 1000, "all": 0}

// seriesPoints are the curve's times: the end of each Prague day (its next midnight) after the range's start and
// before now, then now. The range "all" starts at earliest (0 = no data: now only).
func seriesPoints(rng string, now, earliest int64) []int64 {
	from := now - bankrollRanges[rng]
	if rng == "all" {
		if earliest == 0 || earliest >= now {
			return []int64{now}
		}
		from = earliest
	}
	var out []int64
	_, end := pragueDayOf(time.UnixMilli(from))
	for end.UnixMilli() < now {
		out = append(out, end.UnixMilli())
		end = time.Date(end.Year(), end.Month(), end.Day()+1, 0, 0, 0, 0, prague)
	}
	return append(out, now)
}

// bankrollSeries is the curve at the times points: the total balance (each account with point 0 by then: its last
// stored read ≤ T), the arb profit as of T and the outside money of those accounts as of T (null without any).
func bankrollSeries(accs []accounts.BotAccountRow, hist []accounts.BotBalanceRow, orders []*ledgerOrder, points []int64) []bankrollPoint {
	out := make([]bankrollPoint, 0, len(points))
	for _, t := range points {
		p := bankrollPoint{At: t}
		var balance, outside sum
		for _, a := range accs {
			if a.StartAt > t {
				continue
			}
			k := acctKey{a.CodeID, a.WindowID}
			var last *float64
			for _, r := range hist { // oldest first
				if r.At > t {
					break
				}
				if r.CodeID == k.code && r.WindowID == k.window {
					amount := r.Amount
					last = &amount
				}
			}
			if last == nil {
				continue
			}
			balance.add(*last)
			outside.add(*last - a.StartAmount - arbFlowOf(k, a.StartAt, orders, t))
		}
		var profit float64
		for _, o := range orders {
			profit += o.arbProfitAt(t)
		}
		p.Balance, p.Outside, p.ArbProfit = balance.out(), outside.out(), round(profit, 2)
		out = append(out, p)
	}
	return out
}
