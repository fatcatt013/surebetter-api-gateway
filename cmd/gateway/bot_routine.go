package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
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

// humanProto is the agent protocol that runs a routine: only such an agent gets {"type":"routine"}.
const humanProto = 4

// routineMsg tells an agent to re-read GET /api/me/bot/routine.
var routineMsg = []byte(`{"type":"routine"}`)

// checkedWindow is how far back GET /api/me/bot/routine lists checked orders (by their end).
const checkedWindow = 30 * 24 * time.Hour

// sendRoutineLocked sends {"type":"routine"} to agent codeID's bot socket when it is online with
// protocol 4 or later. b.mu must be held.
func (b *botHub) sendRoutineLocked(userID, codeID int64) {
	if p := b.presenceLocked(userID, codeID); p != nil && p.proto >= humanProto {
		p.client.trySend(routineMsg)
	}
}

// routineView is the GET and PUT /api/me/bot/agents/{agentId}/routine body; routine null = none.
type routineView struct {
	HumanMode bool            `json:"humanMode"`
	Routine   json.RawMessage `json:"routine"`
}

// routineJSONOf is a stored routine as the API answers it: "" → null.
func routineJSONOf(stored string) json.RawMessage {
	if stored == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(stored)
}

// handleGetRoutine is GET /api/me/bot/agents/{agentId}/routine: {"humanMode", "routine"}. 404
// unknown_agent: not one of the user's agents (a bot code that never connected has none).
func (b *botHub) handleGetRoutine(w http.ResponseWriter, r *http.Request) {
	codeID, ok := b.limitAgent(w, r)
	if !ok {
		return
	}
	js, on, found, err := b.acc.BotAgentRoutine(principalOf(r).UserID, codeID)
	if err != nil {
		log.Error().Err(err).Msg("bot routine read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !found {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	accounts.WriteJSON(w, http.StatusOK, routineView{HumanMode: on, Routine: routineJSONOf(js)})
}

// handlePutRoutine is PUT /api/me/bot/agents/{agentId}/routine {"humanMode", "routine"}: it replaces
// both (routine null = none), answers them as stored, tells the agent ({"type":"routine"}) when it is
// online with protocol 4 and the app ({"type":"bot"}: another open Fleet shows the new switch). 404
// unknown_agent (checked first); 400 bad_routine: a body over 256 KiB, not exactly one JSON object
// of that shape, or a routine routineOf refuses.
func (b *botHub) handlePutRoutine(w http.ResponseWriter, r *http.Request) {
	codeID, ok := b.limitAgent(w, r)
	if !ok {
		return
	}
	userID := principalOf(r).UserID
	if _, _, found, err := b.acc.BotAgentRoutine(userID, codeID); err != nil {
		log.Error().Err(err).Msg("bot routine read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	} else if !found { // a bot code that never connected has no agent row
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	var in struct {
		HumanMode *bool           `json:"humanMode"`
		Routine   json.RawMessage `json:"routine"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRoutineBody))
	if err != nil || json.Unmarshal(body, &in) != nil || in.HumanMode == nil || in.Routine == nil {
		accounts.WriteError(w, http.StatusBadRequest, "bad_routine")
		return
	}
	stored := ""
	if !bytes.Equal(in.Routine, []byte("null")) {
		rt, valid := routineOf(in.Routine)
		if !valid {
			accounts.WriteError(w, http.StatusBadRequest, "bad_routine")
			return
		}
		stored = mustJSON(rt)
	}
	found, err := b.acc.SaveBotAgentRoutine(userID, codeID, stored, *in.HumanMode)
	if err != nil {
		log.Error().Err(err).Msg("bot routine save failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !found {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	b.mu.Lock()
	b.sendRoutineLocked(userID, codeID)
	b.mu.Unlock()
	b.changed(userID)
	accounts.WriteJSON(w, http.StatusOK, routineView{HumanMode: *in.HumanMode, Routine: routineJSONOf(stored)})
}

// ownRoutineView is the GET /api/me/bot/routine body.
type ownRoutineView struct {
	routineView
	Checked []string `json:"checked"`
}

// handleOwnRoutine is GET /api/me/bot/routine for the calling agent: its human-mode switch and
// routine (off and null when it has no agent row yet) and the ids of its orders the user marked
// checked that ended within the last 30 days.
func (b *botHub) handleOwnRoutine(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	js, on, _, err := b.acc.BotAgentRoutine(p.UserID, p.CodeID)
	var checked []string
	if err == nil {
		checked, err = b.acc.CheckedBotOrderIDs(p.UserID, p.CodeID, b.now().Add(-checkedWindow).UnixMilli())
	}
	if err != nil {
		log.Error().Err(err).Int64("code", p.CodeID).Msg("bot own routine read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	accounts.WriteJSON(w, http.StatusOK, ownRoutineView{routineView{HumanMode: on, Routine: routineJSONOf(js)}, checked})
}
