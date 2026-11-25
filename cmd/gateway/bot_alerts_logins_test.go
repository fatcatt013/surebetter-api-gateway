package main

import (
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// loginResult posts an agent's result to the alert engine, as the result route does.
func (r *alertRig) loginResult(window, result string) {
	r.t.Helper()
	r.b.mu.Lock()
	fx := &effects{}
	r.b.alertLoginResultLocked(r.uid, r.code.ID, window, window[:strings.LastIndexByte(window, '-')], result, "redacted", fx)
	r.b.release(fx)
}

func loggedIn(w botWindow, in bool) botWindow {
	w.LoggedIn = &in
	return w
}

func TestBotLoginAlerts_Table(t *testing.T) {
	r := newAlertRig(t)
	r.link()
	r.attach()
	want := map[string][2]string{
		"code":           {"Log in needed", "Tipsport 1 (Home PC) · code asked"},
		"captcha":        {"Log in needed", "Tipsport 2 (Home PC) · captcha"},
		"unexpected":     {"Log in needed", "Tipsport 3 (Home PC) · unexpected page"},
		"restricted":     {"Log in needed", "Betano 1 (Home PC) · access restricted · Betano logins stopped"},
		"limit":          {"Log in needed", "Tipsport 5 (Home PC) · logged out too often"},
		"wrong_password": {"Login refused", "Tipsport 6 (Home PC) · wrong password"},
		"blocked":        {"Login refused", "Tipsport 7 (Home PC) · account blocked"},
	}
	windows := map[string]string{"code": "tipsport-1", "captcha": "tipsport-2", "unexpected": "tipsport-3", "restricted": "betano-1",
		"limit": "tipsport-5", "wrong_password": "tipsport-6", "blocked": "tipsport-7"}
	for result, tw := range want {
		r.loginResult(windows[result], result)
		got := r.alerts()
		if len(got) != 1 || got[0].Kind != alertAttention || got[0].Severity != severityAttention || got[0].Title != tw[0] ||
			got[0].Detail != tw[1] || got[0].AgentID != r.code.ID || got[0].WindowID != windows[result] || got[0].OrderID != "" {
			t.Fatalf("%s: %+v", result, got)
		}
		jobs := r.out.take()
		if len(jobs) != 1 || !strings.Contains(jobs[0].Text, tw[0]+" · "+tw[1]+" · ") || !strings.Contains(jobs[0].Text, "https://sb.example/app/bots/fleet") {
			t.Fatalf("%s telegram: %+v", result, jobs)
		}
	}
	r.loginResult("tipsport-9", "ok")
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("ok alerted: %+v", got)
	}
	// One open login alert per window: another result raises nothing.
	r.loginResult("tipsport-1", "captcha")
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("a second alert for the window: %+v", got)
	}
	if rows := r.rows(); len(rows) != 7 {
		t.Fatalf("rows: %d", len(rows))
	}
}

func TestBotLoginAlerts_Resolution(t *testing.T) {
	r := newAlertRig(t)
	r.attach()
	t1, t2, t3 := alertWin("tipsport", 1, "logged_out", "bot", ""), alertWin("tipsport", 2, "logged_out", "bot", ""), alertWin("tipsport", 3, "logged_out", "bot", "")
	r.status(t1, t2, t3)
	for _, w := range []string{"tipsport-1", "tipsport-2", "tipsport-3"} {
		r.loginResult(w, "code")
	}
	if got := r.alerts(); len(got) != 3 {
		t.Fatalf("raised: %+v", got)
	}
	resolved := func(what string, ids ...string) {
		t.Helper()
		got := r.alerts()
		if len(got) != len(ids) {
			t.Fatalf("%s: %+v", what, got)
		}
		for i, a := range got {
			if a.WindowID != ids[i] || a.ResolvedAt == 0 {
				t.Fatalf("%s: %+v", what, got)
			}
		}
	}
	// loggedIn false and null resolve nothing; true does.
	r.status(loggedIn(t1, false), t2, t3)
	resolved("loggedIn false")
	r.status(loggedIn(alertWin("tipsport", 1, "idle", "bot", ""), true), t2, t3)
	resolved("loggedIn true", "tipsport-1")
	// Saved again (PUT) or deleted: loginChanged.
	r.b.loginChanged(r.uid, r.code.ID, "tipsport-2", false)
	resolved("login saved", "tipsport-2")
	// The window gone from the status.
	r.status(loggedIn(alertWin("tipsport", 1, "idle", "bot", ""), true), t2)
	resolved("window gone", "tipsport-3")
	// A new result alerts again once the earlier alert resolved.
	r.loginResult("tipsport-2", "wrong_password")
	if got := r.alerts(); len(got) != 1 || got[0].Title != "Login refused" {
		t.Fatalf("again: %+v", got)
	}
	// Dismissed: resolved quietly (row only), and the next result alerts again.
	open := r.rows()[0]
	row, _, err := r.svc.DismissBotAlert(r.uid, open.ID, r.clk.now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	r.b.alertsDismissed(r.uid, []accounts.BotAlertRow{row})
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("dismissed came back: %+v", got)
	}
	var resolvedAt int64
	r.db.QueryRow(`SELECT resolved_at FROM bot_alerts WHERE id = ?`, open.ID).Scan(&resolvedAt)
	if resolvedAt == 0 {
		t.Fatal("dismissed login alert not resolved in the engine")
	}
	r.loginResult("tipsport-2", "blocked")
	if got := r.alerts(); len(got) != 1 || got[0].Title != "Login refused" {
		t.Fatalf("after dismiss: %+v", got)
	}
	// The agent revoked.
	if err := r.svc.RevokeExtensionCode(r.uid, r.code.ID); err != nil {
		t.Fatal(err)
	}
	resolved("revoked", "tipsport-2")
}

