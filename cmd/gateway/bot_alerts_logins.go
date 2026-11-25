package main

import (
	"slices"
	"strings"

	"github.com/rs/zerolog/log"
)

// alertLogin is the scope kind of a window's login alert (contract §5.4). The alert itself is stored as kind
// attention with agentId and windowId set and no orderId; at most one is open per (agent, window).
const alertLogin = "login"

// loginAlertTexts: the results that raise a login alert, their title and what the detail says.
var loginAlertTexts = map[string]struct{ title, what string }{
	"code":           {"Log in needed", "code asked"},
	"captcha":        {"Log in needed", "captcha"},
	"unexpected":     {"Log in needed", "unexpected page"},
	"restricted":     {"Log in needed", "access restricted"},
	"limit":          {"Log in needed", "logged out too often"},
	"wrong_password": {"Login refused", "wrong password"},
	"blocked":        {"Login refused", "account blocked"},
}

func loginScope(userID, codeID int64, windowID string) alertScope {
	return alertScope{kind: alertLogin, userID: userID, codeID: codeID, windowID: windowID}
}

// loginWindowLabel is a window id as alerts name it: tipsport-2 → "Tipsport 2".
func loginWindowLabel(windowID string) string {
	i := strings.LastIndexByte(windowID, '-')
	if i < 0 {
		return bookLabel(windowID)
	}
	return bookLabel(windowID[:i]) + " " + windowID[i+1:]
}

// hasLoginAlertLocked: the window has an open login alert. b.mu must be held.
func (b *botHub) hasLoginAlertLocked(userID, codeID int64, windowID string) bool {
	return b.alerts != nil && b.alerts.open[loginScope(userID, codeID, windowID)] != nil
}

// alertLoginResultLocked raises the window's login alert for a result that needs the user (loginAlertTexts), unless
// one is open already. detail is the agent's (redacted) detail: only this log line carries it. b.mu must be held.
func (b *botHub) alertLoginResultLocked(userID, codeID int64, windowID, book, result, detail string, fx *effects) {
	t, ok := loginAlertTexts[result]
	if b.alerts == nil || !ok || b.alerts.revoked[codeID] {
		return
	}
	sc := loginScope(userID, codeID, windowID)
	if b.alerts.open[sc] != nil {
		return
	}
	d := joinDetail(loginWindowLabel(windowID)+" ("+b.agentNameLocked(codeID)+")", t.what)
	if result == "restricted" {
		d = joinDetail(d, bookLabel(book)+" logins stopped")
	}
	// A Logged out grace of the window ends: this alert covers it (a down or blocked grace goes on).
	ws := alertScope{kind: alertWindow, userID: userID, codeID: codeID, windowID: windowID}
	if pa := b.alerts.pending[ws]; pa != nil && pa.state == "logged_out" {
		delete(b.alerts.pending, ws)
	}
	b.raiseLocked(userID, botAlert{Kind: alertAttention, Severity: severityAttention, AgentID: codeID, WindowID: windowID,
		Title: t.title, Detail: d}, &sc, fx)
	log.Info().Int64("user", userID).Int64("code", codeID).Str("window", windowID).Str("result", result).Str("detail", detail).
		Msg("bot login alert")
}

// resolveLoginAlertLocked resolves the window's open login alert, if any. b.mu must be held.
func (b *botHub) resolveLoginAlertLocked(userID, codeID int64, windowID string, fx *effects) {
	if b.alerts != nil {
		b.resolveLocked(loginScope(userID, codeID, windowID), fx)
	}
}

// alertLoginsLocked follows codeID's last status for the login alerts: a window that reports loggedIn true, or that is
// gone from the status, resolves its open login alert. b.mu must be held.
func (b *botHub) alertLoginsLocked(userID, codeID int64, p *botPresence, fx *effects) {
	if b.alerts == nil {
		return
	}
	seen := map[string]bool{}
	for _, w := range p.windows {
		seen[w.ID] = true
		if w.LoggedIn != nil && *w.LoggedIn {
			b.resolveLoginAlertLocked(userID, codeID, w.ID, fx)
		}
	}
	var gone []alertScope
	for sc := range b.alerts.open {
		if sc.kind == alertLogin && sc.userID == userID && sc.codeID == codeID && !seen[sc.windowID] {
			gone = append(gone, sc)
		}
	}
	slices.SortFunc(gone, compareScopes)
	for _, sc := range gone {
		b.resolveLocked(sc, fx)
	}
}

// loadLoginAlerts reads back the open login alerts the previous process left (they are not resolved at start: the
// agent does not post its result again), so each keeps its window's scope. Call it at start, without b.mu.
func (b *botHub) loadLoginAlerts() {
	if b.acc == nil {
		return
	}
	rows, err := b.acc.OpenBotLoginAlerts()
	if err != nil {
		log.Error().Err(err).Msg("bot alerts: reading the open login alerts failed")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.alerts == nil {
		return
	}
	for _, row := range rows {
		b.alerts.open[loginScope(row.UserID, row.AgentCodeID, row.WindowID)] = &openAlert{alert: alertOfRow(row), stored: true}
	}
	if len(rows) > 0 {
		log.Info().Int("open", len(rows)).Msg("bot login alerts read back")
	}
}
