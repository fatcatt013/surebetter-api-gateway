package telegram

import (
	"math"
	"strings"
	"testing"
	"time"
)

// ownerArb is the reported case: tipsport 1.35 / kingsbet 4.10, betano's 4.00 an alternative.
func ownerArb() Arb {
	return Arb{
		Sport: "tennis", MarketType: "MATCH_WINNER", ProfitPct: (1 - (1/1.35 + 1/4.10)) * 100, FirstDetectedAt: nowMs - 60_000,
		Stakes: []Stake{
			{Bookmaker: "tipsport", OutcomeID: "HOME", Label: "Home", DecimalOdds: 1.35, StakePct: 75.2, MatchURL: "t/m", MarketName: "Vítěz",
				Alternatives: []Quote{{Bookmaker: "betano", Label: "Home B", DecimalOdds: 1.30, MatchURL: "b/m", MarketName: "Zápas"}}},
			{Bookmaker: "kingsbet", OutcomeID: "AWAY", Label: "Away K", DecimalOdds: 4.10, StakePct: 24.8, MatchURL: "k/m", MarketName: "1-2",
				Alternatives: []Quote{
					{Bookmaker: "betano", Label: "Away B", DecimalOdds: 4.00, MatchURL: "b/m", MarketName: "Zápas"},
					{Bookmaker: "tipsport", Label: "Away T", DecimalOdds: 3.20, MatchURL: "t/m", MarketName: "Vítěz"},
				}},
		},
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestRepick_AllBooksLeavesTheArbUnchanged(t *testing.T) {
	a := ownerArb()
	r, ok := Repick(a, nil)
	if !ok || r.Stakes[1].Bookmaker != "kingsbet" || len(r.Stakes[1].Alternatives) != 2 || r.ProfitPct != a.ProfitPct {
		t.Fatalf("nil books changed the arb: %+v", r)
	}
	r, ok = Repick(a, []string{"tipsport", "kingsbet"})
	if !ok || r.ProfitPct != a.ProfitPct || r.Stakes[0].StakePct != 75.2 || r.Stakes[1].Bookmaker != "kingsbet" {
		t.Fatalf("winners all allowed: profit and stakes must stay the engine's: %+v", r)
	}
}

func TestRepick_SwapsToTheBestAllowedBook(t *testing.T) {
	a := ownerArb()
	r, ok := Repick(a, []string{"tipsport", "betano"})
	if !ok {
		t.Fatal("tipsport + betano price both outcomes")
	}
	away := r.Stakes[1]
	if away.Bookmaker != "betano" || away.DecimalOdds != 4.00 || away.Label != "Away B" || away.MatchURL != "b/m" || away.MarketName != "Zápas" || away.OutcomeID != "AWAY" {
		t.Fatalf("away leg = %+v, want betano's 4.00", away)
	}
	if r.Stakes[0].Bookmaker != "tipsport" || r.Stakes[0].DecimalOdds != 1.35 {
		t.Fatalf("home leg = %+v, want tipsport kept (1.35 > betano 1.30)", r.Stakes[0])
	}
	index := 1/1.35 + 1/4.00
	if !near(r.ProfitPct, (1-index)*100) {
		t.Fatalf("profit = %v, want %v", r.ProfitPct, (1-index)*100)
	}
	if !near(r.Stakes[0].StakePct, (1/1.35)/index*100) || !near(r.Stakes[1].StakePct, (1/4.00)/index*100) {
		t.Fatalf("stake_pct = %v / %v", r.Stakes[0].StakePct, r.Stakes[1].StakePct)
	}
	if r.Stakes[0].Alternatives != nil || r.Stakes[1].Alternatives != nil {
		t.Fatal("a re-picked arb carries no alternatives")
	}
	if a.Stakes[1].Bookmaker != "kingsbet" {
		t.Fatal("the input arb must not be modified")
	}
}

func TestRepick_NoAllowedPriceForAnOutcome(t *testing.T) {
	if _, ok := Repick(ownerArb(), []string{"kingsbet", "fortuna"}); ok {
		t.Fatal("HOME has no price at kingsbet or fortuna")
	}
	if _, ok := Repick(ownerArb(), []string{}); ok {
		t.Fatal("[] allows no book")
	}
}

func TestRepick_ProfitCanFallBelowZero(t *testing.T) {
	r, ok := Repick(ownerArb(), []string{"tipsport"})
	if !ok || r.Stakes[1].DecimalOdds != 3.20 || r.ProfitPct >= 0 {
		t.Fatalf("tipsport alone: 1.35 / 3.20 is a loss, got %+v", r)
	}
}

func TestSelect_ReturnsTheRepickedArbAndJudgesItsProfit(t *testing.T) {
	f := openFilter()
	f.Bookmakers = []string{"tipsport", "betano"}
	r, ok := Select(ownerArb(), f, DefaultBankroll, nowMs)
	if !ok || r.Stakes[1].Bookmaker != "betano" {
		t.Fatalf("Select = %+v, %v", r, ok)
	}
	f.ProfitMin = f64(r.ProfitPct + 0.01)
	if Matches(ownerArb(), f, DefaultBankroll, nowMs) {
		t.Fatal("the profit minimum applies to the re-picked profit, not the engine's")
	}
	f.ProfitMin = nil
	f.Bookmakers = []string{"tipsport"}
	if Matches(ownerArb(), f, DefaultBankroll, nowMs) {
		t.Fatal("a re-picked profit ≤ 0 is not an arb")
	}
}

// A posted arb follows the preset's books: its edits show the re-picked prices, and it goes
// GONE (reason removed) when those books no longer make it an arb.
func TestNotifier_PostsAndEditsTheRepickedArb(t *testing.T) {
	st := newMemStore()
	st.cfg = validConfig()
	st.cfg.Filter.Bookmakers = []string{"tipsport", "betano"}
	n, out := newTestNotifier(t, st, nil)
	now := t0()
	a := ownerArb()
	a.FirstDetectedAt = now.UnixMilli() - 60_000
	n.handle(bg, upsert("k", a), now)
	j := onlyJob(t, out, JobSend)
	if strings.Contains(strings.ToLower(j.Text), "kingsbet") {
		t.Fatalf("posted a leg at an unselected book:\n%s", j.Text)
	}
	n.onResult(bg, Result{Job: j, MessageID: 7}, now)
	if got := n.tracked["k"].Arb.Stakes[1].Bookmaker; got != "betano" {
		t.Fatalf("tracked away leg at %s, want betano", got)
	}

	// betano's away price drops: tipsport + betano no longer make an arb → GONE.
	b := ownerArb()
	b.FirstDetectedAt = a.FirstDetectedAt
	b.Stakes[1].Alternatives[0].DecimalOdds = 3.50
	n.handle(bg, upsert("k", b), now.Add(time.Minute))
	onlyJob(t, out, JobGone)
}
