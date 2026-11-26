package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// Bot auto-login (contract 2026-11-25-bot-auto-login-contract.md §4): the app stores each window's bookmaker login,
// the agent lists its own windows' logins and reveals one right before it types it, then posts the result. Nothing
// here logs a request body, a user name or a password.

// loginProto is the agent protocol that logs in by itself: only such agents get {"type":"logins"}.
const loginProto = 5

// revealsPerDay caps the reveals of one window's login per Prague day.
const revealsPerDay = 10

// maxLoginBody is the largest body of a login PUT or a result POST.
const maxLoginBody = 1 << 10

var (
	// loginWindowPattern is a window id a login can be stored for: <bookmaker>-<1..99>.
	loginWindowPattern = regexp.MustCompile(`^[a-z0-9]{1,30}-[1-9][0-9]?$`)
	loginsMsg          = []byte(`{"type":"logins"}`)
	errLoginFailing    = errors.New("login_failing")
	errTooManyReveals  = errors.New("too_many_reveals")
)

// loginResults are the results an agent posts.
var loginResults = map[string]bool{"ok": true, "code": true, "captcha": true, "unexpected": true, "restricted": true,
	"wrong_password": true, "blocked": true, "limit": true}

// secureRequest: the request came over TLS (r.TLS, or nginx's X-Forwarded-Proto: exactly one value, exactly
// "https"), or BOT_LOGINS_ALLOW_HTTP=1 (development).
func secureRequest(r *http.Request, allowHTTP bool) bool { return accounts.SecureRequest(r, allowHTTP) }

