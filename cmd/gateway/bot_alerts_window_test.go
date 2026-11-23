package main

import (
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

func TestBotAlerts_WindowProblemAfter60s(t *testing.T) {
	r := newAlertRig(t)
	r.link()
	r.attach()
	r.status(alertWin("tipsport", 1, "idle", "bot", ""))
	r.status(alertWin("tipsport", 1, "logged_out", "bot", "Session expired"))
	r.clk.advance(windowAlertGrace - time.Second)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("before 60 s: %+v", got)
	}
	r.clk.advance(time.Second)
	r.b.tick()
	got := r.alerts()
	if len(got) != 1 || got[0].Kind != alertWindow || got[0].Severity != severityProblem || got[0].Title != "Logged out" ||
		got[0].Detail != "Tipsport 1 (Home PC) · Session expired" || got[0].WindowID != "tipsport-1" || got[0].AgentID != r.code.ID {
		t.Fatalf("alert: %+v", got)
	}
	if jobs := r.out.take(); len(jobs) != 1 {
		t.Fatalf("telegram: %+v", jobs)
	}
	// One open alert per window: the problem goes on, nothing new.
	r.status(alertWin("tipsport", 1, "blocked", "bot", ""))
	r.clk.advance(2 * windowAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("a second alert for the same window: %+v", got)
	}
}

func TestBotAlerts_WindowBackBeforeTheGraceNeverAlerts(t *testing.T) {
	r := newAlertRig(t)
	r.attach()
	r.status(alertWin("tipsport", 1, "down", "bot", ""))
	r.clk.advance(30 * time.Second)
	r.status(alertWin("tipsport", 1, "idle", "bot", ""))
	r.clk.advance(time.Minute)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("alerted: %+v", got)
	}
}

func TestBotAlerts_WindowExclusions(t *testing.T) {
	for name, w := range map[string]botWindow{
		"stopped": alertWin("tipsport", 1, "down", "stopped", ""),
		"login":   alertWin("tipsport", 1, "logged_out", "login", ""),
		"off":     alertWin("tipsport", 1, "off", "bot", "sleep"),
		"busy":    alertWin("tipsport", 1, "busy", "bot", ""),
		"no mode": alertWin("tipsport", 1, "idle", "", ""),
	} {
		r := newAlertRig(t)
		r.attach()
		r.status(w)
		r.clk.advance(2 * windowAlertGrace)
		r.b.tick()
		if got := r.alerts(); len(got) != 0 {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	// An older agent without modes still alerts.
	r := newAlertRig(t)
	r.attach()
	r.status(alertWin("tipsport", 1, "down", "", ""))
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 1 || got[0].Title != "Window down" {
		t.Fatalf("no mode, down: %+v", got)
	}
}

func TestBotAlerts_WindowResolvesThenPauses10Min(t *testing.T) {
	r := newAlertRig(t)
	r.attach()
	r.status(alertWin("betano", 2, "blocked", "bot", ""))
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	raised := r.alerts()
	if len(raised) != 1 || raised[0].Title != "Blocked" || raised[0].Detail != "Betano 2 (Home PC)" {
		t.Fatalf("alert: %+v", raised)
	}
	r.clk.advance(time.Minute)
	r.status(alertWin("betano", 2, "idle", "bot", ""))
	got := r.alerts()
	if len(got) != 1 || got[0].ID != raised[0].ID || got[0].ResolvedAt != r.clk.now().UnixMilli() {
		t.Fatalf("resolution: %+v", got)
	}
	if jobs := r.out.take(); len(jobs) != 0 { // not linked anyway; a resolution never goes to Telegram
		t.Fatalf("telegram: %+v", jobs)
	}
	r.status(alertWin("betano", 2, "blocked", "bot", ""))
	r.clk.advance(windowRealertPause - time.Second)
	r.b.tick()
	r.status(alertWin("betano", 2, "blocked", "bot", ""))
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("re-alerted within 10 min: %+v", got)
	}
	r.clk.advance(time.Second) // the pause is over: the next status starts a new grace
	r.status(alertWin("betano", 2, "blocked", "bot", ""))
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 1 || got[0].ID == raised[0].ID {
		t.Fatalf("after the pause: %+v", got)
	}
}

func TestBotAlerts_WindowGoneFromTheStatusCancels(t *testing.T) {
	r := newAlertRig(t)
	r.attach()
	r.status(alertWin("tipsport", 1, "down", "bot", ""), alertWin("betano", 1, "idle", "bot", ""))
	r.status(alertWin("betano", 1, "idle", "bot", ""))
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("a removed window alerted: %+v", got)
	}
}

// A window alert dismissed in the app does not come back when it resolves: the row gets resolved_at, the app no
// message, and the 10 min pause still holds.
func TestBotAlerts_DismissedWindowAlertResolvesQuietly(t *testing.T) {
	r := newAlertRig(t)
	r.attach()
	r.status(alertWin("tipsport", 1, "down", "bot", ""))
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	raised := r.alerts()
	if len(raised) != 1 {
		t.Fatalf("alert: %+v", raised)
	}
	row, found, err := r.svc.DismissBotAlert(r.uid, raised[0].ID, r.clk.now().UnixMilli())
	if err != nil || !found {
		t.Fatalf("dismiss: %v %v", found, err)
	}
	r.b.alertsDismissed(r.uid, []accounts.BotAlertRow{row})
	r.clk.advance(time.Minute)
	r.status(alertWin("tipsport", 1, "idle", "bot", ""))
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("a dismissed alert came back: %+v", got)
	}
	again, _, _ := r.svc.DismissBotAlert(r.uid, raised[0].ID, 0)
	if again.ResolvedAt != r.clk.now().UnixMilli() || again.DismissedAt != row.DismissedAt {
		t.Fatalf("row: %+v", again)
	}
	r.status(alertWin("tipsport", 1, "down", "bot", ""))
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("re-alerted within the pause: %+v", got)
	}
}

// A window removed from the agent's status resolves its open alert; ended pauses are forgotten.
func TestBotAlerts_VanishedWindowResolvesAndPausesExpire(t *testing.T) {
	r := newAlertRig(t)
	r.attach()
	r.status(alertWin("tipsport", 1, "down", "bot", ""), alertWin("betano", 1, "blocked", "bot", ""))
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	raised := r.alerts()
	if len(raised) != 2 {
		t.Fatalf("alerts: %+v", raised)
	}
	r.status(alertWin("tipsport", 1, "idle", "bot", "")) // betano-1 removed
	got := r.alerts()
	if len(got) != 2 || got[0].ResolvedAt == 0 || got[1].ResolvedAt == 0 {
		t.Fatalf("resolutions: %+v", got)
	}
	r.b.mu.Lock()
	open, paused := len(r.b.alerts.open), len(r.b.alerts.pauseTil)
	r.b.mu.Unlock()
	if open != 0 || paused != 1 {
		t.Fatalf("open %d paused %d, want 0 and 1 (tipsport-1)", open, paused)
	}
	r.clk.advance(windowRealertPause)
	r.b.tick()
	r.b.mu.Lock()
	paused = len(r.b.alerts.pauseTil)
	r.b.mu.Unlock()
	if paused != 0 {
		t.Fatalf("an ended pause is kept: %d", paused)
	}
}
