package telegram

import "math"

// Port of surebetter-frontend/src/app/util/bankroll.ts (computeStakes, losesAfterRounding).
// Keep the two in step: a change to one needs the same change in the other.

// StakePlan is computeStakes' result: per-leg stakes in whole units (index-aligned with Arb.Stakes).
type StakePlan struct {
	Stakes []float64
	Total  float64
	Profit float64 // guaranteed (worst-case) profit, whole units
	ROI    float64 // percent
	FavIdx int     // capped leg in maxleg mode, -1 in split
}

// jsRound is JavaScript's Math.round (halves toward +∞), unlike Go's math.Round.
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

// roundingStep: 10 or 100 pass, anything else is 1.
func roundingStep(r int) float64 {
	if r == 10 || r == 100 {
		return float64(r)
	}
	return 1
}

// ComputeStakes sizes an arb's legs against the bankroll exactly like the frontend.
func ComputeStakes(a Arb, b Bankroll) StakePlan {
	n := len(a.Stakes)
	arbROI := a.ProfitPct
	zeros := StakePlan{Stakes: make([]float64, n), ROI: arbROI, FavIdx: -1}
	if n == 0 {
		return zeros
	}

	// Weights: prefer backend stake_pct; otherwise fall back to 1/odds ratios.
	w := make([]float64, n)
	pctSum, pctValid := 0.0, true
	for _, s := range a.Stakes {
		pctSum += s.StakePct
		if !(s.StakePct > 0) {
			pctValid = false
		}
	}
	if pctValid && pctSum > 0 {
		for i, s := range a.Stakes {
			w[i] = s.StakePct / 100
		}
	} else {
		sumInv := 0.0
		for i, s := range a.Stakes {
			if s.DecimalOdds > 0 {
				w[i] = 1 / s.DecimalOdds
			}
			sumInv += w[i]
		}
		if sumInv <= 0 {
			return zeros
		}
		for i := range w {
			w[i] /= sumInv
		}
	}
	for _, x := range w {
		if math.IsNaN(x) || math.IsInf(x, 0) || x <= 0 {
			return zeros
		}
	}

	fav := 0 // favourite = lowest odds
	for i := 1; i < n; i++ {
		if a.Stakes[i].DecimalOdds < a.Stakes[fav].DecimalOdds {
			fav = i
		}
	}

	step := roundingStep(b.Rounding)
	roundStake := func(x float64) float64 { return jsRound(x/step) * step }
	stakes := make([]float64, n)
	total := 0.0

	if b.Mode == "split" {
		if b.Total <= 0 {
			return zeros
		}
		for i := range stakes {
			stakes[i] = roundStake(b.Total * w[i])
			total += stakes[i]
		}
		fav = -1
	} else {
		anchor := -1
		if b.AnchorBook != nil && *b.AnchorBook != "" {
			for i, s := range a.Stakes {
				if s.Bookmaker != *b.AnchorBook {
					continue
				}
				if anchor < 0 || s.DecimalOdds < a.Stakes[anchor].DecimalOdds {
					anchor = i
				}
			}
		}
		if anchor >= 0 {
			fav = anchor
		}
		var capStake float64
		if step > 1 {
			capStake = math.Floor(b.MaxLeg/step) * step // the max is a ceiling
		} else {
			capStake = jsRound(b.MaxLeg)
		}
		if capStake <= 0 {
			return zeros
		}
		beff := capStake / w[fav]
		for i := range stakes {
			if i == fav {
				stakes[i] = capStake
			} else {
				stakes[i] = roundStake(beff * w[i])
			}
			total += stakes[i]
		}
	}

	allOdds := true
	for _, s := range a.Stakes {
		if !(s.DecimalOdds > 0) {
			allOdds = false
		}
	}
	if step > 1 && total > 0 && allOdds {
		worst := math.Inf(1)
		for i, s := range a.Stakes {
			worst = math.Min(worst, stakes[i]*s.DecimalOdds)
		}
		return StakePlan{Stakes: stakes, Total: total, Profit: jsRound(worst - total), ROI: (worst - total) / total * 100, FavIdx: fav}
	}
	return StakePlan{Stakes: stakes, Total: total, Profit: jsRound(total * arbROI / 100), ROI: arbROI, FavIdx: fav}
}

// LosesAfterRounding: stake rounding (10/100) leaves the arb losing money. Always false with rounding off.
func LosesAfterRounding(a Arb, b Bankroll) bool {
	return roundingStep(b.Rounding) > 1 && ComputeStakes(a, b).Profit < 0
}
