package main

import (
	"encoding/csv"
	"errors"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/surebetter/api-gateway/internal/accounts"

	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The Statistics page (admin only): GET /api/admin/stats/arbs and /arbs.csv over the Telegram
// bot's sent-arb rows (telegram_sent_arbs / telegram_sent_legs). Contract:
// ../surebetter-common/docs/superpowers/plans/2026-11-13-statistics-page-contract.md.

const (
	statsMaxRangeMs  = 400 * 24 * 3600 * 1000
	statsMaxPoints   = 2000
	statsMaxBooks    = 20
	statsDefaultDays = 14
)

var (
	statsBookRe   = regexp.MustCompile(`^[a-z0-9]{1,40}$`)
	statsSportRe  = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	statsMarketRe = regexp.MustCompile(`^[A-Z0-9_]{1,60}$`)
)

// statsFilter is the page's query: the range [from, to) on sent_at (ms) and the filters.
// nil sets and pointers, "" strings: not set.
type statsFilter struct {
	from, to             int64
	include, exclude     map[string]bool
	only                 bool // mode=only: every leg at an included book (default any: at least one)
	minProfit, maxProfit *float64
	minLife, maxLife     *float64 // seconds; either set leaves out unknown lifetimes
	sport, market        string
	live                 string // "" (all), "live", "prematch"
	bucket               string // hour, day, week
	hasCursor            bool
	cursorAt, cursorID   int64 // rows strictly older than (cursorAt, cursorID)
}

// parseStatsQuery reads the filters; false: a bad value (400 bad_query).
func parseStatsQuery(q url.Values, now time.Time) (statsFilter, bool) {
	f := statsFilter{bucket: "day"}
	var ok bool
	intParam := func(name string) (int64, bool, bool) {
		s := q.Get(name)
		if s == "" {
			return 0, false, true
		}
		v, err := strconv.ParseInt(s, 10, 64)
		return v, true, err == nil && v >= 0
	}
	from, hasFrom, okFrom := intParam("from")
	to, hasTo, okTo := intParam("to")
	if !okFrom || !okTo {
		return f, false
	}
	if !hasTo {
		to = now.UnixMilli()
	}
	if !hasFrom {
		day, _ := pragueDayOf(time.UnixMilli(to - 1))
		from = time.Date(day.Year(), day.Month(), day.Day()-(statsDefaultDays-1), 0, 0, 0, 0, prague).UnixMilli()
	}
	if from >= to || to-from > statsMaxRangeMs {
		return f, false
	}
	f.from, f.to = from, to

	if f.include, ok = statsBooks(q.Get("include")); !ok {
		return f, false
	}
	if f.exclude, ok = statsBooks(q.Get("exclude")); !ok {
		return f, false
	}
	switch q.Get("mode") {
	case "", "any":
	case "only":
		f.only = true
	default:
		return f, false
	}
	if f.minProfit, ok = statsFloat(q.Get("minProfit"), false); !ok {
		return f, false
	}
	if f.maxProfit, ok = statsFloat(q.Get("maxProfit"), false); !ok {
		return f, false
	}
	if f.minLife, ok = statsFloat(q.Get("minLife"), true); !ok {
		return f, false
	}
	if f.maxLife, ok = statsFloat(q.Get("maxLife"), true); !ok {
		return f, false
	}
	if f.minProfit != nil && f.maxProfit != nil && *f.minProfit > *f.maxProfit ||
		f.minLife != nil && f.maxLife != nil && *f.minLife > *f.maxLife {
		return f, false
	}
	if f.sport = q.Get("sport"); f.sport != "" && !statsSportRe.MatchString(f.sport) {
		return f, false
	}
	if f.market = q.Get("market"); f.market != "" && !statsMarketRe.MatchString(f.market) {
		return f, false
	}
	switch l := q.Get("live"); l {
	case "", "all":
	case "live", "prematch":
		f.live = l
	default:
		return f, false
	}
	switch b := q.Get("bucket"); b {
	case "":
	case "hour", "day", "week":
		f.bucket = b
	default:
		return f, false
	}
	if len(statsBuckets(f.from, f.to, f.bucket, statsMaxPoints+1)) > statsMaxPoints {
		return f, false
	}
	if c := q.Get("cursor"); c != "" {
		at, id, found := strings.Cut(c, ".")
		a, errA := strconv.ParseInt(at, 10, 64)
		i, errI := strconv.ParseInt(id, 10, 64)
		if !found || errA != nil || errI != nil || a < 0 || i < 0 {
			return f, false
		}
		f.hasCursor, f.cursorAt, f.cursorID = true, a, i
	}
	return f, true
}

// statsBooks reads a comma-separated bookmaker list; "" = nil (not set).
func statsBooks(s string) (map[string]bool, bool) {
	if s == "" {
		return nil, true
	}
	parts := strings.Split(s, ",")
	if len(parts) > statsMaxBooks {
		return nil, false
	}
	m := make(map[string]bool, len(parts))
	for _, p := range parts {
		if !statsBookRe.MatchString(p) {
			return nil, false
		}
		m[p] = true
	}
	return m, true
}

// statsFloat reads a finite number; "" = nil. nonNeg rejects negatives (lifetimes).
func statsFloat(s string, nonNeg bool) (*float64, bool) {
	if s == "" {
		return nil, true
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || nonNeg && v < 0 {
		return nil, false
	}
	return &v, true
}

// statsBuckets returns the starts (ms) of the Prague hour, day or week (from Monday) buckets that
// cover [from, to), at most limit of them. A Prague hour is a UTC hour (the offsets are whole
// hours), so the fall-back day has 25 hour buckets and the spring-forward day 23.
func statsBuckets(from, to int64, bucket string, limit int) []int64 {
	t := time.UnixMilli(from).In(prague)
	switch bucket {
	case "hour":
		t = t.Truncate(time.Hour)
	case "day":
		t, _ = pragueDayOf(t)
	case "week":
		t, _ = pragueDayOf(t)
		t = t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
	}
	var out []int64
	for t.UnixMilli() < to && len(out) < limit {
		out = append(out, t.UnixMilli())
		switch bucket {
		case "hour":
			t = t.Add(time.Hour)
		case "day":
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, prague)
		case "week":
			t = time.Date(t.Year(), t.Month(), t.Day()+7, 0, 0, 0, 0, prague)
		}
	}
	return out
}

// statsLifeS is a row's lifetime in seconds, (ended_at − first_detected_at) / 1000; false: unknown.
func statsLifeS(r accounts.TelegramStatRow) (float64, bool) {
	if r.EndedAt == 0 || r.FirstDetectedAt == 0 {
		return 0, false
	}
	return float64(r.EndedAt-r.FirstDetectedAt) / 1000, true
}

// keep reports whether a row in the range passes the filters (the cursor is not a filter).
func (f statsFilter) keep(r accounts.TelegramStatRow) bool {
	if f.sport != "" && r.Sport != f.sport && !(f.sport == "esports" && strings.HasPrefix(r.Sport, "esports-")) {
		return false
	}
	if f.market != "" && r.MarketType != f.market {
		return false
	}
	if f.live == "live" && !r.IsLive || f.live == "prematch" && r.IsLive {
		return false
	}
	if f.minProfit != nil && r.ProfitPct < *f.minProfit || f.maxProfit != nil && r.ProfitPct > *f.maxProfit {
		return false
	}
	if f.minLife != nil || f.maxLife != nil {
		life, ok := statsLifeS(r)
		if !ok || f.minLife != nil && life < *f.minLife || f.maxLife != nil && life > *f.maxLife {
			return false
		}
	}
	anyIncluded, allIncluded := false, true
	for _, b := range r.Books {
		if f.exclude[b] {
			return false
		}
		if f.include[b] {
			anyIncluded = true
		} else {
			allIncluded = false
		}
	}
	if f.include != nil {
		if f.only {
			return allIncluded
		}
		return anyIncluded
	}
	return true
}

// ── the answer ────────────────────────────────────────────────────────────────

const statsPageRows = 50

type statsAnswer struct {
	Range      statsRange       `json:"range"`
	KPIs       statsKPIs        `json:"kpis"`
	Series     statsSeries      `json:"series"`
	Markets    []statsMarket    `json:"markets"`
	Heatmap    [][]int          `json:"heatmap"` // 7 rows Mon..Sun (Prague) × 24 hours
	ProfitHist []statsProfitBin `json:"profitHist"`
	LifeHist   []statsLifeBin   `json:"lifeHist"`
	EndReasons statsEndReasons  `json:"endReasons"`
	Pairs      []statsPair      `json:"pairs"`
	Bookmakers []statsBook      `json:"bookmakers"`
	Rows       []statsRow       `json:"rows"`
	NextCursor *string          `json:"nextCursor"`
}

type statsRange struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

type statsKPIs struct {
	Arbs             int      `json:"arbs"`
	PerDay           float64  `json:"perDay"`
	AvgProfitPct     *float64 `json:"avgProfitPct"`
	MedianProfitPct  *float64 `json:"medianProfitPct"`
	Profit           int64    `json:"profit"`
	Currency         string   `json:"currency"`
	MedianLifeS      *float64 `json:"medianLifeS"`
	MedianAfterSendS *float64 `json:"medianAfterSendS"`
}

type statsSeries struct {
	Bucket string       `json:"bucket"`
	Points []statsPoint `json:"points"`
}

type statsPoint struct {
	T            int64    `json:"t"`
	Live         int      `json:"live"`
	Prematch     int      `json:"prematch"`
	AvgProfitPct *float64 `json:"avgProfitPct"`
	Profit       int64    `json:"profit"`
	pctSum       float64
}

type statsMarket struct {
	MarketType   string  `json:"marketType"`
	Sport        string  `json:"sport"`
	Arbs         int     `json:"arbs"`
	AvgProfitPct float64 `json:"avgProfitPct"`
	pctSum       float64
}

type statsProfitBin struct {
	Label string   `json:"label"`
	From  float64  `json:"from"`
	To    *float64 `json:"to"`
	Arbs  int      `json:"arbs"`
}

type statsLifeBin struct {
	Label string   `json:"label"`
	FromS float64  `json:"fromS"`
	ToS   *float64 `json:"toS"`
	Arbs  int      `json:"arbs"`
}

type statsEndReasons struct {
	Removed    int `json:"removed"`
	Evicted    int `json:"evicted"`
	Redetected int `json:"redetected"`
	Absent     int `json:"absent"`
	Unknown    int `json:"unknown"`
	Open       int `json:"open"` // still live as far as the bot knows (end_reason NULL)
}

type statsPair struct {
	A    string `json:"a"`
	B    string `json:"b"`
	Arbs int    `json:"arbs"`
}

type statsBook struct {
	ID   string `json:"id"`
	Legs int    `json:"legs"`
}

type statsRow struct {
	ID         int64    `json:"id"`
	SentAt     int64    `json:"sentAt"`
	Sport      string   `json:"sport"`
	IsLive     bool     `json:"isLive"`
	MarketType string   `json:"marketType"`
	Line       float64  `json:"line"`
	Bookmakers []string `json:"bookmakers"`
	ProfitPct  float64  `json:"profitPct"`
	Profit     int64    `json:"profit"`
	Currency   string   `json:"currency"`
	LifeS      *float64 `json:"lifeS"`
	EndReason  *string  `json:"endReason"`
}

// The histograms' bins: [from, to), the last one open. Values below the first bin count in it.
var (
	statsProfitEdges  = []float64{0, 1, 2, 3, 4, 5, 6, 8, 10}
	statsProfitLabels = []string{"0–1", "1–2", "2–3", "3–4", "4–5", "5–6", "6–8", "8–10", "10+"}
	statsLifeEdges    = []float64{0, 15, 30, 60, 120, 300, 600}
	statsLifeLabels   = []string{"<15 s", "15–30 s", "30–60 s", "1–2 min", "2–5 min", "5–10 min", ">10 min"}
)

// statsBin is the index of the bin [edges[i], edges[i+1]) holding v (the first for v < edges[0]).
func statsBin(edges []float64, v float64) int {
	i := sort.Search(len(edges), func(i int) bool { return edges[i] > v }) - 1
	return max(i, 0)
}

func round(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}

func roundPtr(v float64, decimals int) *float64 {
	r := round(v, decimals)
	return &r
}

// median of vs (sorted in place); nil when empty.
func median(vs []float64, decimals int) *float64 {
	if len(vs) == 0 {
		return nil
	}
	sort.Float64s(vs)
	n := len(vs)
	if n%2 == 1 {
		return roundPtr(vs[n/2], decimals)
	}
	return roundPtr((vs[n/2-1]+vs[n/2])/2, decimals)
}

// statsCurrency is the currency most rows have (a tie: CZK, else alphabetical); money is summed in
// it only, since a bankroll's currency can change and CZK and EUR do not add up.
func statsCurrency(rows []accounts.TelegramStatRow) string {
	n := map[string]int{}
	for _, r := range rows {
		n[r.Currency]++
	}
	best := "CZK"
	for c, k := range n {
		if k > n[best] || k == n[best] && best != "CZK" && c < best {
			best = c
		}
	}
	return best
}

func statsCursor(r accounts.TelegramStatRow) string {
	return strconv.FormatInt(r.SentAt, 10) + "." + strconv.FormatInt(r.ID, 10)
}

// computeStats builds the page from the rows that passed the filters (oldest first, as
// ScanTelegramStats reads them) and the bookmakers seen in the range before filtering.
func computeStats(f statsFilter, rows []accounts.TelegramStatRow, seen map[string]bool) statsAnswer {
	a := statsAnswer{
		Range: statsRange{From: f.from, To: f.to}, Markets: []statsMarket{}, Pairs: []statsPair{},
		Bookmakers: []statsBook{}, Rows: []statsRow{}, Heatmap: make([][]int, 7),
	}
	for i := range a.Heatmap {
		a.Heatmap[i] = make([]int, 24)
	}
	cur := statsCurrency(rows)
	a.KPIs.Currency = cur

	starts := statsBuckets(f.from, f.to, f.bucket, statsMaxPoints)
	points := make([]statsPoint, len(starts))
	for i, t := range starts {
		points[i].T = t
	}
	for i, e := range statsProfitEdges {
		b := statsProfitBin{Label: statsProfitLabels[i], From: e}
		if i+1 < len(statsProfitEdges) {
			b.To = &statsProfitEdges[i+1]
		}
		a.ProfitHist = append(a.ProfitHist, b)
	}
	for i, e := range statsLifeEdges {
		b := statsLifeBin{Label: statsLifeLabels[i], FromS: e}
		if i+1 < len(statsLifeEdges) {
			b.ToS = &statsLifeEdges[i+1]
		}
		a.LifeHist = append(a.LifeHist, b)
	}

	markets := map[[2]string]*statsMarket{}
	pairs := map[[2]string]int{}
	legs := map[string]int{}
	var pcts, lives, afterSend []float64
	var pctSum float64
	for _, r := range rows {
		pctSum += r.ProfitPct
		pcts = append(pcts, r.ProfitPct)
		if r.Currency == cur {
			a.KPIs.Profit += r.Profit
		}
		life, lifeKnown := statsLifeS(r)
		if lifeKnown {
			lives = append(lives, life)
			a.LifeHist[statsBin(statsLifeEdges, life)].Arbs++
		}
		if r.EndedAt != 0 {
			afterSend = append(afterSend, float64(r.EndedAt-r.SentAt)/1000)
		}
		a.ProfitHist[statsBin(statsProfitEdges, r.ProfitPct)].Arbs++

		if i := sort.Search(len(starts), func(i int) bool { return starts[i] > r.SentAt }) - 1; i >= 0 {
			p := &points[i]
			if r.IsLive {
				p.Live++
			} else {
				p.Prematch++
			}
			p.pctSum += r.ProfitPct
			if r.Currency == cur {
				p.Profit += r.Profit
			}
		}
		local := time.UnixMilli(r.SentAt).In(prague)
		a.Heatmap[(int(local.Weekday())+6)%7][local.Hour()]++

		key := [2]string{r.MarketType, r.Sport}
		m := markets[key]
		if m == nil {
			m = &statsMarket{MarketType: r.MarketType, Sport: r.Sport}
			markets[key] = m
		}
		m.Arbs++
		m.pctSum += r.ProfitPct

		switch r.EndReason {
		case "removed":
			a.EndReasons.Removed++
		case "evicted":
			a.EndReasons.Evicted++
		case "redetected":
			a.EndReasons.Redetected++
		case "absent":
			a.EndReasons.Absent++
		case "":
			a.EndReasons.Open++
		default:
			a.EndReasons.Unknown++
		}

		inArb := map[[2]string]bool{}
		for i, x := range r.Books {
			legs[x]++
			for _, y := range r.Books[i+1:] {
				if x == y {
					continue
				}
				p := [2]string{min(x, y), max(x, y)}
				if !inArb[p] {
					inArb[p] = true
					pairs[p]++
				}
			}
		}
	}

	n := len(rows)
	a.KPIs.Arbs = n
	if days := float64(f.to-f.from) / float64(24*time.Hour/time.Millisecond); days > 0 {
		a.KPIs.PerDay = round(float64(n)/days, 1)
	}
	if n > 0 {
		a.KPIs.AvgProfitPct = roundPtr(pctSum/float64(n), 2)
	}
	a.KPIs.MedianProfitPct = median(pcts, 2)
	a.KPIs.MedianLifeS = median(lives, 1)
	a.KPIs.MedianAfterSendS = median(afterSend, 1)

	for i := range points {
		if k := points[i].Live + points[i].Prematch; k > 0 {
			points[i].AvgProfitPct = roundPtr(points[i].pctSum/float64(k), 2)
		}
	}
	a.Series = statsSeries{Bucket: f.bucket, Points: points}

	for _, m := range markets {
		m.AvgProfitPct = round(m.pctSum/float64(m.Arbs), 2)
		a.Markets = append(a.Markets, *m)
	}
	sort.Slice(a.Markets, func(i, j int) bool {
		x, y := a.Markets[i], a.Markets[j]
		if x.Arbs != y.Arbs {
			return x.Arbs > y.Arbs
		}
		if x.MarketType != y.MarketType {
			return x.MarketType < y.MarketType
		}
		return x.Sport < y.Sport
	})

	for p, k := range pairs {
		a.Pairs = append(a.Pairs, statsPair{A: p[0], B: p[1], Arbs: k})
	}
	sort.Slice(a.Pairs, func(i, j int) bool {
		x, y := a.Pairs[i], a.Pairs[j]
		if x.Arbs != y.Arbs {
			return x.Arbs > y.Arbs
		}
		if x.A != y.A {
			return x.A < y.A
		}
		return x.B < y.B
	})

	for b := range seen {
		if _, ok := legs[b]; !ok {
			legs[b] = 0
		}
	}
	for b, k := range legs {
		a.Bookmakers = append(a.Bookmakers, statsBook{ID: b, Legs: k})
	}
	sort.Slice(a.Bookmakers, func(i, j int) bool {
		x, y := a.Bookmakers[i], a.Bookmakers[j]
		if x.Legs != y.Legs {
			return x.Legs > y.Legs
		}
		return x.ID < y.ID
	})

	// Rows: newest first, the page after the cursor.
	end := n // rows[:end] are older than the cursor
	if f.hasCursor {
		end = sort.Search(n, func(i int) bool {
			r := rows[i]
			return r.SentAt > f.cursorAt || r.SentAt == f.cursorAt && r.ID >= f.cursorID
		})
	}
	for i := end - 1; i >= 0 && len(a.Rows) < statsPageRows; i-- {
		a.Rows = append(a.Rows, statsRowOf(rows[i]))
	}
	if start := end - len(a.Rows); start > 0 {
		c := statsCursor(rows[start])
		a.NextCursor = &c
	}
	return a
}

func statsRowOf(r accounts.TelegramStatRow) statsRow {
	out := statsRow{ID: r.ID, SentAt: r.SentAt, Sport: r.Sport, IsLive: r.IsLive, MarketType: r.MarketType, Line: r.Line,
		Bookmakers: r.Books, ProfitPct: r.ProfitPct, Profit: r.Profit, Currency: r.Currency}
	if out.Bookmakers == nil {
		out.Bookmakers = []string{}
	}
	if life, ok := statsLifeS(r); ok {
		out.LifeS = roundPtr(life, 1)
	}
	if r.EndReason != "" {
		reason := r.EndReason
		out.EndReason = &reason
	}
	return out
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

// statsMaxRows caps the filtered rows one request aggregates in memory (~150 B each with the
// shared strings: 300k rows ≈ 50 MB); more → 413 too_many_rows (narrow the range or filters).
var statsMaxRows = 300_000

var errStatsTooManyRows = errors.New("too many rows")

// newStatsAPI serves GET /api/admin/stats/arbs and /arbs.csv (admin session only; CSRF like the
// accounts API, for the same cookie).
func newStatsAPI(acc *accounts.Service) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/admin/stats/arbs", acc.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleStats(acc, w, r, false)
	})))
	mux.Handle("GET /api/admin/stats/arbs.csv", acc.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleStats(acc, w, r, true)
	})))
	return accounts.CSRF(mux)
}

