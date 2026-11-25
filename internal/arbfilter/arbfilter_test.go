package arbfilter

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

const nowMs = int64(1_800_000_000_000)

func f64(v float64) *float64 { return &v }

func twoLegs(sport, market string, profit float64) Arb {
	return Arb{Sport: sport, MarketType: market, ProfitPct: profit, FirstDetectedAt: nowMs - 60_000,
		Stakes: []Stake{{Bookmaker: "tipsport", DecimalOdds: 2.1, StakePct: 50}, {Bookmaker: "fortuna", DecimalOdds: 2.1, StakePct: 50}}}
}

func openFilter() Filter { return Filter{LiveState: "all", ThreeLeg: "include"} }

// Reject names the first step that fails, in the app's order.
func TestReject_StepsInOrder(t *testing.T) {
	a := twoLegs("tennis-live", "TOTAL_GAMES", 3)
	three := a
	three.Stakes = append(slices.Clone(a.Stakes), Stake{Bookmaker: "betano", DecimalOdds: 9})
	cases := []struct {
		name string
		arb  Arb
		f    func(*Filter)
		want string
	}{
		{"passes", a, func(*Filter) {}, ""},
		{"three legs excluded", three, func(f *Filter) { f.ThreeLeg = "exclude"; f.LiveState = "prematch" }, StepThreeLeg},
		{"live state", a, func(f *Filter) { f.LiveState = "prematch"; f.Sports = []string{"football"} }, StepLiveState},
		{"sport", a, func(f *Filter) { f.Sports = []string{"football"}; f.BlockedMarkets = map[string][]string{"tennis": {"TOTAL_GAMES"}} }, StepSport},
		{"market", a, func(f *Filter) { f.BlockedMarkets = map[string][]string{"tennis": {"TOTAL_GAMES"}}; f.ProfitMin = f64(5) }, StepMarket},
		{"profit min", a, func(f *Filter) { f.ProfitMin = f64(5); f.ProfitMax = f64(1) }, StepProfitMin},
		{"profit max", a, func(f *Filter) { f.ProfitMax = f64(1); f.MinAgeSec = 3600 }, StepProfitMax},
		{"min age", a, func(f *Filter) { f.MinAgeSec = 61 }, StepMinAge},
		{"old enough", a, func(f *Filter) { f.MinAgeSec = 60 }, ""},
	}
	for _, c := range cases {
		f := openFilter()
		c.f(&f)
		if got := Reject(c.arb, f, nowMs); got != c.want {
			t.Errorf("%s: Reject = %q, want %q", c.name, got, c.want)
		}
	}
}

// A leg taken from an alternative carries that quote's selection id, market ref, link, label and fair odds.
func TestRepick_KeepsTheQuotesSourceIDsAndFairOdds(t *testing.T) {
	a := Arb{ProfitPct: 1, Stakes: []Stake{
		{Bookmaker: "tipsport", OutcomeID: "OVER", Label: "Over", DecimalOdds: 2.1, StakePct: 48, MatchURL: "t/m", SourceID: "t-1", SourceMarketRef: "tm", FairOdds: f64(2.0)},
		{Bookmaker: "kingsbet", OutcomeID: "UNDER", Label: "Under K", DecimalOdds: 2.2, StakePct: 52, MatchURL: "k/m", SourceID: "k-1",
			Alternatives: []Quote{{Bookmaker: "fortuna", Label: "Under F", DecimalOdds: 2.05, MatchURL: "f/m", MarketName: "Počet", SourceID: "f-9", SourceMarketRef: "fm", FairOdds: f64(1.98)}}},
	}}
	r, ok := ForBookList(a, []string{"tipsport", "fortuna"})
	if !ok {
		t.Fatal("tipsport + fortuna price both outcomes at a profit")
	}
	u := r.Stakes[1]
	if u.Bookmaker != "fortuna" || u.SourceID != "f-9" || u.SourceMarketRef != "fm" || u.MatchURL != "f/m" || u.Label != "Under F" ||
		u.MarketName != "Počet" || u.OutcomeID != "UNDER" || u.FairOdds == nil || *u.FairOdds != 1.98 {
		t.Fatalf("re-picked leg = %+v", u)
	}
	if o := r.Stakes[0]; o.SourceID != "t-1" || o.SourceMarketRef != "tm" || o.FairOdds == nil || *o.FairOdds != 2.0 {
		t.Fatalf("kept leg lost its fields: %+v", o)
	}
	if _, ok := ForBookList(a, []string{"tipsport"}); ok {
		t.Fatal("tipsport alone has no UNDER price")
	}
}

// ComputeStakes gives the app's stakes for the shared vectors (surebetter-common
// docs/superpowers/plans/2026-11-14-stake-vectors.json, made by the app's computeStakes; copied byte for byte).
func TestComputeStakes_SharedVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/stake-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases []struct {
			Odds     []float64 `json:"odds"`
			Books    []string  `json:"books"`
			Bankroll struct {
				Mode       string  `json:"mode"`
				Total      float64 `json:"total"`
				MaxLeg     float64 `json:"maxLeg"`
				AnchorBook *string `json:"anchorBook"`
				Rounding   int     `json:"rounding"`
			} `json:"bankroll"`
			Stakes []float64 `json:"stakes"`
			Total  float64   `json:"total"`
			FavIdx int       `json:"favIdx"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Cases) == 0 {
		t.Fatalf("vectors: %v (%d cases)", err, len(v.Cases))
	}
	for i, c := range v.Cases {
		a := Arb{}
		for j, o := range c.Odds {
			a.Stakes = append(a.Stakes, Stake{Bookmaker: c.Books[j], DecimalOdds: o})
		}
		b := Bankroll{Mode: c.Bankroll.Mode, Total: c.Bankroll.Total, MaxLeg: c.Bankroll.MaxLeg, AnchorBook: c.Bankroll.AnchorBook, Rounding: c.Bankroll.Rounding}
		p := ComputeStakes(a, b)
		if !slices.Equal(p.Stakes, c.Stakes) || p.Total != c.Total || p.FavIdx != c.FavIdx {
			t.Fatalf("case %d %+v: got %v total %v fav %d, want %v total %v fav %d", i, c, p.Stakes, p.Total, p.FavIdx, c.Stakes, c.Total, c.FavIdx)
		}
	}
}
