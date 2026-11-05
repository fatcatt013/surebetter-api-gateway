package telegram

import (
	"strings"
	"testing"
	"time"
)

func prague(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// sampleNow: 2026-11-04 14:32:43 Prague (CET). SampleArb puts first detection 38s earlier (14:32:05)
// and the start at 14:30:00.
func sampleNow(t *testing.T) time.Time {
	return time.Date(2026, 11, 4, 14, 32, 43, 0, prague(t))
}

func newTestRenderer(t *testing.T) *Renderer {
	t.Helper()
	r, err := NewRenderer("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const goldenLive = `🟢 <b>+296 CZK</b> · <i>+2.96%</i> · 🎾 Tennis · 🔴 LIVE
<b>Cody Benton — Kai-Luca Ampaw</b>
<i>ITF M25 Monastir</i> · started 14:30
━━━━━━━━━━━━━━━━━━━━━━
<blockquote>🟧 <b>Betano</b> · <i>Handicap (gamy)</i> · @ <b>2.10</b>
<b><u>Cody Benton +3.5</u></b>
Stake <b>4 903 CZK</b> · <a href="https://www.betano.cz/">open ↗</a></blockquote>
<blockquote>🟦 <b>Tipsport</b> · <i>Handicap na gamy</i> · @ <b>2.02</b>
<b><u>Kai-Luca Ampaw -3.5</u></b>
Stake <b>5 097 CZK</b> · <a href="https://www.tipsport.cz/">open ↗</a></blockquote>
━━━━━━━━━━━━━━━━━━━━━━
💰 10 000 CZK → 10 296 CZK (<b>+296 CZK</b>)
⏱ found 14:32:05 · preset: <i>Live tennis &gt;2%</i>`

func TestRender_LiveGolden(t *testing.T) {
	now := sampleNow(t)
	got := newTestRenderer(t).Render(View{Arb: SampleArb(now), Bankroll: DefaultBankroll, PresetName: "Live tennis >2%", Now: now})
	if got != goldenLive {
		t.Fatalf("live render mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, goldenLive)
	}
}

func TestRender_GoneGolden(t *testing.T) {
	now := sampleNow(t)
	a := SampleArb(now)
	goneAt := time.UnixMilli(a.FirstDetectedAt).Add(2*time.Minute + 14*time.Second)
	got := newTestRenderer(t).Render(View{Arb: a, Bankroll: DefaultBankroll, PresetName: "Live tennis >2%", Now: goneAt, Gone: true, GoneAt: goneAt, PeakProfit: 3.10})
	wantHead := "❌ <b>GONE</b> after 2m 14s · <s>+2.96%</s> (peak +3.10%)\n<b>Cody Benton — Kai-Luca Ampaw</b>\n"
	if !strings.HasPrefix(got, wantHead) {
		t.Fatalf("gone head mismatch:\n%s", got)
	}
	if !strings.Contains(got, "@ <s>2.10</s>\n") || !strings.Contains(got, "Stake <s>4 903 CZK</s> · <a href=\"https://www.betano.cz/\">open ↗</a>") {
		t.Fatalf("gone leg must strike odds and stake:\n%s", got)
	}
}

func TestRender_TestPrefixPrematchEscapingNoURL(t *testing.T) {
	now := sampleNow(t)
	a := SampleArb(now)
	a.Sport = "football"
	a.MarketType = "TOTAL_GOALS"
	a.MarketName = "Total goals"
	a.Line = 2.5
	a.NameHome = "Brighton & Hove <Albion>"
	a.StartTime = now.Add(26 * time.Hour).UnixMilli()
	a.Stakes = append([]Stake(nil), a.Stakes...)
	a.Stakes[1].MatchURL = ""
	a.Stakes[1].MarketName = ""
	got := newTestRenderer(t).Render(View{Arb: a, Bankroll: DefaultBankroll, Now: now, Test: true})
	for _, want := range []string{
		"🧪 TEST · 🟢 <b>+296 CZK</b> · <i>+2.96%</i> · ⚽ Football · 🕒 PREMATCH",
		"<b>Brighton &amp; Hove &lt;Albion&gt; — Kai-Luca Ampaw</b>",
		"· starts 5 Nov 16:32",
		"🟦 <b>Tipsport</b> · <i>Total goals 2.5</i> · @ <b>2.02</b>", // no leg market name → the arb's market
		"Stake <b>5 097 CZK</b></blockquote>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "preset:") {
		t.Errorf("no preset name → no preset part:\n%s", got)
	}
}

func TestRender_NegativeProfitAndEUR(t *testing.T) {
	now := sampleNow(t)
	a := SampleArb(now)
	a.ProfitPct = 0
	a.Stakes = []Stake{{Bookmaker: "betano", Label: "A", DecimalOdds: 3, StakePct: 33.34}, {Bookmaker: "fortuna", Label: "B", DecimalOdds: 1.5, StakePct: 66.66}}
	bk := Bankroll{Mode: "split", Total: 10000, Currency: "EUR", Rounding: 10}
	got := newTestRenderer(t).Render(View{Arb: a, Bankroll: bk, Now: now})
	for _, want := range []string{"🟠 <b>-10 EUR</b> · <i>-0.10%</i>", "💰 10 000 EUR → 9 990 EUR (<b>-10 EUR</b>)", "🟨 <b>Fortuna</b> · <i>Game handicap ±3.5</i>", "<b><u>B</u></b>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestStripLineSuffix(t *testing.T) {
	for _, c := range []struct {
		name string
		line float64
		want string
	}{
		{"Počet gamů v zápasu 22.5 ", 22.5, "Počet gamů v zápasu"},
		{"Handicap -1,5", 1.5, "Handicap"},
		{"Handicap 2.5", 1.5, "Handicap 2.5"}, // another number is kept
		{"Vítěz zápasu", 0, "Vítěz zápasu"},
		{"", 3.5, ""},
	} {
		if got := stripLineSuffix(c.name, c.line); got != c.want {
			t.Errorf("stripLineSuffix(%q, %v) = %q, want %q", c.name, c.line, got, c.want)
		}
	}
}

func TestFormatHelpers(t *testing.T) {
	if groupNumber(1234567) != "1 234 567" || groupNumber(-10) != "-10" || groupNumber(999) != "999" {
		t.Fatal("groupNumber")
	}
	for d, want := range map[time.Duration]string{45 * time.Second: "45s", 134 * time.Second: "2m 14s", 63 * time.Minute: "1h 03m"} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if bookmakerName("sazkabet") != "SazkaBet" || bookmakerName("newbook") != "Newbook" {
		t.Fatal("bookmakerName")
	}
	if e, l := sportMeta("table-tennis-live"); e != "🏓" || l != "Table tennis" {
		t.Fatal("sportMeta known")
	}
	if e, l := sportMeta("handball"); e != "🏅" || l != "Handball" {
		t.Fatal("sportMeta fallback")
	}
}