// loginGate runs before every login route's auth: 503 logins_unavailable without a key, 403 https_required on a
// request that is not secure.
func (b *botHub) loginGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.logins == nil {
			accounts.WriteError(w, http.StatusServiceUnavailable, "logins_unavailable")
			return
		}
		if !secureRequest(r, b.loginsAllowHTTP) {
			accounts.WriteError(w, http.StatusForbidden, "https_required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loginView is one login in GET /api/me/bot/logins (and the PUT answer). Never a password.
type loginView struct {
	AgentID    int64  `json:"agentId"`
	WindowID   string `json:"windowId"`
	Bookmaker  string `json:"bookmaker"`
	Username   string `json:"username"`
	UpdatedAt  int64  `json:"updatedAt"`
	LastOkAt   int64  `json:"lastOkAt"`
	LastResult string `json:"lastResult"`
	LastAt     int64  `json:"lastAt"`
	Failing    string `json:"failing"`
}

// ownLoginView is one login in GET /api/me/bot/logins/mine: no user name, no password.
type ownLoginView struct {
	WindowID  string `json:"windowId"`
	UpdatedAt int64  `json:"updatedAt"`
	Failing   string `json:"failing"`
}

// failingOf is a row's failing: key_lost when its key is neither the current nor the previous one, else the stored one.
func (b *botHub) failingOf(r accounts.BotLoginRow) string {
	if !b.logins.known(r.KeyID) {
		return "key_lost"
	}
	return r.Failing
}

// loginViewOf is r as the app sees it; the user name is decrypted (none for key_lost). A row of a known key that does
// not open is shown as key_lost too ("Set again"): reveal refuses it (login_failing) all the same.
func (b *botHub) loginViewOf(r accounts.BotLoginRow, username string, decrypt bool) loginView {
	v := loginView{AgentID: r.CodeID, WindowID: r.WindowID, Bookmaker: r.Bookmaker, UpdatedAt: r.UpdatedAt, LastOkAt: r.LastOkAt,
		LastResult: r.LastResult, LastAt: r.LastAt, Failing: b.failingOf(r), Username: username}
	if decrypt && v.Failing != "key_lost" {
		u, _, err := b.logins.open(r.Secret, r.KeyID, r.UserID, r.CodeID, r.WindowID)
		if err != nil {
			log.Error().Int64("user", r.UserID).Int64("code", r.CodeID).Str("window", r.WindowID).Msg("bot login could not be decrypted")
			v.Failing = "key_lost"
		}
		v.Username = u
	}
	return v
}

// handleGetLogins is GET /api/me/bot/logins: the user's logins of non-revoked agents, by agent then window.
func (b *botHub) handleGetLogins(w http.ResponseWriter, r *http.Request) {
	rows, err := b.acc.ListBotLogins(principalOf(r).UserID)
	if err != nil {
		log.Error().Err(err).Msg("bot logins list failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]loginView, 0, len(rows))
	for _, row := range rows {
		out = append(out, b.loginViewOf(row, "", true))
	}
	accounts.WriteJSON(w, http.StatusOK, map[string][]loginView{"logins": out})
}

// noControl: s has no control character (U+0000–U+001F, U+007F).
func noControl(s string) bool {
	return !strings.ContainsFunc(s, func(c rune) bool { return c < 0x20 || c == 0x7f })
}

// loginBodyOf reads a PUT body: a JSON object with exactly the string fields username and password. The user name is
// trimmed; it must be 1–100 runes, the password 1–200, neither with a control character.
func loginBodyOf(body []byte) (username, password string, ok bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil || len(raw) != 2 || !uniqueKeys(body) {
		return "", "", false
	}
	for _, k := range []string{"username", "password"} {
		v := raw[k]
		if len(v) == 0 || v[0] != '"' {
			return "", "", false
		}
	}
	if json.Unmarshal(raw["username"], &username) != nil || json.Unmarshal(raw["password"], &password) != nil {
		return "", "", false
	}
	username = strings.TrimSpace(username)
	if n := runeLen(username); n < 1 || n > 100 || !noControl(username) {
		return "", "", false
	}
	if n := runeLen(password); n < 1 || n > 200 || !noControl(password) {
		return "", "", false
	}
	return username, password, true
}

// uniqueKeys: the JSON object body names no key twice (json.Unmarshal would keep the last one).
func uniqueKeys(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		k, isKey := t.(string)
		if err != nil || !isKey || seen[k] {
			return false
		}
		seen[k] = true
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return false
		}
	}
	return true
}

// loginTarget reads the app's {agentId} and {windowId}: 400 bad_login for a bad window id, 404 unknown_agent when the
// agent is not one of the user's non-revoked bot codes. ok false: answered.
func (b *botHub) loginTarget(w http.ResponseWriter, r *http.Request) (codeID int64, windowID string, ok bool) {
	windowID = r.PathValue("windowId")
	if !loginWindowPattern.MatchString(windowID) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_login")
		return 0, "", false
	}
	codeID, valid := agentParam(r)
	if !valid {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return 0, "", false
	}
	if mine, err := b.agentOf(principalOf(r).UserID, codeID); err != nil {
		log.Error().Err(err).Msg("bot code lookup failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return 0, "", false
	} else if !mine {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent")
		return 0, "", false
	}
	return codeID, windowID, true
}

// loginChanged follows a PUT or DELETE (deleted) of codeID's window login: a DELETE ends the window's code prompt, its
// open login alert resolves, the agent gets {"type":"logins"} (protocol 5) and the user's app sockets {"type":"bot"}.
func (b *botHub) loginChanged(userID, codeID int64, windowID string, deleted bool) {
	b.mu.Lock()
	fx := &effects{}
	if deleted {
		b.endPromptLocked(codeKey{codeID, windowID}, fx)
	}
	b.resolveLoginAlertLocked(userID, codeID, windowID, fx)
	b.sendLoginsLocked(userID, codeID)
	fx.notify(userID)
	b.release(fx)
}

// sendLoginsLocked sends {"type":"logins"} to codeID's bot socket when it is online with protocol 5 or more. b.mu must
// be held.
func (b *botHub) sendLoginsLocked(userID, codeID int64) {
	if p := b.presenceLocked(userID, codeID); p != nil && p.proto >= loginProto {
		p.client.trySend(loginsMsg)
	}
}

// handlePutLogin is PUT /api/me/bot/agents/{agentId}/windows/{windowId}/login {"username","password"}: the window's
// login is stored (sealed with the current key) and answered as in the list.
func (b *botHub) handlePutLogin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLoginBody))
	username, password, valid := loginBodyOf(body)
	if err != nil || !valid {
		accounts.WriteError(w, http.StatusBadRequest, "bad_login")
		return
	}
	clear(body)
	codeID, windowID, ok := b.loginTarget(w, r)
	if !ok {
		return
	}
	userID := principalOf(r).UserID
	secret, keyID, tag, err := b.logins.seal(userID, codeID, windowID, username, password)
	if err != nil {
		log.Error().Msg("bot login could not be sealed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	book := windowID[:strings.LastIndexByte(windowID, '-')]
	row := accounts.BotLoginRow{UserID: userID, CodeID: codeID, WindowID: windowID, Bookmaker: book, Secret: secret, KeyID: keyID,
		UserTag: tag, UpdatedAt: b.now().UnixMilli()}
	if err := b.acc.SaveBotLogin(row); errors.Is(err, accounts.ErrNotFound) {
		accounts.WriteError(w, http.StatusNotFound, "unknown_agent") // revoked or deleted meanwhile
		return
	} else if err != nil {
		log.Error().Err(err).Int64("code", codeID).Str("window", windowID).Msg("bot login save failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	stored, found, err := b.acc.BotLogin(codeID, windowID)
	if err != nil || !found {
		log.Error().Err(err).Int64("code", codeID).Str("window", windowID).Msg("bot login read after save failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	log.Info().Int64("user", userID).Int64("code", codeID).Str("window", windowID).Msg("bot login saved")
	b.loginChanged(userID, codeID, windowID, false)
	accounts.WriteJSON(w, http.StatusOK, b.loginViewOf(stored, username, false))
}

// handleDeleteLogin is DELETE /api/me/bot/agents/{agentId}/windows/{windowId}/login: 204, also when there was none.
func (b *botHub) handleDeleteLogin(w http.ResponseWriter, r *http.Request) {
	codeID, windowID, ok := b.loginTarget(w, r)
	if !ok {
		return
	}
	userID := principalOf(r).UserID
	if _, err := b.acc.DeleteBotLogin(codeID, windowID); err != nil {
		log.Error().Err(err).Int64("code", codeID).Str("window", windowID).Msg("bot login delete failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	log.Info().Int64("user", userID).Int64("code", codeID).Str("window", windowID).Msg("bot login removed")
	b.loginChanged(userID, codeID, windowID, true)
	w.WriteHeader(http.StatusNoContent)
}

// handleOwnLogins is GET /api/me/bot/logins/mine: the calling agent's windows that have a login.
func (b *botHub) handleOwnLogins(w http.ResponseWriter, r *http.Request) {
	rows, err := b.acc.ListBotLoginsOfCode(principalOf(r).CodeID)
	if err != nil {
		log.Error().Err(err).Msg("bot own logins list failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]ownLoginView, 0, len(rows))
	for _, row := range rows {
		out = append(out, ownLoginView{WindowID: row.WindowID, UpdatedAt: row.UpdatedAt, Failing: b.failingOf(row)})
	}
	accounts.WriteJSON(w, http.StatusOK, map[string][]ownLoginView{"logins": out})
}

// handleReveal is POST /api/me/bot/logins/{windowId}/reveal: the calling agent's window login in clear, checked in
// order: its bot socket attached (409 agent_offline) over TLS (403 https_required), a row (404 no_login), not failing
// nor key_lost (409 login_failing), under the day's cap (429 too_many_reveals), and it decrypts (409 login_failing).
// A reveal counts in the same transaction.
func (b *botHub) handleReveal(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	windowID := r.PathValue("windowId")
	b.mu.Lock()
	pres := b.presenceLocked(p.UserID, p.CodeID)
	attached, secure := pres != nil, pres != nil && pres.client.secure
	b.mu.Unlock()
	if !attached {
		accounts.WriteError(w, http.StatusConflict, "agent_offline")
		return
	}
	if !secure {
		accounts.WriteError(w, http.StatusForbidden, "https_required")
		return
	}
	day := b.now().In(prague).Format("2006-01-02")
	var username, password string
	var updatedAt int64
	err := b.acc.RevealBotLogin(p.CodeID, windowID, day, func(row accounts.BotLoginRow, today int) error {
		if row.UserID != p.UserID {
			return accounts.ErrNoLogin
		}
		if b.failingOf(row) != "" {
			return errLoginFailing
		}
		if today >= revealsPerDay {
			return errTooManyReveals
		}
		u, pw, err := b.logins.open(row.Secret, row.KeyID, row.UserID, row.CodeID, row.WindowID)
		if err != nil {
			log.Error().Int64("user", p.UserID).Int64("code", p.CodeID).Str("window", windowID).Msg("bot login could not be decrypted")
			return errLoginFailing
		}
		username, password, updatedAt = u, pw, row.UpdatedAt
		return nil
	})
	switch {
	case errors.Is(err, accounts.ErrNoLogin):
		accounts.WriteError(w, http.StatusNotFound, "no_login")
	case errors.Is(err, errLoginFailing):
		accounts.WriteError(w, http.StatusConflict, "login_failing")
	case errors.Is(err, errTooManyReveals):
		accounts.WriteError(w, http.StatusTooManyRequests, "too_many_reveals")
	case err != nil:
		log.Error().Err(err).Int64("code", p.CodeID).Str("window", windowID).Msg("bot login reveal failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
	default:
		log.Info().Int64("user", p.UserID).Int64("code", p.CodeID).Str("window", windowID).Msg("bot login revealed")
		accounts.WriteJSON(w, http.StatusOK, revealAnswer{Username: username, Password: password, UpdatedAt: updatedAt})
	}
}

// revealAnswer is the reveal's 200 body: the login and the row's updatedAt, which the agent echoes in its result.
type revealAnswer struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	UpdatedAt int64  `json:"updatedAt"`
}

// loginResultIn is the POST …/result body. UpdatedAt: the reveal's updatedAt for this attempt (absent = no reveal,
// accepted as it is).
type loginResultIn struct {
	Result    string `json:"result"`
	Detail    string `json:"detail"`
	UpdatedAt *int64 `json:"updatedAt"`
}

// handleLoginResult is POST /api/me/bot/logins/{windowId}/result {"result","detail"?,"updatedAt"?}: a result whose
// updatedAt is not the row's (the login was saved again since that reveal) is ignored (204, one log line); else the
// window's last result is stored; wrong_password and blocked make it (and its same-user-name siblings) failing and tell the agent; a result
// that needs the user raises the window's login alert (one open per window). The app is told ({"type":"bot"}).
func (b *botHub) handleLoginResult(w http.ResponseWriter, r *http.Request) {
	var in loginResultIn
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLoginBody))
	if err != nil || json.Unmarshal(body, &in) != nil || !loginResults[in.Result] || runeLen(in.Detail) > 120 {
		accounts.WriteError(w, http.StatusBadRequest, "bad_result")
		return
	}
	p := principalOf(r)
	windowID := r.PathValue("windowId")
	row, spread, err := b.acc.SetBotLoginResult(p.CodeID, windowID, in.Result, b.now().UnixMilli(), in.UpdatedAt)
	if errors.Is(err, accounts.ErrNoLogin) || (row.UserID != 0 && row.UserID != p.UserID) {
		b.endCode(p.UserID, p.CodeID, windowID) // a prompt opened as the login was deleted
		accounts.WriteError(w, http.StatusNotFound, "no_login")
		return
	}
	if errors.Is(err, accounts.ErrStaleLogin) {
		b.endCode(p.UserID, p.CodeID, windowID) // any result ends the window's code prompt, an ignored one too
		log.Info().Int64("user", p.UserID).Int64("code", p.CodeID).Str("window", windowID).Str("result", in.Result).
			Int64("updated_at", *in.UpdatedAt).Int64("row_updated_at", row.UpdatedAt).Msg("bot login result ignored: the login was saved again since")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		log.Error().Err(err).Int64("code", p.CodeID).Str("window", windowID).Msg("bot login result save failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	b.mu.Lock()
	fx := &effects{}
	// The prompt ends and its code alert resolves before the result's own alert, also a code alert read back after a
	// restart (no prompt in memory).
	b.endPromptLocked(codeKey{p.CodeID, windowID}, fx)
	b.resolveCodeAlertLocked(p.UserID, p.CodeID, windowID, fx)
	if spread > 0 {
		b.sendLoginsLocked(p.UserID, p.CodeID)
	}
	b.alertLoginResultLocked(p.UserID, p.CodeID, windowID, row.Bookmaker, in.Result, in.Detail, fx)
	fx.notify(p.UserID)
	b.release(fx)
	w.WriteHeader(http.StatusNoContent)
}

// endCode ends the window's code prompt and resolves its code alert (also one read back after a restart). Call it
// without b.mu.
func (b *botHub) endCode(userID, codeID int64, windowID string) {
	b.mu.Lock()
	fx := &effects{}
	b.endPromptLocked(codeKey{codeID, windowID}, fx)
	b.resolveCodeAlertLocked(userID, codeID, windowID, fx)
	b.release(fx)
}

// loginRemoved deletes codeID's window login after the agent removed the window (its ok to a remove command) and
// resolves the window's login alert. Call it without b.mu.
func (b *botHub) loginRemoved(userID, codeID int64, windowID string) {
	if b.acc == nil {
		return
	}
	deleted, err := b.acc.DeleteBotLogin(codeID, windowID)
	if err != nil {
		log.Error().Err(err).Int64("code", codeID).Str("window", windowID).Msg("bot login delete after window remove failed")
		return
	}
	b.mu.Lock()
	fx := &effects{}
	b.endPromptLocked(codeKey{codeID, windowID}, fx)
	b.resolveLoginAlertLocked(userID, codeID, windowID, fx)
	if deleted {
		b.sendLoginsLocked(userID, codeID)
		fx.notify(userID)
	}
	b.release(fx)
}
