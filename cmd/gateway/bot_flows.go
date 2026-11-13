package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// maxFlowsBody is the largest Flows upload (256 KiB).
const maxFlowsBody = 256 << 10

// validFlows reports whether body is one JSON object whose steps, cases, playbooks and rules are arrays.
func validFlows(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	var view map[string]json.RawMessage
	if err := dec.Decode(&view); err != nil || view == nil {
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		return false // more than one value
	}
	for _, key := range []string{"steps", "cases", "playbooks", "rules"} {
		v := bytes.TrimSpace(view[key])
		if len(v) == 0 || v[0] != '[' {
			return false
		}
	}
	return true
}

// handlePutFlows stores the agent's Flows view as is; a change tells the app ({"type":"bot"}).
func (b *botHub) handlePutFlows(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFlowsBody))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		accounts.WriteError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	case err != nil || !validFlows(body):
		accounts.WriteError(w, http.StatusBadRequest, "bad_flows")
		return
	}
	p := principalOf(r)
	if b.acc == nil {
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	changed, err := b.acc.SaveBotAgentFlows(p.UserID, p.CodeID, string(body))
	if err != nil {
		log.Error().Err(err).Int64("code", p.CodeID).Msg("bot flows save failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if changed {
		b.changed(p.UserID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// flowsView is the GET /api/me/bot/flows body.
type flowsView struct {
	AgentID   int64           `json:"agentId"`
	Agent     string          `json:"agent"`
	UpdatedAt int64           `json:"updatedAt"`
	Flows     json.RawMessage `json:"flows"`
}

// handleGetFlows serves one agent's Flows: ?agent=<id>, or else the online agent with flows first
// (by name), else the one whose flows changed last. 404 no_flows: none (or that agent has none);
// 404 unknown_agent: not one of the user's agents.
func (b *botHub) handleGetFlows(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	var want int64
	if q := r.URL.Query().Get("agent"); q != "" {
		id, err := strconv.ParseInt(q, 10, 64)
		if err != nil || id < 1 {
			accounts.WriteError(w, http.StatusBadRequest, "bad_query")
			return
		}
		want = id
	}
	rows, err := b.acc.ListBotAgents(userID) // by name
	if err != nil {
		log.Error().Err(err).Msg("bot agents list failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	var pick *accounts.BotAgentRow
	if want != 0 {
		for i := range rows {
			if rows[i].CodeID == want {
				pick = &rows[i]
			}
		}
		if pick == nil {
			accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
			return
		}
	} else {
		b.mu.Lock()
		for i := range rows {
			if rows[i].HasFlows && b.presenceLocked(userID, rows[i].CodeID) != nil {
				pick = &rows[i] // online, first by name
				break
			}
		}
		b.mu.Unlock()
		if pick == nil {
			for i := range rows {
				if rows[i].HasFlows && (pick == nil || rows[i].FlowsAt > pick.FlowsAt) {
					pick = &rows[i]
				}
			}
		}
	}
	if pick == nil || !pick.HasFlows {
		accounts.WriteError(w, http.StatusNotFound, "no_flows")
		return
	}
	flows, at, err := b.acc.BotAgentFlows(userID, pick.CodeID)
	if err != nil {
		log.Error().Err(err).Msg("bot flows read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if flows == "" {
		accounts.WriteError(w, http.StatusNotFound, "no_flows")
		return
	}
	accounts.WriteJSON(w, http.StatusOK, flowsView{AgentID: pick.CodeID, Agent: pick.Name, UpdatedAt: at, Flows: json.RawMessage(flows)})
}
