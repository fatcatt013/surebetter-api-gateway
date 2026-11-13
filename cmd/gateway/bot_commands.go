package main

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// commandWait is how long an app's window command waits for the agent's answer (a var so a test
// can shorten it).
var commandWait = 10 * time.Second

// botCommand is a window command as the agent's socket receives it ({"type":"command","command":…}).
type botCommand struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	WindowID  string `json:"windowId"`            // "" for add
	Bookmaker string `json:"bookmaker,omitempty"` // add only
}

// commandResult is the agent's answer to a command.
type commandResult struct {
	ok       bool
	windowID string
	err      string
	message  string
}

// pendingCommand is a command sent to an agent whose answer an app request is waiting for.
type pendingCommand struct {
	userID, codeID int64
	done           chan commandResult // buffered (1): the answer never blocks
}

var (
	windowActions    = map[string]bool{"start": true, "stop": true, "restart": true, "login": true, "bot": true, "remove": true}
	bookmakerPattern = regexp.MustCompile(`^[a-z0-9]{1,40}$`)
	errorCodePattern = regexp.MustCompile(`^[a-z_]{1,40}$`)
)

// handleAddWindow is POST /api/me/bot/agents/{agentId}/windows {"bookmaker"}.
func (b *botHub) handleAddWindow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Bookmaker string `json:"bookmaker"`
	}
	codeID, ok := agentParam(r)
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil || !ok || !bookmakerPattern.MatchString(in.Bookmaker) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_command")
		return
	}
	b.runCommand(w, r, codeID, botCommand{Action: "add", Bookmaker: in.Bookmaker})
}

// handleWindowAction is POST /api/me/bot/agents/{agentId}/windows/{windowId}/{action}.
func (b *botHub) handleWindowAction(w http.ResponseWriter, r *http.Request) {
	codeID, ok := agentParam(r)
	cmd := botCommand{Action: r.PathValue("action"), WindowID: r.PathValue("windowId")}
	if !ok || !windowActions[cmd.Action] || !windowIDPattern.MatchString(cmd.WindowID) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_command")
		return
	}
	b.runCommand(w, r, codeID, cmd)
}

// runCommand sends cmd to agent codeID and answers the app with the agent's answer, waiting at most
// commandWait (never holding b.mu while it waits). 404 unknown_agent: not the user's (non-revoked)
// bot code; 409 agent_offline: no socket, an agent older than protocol 2, or its socket does not
// take the command; 409 window_busy: the window is reserved for an order (the agent is not asked).
func (b *botHub) runCommand(w http.ResponseWriter, r *http.Request, codeID int64, cmd botCommand) {
	userID := principalOf(r).UserID
	if ok, err := b.agentOf(userID, codeID); err != nil {
		log.Error().Err(err).Msg("bot code lookup failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	} else if !ok {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	cmd.ID = newOrderID()
	payload, _ := json.Marshal(struct {
		Type    string     `json:"type"`
		Command botCommand `json:"command"`
	}{"command", cmd})
	pc := &pendingCommand{userID: userID, codeID: codeID, done: make(chan commandResult, 1)}
	b.mu.Lock()
	p := b.presenceLocked(userID, codeID)
	switch {
	case p == nil || p.proto < botProto:
		b.mu.Unlock()
		accounts.WriteError(w, http.StatusConflict, "agent_offline")
		return
	case cmd.Action != "add" && b.reservedLocked(userID)[codeID][cmd.WindowID] != "":
		b.mu.Unlock()
		accounts.WriteError(w, http.StatusConflict, "window_busy")
		return
	}
	b.commands[cmd.ID] = pc
	if !p.client.trySend(payload) {
		delete(b.commands, cmd.ID)
		b.mu.Unlock()
		accounts.WriteError(w, http.StatusConflict, "agent_offline")
		return
	}
	b.mu.Unlock()
	log.Info().Int64("user", userID).Int64("code", codeID).Str("command", cmd.ID).Str("action", cmd.Action).
		Str("window", cmd.WindowID).Str("bookmaker", cmd.Bookmaker).Msg("bot window command sent")

	timer := time.NewTimer(commandWait)
	defer timer.Stop()
	select {
	case res := <-pc.done:
		if res.ok {
			accounts.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "windowId": res.windowID})
		} else {
			accounts.WriteJSON(w, http.StatusConflict, map[string]string{"error": res.err, "message": res.message})
		}
	case <-timer.C:
		b.forgetCommand(cmd.ID)
		accounts.WriteError(w, http.StatusGatewayTimeout, "agent_timeout")
	case <-r.Context().Done():
		b.forgetCommand(cmd.ID)
	}
}

func (b *botHub) forgetCommand(id string) {
	b.mu.Lock()
	delete(b.commands, id)
	b.mu.Unlock()
}

// commandAck is the POST /api/me/bot/commands body.
type commandAck struct {
	ID       string `json:"id"`
	OK       *bool  `json:"ok"`
	WindowID string `json:"windowId"`
	Error    string `json:"error"`
	Message  string `json:"message"`
}

// handleCommandAck takes an agent's answer to a command and hands it to the waiting app request.
// 400 bad_command: id not 16 hex, ok missing, a bad window id, an error code not [a-z_]{1,40}
// (required when ok is false) or a message over 200 chars; 404 unknown_command: unknown, expired,
// already answered, or another code's command.
func (b *botHub) handleCommandAck(w http.ResponseWriter, r *http.Request) {
	var in commandAck
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil || json.Unmarshal(body, &in) != nil || !bootPattern.MatchString(in.ID) || in.OK == nil ||
		(in.WindowID != "" && !windowIDPattern.MatchString(in.WindowID)) || runeLen(in.Message) > 200 ||
		(!*in.OK && !errorCodePattern.MatchString(in.Error)) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_command")
		return
	}
	p := principalOf(r)
	b.mu.Lock()
	pc := b.commands[in.ID]
	if pc == nil || pc.userID != p.UserID || pc.codeID != p.CodeID {
		b.mu.Unlock()
		accounts.WriteError(w, http.StatusNotFound, "unknown_command")
		return
	}
	delete(b.commands, in.ID)
	pc.done <- commandResult{ok: *in.OK, windowID: in.WindowID, err: in.Error, message: in.Message}
	b.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
