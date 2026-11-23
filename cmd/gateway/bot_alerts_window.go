package main

import (
	"cmp"
	"slices"
	"strconv"
)

// windowProblemTitles: the window states that alert, and their titles.
var windowProblemTitles = map[string]string{"logged_out": "Logged out", "blocked": "Blocked", "down": "Window down"}

// windowProblem is the state a window alert is about, or "": not a problem (idle, busy, off), or a window the user
// stopped or put in Login mode.
func windowProblem(w botWindow) string {
	if w.Mode == "stopped" || w.Mode == "login" {
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
// grace. b.mu must be held.
func (b *botHub) alertWindowsLocked(userID, codeID int64, p *botPresence, fx *effects) {
	if b.alerts == nil {
		return
	}
	now := b.now()
	seen := map[string]bool{}
	for _, w := range p.windows {
		seen[w.ID] = true
		sc := alertScope{kind: alertWindow, userID: userID, codeID: codeID, windowID: w.ID}
		if st := windowProblem(w); st != "" {
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
	for sc := range b.alerts.pending {
		if sc.kind == alertWindow && sc.userID == userID && sc.codeID == codeID && !seen[sc.windowID] {
			delete(b.alerts.pending, sc)
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
	var due []alertScope
	for sc, pa := range b.alerts.pending {
		if !now.Before(pa.due) {
			due = append(due, sc)
		}
	}
	slices.SortFunc(due, compareScopes)
	for _, sc := range due {
		pa := b.alerts.pending[sc]
		switch sc.kind {
		case alertWindow:
			delete(b.alerts.pending, sc)
			b.raiseLocked(sc.userID, botAlert{Kind: alertWindow, Severity: severityProblem, AgentID: sc.codeID, WindowID: sc.windowID,
				Title: windowProblemTitles[pa.state], Detail: joinDetail(pa.label+" ("+b.agentNameLocked(sc.codeID)+")", pa.reason)}, &sc, fx)
		}
	}
}
