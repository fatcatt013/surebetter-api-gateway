package telegram

import "github.com/surebetter/api-gateway/internal/arbfilter"

// The filter is internal/arbfilter's (a port of the app's filterArbs steps 2–7, shared with the
// bots' auto mode) plus the losesAfterRounding exclusion. Hidden entries (step 1) are per-browser
// and intentionally absent.

const liveSuffix = "-live"

// IsLive reports whether the sport carries the pipeline's "-live" suffix.
func IsLive(sport string) bool { return arbfilter.IsLive(sport) }

// BaseSport strips the "-live" suffix and collapses every per-game esports slug to "esports"
// (arbfilter.BaseSport).
func BaseSport(sport string) string { return arbfilter.BaseSport(sport) }

// esportsGameSlug returns the game part of an esports sport ("esports-cs2-live" → "cs2"), or "".
func esportsGameSlug(sport string) string { return arbfilter.EsportsGameSlug(sport) }

// Matches reports whether the arb passes the bot's filter at nowMs.
func Matches(a Arb, f Filter, b Bankroll, nowMs int64) bool {
	_, ok := Select(a, f, b, nowMs)
	return ok
}

// ForBooks is the arb as the filter's bookmakers can play it (arbfilter.ForBooks).
func ForBooks(a Arb, f Filter) (Arb, bool) { return arbfilter.ForBooks(a, f) }

// Repick returns the arb as the given bookmakers can play it (arbfilter.Repick).
func Repick(a Arb, books []string) (Arb, bool) { return arbfilter.Repick(a, books) }

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
	return arbfilter.Reject(a, f, nowMs) == "" && !LosesAfterRounding(a, b)
}

// StakePlan is computeStakes' result (arbfilter.StakePlan).
type StakePlan = arbfilter.StakePlan

// ComputeStakes sizes an arb's legs against the bankroll exactly like the frontend (arbfilter.ComputeStakes).
func ComputeStakes(a Arb, b Bankroll) StakePlan { return arbfilter.ComputeStakes(a, b) }

// LosesAfterRounding: stake rounding (10/100) leaves the arb losing money (arbfilter.LosesAfterRounding).
func LosesAfterRounding(a Arb, b Bankroll) bool { return arbfilter.LosesAfterRounding(a, b) }

// jsRound is JavaScript's Math.round (arbfilter.JSRound).
func jsRound(x float64) float64 { return arbfilter.JSRound(x) }
