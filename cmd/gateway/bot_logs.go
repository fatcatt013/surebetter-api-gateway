package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

// logDetail is what a line may carry for the Logs side panel. Other keys are dropped. Cashout …
// Choice are rule 1's stuck-mode decision (A = cash out, B = restake Under, the choice).
type logDetail struct {
	Ordered    *float64 `json:"ordered,omitempty"`
	OnSlip     *float64 `json:"onSlip,omitempty"`
	Partner    string   `json:"partner,omitempty"`
	NothingBet *bool    `json:"nothingBet,omitempty"`
	Shot       string   `json:"shot,omitempty"`
	Match      string   `json:"match,omitempty"`
	Market     string   `json:"market,omitempty"`
	Cashout    *float64 `json:"cashout,omitempty"`
	UnderOdds  *float64 `json:"underOdds,omitempty"`
	UnderStake *float64 `json:"underStake,omitempty"`
	LossA      *float64 `json:"lossA,omitempty"`
	LossB      *float64 `json:"lossB,omitempty"`
	Choice     string   `json:"choice,omitempty"`
}

var logLevels = map[string]bool{"info": true, "warn": true, "error": true}

const (
	maxLogBody     = 2 << 20 // POST body bytes: 200 lines of escaped 500-char messages and details can pass 1 MiB
	maxLogBatch    = 200     // lines per POST
	maxDetailBytes = 2000    // stored detail JSON
	logsPerMinute  = 2000    // lines per user per minute (all its codes together)
)

// logFuture is how far ahead of the gateway's clock a line's ts may be (agent clock skew).
const logFuture = time.Hour

