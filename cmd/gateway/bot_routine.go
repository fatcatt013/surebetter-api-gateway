package main

import (
	"encoding/json"
	"regexp"
)

// Human mode Phase A (contract 2026-11-14-bot-human-mode-a-contract.md §2, §3): each agent's
// routine and human-mode switch. The gateway validates and stores the routine; the agent resolves it.

// routineBlock is one block of a window's routine: an activity on some weekdays in [from, to).
type routineBlock struct {
	ID       string  `json:"id"`
	Days     []int   `json:"days"`
	From     string  `json:"from"`
	To       string  `json:"to"`
	Activity string  `json:"activity"`
	Sport    *string `json:"sport,omitempty"` // only and always with prematch and live
}

// routineSleep is a sleep period: it starts on each of days at from; to < from ends it the next day.
type routineSleep struct {
	Days []int  `json:"days"`
	From string `json:"from"`
	To   string `json:"to"`
}

type routineWindow struct {
	Blocks []routineBlock `json:"blocks"`
}

// routineTrigger is "after every bet, check my bets for minMin–maxMin minutes".
type routineTrigger struct {
	On     *bool `json:"on"`
	MinMin *int  `json:"minMin"`
	MaxMin *int  `json:"maxMin"`
}

type routineTriggers struct {
	AfterBetCheckMyBets *routineTrigger `json:"afterBetCheckMyBets"`
}

// routine is a routine, version 1. Slices, maps and pointers are nil when the field is missing (or
// null), which routineOf refuses: every field is required.
type routine struct {
	Version  int                      `json:"version"`
	TZ       string                   `json:"tz"`
	Sleep    []routineSleep           `json:"sleep"`
	Windows  map[string]routineWindow `json:"windows"`
	Triggers *routineTriggers         `json:"triggers"`
}

// Limits of a routine.
const (
	maxRoutineSleep   = 20
	maxRoutineWindows = 32
	maxRoutineBlocks  = 200
	maxRoutineBody    = 256 << 10
)

var (
	routineActivities = map[string]bool{"prematch": true, "live": true, "community": true, "mybets": true, "open": true, "inactive": true}
	// sportActivities carry a sport, and only they do.
	sportActivities = map[string]bool{"prematch": true, "live": true}
	clockPattern    = regexp.MustCompile(`^[0-9]{2}:[0-9]{2}$`)
	blockIDPattern  = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	sportPattern    = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
)

// clockMinutes reads an HH:MM wall-clock time (00:00–24:00) as minutes after midnight.
func clockMinutes(s string) (int, bool) {
	if !clockPattern.MatchString(s) {
		return 0, false
	}
	h, m := int(s[0]-'0')*10+int(s[1]-'0'), int(s[3]-'0')*10+int(s[4]-'0')
	if m > 59 || h > 24 || (h == 24 && m != 0) {
		return 0, false
	}
	return h*60 + m, true
}

// validDays: non-empty, unique, each 1 (Monday) – 7 (Sunday).
func validDays(days []int) bool {
	if len(days) == 0 {
		return false
	}
	seen := [8]bool{}
	for _, d := range days {
		if d < 1 || d > 7 || seen[d] {
			return false
		}
		seen[d] = true
	}
	return true
}

func sharesDay(a, b []int) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// validTrigger: on set, minMin and maxMin whole minutes 1–60, minMin ≤ maxMin.
func validTrigger(tr *routineTrigger) bool {
	return tr != nil && tr.On != nil && tr.MinMin != nil && tr.MaxMin != nil &&
		*tr.MinMin >= 1 && *tr.MaxMin <= 60 && *tr.MinMin <= *tr.MaxMin
}

// validBlocks checks one window's blocks: at most 200, ids [a-z0-9]{1,16} unique in the window,
// valid days, 00:00 ≤ from < to ≤ 24:00, a known activity, a sport ([a-z0-9_-]{1,40}, "any"
// included) only and always with prematch/live, and no two blocks overlapping on a shared day.
func validBlocks(bs []routineBlock) bool {
	if bs == nil || len(bs) > maxRoutineBlocks {
		return false
	}
	type span struct{ from, to int }
	spans := make([]span, len(bs))
	ids := map[string]bool{}
	for i, b := range bs {
		from, okFrom := clockMinutes(b.From)
		to, okTo := clockMinutes(b.To)
		if !okFrom || !okTo || from >= to || !blockIDPattern.MatchString(b.ID) || ids[b.ID] || !validDays(b.Days) ||
			!routineActivities[b.Activity] || sportActivities[b.Activity] != (b.Sport != nil) ||
			(b.Sport != nil && !sportPattern.MatchString(*b.Sport)) {
			return false
		}
		ids[b.ID] = true
		spans[i] = span{from, to}
		for j := range i {
			if spans[j].from < to && from < spans[j].to && sharesDay(bs[j].Days, b.Days) {
				return false
			}
		}
	}
	return true
}

// routineOf parses and validates a routine (contract §2); false: bad_routine.
func routineOf(raw []byte) (*routine, bool) {
	var r routine
	if json.Unmarshal(raw, &r) != nil || r.Version != 1 || r.TZ != "Europe/Prague" || r.Sleep == nil || len(r.Sleep) > maxRoutineSleep ||
		r.Windows == nil || len(r.Windows) > maxRoutineWindows || r.Triggers == nil || !validTrigger(r.Triggers.AfterBetCheckMyBets) {
		return nil, false
	}
	for _, s := range r.Sleep {
		from, okFrom := clockMinutes(s.From)
		to, okTo := clockMinutes(s.To)
		if !okFrom || !okTo || from > 23*60+59 || from == to || !validDays(s.Days) {
			return nil, false
		}
	}
	for id, w := range r.Windows {
		if !windowIDPattern.MatchString(id) || !validBlocks(w.Blocks) {
			return nil, false
		}
	}
	return &r, true
}
