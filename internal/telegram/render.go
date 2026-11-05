package telegram

import (
	"fmt"
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the scratch image copies zoneinfo too; this makes LoadLocation independent of it
)

// Renderer turns an arb into the Telegram HTML message (parse_mode HTML).
type Renderer struct{ loc *time.Location }

// NewRenderer loads the timezone used for every time in a message (TELEGRAM_TZ).
func NewRenderer(tz string) (*Renderer, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("TELEGRAM_TZ %q: %w", tz, err)
	}
	return &Renderer{loc: loc}, nil
}

// View is everything one render needs.
type View struct {
	Arb        Arb
	Bankroll   Bankroll
	PresetName string
	Now        time.Time // decides "started"/"starts" and whether a date is shown
	Gone       bool
	GoneAt     time.Time
	PeakProfit float64 // highest profit_pct seen while live (GONE line only)
	Test       bool
}

// Render builds the message text.
func (r *Renderer) Render(v View) string {
	a := v.Arb
	plan := ComputeStakes(a, v.Bankroll)
	var b strings.Builder

	var head string
	if v.Gone {
		head = "❌ <b>GONE</b>"
		if a.firstSeen() > 0 {
			head += " after " + formatDuration(v.GoneAt.Sub(time.UnixMilli(a.firstSeen())))
		}
		head += " · <s>" + pct(plan.ROI) + "</s>"
		if v.PeakProfit > a.ProfitPct+0.005 {
			head += " (peak " + pct(v.PeakProfit) + ")"
		}
	} else {
		dot := "🟢"
		if plan.Profit < 0 {
			dot = "🟠"
		}
		emoji, label := sportMeta(a.Sport)
		state := "🕒 PREMATCH"
		if IsLive(a.Sport) {
			state = "🔴 LIVE"
		}
		profit := "<b>" + pct(plan.ROI) + "</b>"
		if plan.Total > 0 { // money first, % after it — as the app's profit column
			profit = "<b>" + signed(plan.Profit, v.Bankroll.Currency) + "</b> · <i>" + pct(plan.ROI) + "</i>"
		}
		head = fmt.Sprintf("%s %s · %s %s · %s", dot, profit, emoji, esc(label), state)
	}
	if v.Test {
		head = "🧪 TEST · " + head
	}
	b.WriteString(head + "\n")

	b.WriteString("<b>" + esc(a.NameHome) + " — " + esc(a.NameAway) + "</b>\n")

	var meta []string
	if a.League != "" {
		meta = append(meta, "<i>"+esc(a.League)+"</i>")
	}
	if a.StartTime > 0 {
		st := time.UnixMilli(a.StartTime)
		verb := "starts"
		if IsLive(a.Sport) || !st.After(v.Now) {
			verb = "started"
		}
		meta = append(meta, verb+" "+r.clock(st, v.Now, false))
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · ") + "\n")
	}
	b.WriteString(rule + "\n")

	mark := func(x string) string {
		if v.Gone {
			return "<s>" + x + "</s>"
		}
		return "<b>" + x + "</b>"
	}
	// One card per leg, as the app's leg card: book + its own market name + odds / pick / stake.
	for i, s := range a.Stakes {
		label := s.Label
		if label == "" {
			label = s.OutcomeID
		}
		market := stripLineSuffix(s.MarketName, a.Line)
		if market == "" {
			market = marketLabel(a)
		}
		amount := fmt.Sprintf("%.1f%%", s.StakePct)
		if plan.Total > 0 {
			amount = money(plan.Stakes[i], v.Bankroll.Currency)
		}
		fmt.Fprintf(&b, "<blockquote>%s <b>%s</b> · <i>%s</i> · @ %s\n", bookmakerDot(s.Bookmaker), esc(bookmakerName(s.Bookmaker)),
			esc(market), mark(fmt.Sprintf("%.2f", s.DecimalOdds)))
		fmt.Fprintf(&b, "<b><u>%s</u></b>\n", esc(label))
		fmt.Fprintf(&b, "Stake %s", mark(amount))
		if s.MatchURL != "" {
			fmt.Fprintf(&b, ` · <a href="%s">open ↗</a>`, esc(s.MatchURL))
		}
		b.WriteString("</blockquote>\n")
	}
	b.WriteString(rule + "\n")

	if plan.Total > 0 {
		cur := v.Bankroll.Currency
		fmt.Fprintf(&b, "💰 %s → %s (<b>%s</b>)\n", money(plan.Total, cur), money(plan.Total+plan.Profit, cur), signed(plan.Profit, cur))
	}

	var foot []string
	if a.firstSeen() > 0 {
		f := "⏱ found " + r.clock(time.UnixMilli(a.firstSeen()), v.Now, true)
		if a.FirstDetectedAt > 0 && a.LastChangedAt > a.FirstDetectedAt {
			f += " · changed " + r.clock(time.UnixMilli(a.LastChangedAt), v.Now, true)
		}
		foot = append(foot, f)
	}
	if v.PresetName != "" {
		foot = append(foot, "preset: <i>"+esc(v.PresetName)+"</i>")
	}
	b.WriteString(strings.Join(foot, " · "))
	return strings.TrimRight(b.String(), "\n")
}

