package main

import (
	"cmp"
	"slices"
	"strconv"
)

// windowProblemTitles: the window states that alert, and their titles.
var windowProblemTitles = map[string]string{"logged_out": "Logged out", "blocked": "Blocked", "down": "Window down"}

// windowProblem is the state a window alert is about, or "": not a problem (idle, busy, off, logging_in), a window the
// user stopped or put in Login mode, or logged_out while the window has an open login alert (loginAlert).
func windowProblem(w botWindow, loginAlert bool) string {
	if w.Mode == "stopped" || w.Mode == "login" || (w.State == "logged_out" && loginAlert) {
		return ""
	}
	if _, ok := windowProblemTitles[w.State]; ok {
		return w.State
	}
	return ""
}

// windowText is a window as alerts name it: "Tipsport 1".
func windowText(w botWindow) string {
	return bookLabel(w.Bookmaker) + " " + strconv.Itoa(w.Number)
}

// alertWindowsLocked follows the windows of codeID's last status (p.windows): a problem starts a 60 s grace (unless
// the window has an open alert or is in its 10 min pause after one); a window no longer a problem cancels its grace,
// and back to idle or busy resolves its open alert and starts the pause. A window gone from the status cancels its
// grace, resolves its open alert and forgets its pause. b.mu must be held.
func (b *botHub) alertWindowsLocked(userID, codeID int64, p *botPresence, fx *effects) {
	if b.alerts == nil {
		return
	}
	now := b.now()
	seen := map[string]bool{}
	for _, w := range p.windows {
		seen[w.ID] = true
		sc := alertScope{kind: alertWindow, userID: userID, codeID: codeID, windowID: w.ID}
		if st := windowProblem(w, b.hasLoginAlertLocked(userID, codeID, w.ID)); st != "" {
			if pa := b.alerts.pending[sc]; pa != nil {
				pa.state, pa.reason, pa.label = st, w.Reason, windowText(w)
				continue
			}
			if b.alerts.open[sc] != nil || now.Before(b.alerts.pauseTil[sc]) {
				continue
			}
			b.alerts.pending[sc] = &pendingAlert{due: now.Add(windowAlertGrace), state: st, reason: w.Reason, label: windowText(w)}
			continue
		}
		delete(b.alerts.pending, sc)
		if (w.State == "idle" || w.State == "busy") && b.alerts.open[sc] != nil {
			b.resolveLocked(sc, fx)
			b.alerts.pauseTil[sc] = now.Add(windowRealertPause)
		}
	}
	gone := func(sc alertScope) bool {
		return sc.kind == alertWindow && sc.userID == userID && sc.codeID == codeID && !seen[sc.windowID]
	}
	for sc := range b.alerts.pending {
		if gone(sc) {
			delete(b.alerts.pending, sc)
		}
	}
	var vanished []alertScope
	for sc := range b.alerts.open {
		if gone(sc) {
			vanished = append(vanished, sc)
		}
	}
	slices.SortFunc(vanished, compareScopes)
	for _, sc := range vanished {
		b.resolveLocked(sc, fx)
	}
	for sc := range b.alerts.pauseTil {
		if gone(sc) {
			delete(b.alerts.pauseTil, sc)
		}
	}
}

func compareScopes(x, y alertScope) int {
	return cmp.Or(cmp.Compare(x.userID, y.userID), cmp.Compare(x.codeID, y.codeID), cmp.Compare(x.kind, y.kind), cmp.Compare(x.windowID, y.windowID))
}

// fireAlertsLocked raises the alerts whose grace is over (the scheduler's 1 s tick), in a fixed order. b.mu must be held.
func (b *botHub) fireAlertsLocked(fx *effects) {
	if b.alerts == nil {
		return
	}
	now := b.now()
	for sc, til := range b.alerts.pauseTil {
		if !now.Before(til) {
			delete(b.alerts.pauseTil, sc)
		}
	}
	var due []alertScope
	for sc, pa := range b.alerts.pending {
		if !now.Before(pa.due) {
			due = append(due, sc)
		}
	}
	slices.SortFunc(due, compareScopes)
	quietEnd := b.alerts.started.Add(alertStartQuiet)
	for _, sc := range due {
		pa := b.alerts.pending[sc]
		switch sc.kind {
		case alertAgent:
			if now.Before(quietEnd) { // every agent reconnects after a start: wait for the end of the quiet period
				pa.due = quietEnd
				continue
			}
			delete(b.alerts.pending, sc)
			b.raiseLocked(sc.userID, botAlert{Kind: alertAgent, Severity: severityProblem, AgentID: sc.codeID, Title: titleAgentOffline,
				Detail: b.agentNameLocked(sc.codeID)}, &sc, fx)
		case alertWindow:
			delete(b.alerts.pending, sc)
			if pa.state == "logged_out" && b.hasLoginAlertLocked(sc.userID, sc.codeID, sc.windowID) {
				continue // the window's login alert covers it
			}
			b.raiseLocked(sc.userID, botAlert{Kind: alertWindow, Severity: severityProblem, AgentID: sc.codeID, WindowID: sc.windowID,
				Title: windowProblemTitles[pa.state], Detail: joinDetail(pa.label+" ("+b.agentNameLocked(sc.codeID)+")", pa.reason)}, &sc, fx)
		}
	}
}
