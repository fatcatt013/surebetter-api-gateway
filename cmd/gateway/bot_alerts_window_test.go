package main

import (
	"testing"
	"time"
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
