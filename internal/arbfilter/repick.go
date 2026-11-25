package arbfilter

import "slices"

// Repick returns the arb as the given bookmakers can play it: each outcome takes the best price
// among its stake and the stake's Alternatives at a book in books (the stake first on a tie), and
// profit_pct and every stake_pct are recomputed with the engine's formula (index = Σ 1/odds,
// profit = (1 − index) × 100, stake_i = (1/odds_i) / index × 100). false when some outcome has no
// price at any of the books. nil books = every book: the arb is returned unchanged. The result may
// have profit ≤ 0 (no longer an arb for these books); the caller decides. A re-picked arb's stakes
// carry no Alternatives; a stake taken from an alternative keeps that quote's every field (source ids,
// fair odds). The frontend's repickArb (util/repick.ts) is the same rule.
func Repick(a Arb, books []string) (Arb, bool) {
	if books == nil {
		return a, true
	}
	out := a
	out.Stakes = make([]Stake, len(a.Stakes))
	changed := false
	for i, s := range a.Stakes {
		best := s
		best.Alternatives = nil
		found := slices.Contains(books, s.Bookmaker)
		for _, q := range s.Alternatives {
			if !slices.Contains(books, q.Bookmaker) || (found && q.DecimalOdds <= best.DecimalOdds) {
				continue
			}
			best = Stake{Bookmaker: q.Bookmaker, OutcomeID: s.OutcomeID, Label: q.Label, DecimalOdds: q.DecimalOdds,
				MatchURL: q.MatchURL, MarketName: q.MarketName, SourceID: q.SourceID, SourceMarketRef: q.SourceMarketRef, FairOdds: q.FairOdds}
			found = true
		}
		if !found {
			return Arb{}, false
		}
		changed = changed || best.Bookmaker != s.Bookmaker
		out.Stakes[i] = best
	}
	if !changed {
		return out, true
	}
	index := 0.0
	for _, s := range out.Stakes {
		index += 1 / s.DecimalOdds
	}
	out.ProfitPct = (1 - index) * 100
	for i := range out.Stakes {
		out.Stakes[i].StakePct = (1 / out.Stakes[i].DecimalOdds) / index * 100
	}
	return out, true
}