func TestBotLoginAlerts_SuppressTheLoggedOutWindowAlert(t *testing.T) {
	r := newAlertRig(t)
	r.attach()
	// A login alert open before the window reports logged_out: no window alert.
	r.status(alertWin("tipsport", 1, "idle", "bot", ""), alertWin("tipsport", 2, "idle", "bot", ""))
	r.loginResult("tipsport-1", "code")
	r.alerts()
	r.status(alertWin("tipsport", 1, "logged_out", "bot", "Login: code asked"), alertWin("tipsport", 2, "logged_out", "bot", ""))
	// The Logged out grace already running when the login alert comes: it ends.
	r.loginResult("tipsport-2", "captcha")
	r.alerts()
	r.clk.advance(2 * windowAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("window alert despite the login alert: %+v", got)
	}
	// A down grace running when a login result comes goes on.
	r.status(alertWin("tipsport", 3, "down", "bot", ""), alertWin("tipsport", 1, "logged_out", "bot", ""), alertWin("tipsport", 2, "logged_out", "bot", ""))
	r.clk.advance(30 * time.Second)
	r.loginResult("tipsport-3", "code")
	r.alerts()
	r.clk.advance(30 * time.Second)
	r.b.tick()
	if got := r.alerts(); len(got) != 1 || got[0].WindowID != "tipsport-3" || got[0].Title != "Window down" {
		t.Fatalf("down grace kept: %+v", got)
	}
	// Another problem (down) still alerts, while its login alert is open.
	r.status(alertWin("tipsport", 1, "down", "bot", ""), alertWin("tipsport", 2, "logged_out", "bot", ""), alertWin("tipsport", 3, "down", "bot", ""))
	r.clk.advance(windowAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 1 || got[0].Kind != alertWindow || got[0].Title != "Window down" {
		t.Fatalf("down: %+v", got)
	}
	// logging_in is never a window problem.
	r.status(alertWin("tipsport", 1, "logging_in", "bot", "Logging in"), alertWin("tipsport", 3, "logging_in", "bot", "Login queued"),
		alertWin("tipsport", 2, "logged_out", "bot", ""))
	r.clk.advance(2 * windowAlertGrace)
	r.b.tick()
	for _, a := range r.alerts() {
		if a.ResolvedAt == 0 {
			t.Fatalf("logging_in alerted: %+v", a)
		}
	}
}

func TestBotLoginAlerts_ReadBackAtStart(t *testing.T) {
	r := newAlertRig(t)
	if err := r.svc.InsertBotAlert(accounts.BotAlertRow{ID: "0123456789abcdef", UserID: r.uid, Kind: alertAttention, Severity: severityAttention,
		AgentCodeID: r.code.ID, WindowID: "tipsport-1", Title: "Log in needed", Detail: "Tipsport 1 (Home PC) · code asked", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	// What main does at start: the window and agent alerts resolve, the login alerts are read back.
	if _, err := r.svc.ResolveOpenBotAlerts([]string{alertWindow, alertAgent}, 2); err != nil {
		t.Fatal(err)
	}
	r.b.loadLoginAlerts()
	r.attach()
	r.status(alertWin("tipsport", 1, "logged_out", "bot", ""))
	r.loginResult("tipsport-1", "captcha")
	r.clk.advance(2 * windowAlertGrace)
	r.b.tick()
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("after the read-back: %+v", got)
	}
	r.status(loggedIn(alertWin("tipsport", 1, "idle", "bot", ""), true))
	if got := r.alerts(); len(got) != 1 || got[0].ID != "0123456789abcdef" || got[0].ResolvedAt == 0 {
		t.Fatalf("read-back alert resolution: %+v", got)
	}
}

func TestBotLoginAlerts_DisabledUserResolvesAndDeletes(t *testing.T) {
	r := newAlertRig(t)
	r.b.logins = mustCipher(t, testLoginKey(0), "")
	r.attach()
	r.status(alertWin("tipsport", 1, "logged_out", "bot", ""))
	secret, keyID, tag, _ := r.b.logins.seal(r.uid, r.code.ID, "tipsport-1", "u", "p")
	r.svc.SaveBotLogin(accounts.BotLoginRow{UserID: r.uid, CodeID: r.code.ID, WindowID: "tipsport-1", Bookmaker: "tipsport", Secret: secret, KeyID: keyID, UserTag: tag})
	r.loginResult("tipsport-1", "code")
	r.alerts()
	admin, err := r.svc.CreateInvite(0, accounts.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	ap, _, err := r.svc.Register("10.0.0.1", admin.Code, "admin@example.com", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	r.b.reg.remove(r.app) // a channel, not a socket: CloseUser would close it
	if err := r.svc.SetDisabled(ap.UserID, r.uid, true); err != nil {
		t.Fatal(err)
	}
	if rows := r.rows(); len(rows) != 1 || rows[0].ResolvedAt == 0 {
		t.Fatalf("disabled: %+v", rows)
	}
	if l, _ := r.svc.ListBotLoginsOfCode(r.code.ID); len(l) != 0 {
		t.Fatalf("logins kept: %+v", l)
	}
}
