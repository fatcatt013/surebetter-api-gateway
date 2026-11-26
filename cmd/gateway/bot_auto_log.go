package main

import (
	"cmp"
	"slices"
)

// The skip log (contract §5.3, §4.1 GET …/auto/log): per user, the newest maxAutoLog lines in memory, one per arb life
// and reason (the same again counts up), and today's evaluation counts per reason code (both stages). Lost at a
// gateway restart. Guarded by autoPicker.logMu (taken after botHub.mu when both are held).

// maxAutoLog is the most lines kept per user (and the largest ?limit=).
const maxAutoLog = 2000

// The reason codes, stage 1 then stage 2, in the order they are checked.
var autoReasonCodes = []string{
	"three_legs", "books", "live_state", "sport", "market", "profit_min", "profit_max", "min_age",
	"hidden", "no_selection_id", "bots_paused", "auto_paused", "match_placed", "match_blocked", "order_open", "match_running",
	"match_cooldown", "same_prices", "arb_attempts", "rounded_below_min", "stake_under_min", "stake_over_max", "limit_leg",
	"limit_order", "auto_day_count", "auto_day_stake", "book_day_stake", "limit_day", "auto_at_once", "orders_at_once", "gap",
	"no_agent", "no_free_window", "balance_low", "balance_unknown",
}

// autoLogLeg is one leg of a log line: its stake is 0 when the reason came before the sizing.
type autoLogLeg struct {
	Bookmaker string  `json:"bookmaker"`
	Odds      float64 `json:"odds"`
	Stake     float64 `json:"stake"`
}

// autoLogLine is one skip-log line.
type autoLogLine struct {
	At        int64        `json:"at"`
	LastAt    int64        `json:"lastAt"`
	Count     int          `json:"count"`
	Kind      string       `json:"kind"`   // skip, would_send or sent
	Reason    string       `json:"reason"` // a reason code; "" for would_send and sent
	Text      string       `json:"text"`
	ArbKey    string       `json:"arbKey"`
	EventID   string       `json:"eventId"`
	Match     string       `json:"match"`
	Market    string       `json:"market"`
	Sport     string       `json:"sport"`
	Live      bool         `json:"live"`
	ProfitPct float64      `json:"profitPct"`
	Legs      []autoLogLeg `json:"legs"`
	OrderID   string       `json:"orderId"`

	life string // the arb's identity and first detection (one line per life and reason)
	seq  int64  // when it was last recorded, in the user's order (orders lines of the same millisecond)
}

// key is the line's identity: its arb life, kind and reason (a sent line: its order).
func (l *autoLogLine) key() string { return l.life + "|" + l.Kind + "|" + l.Reason + "|" + l.OrderID }

// autoUserLog is one user's lines and today's counts.
type autoUserLog struct {
	lines     []*autoLogLine
	byKey     map[string]*autoLogLine
	seq       int64 // the last line's seq
	countsDay int64 // the Prague day (start, Unix ms) of counts
	counts    map[string]int
}

func (p *autoPicker) userLogLocked(userID int64) *autoUserLog {
	l := p.logs[userID]
	if l == nil {
		l = &autoUserLog{byKey: map[string]*autoLogLine{}, counts: map[string]int{}}
		p.logs[userID] = l
	}
	return l
}

// countLocked counts one evaluation that failed at code on day (the counts start again on a new day). logMu held.
func (l *autoUserLog) countLocked(day int64, code string) {
	if l.countsDay != day {
		l.countsDay, l.counts = day, map[string]int{}
	}
	l.counts[code]++
}

// count counts one stage-1 failure of userID on day.
func (p *autoPicker) count(userID, day int64, code string) {
	p.logMu.Lock()
	p.userLogLocked(userID).countLocked(day, code)
	p.logMu.Unlock()
}

// record adds line (At and LastAt set to now) to userID's log on day: a skip line is counted under its reason; a
// line already there for the same arb life, kind and reason counts up and takes line's text, profit, legs and time.
// Beyond maxAutoLog lines the one recorded longest ago goes.
func (p *autoPicker) record(userID, day int64, line autoLogLine, now int64) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	l := p.userLogLocked(userID)
	if line.Kind == "skip" {
		l.countLocked(day, line.Reason)
	}
	l.seq++
	if old := l.byKey[line.key()]; old != nil {
		old.Count++
		old.LastAt, old.Text, old.ProfitPct, old.Legs, old.seq = now, line.Text, line.ProfitPct, line.Legs, l.seq
		return
	}
	line.At, line.LastAt, line.Count, line.seq = now, now, 1, l.seq
	n := &line
	l.lines = append(l.lines, n)
	l.byKey[n.key()] = n
	if len(l.lines) > maxAutoLog {
		oldest := 0
		for i, x := range l.lines {
			if x.seq < l.lines[oldest].seq {
				oldest = i
			}
		}
		delete(l.byKey, l.lines[oldest].key())
		l.lines = slices.Delete(l.lines, oldest, oldest+1)
	}
}

// countsOf is a copy of userID's counts of day (empty when none, never nil).
func (p *autoPicker) countsOf(userID, day int64) map[string]int {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	out := map[string]int{}
	if l := p.logs[userID]; l != nil && l.countsDay == day {
		for k, v := range l.counts {
			out[k] = v
		}
	}
	return out
}

// read is userID's lines with reason and kind ("" = any), newest lastAt first, at most limit, and the counts of day.
func (p *autoPicker) read(userID, day int64, reason, kind string, limit int) ([]autoLogLine, map[string]int) {
	counts := p.countsOf(userID, day)
	p.logMu.Lock()
	defer p.logMu.Unlock()
	out := []autoLogLine{}
	if l := p.logs[userID]; l != nil {
		for _, x := range l.lines {
			if (reason == "" || x.Reason == reason) && (kind == "" || x.Kind == kind) {
				c := *x
				c.Legs = slices.Clone(x.Legs)
				out = append(out, c)
			}
		}
	}
	slices.SortFunc(out, func(a, b autoLogLine) int { // newest lastAt first; the same millisecond: the later recorded first
		if a.LastAt != b.LastAt {
			return cmp.Compare(b.LastAt, a.LastAt)
		}
		return cmp.Compare(b.seq, a.seq)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, counts
}
