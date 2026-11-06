package telegram

import "testing"

const nowMs = int64(1_800_000_000_000)

func fArb(sport, market string, profit float64, books ...string) Arb {
	a := Arb{Sport: sport, MarketType: market, ProfitPct: profit, FirstDetectedAt: nowMs - 60_000}
	for _, b := range books {
		a.Stakes = append(a.Stakes, Stake{Bookmaker: b, DecimalOdds: 2.1, StakePct: 50})
	}
	return a
}

func f64(v float64) *float64 { return &v }

func TestMatches(t *testing.T) {
	bk := DefaultBankroll
	base := fArb("tennis-live", "MATCH_WINNER", 2.5, "betano", "tipsport")
	cases := []struct {
		name string
		arb  Arb
		f    func(*Filter)
		want bool
	}{
		{"open filter passes", base, func(*Filter) {}, true},
		{"every leg at a selected book", base, func(f *Filter) { f.Bookmakers = []string{"betano", "tipsport"} }, true},
		{"one leg outside the selection", base, func(f *Filter) { f.Bookmakers = []string{"betano"} }, false},
		{"[] keeps nothing", base, func(f *Filter) { f.Bookmakers = []string{} }, false},
		{"no legs never passes a book filter", fArb("tennis", "MATCH_WINNER", 2), func(f *Filter) { f.Bookmakers = []string{"betano"} }, false},
		{"2-leg arb kept when 3-leg excluded", base, func(f *Filter) { f.ThreeLeg = "exclude" }, true},
		{"3-leg arb dropped when excluded", fArb("football", "MATCH_WINNER", 2, "betano", "tipsport", "fortuna"), func(f *Filter) { f.ThreeLeg = "exclude" }, false},
		{"3-leg arb kept when included", fArb("football", "MATCH_WINNER", 2, "betano", "tipsport", "fortuna"), func(*Filter) {}, true},
		{"live only keeps live", base, func(f *Filter) { f.LiveState = "live" }, true},
		{"prematch only drops live", base, func(f *Filter) { f.LiveState = "prematch" }, false},
		{"live only drops prematch", fArb("tennis", "MATCH_WINNER", 2, "betano"), func(f *Filter) { f.LiveState = "live" }, false},
		{"sport by base sport", base, func(f *Filter) { f.Sports = []string{"tennis"} }, true},
		{"other sport", base, func(f *Filter) { f.Sports = []string{"football"} }, false},
		{"blocked market", base, func(f *Filter) { f.BlockedMarkets = map[string][]string{"tennis": {"MATCH_WINNER"}} }, false},
		{"blocked in another sport only", base, func(f *Filter) { f.BlockedMarkets = map[string][]string{"football": {"MATCH_WINNER"}} }, true},
		{"profit min inclusive", base, func(f *Filter) { f.ProfitMin = f64(2.5) }, true},
		{"profit below min", base, func(f *Filter) { f.ProfitMin = f64(2.6) }, false},
		{"profit max inclusive", base, func(f *Filter) { f.ProfitMax = f64(2.5) }, true},
		{"zero bound is applied", base, func(f *Filter) { f.ProfitMax = f64(0) }, false},
		{"old enough", base, func(f *Filter) { f.MinAgeSec = 60 }, true},
		{"too young", base, func(f *Filter) { f.MinAgeSec = 61 }, false},
		{"no timestamps = infinitely old", Arb{Sport: "tennis", Stakes: base.Stakes, ProfitPct: 2}, func(f *Filter) { f.MinAgeSec = 3600 }, true},
	}
	for _, c := range cases {
		f := openFilter()
		c.f(&f)
		if got := Matches(c.arb, f, bk, nowMs); got != c.want {
			t.Errorf("%s: Matches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMatches_DropsArbsThatLoseAfterRounding(t *testing.T) {
	a := Arb{Sport: "tennis", ProfitPct: 0, Stakes: legs(3, 33.34, 1.5, 66.66)}
	if Matches(a, openFilter(), split(10000, 10), nowMs) {
		t.Fatal("an arb losing money after rounding must not match")
	}
	if !Matches(a, openFilter(), split(10000, 1), nowMs) {
		t.Fatal("with rounding off it matches")
	}
}

func TestBaseSportAndIsLive(t *testing.T) {
	if !IsLive("ice-hockey-live") || IsLive("ice-hockey") || BaseSport("ice-hockey-live") != "ice-hockey" {
		t.Fatal("suffix helpers are wrong")
	}
}

func TestBaseSport_CollapsesEsportsGames(t *testing.T) {
	for in, want := range map[string]string{
		"esports-cs2": "esports", "esports-cs2-live": "esports", "esports-dota2": "esports",
		"esports": "esports", "tennis-doubles-live": "tennis-doubles", "football": "football",
	} {
		if got := BaseSport(in); got != want {
			t.Errorf("BaseSport(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsLive("esports-cs2-live") || IsLive("esports-cs2") {
		t.Fatal("IsLive must read the -live suffix of an esports slug")
	}
}

// The app's sport filter and market prefs are keyed "esports" for every game.
func TestMatches_EsportsFilteredAsOneSport(t *testing.T) {
	a := fArb("esports-dota2-live", "MATCH_WINNER", 2.5, "betano", "tipsport")
	f := openFilter()
	f.Sports = []string{"esports"}
	if !Matches(a, f, DefaultBankroll, nowMs) {
		t.Fatal("an esports-dota2-live arb must pass an Esports sport filter")
	}
	f.Sports = []string{"football"}
	if Matches(a, f, DefaultBankroll, nowMs) {
		t.Fatal("an esports arb must not pass a football-only filter")
	}
	f = openFilter()
	f.BlockedMarkets = map[string][]string{"esports": {"MATCH_WINNER"}}
	if Matches(a, f, DefaultBankroll, nowMs) {
		t.Fatal("markets blocked under esports must block every game")
	}
}
