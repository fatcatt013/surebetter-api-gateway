package telegram

import (
	"math"
	"slices"
	"strings"
)

// Port of filterArbs (surebetter-frontend/src/app/util/apply-filters.ts) steps 2–7 plus the
// losesAfterRounding exclusion. Hidden entries (step 1) are per-browser and intentionally absent.
// Market prefs arrive pre-resolved as Filter.BlockedMarkets (see util/telegram-bot.ts).

const liveSuffix = "-live"

// IsLive reports whether the sport carries the pipeline's "-live" suffix.
func IsLive(sport string) bool { return strings.HasSuffix(sport, liveSuffix) }

// Esports carries the game in the sport slug, one per game ("esports-cs2",
// "esports-dota2-live"); the app shows them as one sport, "esports".
const (
	esportsSport  = "esports"
	esportsPrefix = esportsSport + "-"
)

// BaseSport strips the "-live" suffix and collapses every per-game esports slug to
// "esports" — the frontend's baseSport, which keys Filter.Sports and
// Filter.BlockedMarkets. Without the collapse a preset with the Esports tile on
// would match no esports arb at all.
func BaseSport(sport string) string {
	bare := strings.TrimSuffix(sport, liveSuffix)
	if strings.HasPrefix(bare, esportsPrefix) {
		return esportsSport
	}
	return bare
}

// esportsGameSlug returns the game part of an esports sport ("esports-cs2-live" →
// "cs2"), or "" for any other sport.
func esportsGameSlug(sport string) string {
	bare := strings.TrimSuffix(sport, liveSuffix)
	if !strings.HasPrefix(bare, esportsPrefix) {
		return ""
	}
	return bare[len(esportsPrefix):]
}

func ageSec(a Arb, nowMs int64) float64 {
	t := a.firstSeen()
	if t == 0 {
		return math.Inf(1)
	}
	return float64(nowMs-t) / 1000
}

// Matches reports whether the arb passes the bot's filter at nowMs.
func Matches(a Arb, f Filter, b Bankroll, nowMs int64) bool {
	_, ok := Select(a, f, b, nowMs)
	return ok
}

// ForBooks is the arb as the filter's bookmakers can play it (Repick), or false when it is not an
// arb for them: an outcome has no price at a selected book, the re-picked profit is ≤ 0, or the arb
// has no legs. nil bookmakers = every book: the arb unchanged.
func ForBooks(a Arb, f Filter) (Arb, bool) {
	if f.Bookmakers == nil {
		return a, true
	}
	if len(a.Stakes) == 0 {
		return Arb{}, false
	}
	r, ok := Repick(a, f.Bookmakers)
	if !ok || r.ProfitPct <= 0 {
		return Arb{}, false
	}
	return r, true
}

// Select is Matches returning the arb the bot posts: re-picked among the filter's bookmakers
// (ForBooks), every later step judging the re-picked prices.
func Select(a Arb, f Filter, b Bankroll, nowMs int64) (Arb, bool) {
	a, ok := ForBooks(a, f)
	if !ok {
		return Arb{}, false
	}
	if !matchesRest(a, f, b, nowMs) {
		return Arb{}, false
	}
	return a, true
}

func matchesRest(a Arb, f Filter, b Bankroll, nowMs int64) bool {
	if f.ThreeLeg == "exclude" && len(a.Stakes) >= 3 {
		return false
	}
	live := IsLive(a.Sport)
	if f.LiveState == "live" && !live {
		return false
	}
	if f.LiveState == "prematch" && live {
		return false
	}
	base := BaseSport(a.Sport)
	if f.Sports != nil && !slices.Contains(f.Sports, base) {
		return false
	}
	if slices.Contains(f.BlockedMarkets[base], a.MarketType) {
		return false
	}
	if f.ProfitMin != nil && a.ProfitPct < *f.ProfitMin {
		return false
	}
	if f.ProfitMax != nil && a.ProfitPct > *f.ProfitMax {
		return false
	}
	if f.MinAgeSec > 0 && ageSec(a, nowMs) < float64(f.MinAgeSec) {
		return false
	}
	return !LosesAfterRounding(a, b)
}
