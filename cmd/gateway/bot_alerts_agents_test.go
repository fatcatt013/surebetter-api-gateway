package main

import (
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// 3 min after a start, an agent seen in the last 24 h that did not come back gets one offline alert.
func TestBotAlerts_AgentsNotBackAfterAStart(t *testing.T) {
	r := newAlertRig(t)
	r.b.alerts.started, r.b.alerts.startChecked = r.clk.now(), false
	vps, err := r.svc.CreateCode(r.uid, "VPS Praha", accounts.KindBot)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.TouchBotAgent(r.uid, vps.ID); err != nil { // seen before the restart
		t.Fatal(err)
	}
	r.attach() // Home PC came back
	r.clk.advance(alertStartQuiet - time.Second)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("within the quiet period: %+v", got)
	}
	r.clk.advance(time.Second)
	r.b.tick()
	got := r.alerts()
	if len(got) != 1 || got[0].Title != titleAgentOffline || got[0].AgentID != vps.ID || got[0].Detail != "VPS Praha" ||
		got[0].Severity != severityProblem {
		t.Fatalf("start check: %+v (one alert, for the VPS only)", got)
	}
	r.clk.advance(time.Hour)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("the start check ran twice: %+v", got)
	}
	r.b.attach(&Client{principal: accounts.Principal{UserID: r.uid, CodeID: vps.ID, CodeName: "VPS Praha", CodeKind: accounts.KindBot},
		send: make(chan []byte, 256), bot: true})
	if got := r.alerts(); len(got) != 2 || got[0].ResolvedAt == 0 || got[1].Title != titleAgentBack || got[1].Detail != "VPS Praha" {
		t.Fatalf("back: %+v", got)
	}
}

func TestBotAlerts_AgentOfflineAfter2MinThenBack(t *testing.T) {
	r := newAlertRig(t)
	r.link()
	c := r.attach()
	r.b.detach(c)
	r.clk.advance(agentAlertGrace - time.Second)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("before 2 min: %+v", got)
	}
	r.clk.advance(time.Second)
	r.b.tick()
	off := r.alerts()
	if len(off) != 1 || off[0].Kind != alertAgent || off[0].Severity != severityProblem || off[0].Title != titleAgentOffline ||
		off[0].Detail != "Home PC" || off[0].AgentID != r.code.ID {
		t.Fatalf("offline: %+v", off)
	}
	if jobs := r.out.take(); len(jobs) != 0 {
		t.Fatalf("telegram (agent default off): %+v", jobs)
	}
	r.clk.advance(time.Hour)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("offline alerted twice: %+v", got)
	}
	r.attach()
	got := r.alerts()
	if len(got) != 2 || got[0].ID != off[0].ID || got[0].ResolvedAt == 0 || got[1].Title != titleAgentBack || got[1].Severity != severityInfo ||
		got[1].Detail != "Home PC" {
		t.Fatalf("back: %+v", got)
	}
}

func TestBotAlerts_AgentBackWithin2MinSaysNothing(t *testing.T) {
	r := newAlertRig(t)
	c := r.attach()
	r.b.detach(c)
	r.clk.advance(time.Minute)
	r.attach()
	r.clk.advance(time.Hour)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("alerted: %+v", got)
	}
}

func TestBotAlerts_AgentQuietAfterTheGatewayStart(t *testing.T) {
	r := newAlertRig(t)
	r.b.alerts.started = r.clk.now()
	c := r.attach()
	r.clk.advance(10 * time.Second)
	r.b.detach(c)
	r.clk.advance(agentAlertGrace) // 2 min 10 s after the start
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("within the quiet period: %+v", got)
	}
	r.clk.advance(alertStartQuiet - agentAlertGrace - 10*time.Second) // 3 min after the start, still offline
	r.b.tick()
	if got := r.alerts(); len(got) != 1 || got[0].Title != titleAgentOffline {
		t.Fatalf("after the quiet period: %+v", got)
	}

	q := newAlertRig(t)
	q.b.alerts.started = q.clk.now()
	c = q.attach()
	q.b.detach(c)
	q.clk.advance(2*time.Minute + 30*time.Second)
	q.b.tick()
	q.attach() // back within the quiet period
	q.clk.advance(time.Hour)
	q.b.tick()
	if got := q.alerts(); len(got) != 0 {
		t.Fatalf("an agent back in the quiet period alerted: %+v", got)
	}
}

func TestBotAlerts_AgentOfflineCancelsItsWindowGraces(t *testing.T) {
	r := newAlertRig(t)
	c := r.attach()
	r.status(alertWin("tipsport", 1, "down", "bot", ""))
	r.b.detach(c)
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("a window of an offline agent alerted: %+v", got)
	}
}

// The start check runs again on the next tick when the agents could not be read.
func TestBotAlerts_StartCheckRetriesAfterAReadError(t *testing.T) {
	r := newAlertRig(t)
	r.b.alerts.started, r.b.alerts.startChecked = r.clk.now(), false
	r.db.Close()
	r.clk.advance(alertStartQuiet)
	r.b.tick()
	r.b.mu.Lock()
	checked := r.b.alerts.startChecked
	r.b.mu.Unlock()
	if checked {
		t.Fatal("the failed start check counted as done")
	}
}

