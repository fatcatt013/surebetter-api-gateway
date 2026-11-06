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

// BaseSport strips the "-live" suffix.
func BaseSport(sport string) string { return strings.TrimSuffix(sport, liveSuffix) }

func ageSec(a Arb, nowMs int64) float64 {
	t := a.firstSeen()
	if t == 0 {
		return math.Inf(1)
	}
	return float64(nowMs-t) / 1000
}

// Matches reports whether the arb passes the bot's filter at nowMs.
func Matches(a Arb, f Filter, b Bankroll, nowMs int64) bool {
	// bookmakers: every leg must be at a selected book; a leg-less arb never qualifies
	if f.Bookmakers != nil {
		if len(a.Stakes) == 0 {
			return false
		}
		for _, s := range a.Stakes {
			if !slices.Contains(f.Bookmakers, s.Bookmaker) {
				return false
			}
		}
	}
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
