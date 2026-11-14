package main

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func pragueAt(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, prague)
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func mustQuery(t *testing.T, raw string, now time.Time) statsFilter {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := parseStatsQuery(q, now)
	if !ok {
		t.Fatalf("%q: rejected", raw)
	}
	return f
}

func fptr(v float64) *float64 { return &v }

func TestStatsQuery_Defaults(t *testing.T) {
	now := pragueAt(2026, 11, 13, 12, 30)
	f := mustQuery(t, "", now)
	// The last 14 Prague days: today and the 13 before, from midnight.
	want := statsFilter{from: ms(pragueAt(2026, 10, 31, 0, 0)), to: ms(now), bucket: "day"}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("got %+v\nwant %+v", f, want)
	}
	// Only to: the 14 Prague days ending with to's day (to itself excluded: a midnight to ends the day before).
	f = mustQuery(t, "to="+itoa(ms(pragueAt(2026, 10, 26, 0, 0))), now)
	if f.from != ms(pragueAt(2026, 10, 12, 0, 0)) || f.to != ms(pragueAt(2026, 10, 26, 0, 0)) {
		t.Fatalf("only to: %+v", f)
	}
	// Only from: to = now.
	f = mustQuery(t, "from="+itoa(ms(now)-1000), now)
	if f.from != ms(now)-1000 || f.to != ms(now) {
		t.Fatalf("only from: %+v", f)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestStatsQuery_AllParameters(t *testing.T) {
	now := pragueAt(2026, 11, 13, 12, 30)
	f := mustQuery(t, "from=1000&to=2000&include=tipsport,betano&exclude=fortuna&mode=only&minProfit=1.5&maxProfit=0.5e1"+
		"&minLife=0&maxLife=600&sport=esports,tennis&exSport=football&market=TOTAL_GAMES&live=prematch&bucket=week&cursor=1500.7", now)
	want := statsFilter{from: 1000, to: 2000, include: map[string]bool{"tipsport": true, "betano": true},
		exclude: map[string]bool{"fortuna": true}, only: true, minProfit: fptr(1.5), maxProfit: fptr(5),
		minLife: fptr(0), maxLife: fptr(600), sports: map[string]bool{"esports": true, "tennis": true}, exSports: map[string]bool{"football": true},
		market: "TOTAL_GAMES", live: "prematch", bucket: "week",
		hasCursor: true, cursorAt: 1500, cursorID: 7}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("got %+v\nwant %+v", f, want)
	}
	// Empty values are "not set"; mode=any and live=all are the defaults.
	f = mustQuery(t, "include=&exclude=&mode=any&live=all&sport=&market=&minProfit=&bucket=hour&from=0&to=3600000", now)
	if f.include != nil || f.exclude != nil || f.only || f.live != "" || f.minProfit != nil || f.bucket != "hour" {
		t.Fatalf("empty values: %+v", f)
	}
}

func TestStatsQuery_BadValues(t *testing.T) {
	now := pragueAt(2026, 11, 13, 12, 30)
	day := int64(24 * time.Hour / time.Millisecond)
	for _, raw := range []string{
		"from=abc", "to=1.5", "from=-1", "from=2000&to=2000", "from=3000&to=2000",
		"from=0&to=" + itoa(400*day+1), // more than 400 days
		"include=Tipsport", "include=a,,b", "include=x_y", "exclude=" + long(41),
		"include=a1,a2,a3,a4,a5,a6,a7,a8,a9,a10,a11,a12,a13,a14,a15,a16,a17,a18,a19,a20,a21",
		"mode=all", "minProfit=x", "maxProfit=NaN", "minProfit=Inf", "minProfit=5&maxProfit=4",
		"minLife=-1", "maxLife=y", "minLife=10&maxLife=5", "sport=Tennis", "sport=" + long(41), "sport=tennis,,football", "exSport=Football",
		"sport=a1,a2,a3,a4,a5,a6,a7,a8,a9,a10,a11,a12,a13,a14,a15,a16,a17,a18,a19,a20,a21", "market=total goals",
		"live=yes", "bucket=month", "cursor=x", "cursor=1.", "cursor=.5", "cursor=1.-5",
		"from=0&to=" + itoa(2001*3600_000) + "&bucket=hour", // more than 2000 points
	} {
		q, _ := url.ParseQuery(raw)
		if f, ok := parseStatsQuery(q, now); ok {
			t.Errorf("%q accepted: %+v", raw, f)
		}
	}
	// 400 days exactly and 2000 hour buckets exactly are fine.
	mustQuery(t, "from=0&to="+itoa(400*day), now)
	mustQuery(t, "from=0&to="+itoa(2000*3600_000)+"&bucket=hour", now)
}

