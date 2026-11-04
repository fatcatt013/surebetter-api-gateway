package telegram

import (
	"math"
	"reflect"
	"testing"
)

func legs(oddsPct ...float64) []Stake {
	out := make([]Stake, 0, len(oddsPct)/2)
	for i := 0; i+1 < len(oddsPct); i += 2 {
		out = append(out, Stake{Bookmaker: "b", DecimalOdds: oddsPct[i], StakePct: oddsPct[i+1]})
	}
	return out
}

func split(total float64, rounding int) Bankroll {
	return Bankroll{Mode: "split", Total: total, MaxLeg: 3000, Currency: "CZK", Rounding: rounding}
}

func maxleg(max float64, rounding int) Bankroll {
	return Bankroll{Mode: "maxleg", Total: 10000, MaxLeg: max, Currency: "CZK", Rounding: rounding}
}

func TestComputeStakes_ParityWithFrontend(t *testing.T) {
	anchor := "betano"
	cases := []struct {
		name               string
		arb                Arb
		bk                 Bankroll
		stakes             []float64
		total, profit, roi float64
		favIdx             int
	}{
		{"split 2-way", Arb{ProfitPct: 4, Stakes: legs(2, 50, 2, 50)}, split(10000, 1), []float64{5000, 5000}, 10000, 400, 4, -1},
		{"maxleg 2-way", Arb{ProfitPct: 3, Stakes: legs(1.8, 55, 2.2, 45)}, maxleg(3000, 1), []float64{3000, 2455}, 5455, 164, 3, 0},
		{"3-way split", Arb{ProfitPct: 3, Stakes: legs(2.5, 40, 2.86, 35, 4, 25)}, split(12000, 1), []float64{4800, 4200, 3000}, 12000, 360, 3, -1},
		{"rounding 100", Arb{ProfitPct: 5, Stakes: legs(2.1, 50, 2.1, 50)}, split(10050, 100), []float64{5000, 5000}, 10000, 500, 5, -1},
		{"rounding 10 negative", Arb{ProfitPct: 0, Stakes: legs(3, 33.34, 1.5, 66.66)}, split(10000, 10), []float64{3330, 6670}, 10000, -10, -0.1, -1},
		{"maxleg rounding", Arb{ProfitPct: 3, Stakes: legs(1.8, 55, 2.2, 45)}, maxleg(3090, 100), []float64{3000, 2500}, 5500, -100, -100.0 / 5500 * 100, 0},
		{"max below step", Arb{ProfitPct: 3, Stakes: legs(1.8, 55, 2.2, 45)}, maxleg(90, 100), []float64{0, 0}, 0, 0, 3, -1},
		{"whole units", Arb{ProfitPct: 3, Stakes: legs(3, 33.34, 1.5, 66.66)}, split(10000, 1), []float64{3334, 6666}, 10000, 300, 3, -1},
		{"fav by lowest odds", Arb{ProfitPct: 3, Stakes: legs(2.4, 45, 1.7, 55)}, maxleg(2000, 1), []float64{1636, 2000}, 3636, 109, 3, 1},
		{"zero total", Arb{ProfitPct: 3, Stakes: legs(2, 50, 2, 50)}, split(0, 1), []float64{0, 0}, 0, 0, 3, -1},
		{"1/odds fallback", Arb{ProfitPct: 3, Stakes: legs(1.5, 0, 3, 0)}, split(9000, 1), []float64{6000, 3000}, 9000, 270, 3, -1},
		{"anchor book", Arb{ProfitPct: 2, Stakes: []Stake{{Bookmaker: "fortuna", DecimalOdds: 1.7, StakePct: 58}, {Bookmaker: "betano", DecimalOdds: 2.4, StakePct: 42}}},
			Bankroll{Mode: "maxleg", MaxLeg: 2000, Currency: "CZK", Rounding: 1, AnchorBook: &anchor}, []float64{2762, 2000}, 4762, 95, 2, 1},
	}
	for _, c := range cases {
		p := ComputeStakes(c.arb, c.bk)
		if !reflect.DeepEqual(p.Stakes, c.stakes) || p.Total != c.total || p.Profit != c.profit || p.FavIdx != c.favIdx || math.Abs(p.ROI-c.roi) > 1e-9 {
			t.Errorf("%s: got %+v, want stakes=%v total=%v profit=%v roi=%v fav=%d", c.name, p, c.stakes, c.total, c.profit, c.roi, c.favIdx)
		}
	}
}

func TestJSRound_HalvesGoUp(t *testing.T) {
	if jsRound(2.5) != 3 || jsRound(-2.5) != -2 || jsRound(-2.6) != -3 {
		t.Fatalf("jsRound must match Math.round: got %v %v %v", jsRound(2.5), jsRound(-2.5), jsRound(-2.6))
	}
}

func TestLosesAfterRounding(t *testing.T) {
	a := Arb{ProfitPct: 0, Stakes: legs(3, 33.34, 1.5, 66.66)}
	if !LosesAfterRounding(a, split(10000, 10)) {
		t.Fatal("rounded stakes lose 10 → want true")
	}
	if LosesAfterRounding(a, split(10000, 1)) {
		t.Fatal("rounding off → always false")
	}
}
