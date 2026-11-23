package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// alertRig is a bot hub with the alert engine on, a real accounts DB, one user with one bot code ("Home PC"), the
// user's app socket (a channel), a settable clock and Telegram as a recording outbox.
type alertRig struct {
	t    *testing.T
	b    *botHub
	svc  *accounts.Service
	clk  *botClock
	uid  int64
	code accounts.NewExtCode
	app  *Client
	out  *fakeOutbox
	db   *sql.DB
}

func newAlertRig(t *testing.T) *alertRig {
	t.Helper()
	svc, uid, db := newAlertsAccountsDB(t)
	reg := newConnRegistry()
	svc.SetConnections(reg)
	b := newBotHub(reg, svc)
	// The real time: the agents' last seen (TouchBotAgent) is written with it, and the start check compares to it.
	clk := &botClock{t: time.Now().UTC().Truncate(time.Millisecond)}
	b.now = clk.now
	b.alerts = newAlertEngine(clk.now().Add(-time.Hour)) // long past the start's quiet period
	b.alerts.startChecked = true                         // the start check has its own test
	out := &fakeOutbox{}
	b.tg = &telegramAlerts{acc: svc, out: out, loc: prague, appURL: "https://sb.example"}
	reg.codeRevoked = b.alertsCodeRevoked // as main wires it
	reg.userClosed = b.alertsUserClosed
	code, err := svc.CreateCode(uid, "Home PC", accounts.KindBot)
	if err != nil {
		t.Fatal(err)
	}
	app := &Client{principal: accounts.Principal{UserID: uid}, send: make(chan []byte, 256), notify: true}
	reg.add(app)
	return &alertRig{t: t, b: b, svc: svc, clk: clk, uid: uid, code: code, app: app, out: out, db: db}
}

// link links the user's Telegram to chat 555 (the rig's APP_PUBLIC_URL is https://sb.example).
func (r *alertRig) link() {
	if err := r.svc.SetTelegramLink(r.uid, 555); err != nil {
		r.t.Fatal(err)
	}
}

// attach opens the code's bot socket (a channel).
func (r *alertRig) attach() *Client {
	c := &Client{principal: accounts.Principal{UserID: r.uid, CodeID: r.code.ID, CodeName: "Home PC", CodeKind: accounts.KindBot},
		send: make(chan []byte, 256), bot: true}
	r.b.attach(c)
	return c
}

// status posts a protocol-2 status with windows.
func (r *alertRig) status(windows ...botWindow) {
	r.t.Helper()
	if !r.b.setStatus(r.uid, r.code.ID, statusOf(botStatusIn{Proto: 2, Windows: windows})) {
		r.t.Fatal("status: no bot socket")
	}
}

func alertWin(book string, n int, state, mode, reason string) botWindow {
	return botWindow{ID: fmt.Sprintf("%s-%d", book, n), Bookmaker: book, Number: n, Device: "desktop", State: state, Mode: mode, Reason: reason}
}

// alerts drains the app socket and returns its alert messages (other messages are skipped).
func (r *alertRig) alerts() []botAlert {
	var out []botAlert
	for {
		select {
		case b := <-r.app.send:
			var m struct {
				Type  string   `json:"type"`
				Alert botAlert `json:"alert"`
			}
			if json.Unmarshal(b, &m) == nil && m.Type == "alert" {
				out = append(out, m.Alert)
			}
		default:
			return out
		}
	}
}

// raise raises a directly, as a hook would.
func (r *alertRig) raise(a botAlert, scope *alertScope) {
	r.b.mu.Lock()
	fx := &effects{}
	r.b.raiseLocked(r.uid, a, scope, fx)
	r.b.release(fx)
}

func (r *alertRig) rows() []accounts.BotAlertRow {
	rows, err := r.svc.ListBotAlerts(r.uid, 100)
	if err != nil {
		r.t.Fatal(err)
	}
	return rows
}

