package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// logLineIn is one line of a POST /api/me/bot/logs body.
type logLineIn struct {
	TS       int64      `json:"ts"`
	Level    string     `json:"level"`
	WindowID string     `json:"windowId"`
	OrderID  string     `json:"orderId"`
	Step     string     `json:"step"`
	Msg      string     `json:"msg"`
	Detail   *logDetail `json:"detail"`
}

// logDetail is what a line may carry for the Logs side panel. Other keys are dropped.
type logDetail struct {
	Ordered    *float64 `json:"ordered,omitempty"`
	OnSlip     *float64 `json:"onSlip,omitempty"`
	Partner    string   `json:"partner,omitempty"`
	NothingBet *bool    `json:"nothingBet,omitempty"`
	Shot       string   `json:"shot,omitempty"`
	Match      string   `json:"match,omitempty"`
	Market     string   `json:"market,omitempty"`
}

var logLevels = map[string]bool{"info": true, "warn": true, "error": true}

const (
	maxLogBatch    = 200  // lines per POST
	maxDetailBytes = 2000 // stored detail JSON
	logsPerMinute  = 2000 // lines per code per minute
)

// logLinesOf validates a posted batch (1–200 lines; ts > 0; level info|warn|error; msg 1–500 chars;
// step ≤ 40, windowId ≤ 40, orderId ≤ 32 chars; detail JSON ≤ 2000 bytes) and converts it for
// storage. An empty detail is stored as none. false: bad_logs.
func logLinesOf(in []logLineIn) ([]accounts.BotLogLine, bool) {
	if len(in) < 1 || len(in) > maxLogBatch {
		return nil, false
	}
	out := make([]accounts.BotLogLine, 0, len(in))
	for _, l := range in {
		if l.TS <= 0 || !logLevels[l.Level] || l.Msg == "" || runeLen(l.Msg) > 500 || runeLen(l.Step) > 40 ||
			runeLen(l.WindowID) > 40 || runeLen(l.OrderID) > 32 {
			return nil, false
		}
		detail := ""
		if l.Detail != nil && *l.Detail != (logDetail{}) {
			js, _ := json.Marshal(l.Detail) // plain struct: cannot fail
			if len(js) > maxDetailBytes {
				return nil, false
			}
			detail = string(js)
		}
		out = append(out, accounts.BotLogLine{TS: l.TS, Level: l.Level, WindowID: l.WindowID, OrderID: l.OrderID,
			Step: l.Step, Msg: l.Msg, Detail: detail})
	}
	return out, true
}

// logLimiter allows each code logsPerMinute lines per fixed one-minute window.
type logLimiter struct {
	mu      sync.Mutex
	windows map[int64]logWindow // code id → its current window
}

type logWindow struct {
	start time.Time
	n     int
}

// allow counts n lines for codeID at now; false (nothing counted) when that would exceed the limit.
func (l *logLimiter) allow(codeID int64, n int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windows == nil {
		l.windows = map[int64]logWindow{}
	}
	w := l.windows[codeID]
	if now.Sub(w.start) >= time.Minute {
		w = logWindow{start: now}
	}
	if w.n+n > logsPerMinute {
		l.windows[codeID] = w
		return false
	}
	w.n += n
	l.windows[codeID] = w
	return true
}

// prune forgets windows that ended before now.
func (l *logLimiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, w := range l.windows {
		if now.Sub(w.start) >= time.Minute {
			delete(l.windows, id)
		}
	}
}

func (b *botHub) handlePostLogs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Lines []logLineIn `json:"lines"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		accounts.WriteError(w, http.StatusBadRequest, "bad_logs")
		return
	}
	lines, ok := logLinesOf(in.Lines)
	if !ok {
		accounts.WriteError(w, http.StatusBadRequest, "bad_logs")
		return
	}
	p := principalOf(r)
	if !b.logs.allow(p.CodeID, len(lines), b.now()) {
		accounts.WriteError(w, http.StatusTooManyRequests, "too_many_logs")
		return
	}
	if err := b.acc.AddBotLogs(p.UserID, p.CodeID, lines); err != nil {
		log.Error().Err(err).Msg("bot logs insert failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
