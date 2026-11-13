package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// maxRecordingBody is the largest recording upload (1 MiB).
const maxRecordingBody = 1 << 20

// mouseRecording is a raw mouse recording: paths of [x, y, tMs] points.
type mouseRecording struct {
	Paths []struct {
		Points [][]float64 `json:"points"`
	} `json:"paths"`
}

// typingEvent is one key event of a typing session.
type typingEvent struct {
	Type string   `json:"type"`
	Key  string   `json:"key"`
	T    *float64 `json:"t"`
}

// typingRecording is a raw typing recording: sessions of key events after the field got focus.
type typingRecording struct {
	Sessions []struct {
		FocusAt *float64      `json:"focusAt"`
		Events  []typingEvent `json:"events"`
	} `json:"sessions"`
}

// recordingOf validates a raw recording of kind and returns it re-encoded (only the known keys) with
// its path or session count. false: bad_recording. Mouse: 1–200 paths of 1–5 000 points of three
// numbers. Typing: 1–200 sessions with a focusAt and 1–500 events (type down|up, key 1–20 chars, t).
func recordingOf(kind string, body []byte) (string, int, bool) {
	var v any
	n := 0
	switch kind {
	case "mouse":
		var m mouseRecording
		if json.Unmarshal(body, &m) != nil || len(m.Paths) < 1 || len(m.Paths) > 200 {
			return "", 0, false
		}
		for _, p := range m.Paths {
			if len(p.Points) < 1 || len(p.Points) > 5000 {
				return "", 0, false
			}
			for _, pt := range p.Points {
				if len(pt) != 3 {
					return "", 0, false
				}
			}
		}
		v, n = m, len(m.Paths)
	case "typing":
		var tr typingRecording
		if json.Unmarshal(body, &tr) != nil || len(tr.Sessions) < 1 || len(tr.Sessions) > 200 {
			return "", 0, false
		}
		for _, s := range tr.Sessions {
			if s.FocusAt == nil || len(s.Events) < 1 || len(s.Events) > 500 {
				return "", 0, false
			}
			for _, e := range s.Events {
				if (e.Type != "down" && e.Type != "up") || e.Key == "" || runeLen(e.Key) > 20 || e.T == nil {
					return "", 0, false
				}
			}
		}
		v, n = tr, len(tr.Sessions)
	default:
		return "", 0, false
	}
	js, err := json.Marshal(v)
	if err != nil {
		return "", 0, false
	}
	return string(js), n, true
}

var recordingKinds = map[string]bool{"mouse": true, "typing": true}

// agentOf reports whether codeID is one of userID's bot codes that is not revoked (an agent, online
// or not, connected or not).
func (b *botHub) agentOf(userID, codeID int64) (bool, error) {
	codes, err := b.acc.ListExtensionCodes(userID)
	if err != nil {
		return false, err
	}
	for _, c := range codes {
		if c.ID == codeID && c.Kind == accounts.KindBot && c.RevokedAt == nil {
			return true, nil
		}
	}
	return false, nil
}

// agentParam reads the {agentId} path value; false: not a whole number ≥ 1.
func agentParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("agentId"), 10, 64)
	return id, err == nil && id >= 1
}

// handlePutRecording stores the app's raw recording for one agent and tells that agent's socket
// ({"type":"recordings"}) so it downloads it.
func (b *botHub) handlePutRecording(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	codeID, ok := agentParam(r)
	if !recordingKinds[kind] || !ok {
		accounts.WriteError(w, http.StatusNotFound, "not_found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRecordingBody))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		accounts.WriteError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	js, count, valid := recordingOf(kind, body)
	if err != nil || !valid {
		accounts.WriteError(w, http.StatusBadRequest, "bad_recording")
		return
	}
	userID := principalOf(r).UserID
	if ok, err := b.agentOf(userID, codeID); err != nil {
		log.Error().Err(err).Msg("bot code lookup failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	} else if !ok {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	at, err := b.acc.SaveBotRecording(userID, codeID, kind, js, count)
	if err != nil {
		log.Error().Err(err).Msg("bot recording save failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	b.mu.Lock()
	if p := b.presenceLocked(userID, codeID); p != nil {
		p.client.trySend(recordingsMsg)
	}
	b.mu.Unlock()
	accounts.WriteJSON(w, http.StatusOK, map[string]int64{"count": int64(count), "updatedAt": at})
}

var recordingsMsg = []byte(`{"type":"recordings"}`)

// recordingSummary is one kind in the recordings summary.
type recordingSummary struct {
	Count     int   `json:"count"`
	UpdatedAt int64 `json:"updatedAt"`
}

// writeRecordings answers codeID's recordings summary: mouse and typing, each null when missing.
func (b *botHub) writeRecordings(w http.ResponseWriter, codeID int64) {
	list, err := b.acc.BotRecordings(codeID)
	if err != nil {
		log.Error().Err(err).Msg("bot recordings read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := map[string]*recordingSummary{"mouse": nil, "typing": nil}
	for kind, r := range list {
		out[kind] = &recordingSummary{Count: r.Count, UpdatedAt: r.UpdatedAt}
	}
	accounts.WriteJSON(w, http.StatusOK, out)
}

// handleAgentRecordings is the app's summary of one agent's recordings.
func (b *botHub) handleAgentRecordings(w http.ResponseWriter, r *http.Request) {
	codeID, ok := agentParam(r)
	if !ok {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	if ok, err := b.agentOf(principalOf(r).UserID, codeID); err != nil {
		log.Error().Err(err).Msg("bot code lookup failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	} else if !ok {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	b.writeRecordings(w, codeID)
}

// handleOwnRecordings is the calling agent's summary of its own recordings.
func (b *botHub) handleOwnRecordings(w http.ResponseWriter, r *http.Request) {
	b.writeRecordings(w, principalOf(r).CodeID)
}

// handleOwnRecording serves the calling agent's own recording of {kind}: {"updatedAt", "paths"|"sessions"}.
func (b *botHub) handleOwnRecording(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !recordingKinds[kind] {
		accounts.WriteError(w, http.StatusNotFound, "no_recording")
		return
	}
	body, at, found, err := b.acc.BotRecording(principalOf(r).CodeID, kind)
	if err != nil {
		log.Error().Err(err).Msg("bot recording read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !found || len(body) < 2 {
		accounts.WriteError(w, http.StatusNotFound, "no_recording")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"updatedAt":%d,%s`, at, body[1:]) // body is {"paths":…} or {"sessions":…}
}
