package main

import (
	"time"

	"github.com/rs/zerolog/log"
)

// agentsSeenWithin: the start check looks at the agents seen this long before it.
const agentsSeenWithin = 24 * time.Hour

// alertAttachLocked: codeID's bot socket opened (name = its code's name). It cancels the agent's offline grace; when
// its offline alert had fired, that alert resolves and an info "Agent back" alert is raised. b.mu must be held.
func (b *botHub) alertAttachLocked(userID, codeID int64, name string, fx *effects) {
	if b.alerts == nil || b.alerts.revoked[codeID] {
		return
	}
	if name != "" {
		b.alerts.names[codeID] = name
	}
	sc := alertScope{kind: alertAgent, userID: userID, codeID: codeID}
	delete(b.alerts.pending, sc)
	if b.alerts.open[sc] == nil {
		return
	}
	b.resolveLocked(sc, fx)
	b.raiseLocked(userID, botAlert{Kind: alertAgent, Severity: severityInfo, AgentID: codeID, Title: titleAgentBack,
		Detail: b.agentNameLocked(codeID)}, nil, fx)
}

// alertDetachLocked: codeID's bot socket closed. Its windows' graces end (the agent alert covers them) and, unless its
// offline alert is already open, a 2 min grace starts. b.mu must be held.
func (b *botHub) alertDetachLocked(userID, codeID int64) {
	if b.alerts == nil || b.alerts.revoked[codeID] {
		return
	}
	for sc := range b.alerts.pending {
		if sc.kind == alertWindow && sc.userID == userID && sc.codeID == codeID {
			delete(b.alerts.pending, sc)
		}
	}
	sc := alertScope{kind: alertAgent, userID: userID, codeID: codeID}
	if b.alerts.open[sc] == nil {
		b.alerts.pending[sc] = &pendingAlert{due: b.now().Add(agentAlertGrace)}
	}
}

// checkAgentsAfterStart is the start check, run by the first tick alertStartQuiet after the gateway start (again on
// the next tick when the agents could not be read): every
// agent seen in the last 24 h that is not connected (and has no offline grace of its own running) gets one "Agent
// offline" alert, which resolves with "Agent back" when it reconnects. A restart sees no detach, so without it an agent
// that never came back would never alert. Call it without b.mu (it reads the registry).
func (b *botHub) checkAgentsAfterStart() {
	b.mu.Lock()
	if b.alerts == nil || b.alerts.startChecked || b.now().Before(b.alerts.started.Add(alertStartQuiet)) {
		b.mu.Unlock()
		return
	}
	b.alerts.startChecked = true
	since := b.now().Add(-agentsSeenWithin).UnixMilli()
	b.mu.Unlock()
	if b.acc == nil {
		return
	}
	recent, err := b.acc.RecentBotAgents(since)
	if err != nil {
		log.Error().Err(err).Msg("bot alerts: the start check could not read the agents; trying again")
		b.mu.Lock()
		b.alerts.startChecked = false
		b.mu.Unlock()
		return
	}
	for _, a := range recent {
		b.loadAlertSettings(a.UserID)
	}
	b.mu.Lock()
	fx := &effects{}
	for _, a := range recent {
		sc := alertScope{kind: alertAgent, userID: a.UserID, codeID: a.CodeID}
		if b.presenceLocked(a.UserID, a.CodeID) != nil || b.alerts.pending[sc] != nil || b.alerts.open[sc] != nil ||
			b.alerts.revoked[a.CodeID] {
			continue
		}
		if b.alerts.names[a.CodeID] == "" {
			b.alerts.names[a.CodeID] = a.Name
		}
		b.raiseLocked(a.UserID, botAlert{Kind: alertAgent, Severity: severityProblem, AgentID: a.CodeID, Title: titleAgentOffline,
			Detail: b.agentNameLocked(a.CodeID)}, &sc, fx)
	}
	b.release(fx)
}