func TestBotAlerts_RaiseFollowsTheSwitches(t *testing.T) {
	r := newAlertRig(t)
	r.link()
	r.raise(botAlert{Kind: alertWindow, Severity: severityProblem, Title: "Logged out", Detail: "Tipsport 1 (Home PC)"}, nil)
	got := r.alerts()
	if len(got) != 1 || len(got[0].ID) != 16 || got[0].CreatedAt != r.clk.now().UnixMilli() || got[0].Title != "Logged out" {
		t.Fatalf("app: %+v", got)
	}
	if rows := r.rows(); len(rows) != 1 || rows[0].ID != got[0].ID {
		t.Fatalf("rows: %+v", rows)
	}
	if jobs := r.out.take(); len(jobs) != 1 || jobs[0].ChatID != 555 {
		t.Fatalf("telegram (window default on): %+v", jobs)
	}
	r.raise(botAlert{Kind: alertAgent, Severity: severityProblem, Title: titleAgentOffline, Detail: "Home PC"}, nil)
	if got := r.alerts(); len(got) != 1 {
		t.Fatalf("agent in the app: %+v", got)
	}
	if jobs := r.out.take(); len(jobs) != 0 {
		t.Fatalf("telegram (agent default off): %+v", jobs)
	}
	r.b.mu.Lock()
	r.b.alerts.settings[r.uid] = alertSettings{Window: alertChannels{App: false, Telegram: true}}
	r.b.mu.Unlock()
	r.raise(botAlert{Kind: alertWindow, Severity: severityProblem, Title: "Blocked"}, nil)
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("app off, yet: %+v", got)
	}
	if rows := r.rows(); len(rows) != 2 {
		t.Fatalf("app off stored a row: %+v", rows)
	}
	if jobs := r.out.take(); len(jobs) != 1 {
		t.Fatalf("telegram on: %+v", jobs)
	}
}

func TestBotAlerts_ResolveTellsTheAppOfStoredAlertsOnly(t *testing.T) {
	r := newAlertRig(t)
	sc := alertScope{kind: alertWindow, userID: r.uid, codeID: r.code.ID, windowID: "tipsport-1"}
	r.raise(botAlert{Kind: alertWindow, Severity: severityProblem, Title: "Logged out"}, &sc)
	raised := r.alerts()[0]
	r.clk.advance(time.Minute)
	r.b.mu.Lock()
	fx := &effects{}
	r.b.resolveLocked(sc, fx)
	r.b.resolveLocked(sc, fx) // no longer open: nothing
	r.b.release(fx)
	got := r.alerts()
	if len(got) != 1 || got[0].ID != raised.ID || got[0].ResolvedAt != r.clk.now().UnixMilli() {
		t.Fatalf("resolved: %+v", got)
	}
	if rows := r.rows(); rows[0].ResolvedAt != r.clk.now().UnixMilli() {
		t.Fatalf("row: %+v", rows)
	}
	r.b.mu.Lock()
	r.b.alerts.settings[r.uid] = alertSettings{}
	r.b.mu.Unlock()
	r.raise(botAlert{Kind: alertWindow, Severity: severityProblem, Title: "Blocked"}, &sc)
	r.b.mu.Lock()
	fx = &effects{}
	r.b.resolveLocked(sc, fx)
	r.b.release(fx)
	if got := r.alerts(); len(got) != 0 {
		t.Fatalf("an alert that was never in the app: %+v", got)
	}
}

func TestBotAlerts_SettingsReadFromTheDB(t *testing.T) {
	r := newAlertRig(t)
	if err := r.svc.SaveBotAlertSettings(r.uid, `{"order":{"app":false,"telegram":true}}`); err != nil {
		t.Fatal(err)
	}
	if err := r.b.loadSettings(r.uid); err != nil { // loads the alert switches too
		t.Fatal(err)
	}
	r.b.mu.Lock()
	st := r.b.alertSettingsLocked(r.uid)
	r.b.mu.Unlock()
	want := defaultAlertSettings()
	want.Order = alertChannels{App: false, Telegram: true}
	if st != want {
		t.Fatalf("settings %+v, want %+v", st, want)
	}
	if got := r.b.agentNameLocked(42); got != "Agent 42" {
		t.Fatalf("unknown agent name: %q", got)
	}
}

// Alert switches that cannot be read: the alert goes to the app only (never to Telegram on a guess).
func TestBotAlerts_UnreadableSettingsAppOnly(t *testing.T) {
	r := newAlertRig(t)
	r.link()
	r.db.Close()
	r.b.loadAlertSettings(r.uid)
	r.raise(botAlert{Kind: alertWindow, Severity: severityProblem, Title: "Logged out"}, nil)
	if got := r.alerts(); len(got) != 1 {
		t.Fatalf("app: %+v", got)
	}
	if jobs := r.out.take(); len(jobs) != 0 {
		t.Fatalf("telegram: %+v", jobs)
	}
}