// A revoked agent is forgotten: its open alerts resolve, its graces end, and its socket closing starts no offline
// grace.
func TestBotAlerts_RevokedAgentIsForgotten(t *testing.T) {
	r := newAlertRig(t)
	c := r.attach()
	r.status(alertWin("tipsport", 1, "down", "bot", ""), alertWin("betano", 1, "blocked", "bot", ""))
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 2 {
		t.Fatalf("window alerts: %+v", got)
	}
	r.status(alertWin("tipsport", 1, "down", "bot", ""), alertWin("betano", 1, "idle", "bot", ""))
	r.alerts()
	if err := r.svc.RevokeExtensionCode(r.uid, r.code.ID); err != nil {
		t.Fatal(err)
	}
	r.b.detach(c)
	if got := r.alerts(); len(got) != 1 || got[0].WindowID != "tipsport-1" || got[0].ResolvedAt == 0 {
		t.Fatalf("resolved at revoke: %+v", got)
	}
	r.clk.advance(time.Hour)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("a revoked agent alerted: %+v", got)
	}
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	if len(r.b.alerts.open) != 0 || len(r.b.alerts.pending) != 0 || len(r.b.alerts.pauseTil) != 0 || r.b.alerts.names[r.code.ID] != "" {
		t.Fatalf("engine state kept: open %v pending %v pause %v names %v", r.b.alerts.open, r.b.alerts.pending, r.b.alerts.pauseTil, r.b.alerts.names)
	}
}

// After a failed read the start check waits 30 s before it tries again.
func TestBotAlerts_StartCheckBacksOff(t *testing.T) {
	r := newAlertRig(t)
	r.b.alerts.started, r.b.alerts.startChecked = r.clk.now(), false
	r.db.Close()
	r.clk.advance(alertStartQuiet)
	r.b.tick()
	retryAt := func() time.Time {
		r.b.mu.Lock()
		defer r.b.mu.Unlock()
		return r.b.alerts.startRetryAt
	}
	first := retryAt()
	if !first.Equal(r.clk.now().Add(startCheckRetry)) {
		t.Fatalf("retry at %v, want %v", first, r.clk.now().Add(startCheckRetry))
	}
	r.clk.advance(startCheckRetry - time.Second)
	r.b.tick()
	if !retryAt().Equal(first) {
		t.Fatal("tried again before 30 s")
	}
	r.clk.advance(time.Second)
	r.b.tick()
	if !retryAt().Equal(r.clk.now().Add(startCheckRetry)) {
		t.Fatal("did not try again after 30 s")
	}
}

// Disabling a user (connRegistry.CloseUser) drops their agents' graces and open alerts; their sockets closing starts no
// offline grace. Signing everyone out (a password reset: same CloseUser, user still enabled) changes nothing; an agent
// of a re-enabled user alerts again.
func TestBotAlerts_DisabledUserIsForgotten(t *testing.T) {
	r := newAlertRig(t)
	c := r.attach()
	r.status(alertWin("tipsport", 1, "down", "bot", ""))
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	r.status(alertWin("tipsport", 1, "down", "bot", ""), alertWin("betano", 1, "down", "bot", "")) // betano-1: a grace
	r.alerts()
	r.b.alertsUserClosed(r.uid) // a password reset
	r.b.mu.Lock()
	open, pending := len(r.b.alerts.open), len(r.b.alerts.pending)
	r.b.mu.Unlock()
	if open != 1 || pending != 1 {
		t.Fatalf("enabled user: open %d pending %d, want 1 and 1", open, pending)
	}
	if _, err := r.db.Exec(`UPDATE users SET disabled_at = 1 WHERE id = ?`, r.uid); err != nil {
		t.Fatal(err)
	}
	r.b.alertsUserClosed(r.uid)
	if got := r.alerts(); len(got) != 1 || got[0].WindowID != "tipsport-1" || got[0].ResolvedAt == 0 {
		t.Fatalf("resolved: %+v", got)
	}
	r.b.detach(c)
	r.clk.advance(time.Hour)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("a disabled user's agent alerted: %+v", got)
	}
	r.b.mu.Lock()
	if len(r.b.alerts.open) != 0 || len(r.b.alerts.pending) != 0 || r.b.alerts.names[r.code.ID] != "" {
		t.Fatalf("engine state kept: open %v pending %v names %v", r.b.alerts.open, r.b.alerts.pending, r.b.alerts.names)
	}
	r.b.mu.Unlock()
	if _, err := r.db.Exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, r.uid); err != nil {
		t.Fatal(err)
	}
	c = r.attach() // re-enabled: its agent connects again
	r.b.detach(c)
	r.clk.advance(agentAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 1 || got[0].Title != titleAgentOffline {
		t.Fatalf("re-enabled: %+v", got)
	}
}

func TestConnRegistry_CloseUserAndCodeTellTheHooks(t *testing.T) {
	reg := newConnRegistry()
	var users, codes []int64
	reg.userClosed = func(id int64) { users = append(users, id) }
	reg.codeRevoked = func(id int64) { codes = append(codes, id) }
	reg.CloseUser(5)
	reg.CloseCode(7)
	reg.CloseUser(0)
	if len(users) != 1 || users[0] != 5 || len(codes) != 1 || codes[0] != 7 {
		t.Fatalf("users %v codes %v", users, codes)
	}
}