func long(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

// statRow is a 2-leg tennis live arb sent at `at`, detected 10 s before, removed 60 s after.
func statRow(id int64, at time.Time, books ...string) accounts.TelegramStatRow {
	if books == nil {
		books = []string{"fortuna", "tipsport"}
	}
	return accounts.TelegramStatRow{ID: id, SentAt: ms(at), FirstDetectedAt: ms(at) - 10_000, EndedAt: ms(at) + 60_000,
		Sport: "tennis", IsLive: true, MarketType: "TOTAL_GAMES", Line: 21.5, ProfitPct: 2.5, Profit: 250, Currency: "CZK",
		EndReason: "removed", Books: books}
}

func TestStatsFilter_Keep(t *testing.T) {
	now := pragueAt(2026, 11, 13, 12, 0)
	at := pragueAt(2026, 11, 13, 10, 0)
	base := statRow(1, at) // fortuna + tipsport, 2.5 %, lifetime 70 s
	three := statRow(2, at, "fortuna", "tipsport", "betano")
	noEnd := base
	noEnd.EndedAt, noEnd.EndReason = 0, "unknown"
	noFirst := base
	noFirst.FirstDetectedAt = 0
	cs2 := base
	cs2.Sport = "esports-cs2"
	pre := base
	pre.IsLive = false
	for _, c := range []struct {
		query string
		row   accounts.TelegramStatRow
		want  bool
	}{
		{"", base, true},
		{"", noEnd, true},
		{"include=tipsport", base, true},
		{"include=betano", base, false},
		{"include=betano,fortuna", base, true},
		{"include=fortuna,tipsport&mode=only", base, true},
		{"include=fortuna,tipsport&mode=only", three, false},
		{"include=fortuna,tipsport,betano&mode=only", three, true},
		{"include=fortuna&mode=only", base, false},
		{"exclude=betano", base, true},
		{"exclude=betano", three, false},
		{"include=fortuna&exclude=betano", three, false},
		{"minProfit=2.5", base, true},
		{"minProfit=2.51", base, false},
		{"maxProfit=2.5", base, true},
		{"maxProfit=2.49", base, false},
		{"minLife=70&maxLife=70", base, true},
		{"minLife=71", base, false},
		{"maxLife=69", base, false},
		{"minLife=0", noEnd, false}, // unknown lifetime: out once a life filter is set
		{"maxLife=1000", noFirst, false},
		{"minProfit=0", noFirst, true},
		{"sport=tennis", base, true},
		{"sport=football", base, false},
		{"sport=esports", cs2, true},
		{"sport=esports-cs2", cs2, true},
		{"sport=esports-lol", cs2, false},
		{"sport=esports", base, false},
		{"sport=football,tennis", base, true},
		{"sport=football,esports", cs2, true},
		{"sport=football,esports", base, false},
		{"exSport=esports", cs2, false},
		{"exSport=esports", base, true},
		{"exSport=tennis", base, false},
		{"sport=tennis,esports&exSport=esports", cs2, false},
		{"market=TOTAL_GAMES", base, true},
		{"market=TOTAL_GOALS", base, false},
		{"live=live", base, true},
		{"live=live", pre, false},
		{"live=prematch", pre, true},
		{"live=prematch", base, false},
	} {
		f := mustQuery(t, c.query, now)
		if got := f.keep(c.row); got != c.want {
			t.Errorf("%q on %v (books %v): %v, want %v", c.query, c.row.ID, c.row.Books, got, c.want)
		}
	}
}

func compute(t *testing.T, query string, now time.Time, rows []accounts.TelegramStatRow, seen ...string) statsAnswer {
	t.Helper()
	f := mustQuery(t, query, now)
	set := map[string]bool{}
	for _, b := range seen {
		set[b] = true
	}
	var kept []accounts.TelegramStatRow
	for _, r := range rows {
		for _, b := range r.Books {
			set[b] = true
		}
		if f.keep(r) {
			kept = append(kept, r)
		}
	}
	return computeStats(f, kept, set)
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // readable "<15 s"; the API's \u003c is the same JSON
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func TestStats_EmptySet(t *testing.T) {
	now := pragueAt(2026, 11, 13, 12, 0)
	from, to := pragueAt(2026, 11, 12, 0, 0), pragueAt(2026, 11, 13, 0, 0)
	a := compute(t, "from="+itoa(ms(from))+"&to="+itoa(ms(to)), now, nil, "tipsport")
	got := jsonOf(t, a)
	for _, want := range []string{
		`"range":{"from":` + itoa(ms(from)) + `,"to":` + itoa(ms(to)) + `}`,
		`"kpis":{"arbs":0,"perDay":0,"avgProfitPct":null,"medianProfitPct":null,"profit":0,"currency":"CZK","medianLifeS":null,"medianAfterSendS":null}`,
		`"series":{"bucket":"day","points":[{"t":` + itoa(ms(from)) + `,"live":0,"prematch":0,"avgProfitPct":null,"profit":0}]}`,
		`"markets":[]`, `"pairs":[]`, `"rows":[]`, `"nextCursor":null`,
		`"bookmakers":[{"id":"tipsport","legs":0}]`,
		`"endReasons":{"removed":0,"evicted":0,"redetected":0,"absent":0,"unknown":0,"open":0}`,
		`"profitHist":[{"label":"0–1","from":0,"to":1,"arbs":0},{"label":"1–2","from":1,"to":2,"arbs":0}`,
		`{"label":"10+","from":10,"to":null,"arbs":0}]`,
		`"lifeHist":[{"label":"<15 s","fromS":0,"toS":15,"arbs":0}`, `{"label":">10 min","fromS":600,"toS":null,"arbs":0}]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s\nin %s", want, got)
		}
	}
	if len(a.Heatmap) != 7 || len(a.Heatmap[0]) != 24 || len(a.ProfitHist) != 9 || len(a.LifeHist) != 7 {
		t.Fatalf("shapes: %+v", a)
	}
}

func TestStats_KPIs(t *testing.T) {
	now := pragueAt(2026, 11, 13, 12, 0)
	from, to := pragueAt(2026, 11, 11, 0, 0), pragueAt(2026, 11, 13, 0, 0) // 2 days
	r1 := statRow(1, pragueAt(2026, 11, 11, 10, 0))                        // life 70, after send 60
	r1.ProfitPct, r1.Profit = 1, 100
	r2 := statRow(2, pragueAt(2026, 11, 11, 11, 0))
	r2.ProfitPct, r2.Profit, r2.EndedAt = 2, 200, r2.SentAt+20_000 // life 30, after send 20
	r3 := statRow(3, pragueAt(2026, 11, 12, 9, 0))
	r3.ProfitPct, r3.Profit, r3.Currency, r3.EndedAt, r3.EndReason = 10, 50, "EUR", 0, "" // open
	r4 := statRow(4, pragueAt(2026, 11, 12, 10, 0))
	r4.ProfitPct, r4.Profit, r4.FirstDetectedAt = 4, -10, 0 // no detection: no lifetime, after send 60
	a := compute(t, "from="+itoa(ms(from))+"&to="+itoa(ms(to)), now, []accounts.TelegramStatRow{r1, r2, r3, r4})
	// Profit sums the currency most rows have (CZK: 3 of 4); medians of an even count average the middle two.
	want := `{"arbs":4,"perDay":2,"avgProfitPct":4.25,"medianProfitPct":3,"profit":290,"currency":"CZK","medianLifeS":50,"medianAfterSendS":60}`
	if got := jsonOf(t, a.KPIs); got != want {
		t.Fatalf("kpis\n got %s\nwant %s", got, want)
	}
	// Rounding: % to 2 decimals, seconds and per day to 1.
	r2.ProfitPct, r2.EndedAt = 2.0049, r2.FirstDetectedAt+1234
	a = compute(t, "from="+itoa(ms(from))+"&to="+itoa(ms(from)+3*86_400_000), now, []accounts.TelegramStatRow{r1, r2})
	want = `{"arbs":2,"perDay":0.7,"avgProfitPct":1.5,"medianProfitPct":1.5,"profit":300,"currency":"CZK","medianLifeS":35.6,"medianAfterSendS":25.6}`
	if got := jsonOf(t, a.KPIs); got != want {
		t.Fatalf("rounded kpis\n got %s\nwant %s", got, want)
	}
	// A tie between currencies: CZK.
	a = compute(t, "from="+itoa(ms(from))+"&to="+itoa(ms(to)), now, []accounts.TelegramStatRow{r1, r3})
	if a.KPIs.Currency != "CZK" || a.KPIs.Profit != 100 {
		t.Fatalf("tie: %+v", a.KPIs)
	}
	a = compute(t, "from="+itoa(ms(from))+"&to="+itoa(ms(to)), now, []accounts.TelegramStatRow{r3})
	if a.KPIs.Currency != "EUR" || a.KPIs.Profit != 50 {
		t.Fatalf("EUR only: %+v", a.KPIs)
	}
}

func TestStats_SeriesAndHeatmapAcrossTheAutumnClockChange(t *testing.T) {
	now := pragueAt(2026, 11, 13, 12, 0)
	// 25 Oct 2026: 03:00 CEST becomes 02:00 CET, so 02:30 happens twice.
	first := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)  // 02:30 CEST
	second := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC) // 02:30 CET
	late := pragueAt(2026, 10, 25, 23, 30)
	pre := statRow(3, late)
	pre.IsLive, pre.ProfitPct, pre.Profit = false, 5, 500
	rows := []accounts.TelegramStatRow{statRow(1, first), statRow(2, second), pre}

	dayFrom, dayTo := pragueAt(2026, 10, 25, 0, 0), pragueAt(2026, 10, 26, 0, 0)
	a := compute(t, "bucket=hour&from="+itoa(ms(dayFrom))+"&to="+itoa(ms(dayTo)), now, rows)
	if p := a.Series.Points; len(p) != 25 || p[0].T != ms(dayFrom) || p[2].T != ms(first.Truncate(time.Hour)) || p[3].T != ms(second.Truncate(time.Hour)) ||
		p[2].Live != 1 || p[3].Live != 1 || p[24].Prematch != 1 || p[24].T != ms(pragueAt(2026, 10, 25, 23, 0)) {
		t.Fatalf("hours: %s", jsonOf(t, a.Series))
	}
	if a.Series.Bucket != "hour" || a.Heatmap[6][2] != 2 || a.Heatmap[6][23] != 1 { // Sunday, 02:00 and 23:00 Prague
		t.Fatalf("heatmap: %v", a.Heatmap)
	}

	a = compute(t, "from="+itoa(ms(pragueAt(2026, 10, 24, 0, 0)))+"&to="+itoa(ms(pragueAt(2026, 10, 27, 0, 0))), now, rows)
	want := `{"bucket":"day","points":[` +
		`{"t":` + itoa(ms(pragueAt(2026, 10, 24, 0, 0))) + `,"live":0,"prematch":0,"avgProfitPct":null,"profit":0},` +
		`{"t":` + itoa(ms(pragueAt(2026, 10, 25, 0, 0))) + `,"live":2,"prematch":1,"avgProfitPct":3.33,"profit":1000},` +
		`{"t":` + itoa(ms(pragueAt(2026, 10, 26, 0, 0))) + `,"live":0,"prematch":0,"avgProfitPct":null,"profit":0}]}`
	if got := jsonOf(t, a.Series); got != want {
		t.Fatalf("days\n got %s\nwant %s", got, want)
	}

	// Weeks start on Prague Monday midnight; the first covers from (a Wednesday).
	a = compute(t, "bucket=week&from="+itoa(ms(pragueAt(2026, 10, 21, 15, 0)))+"&to="+itoa(ms(pragueAt(2026, 11, 3, 0, 0))), now, rows)
	if p := a.Series.Points; len(p) != 3 || p[0].T != ms(pragueAt(2026, 10, 19, 0, 0)) || p[1].T != ms(pragueAt(2026, 10, 26, 0, 0)) ||
		p[2].T != ms(pragueAt(2026, 11, 2, 0, 0)) || p[0].Live != 2 || p[0].Prematch != 1 {
		t.Fatalf("weeks: %s", jsonOf(t, a.Series))
	}
}

func TestStats_SpringClockChangeHasA23HourDay(t *testing.T) {
	now := pragueAt(2026, 11, 13, 12, 0)
	from, to := pragueAt(2026, 3, 29, 0, 0), pragueAt(2026, 3, 30, 0, 0)
	a := compute(t, "bucket=hour&from="+itoa(ms(from))+"&to="+itoa(ms(to)), now, []accounts.TelegramStatRow{statRow(1, pragueAt(2026, 3, 29, 3, 15))})
	if p := a.Series.Points; len(p) != 23 || p[2].T != ms(pragueAt(2026, 3, 29, 3, 0)) || p[2].Live != 1 {
		t.Fatalf("spring hours: %s", jsonOf(t, a.Series))
	}
	if a.Heatmap[6][3] != 1 {
		t.Fatalf("heatmap: %v", a.Heatmap)
	}
	a = compute(t, "bucket=hour&from="+itoa(ms(pragueAt(2026, 11, 9, 0, 0)))+"&to="+itoa(ms(pragueAt(2026, 11, 9, 1, 0))), now,
		[]accounts.TelegramStatRow{statRow(1, pragueAt(2026, 11, 9, 0, 59))})
	if a.Heatmap[0][0] != 1 { // Monday, 00:00
		t.Fatalf("monday first: %v", a.Heatmap)
	}
}

func TestStats_MarketsHistogramsReasonsPairsBooks(t *testing.T) {
	now := pragueAt(2026, 11, 13, 12, 0)
	at := pragueAt(2026, 11, 13, 9, 0)
	var rows []accounts.TelegramStatRow
	add := func(pct, lifeS float64, reason string, books ...string) {
		r := statRow(int64(len(rows)+1), at, books...)
		r.ProfitPct = pct
		r.EndedAt = r.FirstDetectedAt + int64(lifeS*1000)
		r.EndReason = reason
		if lifeS < 0 {
			r.EndedAt = 0
		}
		rows = append(rows, r)
	}
	add(0.5, 14.9, "removed", "fortuna", "tipsport")
	add(1, 15, "removed", "tipsport", "fortuna")
	add(5.99, 59.9, "evicted", "betano", "fortuna", "tipsport")
	add(6, 60, "redetected", "tipsport", "tipsport")
	add(7.9, 299, "absent")
	add(8, 300, "removed")
	add(10, 600, "removed")
	add(25, -1, "unknown")
	add(-1, -1, "")
	rows[0].MarketType, rows[0].Sport = "TOTAL_GOALS", "football"
	rows[1].MarketType, rows[1].Sport = "TOTAL_GOALS", "football"
	rows[2].MarketType = "TOTAL_GOALS" // tennis
	rows[3].MarketType, rows[3].Sport = "MATCH_WINNER", "esports-cs2" // the esports games are one sport
	rows[4].MarketType, rows[4].Sport = "MATCH_WINNER", "esports-lol"

	a := compute(t, "", now, rows, "pinnacle")
	var ph, lh []int
	for _, b := range a.ProfitHist {
		ph = append(ph, b.Arbs)
	}
	for _, b := range a.LifeHist {
		lh = append(lh, b.Arbs)
	}
	// A negative % counts in the first bin, like a lifetime below 0 would.
	if !reflect.DeepEqual(ph, []int{2, 1, 0, 0, 0, 1, 2, 1, 2}) {
		t.Fatalf("profit hist %v", ph)
	}
	if !reflect.DeepEqual(lh, []int{1, 1, 1, 1, 1, 1, 1}) { // bins are [from, to): 15 s is "15–30 s", 600 s ">10 min"
		t.Fatalf("life hist %v", lh)
	}
	if got := jsonOf(t, a.EndReasons); got != `{"removed":4,"evicted":1,"redetected":1,"absent":1,"unknown":1,"open":1}` {
		t.Fatalf("reasons %s", got)
	}
	if got := jsonOf(t, a.Markets); got != `[{"marketType":"TOTAL_GAMES","sport":"tennis","arbs":4,"avgProfitPct":10.5},`+
		`{"marketType":"MATCH_WINNER","sport":"esports","arbs":2,"avgProfitPct":6.95},`+
		`{"marketType":"TOTAL_GOALS","sport":"football","arbs":2,"avgProfitPct":0.75},`+
		`{"marketType":"TOTAL_GOALS","sport":"tennis","arbs":1,"avgProfitPct":5.99}]` {
		t.Fatalf("markets %s", got)
	}
	// Unordered pairs (a < b), one per arb; a 3-leg arb counts all three; two legs at one book make no pair.
	if got := jsonOf(t, a.Pairs); got != `[{"a":"fortuna","b":"tipsport","arbs":8},{"a":"betano","b":"fortuna","arbs":1},{"a":"betano","b":"tipsport","arbs":1}]` {
		t.Fatalf("pairs %s", got)
	}
	// Legs per book; a book seen in the range but filtered out (or only seen) has 0.
	if got := jsonOf(t, a.Bookmakers); got != `[{"id":"tipsport","legs":10},{"id":"fortuna","legs":8},{"id":"betano","legs":1},{"id":"pinnacle","legs":0}]` {
		t.Fatalf("books %s", got)
	}
	a = compute(t, "exclude=betano", now, rows, "pinnacle")
	if got := jsonOf(t, a.Bookmakers); got != `[{"id":"tipsport","legs":9},{"id":"fortuna","legs":7},{"id":"betano","legs":0},{"id":"pinnacle","legs":0}]` {
		t.Fatalf("books with betano excluded %s", got)
	}
}

func TestStats_RowsNewestFirstInPagesOf50(t *testing.T) {
	now := pragueAt(2026, 11, 13, 12, 0)
	base := pragueAt(2026, 11, 13, 0, 0)
	var rows []accounts.TelegramStatRow
	for i := 0; i < 120; i++ {
		at := base.Add(time.Duration(i/2) * time.Minute) // two rows per sent_at: ties by id
		rows = append(rows, statRow(int64(i+1), at))
	}
	rows[119].EndedAt, rows[119].EndReason = 0, ""
	rows[118].FirstDetectedAt = 0
	a := compute(t, "", now, rows)
	if len(a.Rows) != 50 || a.Rows[0].ID != 120 || a.Rows[1].ID != 119 || a.Rows[49].ID != 71 || a.NextCursor == nil {
		t.Fatalf("page 1: %d rows, first %d, last %d, next %v", len(a.Rows), a.Rows[0].ID, a.Rows[len(a.Rows)-1].ID, a.NextCursor)
	}
	if got := jsonOf(t, a.Rows[0]); got != `{"id":120,"sentAt":`+itoa(rows[119].SentAt)+`,"sport":"tennis","isLive":true,"marketType":"TOTAL_GAMES",`+
		`"line":21.5,"bookmakers":["fortuna","tipsport"],"profitPct":2.5,"profit":250,"currency":"CZK","lifeS":null,"endReason":null}` {
		t.Fatalf("open row %s", got)
	}
	if a.Rows[1].LifeS != nil || a.Rows[2].LifeS == nil || *a.Rows[2].LifeS != 70 || *a.Rows[2].EndReason != "removed" {
		t.Fatalf("rows 2-3: %s", jsonOf(t, a.Rows[1:3]))
	}
	a2 := compute(t, "cursor="+*a.NextCursor, now, rows)
	if len(a2.Rows) != 50 || a2.Rows[0].ID != 70 || a2.Rows[49].ID != 21 || a2.NextCursor == nil || a2.KPIs.Arbs != 120 {
		t.Fatalf("page 2: first %d last %d", a2.Rows[0].ID, a2.Rows[49].ID)
	}
	a3 := compute(t, "cursor="+*a2.NextCursor, now, rows)
	if len(a3.Rows) != 20 || a3.Rows[0].ID != 20 || a3.Rows[19].ID != 1 || a3.NextCursor != nil {
		t.Fatalf("page 3: %d rows, next %v", len(a3.Rows), a3.NextCursor)
	}
	// Exactly 50 left: no further page.
	a4 := compute(t, "", now, rows[:50])
	if len(a4.Rows) != 50 || a4.NextCursor != nil {
		t.Fatalf("exactly 50: next %v", a4.NextCursor)
	}
}
