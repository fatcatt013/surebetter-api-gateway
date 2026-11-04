package telegram

import (
	"fmt"
	"html"
	"math"
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
		head = fmt.Sprintf("%s <b>%s</b> · %s %s · %s", dot, pct(plan.ROI), emoji, esc(label), state)
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

	b.WriteString("📋 " + esc(marketLabel(a)) + "\n")

	mark := func(x string) string {
		if v.Gone {
			return "<s>" + x + "</s>"
		}
		return "<b>" + x + "</b>"
	}
	legs := make([]string, len(a.Stakes))
	for i, s := range a.Stakes {
		label := s.Label
		if label == "" {
			label = s.OutcomeID
		}
		amount := fmt.Sprintf("%.1f%%", s.StakePct)
		if plan.Total > 0 {
			amount = money(plan.Stakes[i], v.Bankroll.Currency)
		}
		line := fmt.Sprintf("<b>%s</b> · %s\n@ %s → %s", esc(bookmakerName(s.Bookmaker)), esc(label),
			mark(fmt.Sprintf("%.2f", s.DecimalOdds)), mark(amount))
		if s.MatchURL != "" {
			line += fmt.Sprintf(` · <a href="%s">open ↗</a>`, esc(s.MatchURL))
		}
		legs[i] = line
	}
	b.WriteString("\n<blockquote>" + strings.Join(legs, "\n\n") + "</blockquote>\n")

	if plan.Total > 0 {
		cur := v.Bankroll.Currency
		sign := "+"
		if plan.Profit < 0 {
			sign = ""
		}
		fmt.Fprintf(&b, "💰 %s → %s (<b>%s%s</b>)\n", money(plan.Total, cur), money(plan.Total+plan.Profit, cur), sign, money(plan.Profit, cur))
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