// logLinesOf validates a posted batch (1–200 lines; ts within [now − 7 days, now + 1 h]; level
// info|warn|error; msg 1–500 chars; step ≤ 40, windowId ≤ 40, orderId ≤ 32 chars; detail JSON
// ≤ 2000 bytes) and converts it for storage. An empty detail is stored as none. false: bad_logs.
func logLinesOf(in []logLineIn, now time.Time) ([]accounts.BotLogLine, bool) {
	if len(in) < 1 || len(in) > maxLogBatch {
		return nil, false
	}
	minTS, maxTS := now.Add(-botRetention).UnixMilli(), now.Add(logFuture).UnixMilli()
	out := make([]accounts.BotLogLine, 0, len(in))
	for _, l := range in {
		if l.TS < minTS || l.TS > maxTS || !logLevels[l.Level] || l.Msg == "" || runeLen(l.Msg) > 500 || runeLen(l.Step) > 40 ||
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

// minuteLimiter allows each key (a user id) limit units per fixed one-minute window.
type minuteLimiter struct {
	limit   int
	mu      sync.Mutex
	windows map[int64]logWindow // key → its current window
}

type logWindow struct {
	start time.Time
	n     int
}

// allow counts n units for key at now; false (nothing counted) when that would exceed the limit.
// start identifies the window counted in, for refund.
func (l *minuteLimiter) allow(key int64, n int, now time.Time) (start time.Time, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windows == nil {
		l.windows = map[int64]logWindow{}
	}
	w := l.windows[key]
	if now.Sub(w.start) >= time.Minute {
		w = logWindow{start: now}
	}
	if w.n+n > l.limit {
		l.windows[key] = w
		return w.start, false
	}
	w.n += n
	l.windows[key] = w
	return w.start, true
}

// refund takes back n units allow counted in the window that began at start, if that window is
// still key's current one (units that were not used: nothing was stored).
func (l *minuteLimiter) refund(key int64, n int, start time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w, ok := l.windows[key]; ok && w.start.Equal(start) {
		w.n = max(w.n-n, 0)
		l.windows[key] = w
	}
}

// prune forgets windows that ended before now.
func (l *minuteLimiter) prune(now time.Time) {
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLogBody)).Decode(&in); err != nil {
		accounts.WriteError(w, http.StatusBadRequest, "bad_logs")
		return
	}
	lines, ok := logLinesOf(in.Lines, b.now())
	if !ok {
		accounts.WriteError(w, http.StatusBadRequest, "bad_logs")
		return
	}
	p := principalOf(r)
	window, ok := b.logs.allow(p.UserID, len(lines), b.now())
	if !ok {
		accounts.WriteError(w, http.StatusTooManyRequests, "too_many_logs")
		return
	}
	if err := b.acc.AddBotLogs(p.UserID, p.CodeID, lines); err != nil {
		b.logs.refund(p.UserID, len(lines), window) // not stored: not counted
		log.Error().Err(err).Msg("bot logs insert failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// logsTextPage is how many rows logs.txt reads per query (a var so a test can page by 1).
var logsTextPage = 5000

// positiveInt parses a query value that must be an integer above 0.
func positiveInt(v string) (int64, bool) {
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil && n > 0
}

// logFilterOf reads the GET /api/me/bot/logs query: agent (code id), window (≤ 40 chars), order
// (≤ 32), problems=1, q (≤ 100 chars), after or before (log ids, not both), limit (1–500, default
// 200). With after the lines come oldest first, else newest first. false: bad_query.
func logFilterOf(q url.Values) (accounts.BotLogFilter, bool) {
	f := accounts.BotLogFilter{WindowID: q.Get("window"), OrderID: q.Get("order"), Problems: q.Get("problems") == "1",
		Query: q.Get("q"), Limit: 200}
	if runeLen(f.WindowID) > 40 || runeLen(f.OrderID) > 32 || runeLen(f.Query) > 100 {
		return f, false
	}
	var ok bool
	if v := q.Get("agent"); v != "" {
		if f.AgentID, ok = positiveInt(v); !ok {
			return f, false
		}
	}
	if v := q.Get("after"); v != "" {
		if f.After, ok = positiveInt(v); !ok {
			return f, false
		}
		f.Ascending = true
	}
	if v := q.Get("before"); v != "" {
		if f.Before, ok = positiveInt(v); !ok || f.After != 0 {
			return f, false
		}
	}
	if v := q.Get("limit"); v != "" {
		n, ok := positiveInt(v)
		if !ok || n > 500 {
			return f, false
		}
		f.Limit = int(n)
	}
	return f, true
}

// logFilter is logFilterOf; a q search also reads only the last botRetention (lines the hourly prune
// has not reached yet are not scanned).
func (b *botHub) logFilter(q url.Values) (accounts.BotLogFilter, bool) {
	f, ok := logFilterOf(q)
	if ok && f.Query != "" {
		f.Since = b.now().Add(-botRetention).UnixMilli()
	}
	return f, ok
}

func (b *botHub) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	f, ok := b.logFilter(r.URL.Query())
	if !ok {
		accounts.WriteError(w, http.StatusBadRequest, "bad_query")
		return
	}
	lines, err := b.acc.QueryBotLogs(principalOf(r).UserID, f)
	if err != nil {
		log.Error().Err(err).Msg("bot logs query failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	accounts.WriteJSON(w, http.StatusOK, map[string][]accounts.BotLog{"lines": lines})
}

var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// logText is one logs.txt line: `<ISO time>  <agent> · <window>  <order>  <step>  <level>  <msg>`,
// time in UTC with milliseconds, an agent-wide line shows just the agent, an empty order or step is
// "-". A line break in any field becomes a space, so one log is always one line.
func logText(l accounts.BotLog) string {
	one := lineBreaks.Replace
	where := one(l.Agent)
	if l.WindowID != "" {
		where += " · " + one(l.WindowID)
	}
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return one(s)
	}
	return fmt.Sprintf("%s  %s  %s  %s  %s  %s", time.UnixMilli(l.TS).UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		where, dash(l.OrderID), dash(l.Step), one(l.Level), one(l.Msg))
}

// handleLogsText serves every line matching the filters (after, before and limit are ignored), oldest
// first, as a text download. It reads logsTextPage rows per query so the database is never held
// while a slow client downloads.
func (b *botHub) handleLogsText(w http.ResponseWriter, r *http.Request) {
	f, ok := b.logFilter(r.URL.Query())
	if !ok {
		accounts.WriteError(w, http.StatusBadRequest, "bad_query")
		return
	}
	f.After, f.Before, f.Limit, f.Ascending = 0, 0, logsTextPage, true
	userID := principalOf(r).UserID
	page, err := b.acc.QueryBotLogs(userID, f)
	if err != nil {
		log.Error().Err(err).Msg("bot logs query failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="surebetter-bot-logs.txt"`)
	w.Header().Set("Cache-Control", "no-store")
	out := bufio.NewWriter(w)
	defer out.Flush()
	for {
		for _, l := range page {
			out.WriteString(logText(l))
			out.WriteByte('\n')
		}
		if len(page) < logsTextPage {
			return
		}
		f.After = page[len(page)-1].ID
		if page, err = b.acc.QueryBotLogs(userID, f); err != nil {
			log.Error().Err(err).Msg("bot logs query failed mid-download")
			return
		}
	}
}