// clock formats t in the renderer's zone as HH:MM (or HH:MM:SS), prefixed "2 Jan " when t is not on now's day.
func (r *Renderer) clock(t, now time.Time, secs bool) string {
	lt, ln := t.In(r.loc), now.In(r.loc)
	layout := "15:04"
	if secs {
		layout = "15:04:05"
	}
	if lt.YearDay() != ln.YearDay() || lt.Year() != ln.Year() {
		layout = "2 Jan " + layout
	}
	return lt.Format(layout)
}

func marketLabel(a Arb) string {
	name := a.MarketName
	if name == "" {
		name = a.MarketType
	}
	if a.Line == 0 {
		return name
	}
	abs := strconv.FormatFloat(math.Abs(a.Line), 'f', -1, 64)
	if strings.Contains(name, abs) {
		return name
	}
	t := a.MarketType
	switch {
	case strings.Contains(t, "HANDICAP") || t == "PUCK_LINE":
		return name + " ±" + abs
	case strings.HasPrefix(t, "TOTAL_") || strings.HasPrefix(t, "TEAM_TOTAL_") || strings.Contains(t, "_TOTAL_"):
		return name + " " + strconv.FormatFloat(a.Line, 'f', -1, 64)
	}
	return name
}

// rule frames the leg cards. Telegram sizes a bubble to its longest line, so it also keeps the
// message wide; short enough not to wrap in a phone-width bubble.
const rule = "━━━━━━━━━━━━━━━━━━━━━━"

// trailingLine mirrors the frontend's StripLineSuffixPipe: a trailing (signed) number, then only
// whitespace / zero-width characters.
var trailingLine = regexp.MustCompile(`[\s\p{Zs}]*[+\-−]?[\s\p{Zs}]*(\d+(?:[.,]\d+)?)[\s\p{Zs}\x{200B}\x{2060}\x{FEFF}]*$`)

// stripLineSuffix drops a bookmaker market name's trailing line number when it equals the arb's
// line (sign ignored) — the pick label already shows it. Any other name is returned unchanged.
func stripLineSuffix(name string, line float64) string {
	if name == "" || line == 0 {
		return strings.TrimSpace(name)
	}
	m := trailingLine.FindStringSubmatchIndex(name)
	if m == nil {
		return strings.TrimSpace(name)
	}
	n, err := strconv.ParseFloat(strings.Replace(name[m[2]:m[3]], ",", ".", 1), 64)
	if err == nil && math.Abs(n-math.Abs(line)) < 1e-9 {
		return strings.TrimSpace(name[:m[0]])
	}
	return strings.TrimSpace(name)
}

func signed(n float64, currency string) string {
	if n >= 0 {
		return "+" + money(n, currency)
	}
	return money(n, currency)
}

func esc(s string) string { return html.EscapeString(s) }

func pct(x float64) string { return fmt.Sprintf("%+.2f%%", x) }

// groupNumber mirrors the frontend: whole units, space thousands separator.
func groupNumber(n float64) string {
	r := int64(jsRound(n))
	neg := r < 0
	if neg {
		r = -r
	}
	s := strconv.FormatInt(r, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func money(n float64, currency string) string { return groupNumber(n) + " " + currency }

func formatDuration(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s < 0 {
		s = 0
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh %02dm", s/3600, (s%3600)/60)
	}
}

// Mirrors surebetter-frontend/src/app/config/bookmakers.ts BOOKMAKER_BRANDS names.
var bookmakerNames = map[string]string{
	"fortuna": "Fortuna", "betano": "Betano", "tipsport": "Tipsport", "sazkabet": "SazkaBet",
	"merkurxtip": "MerkurXtip", "synottip": "SynotTip", "kingsbet": "Kingsbet", "pinnacle": "Pinnacle",
}

// Closest coloured emoji to each brand colour in config/bookmakers.ts, so books are told apart
// by colour as with the app's chips.
var bookmakerDots = map[string]string{
	"fortuna": "🟨", "betano": "🟧", "tipsport": "🟦", "sazkabet": "🔵",
	"merkurxtip": "🟥", "synottip": "🟩", "kingsbet": "🟪", "pinnacle": "⬛",
}

func bookmakerDot(id string) string {
	if d, ok := bookmakerDots[id]; ok {
		return d
	}
	return "⬜"
}

func bookmakerName(id string) string {
	if n, ok := bookmakerNames[id]; ok {
		return n
	}
	return titleCase(id)
}

// Mirrors surebetter-frontend/src/app/config/sport-meta.ts labels.
var sports = map[string][2]string{
	"football": {"⚽", "Football"}, "ice-hockey": {"🏒", "Hockey"}, "tennis": {"🎾", "Tennis"},
	"volleyball": {"🏐", "Volleyball"}, "basketball": {"🏀", "Basketball"}, "table-tennis": {"🏓", "Table tennis"},
}

func sportMeta(sport string) (emoji, label string) {
	base := BaseSport(sport)
	if m, ok := sports[base]; ok {
		return m[0], m[1]
	}
	return "🏅", titleCase(strings.NewReplacer("-", " ", "_", " ").Replace(base))
}

func titleCase(s string) string {
	if s == "" {
		return "Unknown"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