// loadStats reads the range once and returns the rows that pass the filters (oldest first) and
// every bookmaker seen in the range.
func loadStats(acc *accounts.Service, f statsFilter) ([]accounts.TelegramStatRow, map[string]bool, error) {
	var rows []accounts.TelegramStatRow
	seen := map[string]bool{}
	err := acc.ScanTelegramStats(f.from, f.to, func(r accounts.TelegramStatRow) error {
		for _, b := range r.Books {
			seen[b] = true
		}
		if !f.keep(r) {
			return nil
		}
		if len(rows) >= statsMaxRows {
			return errStatsTooManyRows
		}
		rows = append(rows, r)
		return nil
	})
	return rows, seen, err
}

func handleStats(acc *accounts.Service, w http.ResponseWriter, r *http.Request, csv bool) {
	f, ok := parseStatsQuery(r.URL.Query(), time.Now())
	if !ok {
		accounts.WriteError(w, http.StatusBadRequest, "bad_query")
		return
	}
	rows, seen, err := loadStats(acc, f)
	if errors.Is(err, errStatsTooManyRows) {
		accounts.WriteError(w, http.StatusRequestEntityTooLarge, "too_many_rows")
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("stats: read sent arbs")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if csv {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="surebetter-arbs.csv"`)
		w.Header().Set("Cache-Control", "no-store")
		writeStatsCSV(w, rows)
		return
	}
	accounts.WriteJSON(w, http.StatusOK, computeStats(f, rows, seen))
}

// writeStatsCSV writes one line per arb, newest first: the sent time in Prague (RFC 3339), sport,
// live/prematch, market, line, bookmakers joined by |, profit %, profit, currency, lifetime and
// after-send seconds (empty when unknown) and the end reason ("open" while it lives).
func writeStatsCSV(w io.Writer, rows []accounts.TelegramStatRow) {
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	cw.Write([]string{"sent_at", "sport", "live", "market", "line", "bookmakers", "profit_pct", "profit", "currency",
		"lifetime_s", "after_send_s", "end_reason"})
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		live, life, after, reason := "prematch", "", "", r.EndReason
		if r.IsLive {
			live = "live"
		}
		if l, ok := statsLifeS(r); ok {
			life = num(round(l, 1))
		}
		if r.EndedAt != 0 {
			after = num(round(float64(r.EndedAt-r.SentAt)/1000, 1))
		}
		if reason == "" {
			reason = "open"
		}
		cw.Write([]string{time.UnixMilli(r.SentAt).In(prague).Format(time.RFC3339), r.Sport, live, r.MarketType, num(r.Line),
			strings.Join(r.Books, "|"), num(r.ProfitPct), strconv.FormatInt(r.Profit, 10), r.Currency, life, after, reason})
	}
	cw.Flush()
}
