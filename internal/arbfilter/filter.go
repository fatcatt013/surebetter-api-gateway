package arbfilter

import (
	"math"
	"slices"
	"strings"
)

// Port of filterArbs (surebetter-frontend/src/app/util/apply-filters.ts) steps 2–7. Hidden entries
// (step 1) are the caller's business (the Telegram bot has none; auto mode reads the user's).
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

// EsportsGameSlug returns the game part of an esports sport ("esports-cs2-live" →
// "cs2"), or "" for any other sport.
func EsportsGameSlug(sport string) string {
	bare := strings.TrimSuffix(sport, liveSuffix)
	if !strings.HasPrefix(bare, esportsPrefix) {
		return ""
	}
	return bare[len(esportsPrefix):]
}

// AgeSec is how long the arb has been seen at nowMs, in seconds (+Inf without a timestamp).
func AgeSec(a Arb, nowMs int64) float64 {
	t := a.FirstSeen()
	if t == 0 {
		return math.Inf(1)
	}
	return float64(nowMs-t) / 1000
}

// ForBooks is the arb as the filter's bookmakers can play it (Repick), or false when it is not an
// arb for them: an outcome has no price at a selected book, the re-picked profit is ≤ 0, or the arb
// has no legs. nil bookmakers = every book: the arb unchanged.
func ForBooks(a Arb, f Filter) (Arb, bool) {
	if f.Bookmakers == nil {
		return a, true
	}
	return ForBookList(a, f.Bookmakers)
}

// ForBookList is ForBooks for a non-nil list of books.
func ForBookList(a Arb, books []string) (Arb, bool) {
	if len(a.Stakes) == 0 {
		return Arb{}, false
	}
	r, ok := Repick(a, books)
	if !ok || r.ProfitPct <= 0 {
		return Arb{}, false
	}
	return r, true
}

// The filter steps Reject names, in the order it checks them (auto mode counts by them).
const (
	StepThreeLeg  = "three_legs"
	StepLiveState = "live_state"
	StepSport     = "sport"
	StepMarket    = "market"
	StepProfitMin = "profit_min"
	StepProfitMax = "profit_max"
	StepMinAge    = "min_age"
)

// Reject is the first filter step (apply-filters.ts steps 3–7) the arb fails at nowMs, "" when it
// passes them all: 3-leg exclusion, live/prematch, sport (base sport), blocked market, profit min,
// profit max, min age (from first_detected_at). The bookmakers (ForBooks) and the stakes rounding
// (LosesAfterRounding) are the caller's.
func Reject(a Arb, f Filter, nowMs int64) string {
	if f.ThreeLeg == "exclude" && len(a.Stakes) >= 3 {
		return StepThreeLeg
	}
	live := IsLive(a.Sport)
	if (f.LiveState == "live" && !live) || (f.LiveState == "prematch" && live) {
		return StepLiveState
	}
	base := BaseSport(a.Sport)
	if f.Sports != nil && !slices.Contains(f.Sports, base) {
		return StepSport
	}
	if slices.Contains(f.BlockedMarkets[base], a.MarketType) {
		return StepMarket
	}
	if f.ProfitMin != nil && a.ProfitPct < *f.ProfitMin {
		return StepProfitMin
	}
	if f.ProfitMax != nil && a.ProfitPct > *f.ProfitMax {
		return StepProfitMax
	}
	if f.MinAgeSec > 0 && AgeSec(a, nowMs) < float64(f.MinAgeSec) {
		return StepMinAge
	}
	return ""
}
