package main

import (
	"cmp"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// SMS codes from the app (contract 2026-11-25-bot-auto-login-contract.md §11): a protocol-6 agent whose login asks
// for an SMS code opens a prompt, the user types the code in the app, the agent fetches it. Prompts live in memory
// only (botHub.codes, guarded by b.mu): a gateway restart drops them, and the code itself is deleted the moment the
// agent fetches it. No code is ever logged or stored.

// codeProto is the agent protocol that opens code prompts and takes {"type":"loginCode"}.
const codeProto = 6

const (
	codeMaxTries     = 3                              // codes the app may submit for one prompt
	codePromptAhead  = 5*time.Minute + 30*time.Second // the latest expiresAt a prompt may ask for, from now
	codePromptGrace  = 30 * time.Second               // a prompt ends this long after its expiresAt
	maxCodeBody      = 256                            // the app's code POST
	maxCodeAgentBody = 1 << 10                        // the agent's prompt, fetch and wrong POSTs
	titleCodeNeeded  = "SMS code needed"              // the prompt's alert
	codeStateWaiting = "waiting"                      // no code yet, or after a wrong one
	codeStateSent    = "sent"                         // a code waits for the agent to fetch it
	codeStateCheck   = "checking"                     // the agent fetched it
)

var (
	codePattern     = regexp.MustCompile(`^[0-9]{4,8}$`)
	promptIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// codeKey is a prompt's window: one prompt at most per (agent code, window).
type codeKey struct {
	codeID   int64
	windowID string
}

// codePrompt is one open prompt. code holds the submitted code until the agent fetches it.
type codePrompt struct {
	id        string
	userID    int64
	codeID    int64
	windowID  string
	bookmaker string
	digits    int
	expiresAt time.Time
	state     string
	tries     int
	wrong     bool // the agent reported a wrong code and no new code was sent since
	code      string
}

func loginCodeMsg(windowID string) []byte {
	b, _ := json.Marshal(struct {
		Type     string `json:"type"`
		WindowID string `json:"windowId"`
	}{"loginCode", windowID})
	return b
}

// promptLocked is codeID's window prompt when it is still alive (an expired one is ended here). b.mu must be held.
func (b *botHub) promptLocked(codeID int64, windowID string, fx *effects) *codePrompt {
	k := codeKey{codeID, windowID}
	pr := b.codes[k]
	if pr != nil && !b.now().Before(pr.expiresAt.Add(codePromptGrace)) {
		b.endPromptLocked(k, fx)
		return nil
	}
	return pr
}

// endPromptLocked ends the window's prompt, if any: its code is dropped, its alert resolves and the app is told.
// b.mu must be held.
func (b *botHub) endPromptLocked(k codeKey, fx *effects) {
	pr := b.codes[k]
	if pr == nil {
		return
	}
	pr.code = ""
	delete(b.codes, k)
	b.resolveCodeAlertLocked(pr.userID, pr.codeID, pr.windowID, fx)
	fx.notify(pr.userID)
}

// endPromptsLocked ends every prompt that match selects. b.mu must be held.
func (b *botHub) endPromptsLocked(match func(*codePrompt) bool, fx *effects) {
	var keys []codeKey
	for k, pr := range b.codes {
		if match(pr) {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(x, y codeKey) int {
		return cmp.Or(cmp.Compare(x.codeID, y.codeID), strings.Compare(x.windowID, y.windowID))
	})
	for _, k := range keys {
		b.endPromptLocked(k, fx)
	}
}

// expirePromptsLocked ends the prompts whose expiresAt + 30 s has passed (the scheduler's tick), and resolves a code
// alert left without a prompt (read back after a restart, the agent never reopened) once no prompt opened with it
// could still live: 5 min 30 s + 30 s after it was raised. b.mu must be held.
func (b *botHub) expirePromptsLocked(fx *effects) {
	now := b.now()
	b.endPromptsLocked(func(pr *codePrompt) bool { return !now.Before(pr.expiresAt.Add(codePromptGrace)) }, fx)
	if b.alerts == nil {
		return
	}
	var stale []alertScope
	for sc, oa := range b.alerts.open {
		if sc.kind == alertLogin && oa.alert.Title == titleCodeNeeded && b.codes[codeKey{sc.codeID, sc.windowID}] == nil &&
			now.UnixMilli() >= oa.alert.CreatedAt+(codePromptAhead+codePromptGrace).Milliseconds() {
			stale = append(stale, sc)
		}
	}
	slices.SortFunc(stale, compareScopes)
	for _, sc := range stale {
		b.resolveLocked(sc, fx)
	}
}

// resolveCodeAlertLocked resolves the window's open login alert when it is the code one. b.mu must be held.
func (b *botHub) resolveCodeAlertLocked(userID, codeID int64, windowID string, fx *effects) {
	if b.alerts == nil {
		return
	}
	sc := loginScope(userID, codeID, windowID)
	if oa := b.alerts.open[sc]; oa != nil && oa.alert.Title == titleCodeNeeded {
		b.resolveLocked(sc, fx)
	}
}

// alertCodeLocked raises the window's SMS code alert at a prompt's open: an earlier login alert of the window (the
// agent is trying again) resolves first; an open code alert (a replaced prompt, or one read back after a restart)
// stays and nothing new is raised. b.mu must be held.
func (b *botHub) alertCodeLocked(pr *codePrompt, fx *effects) {
	if b.alerts == nil || b.alerts.revoked[pr.codeID] {
		return
	}
	sc := loginScope(pr.userID, pr.codeID, pr.windowID)
	if oa := b.alerts.open[sc]; oa != nil {
		if oa.alert.Title == titleCodeNeeded {
			return
		}
		b.resolveLocked(sc, fx)
	}
	b.raiseLocked(pr.userID, botAlert{Kind: alertAttention, Severity: severityAttention, AgentID: pr.codeID, WindowID: pr.windowID,
		Title: titleCodeNeeded, Detail: loginWindowLabel(pr.windowID) + " (" + b.agentNameLocked(pr.codeID) + ")"}, &sc, fx)
}

// readSmall reads a body of at most max bytes; false: too big or unreadable.
func readSmall(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	return body, err == nil
}

// handleOpenCodePrompt is POST /api/me/bot/logins/{windowId}/code-prompt {"digits","expiresAt"}: the calling agent's
// window waits for an SMS code. 409 agent_update (its last status said proto 1–5; an agent with no bot socket, or
// whose socket has posted no status yet, is let through: right after a gateway restart its HTTP poll reopens the prompt
// before its socket and first status are back, and only a protocol-6 agent knows this route); 400 bad_prompt; 404
// no_login. A prompt replaces the window's earlier one.
func (b *botHub) handleOpenCodePrompt(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	windowID := r.PathValue("windowId")
	b.mu.Lock()
	proto := 0
	if pres := b.presenceLocked(p.UserID, p.CodeID); pres != nil {
		proto = pres.proto
	}
	b.mu.Unlock()
	if proto != 0 && proto < codeProto {
		accounts.WriteError(w, http.StatusConflict, "agent_update")
		return
	}
	var in struct {
		Digits    *int   `json:"digits"`
		ExpiresAt *int64 `json:"expiresAt"`
	}
	body, ok := readSmall(w, r, maxCodeAgentBody)
	now := b.now()
	if !ok || json.Unmarshal(body, &in) != nil || in.Digits == nil || in.ExpiresAt == nil || *in.Digits < 4 || *in.Digits > 8 ||
		*in.ExpiresAt <= now.UnixMilli() || *in.ExpiresAt > now.Add(codePromptAhead).UnixMilli() {
		accounts.WriteError(w, http.StatusBadRequest, "bad_prompt")
		return
	}
	row, found, err := b.acc.BotLogin(p.CodeID, windowID)
	if err != nil {
		log.Error().Err(err).Int64("code", p.CodeID).Str("window", windowID).Msg("bot login read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !found || row.UserID != p.UserID {
		accounts.WriteError(w, http.StatusNotFound, "no_login")
		return
	}
	pr := &codePrompt{id: newOrderID(), userID: p.UserID, codeID: p.CodeID, windowID: windowID, bookmaker: row.Bookmaker,
		digits: *in.Digits, expiresAt: time.UnixMilli(*in.ExpiresAt), state: codeStateWaiting}
	b.mu.Lock()
	fx := &effects{}
	if b.alerts != nil && (b.alerts.revoked[p.CodeID] || b.alerts.disabled[p.UserID]) { // revoked or disabled meanwhile
		b.release(fx)
		accounts.WriteError(w, http.StatusNotFound, "no_login")
		return
	}
	k := codeKey{p.CodeID, windowID}
	if old := b.codes[k]; old != nil {
		old.code = ""
		delete(b.codes, k) // replaced: its alert stays for the new prompt
	}
	b.codes[k] = pr
	b.alertCodeLocked(pr, fx)
	fx.notify(p.UserID)
	b.release(fx)
	log.Info().Int64("user", p.UserID).Int64("code", p.CodeID).Str("window", windowID).Int("digits", pr.digits).
		Int64("expires_at", *in.ExpiresAt).Msg("bot login code prompt opened")
	accounts.WriteJSON(w, http.StatusOK, map[string]any{"promptId": pr.id, "triesLeft": codeMaxTries})
}

// agentPromptOf reads the agent's {"promptId"} body and returns its window's live prompt with that id, or answers
// (400 bad_prompt, 404 no_prompt) and returns nil. b.mu is held on a non-nil return; the caller releases fx.
func (b *botHub) agentPromptOf(w http.ResponseWriter, r *http.Request, fx *effects) *codePrompt {
	var in struct {
		PromptID string `json:"promptId"`
	}
	body, ok := readSmall(w, r, maxCodeAgentBody)
	if !ok || json.Unmarshal(body, &in) != nil || !promptIDPattern.MatchString(in.PromptID) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_prompt")
		return nil
	}
	p := principalOf(r)
	b.mu.Lock()
	pr := b.promptLocked(p.CodeID, r.PathValue("windowId"), fx)
	if pr == nil || pr.id != in.PromptID || pr.userID != p.UserID {
		b.release(fx)
		accounts.WriteError(w, http.StatusNotFound, "no_prompt")
		return nil
	}
	return pr
}

// handleFetchCode is POST /api/me/bot/logins/{windowId}/code {"promptId"}: 200 {"code"} once (the code is deleted from
// memory in the same step, state checking), 204 while no code is waiting, 404 no_prompt.
func (b *botHub) handleFetchCode(w http.ResponseWriter, r *http.Request) {
	fx := &effects{}
	pr := b.agentPromptOf(w, r, fx)
	if pr == nil {
		return
	}
	if pr.state != codeStateSent || pr.code == "" {
		b.release(fx)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	code := pr.code
	pr.code, pr.state = "", codeStateCheck
	fx.notify(pr.userID)
	userID, codeID, windowID := pr.userID, pr.codeID, pr.windowID
	b.release(fx)
	log.Info().Int64("user", userID).Int64("code", codeID).Str("window", windowID).Msg("bot login code fetched")
	accounts.WriteJSON(w, http.StatusOK, map[string]string{"code": code})
}

// handleWrongCode is POST /api/me/bot/logins/{windowId}/code-prompt/wrong {"promptId"}: the code it fetched was wrong;
// the prompt waits again → 200 {"triesLeft"}; 404 no_prompt. Only a prompt being checked changes: a late or repeated
// report (no code fetched, or a new code waiting) answers the current triesLeft and changes nothing.
func (b *botHub) handleWrongCode(w http.ResponseWriter, r *http.Request) {
	fx := &effects{}
	pr := b.agentPromptOf(w, r, fx)
	if pr == nil {
		return
	}
	left := codeMaxTries - pr.tries
	if pr.state != codeStateCheck {
		b.release(fx)
		accounts.WriteJSON(w, http.StatusOK, map[string]int{"triesLeft": left})
		return
	}
	pr.state, pr.wrong = codeStateWaiting, true
	fx.notify(pr.userID)
	userID, codeID, windowID := pr.userID, pr.codeID, pr.windowID
	b.release(fx)
	log.Info().Int64("user", userID).Int64("code", codeID).Str("window", windowID).Int("tries_left", left).Msg("bot login code wrong")
	accounts.WriteJSON(w, http.StatusOK, map[string]int{"triesLeft": left})
}

// codePromptView is one prompt in GET /api/me/bot/login-codes. Never a code.
type codePromptView struct {
	AgentID   int64  `json:"agentId"`
	WindowID  string `json:"windowId"`
	Bookmaker string `json:"bookmaker"`
	Digits    int    `json:"digits"`
	ExpiresAt int64  `json:"expiresAt"`
	State     string `json:"state"`
	TriesLeft int    `json:"triesLeft"`
	Wrong     bool   `json:"wrong"`
}

// handleGetLoginCodes is GET /api/me/bot/login-codes: the user's live prompts, by agent then window.
func (b *botHub) handleGetLoginCodes(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	b.mu.Lock()
	fx := &effects{}
	b.expirePromptsLocked(fx)
	out := []codePromptView{}
	for _, pr := range b.codes {
		if pr.userID == userID {
			out = append(out, codePromptView{AgentID: pr.codeID, WindowID: pr.windowID, Bookmaker: pr.bookmaker, Digits: pr.digits,
				ExpiresAt: pr.expiresAt.UnixMilli(), State: pr.state, TriesLeft: codeMaxTries - pr.tries, Wrong: pr.wrong})
		}
	}
	b.release(fx)
	slices.SortFunc(out, func(x, y codePromptView) int {
		return cmp.Or(cmp.Compare(x.AgentID, y.AgentID), strings.Compare(x.WindowID, y.WindowID))
	})
	accounts.WriteJSON(w, http.StatusOK, map[string][]codePromptView{"prompts": out})
}

// handleSubmitCode is POST /api/me/bot/agents/{agentId}/windows/{windowId}/login-code {"code"}: the user's SMS code
// for the window's prompt. Checks in order: 404 unknown_agent; 400 bad_code (body over 256 bytes, code not 4–8
// digits); 404 no_prompt (none, or past its expiresAt: the agent no longer fetches); 400 bad_code (not the prompt's digits); 409 code_pending (not waiting); 429 too_many_codes;
// 409 agent_offline (no bot socket, or not over TLS). Then the agent is told ({"type":"loginCode"}, protocol 6).
func (b *botHub) handleSubmitCode(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	codeID, valid := agentParam(r)
	if !valid {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	if mine, err := b.agentOf(userID, codeID); err != nil {
		log.Error().Err(err).Msg("bot code lookup failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	} else if !mine {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return
	}
	var in struct {
		Code *string `json:"code"`
	}
	body, ok := readSmall(w, r, maxCodeBody)
	if !ok || json.Unmarshal(body, &in) != nil || in.Code == nil || !codePattern.MatchString(*in.Code) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_code")
		return
	}
	clear(body)
	code, windowID := *in.Code, r.PathValue("windowId")
	b.mu.Lock()
	fx := &effects{}
	var status int
	var errCode string
	pr := b.promptLocked(codeID, windowID, fx)
	pres := b.presenceLocked(userID, codeID)
	switch {
	case pr == nil || pr.userID != userID || !b.now().Before(pr.expiresAt):
		status, errCode = http.StatusNotFound, "no_prompt"
	case len(code) != pr.digits:
		status, errCode = http.StatusBadRequest, "bad_code"
	case pr.state != codeStateWaiting:
		status, errCode = http.StatusConflict, "code_pending"
	case pr.tries >= codeMaxTries:
		status, errCode = http.StatusTooManyRequests, "too_many_codes"
	case pres == nil || !pres.client.secure:
		status, errCode = http.StatusConflict, "agent_offline"
	}
	if errCode != "" {
		b.release(fx)
		accounts.WriteError(w, status, errCode)
		return
	}
	pr.tries++
	pr.state, pr.code, pr.wrong = codeStateSent, code, false
	left := codeMaxTries - pr.tries
	if pres.proto >= codeProto {
		pres.client.trySend(loginCodeMsg(windowID))
	}
	fx.notify(userID)
	b.release(fx)
	log.Info().Int64("user", userID).Int64("code", codeID).Str("window", windowID).Int("tries", codeMaxTries-left).
		Msg("bot login code submitted")
	accounts.WriteJSON(w, http.StatusOK, map[string]any{"state": codeStateSent, "triesLeft": left})
}
