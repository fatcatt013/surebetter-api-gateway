package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
	"github.com/surebetter/api-gateway/internal/telegram"
)

// fakeOutbox records the Telegram jobs; with answer set, each job's Done gets answer(job) on its own goroutine.
type fakeOutbox struct {
	mu     sync.Mutex
	jobs   []telegram.Job
	answer func(telegram.Job) telegram.Result
}

func (f *fakeOutbox) Enqueue(j telegram.Job) {
	f.mu.Lock()
	f.jobs = append(f.jobs, j)
	answer := f.answer
	f.mu.Unlock()
	if answer != nil && j.Done != nil {
		go j.Done(answer(j))
	}
}

func (f *fakeOutbox) take() []telegram.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs
	f.jobs = nil
	return j
}

// newAlertsAccounts opens a fresh accounts DB with one user.
func newAlertsAccounts(t *testing.T) (*accounts.Service, int64) {
	t.Helper()
	db, err := accounts.OpenDB(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := accounts.NewService(db, nil)
	return svc, userIDOf(t, svc, newUser(t, svc, "u@example.com", accounts.RoleUser))
}

func TestAlertText(t *testing.T) {
	at := time.Date(2026, 11, 23, 13, 32, 0, 0, time.UTC).UnixMilli() // 14:32 in Prague
	a := botAlert{Kind: alertAttention, Severity: severityAttention, OrderID: "8d10c2aa00000000", Title: titleOutcomeUnknown,
		Detail: "Order 8d10c2aa · Tipsport 1 (Home PC)", CreatedAt: at}
	want := "⚠️ Outcome unknown · Order 8d10c2aa · Tipsport 1 (Home PC) · 14:32 — check the bookmaker before retrying\n" +
		"https://sb.example/app/bots/orders?order=8d10c2aa00000000"
	if got := alertText(a, "https://sb.example", prague); got != want {
		t.Fatalf("attention:\n%q\nwant\n%q", got, want)
	}
	w := botAlert{Kind: alertWindow, Severity: severityProblem, AgentID: 12, WindowID: "tipsport-1", Title: "Logged out",
		Detail: "Tipsport 1 (Home PC)", CreatedAt: at}
	if got := alertText(w, "https://sb.example", prague); got != "🔴 Logged out · Tipsport 1 (Home PC) · 14:32\nhttps://sb.example/app/bots/fleet" {
		t.Fatalf("window: %q", got)
	}
	o := botAlert{Kind: alertOrder, Severity: severityInfo, OrderID: "c1c1c1c1deadbeef", Title: "Placed", Detail: "Order c1c1c1c1", CreatedAt: at}
	if got := alertText(o, "", prague); got != "🟢 Placed · Order c1c1c1c1 · 14:32" {
		t.Fatalf("no app address: %q", got)
	}
}

func TestTelegramAlerts_SendToTheLinkedChat403Unlinks(t *testing.T) {
	svc, uid := newAlertsAccounts(t)
	out := &fakeOutbox{}
	tg := &telegramAlerts{acc: svc, out: out, loc: prague, appURL: "https://sb.example"}
	a := botAlert{Kind: alertWindow, Severity: severityProblem, Title: "Logged out", Detail: "Tipsport 1 (Home PC)", CreatedAt: 1}
	tg.send(uid, a)
	if jobs := out.take(); len(jobs) != 0 {
		t.Fatalf("not linked, yet: %+v", jobs)
	}
	if err := svc.SetTelegramLink(uid, 555); err != nil {
		t.Fatal(err)
	}
	tg.send(uid, a)
	jobs := out.take()
	if len(jobs) != 1 || jobs[0].Kind != telegram.JobAlert || jobs[0].ChatID != 555 || jobs[0].Text != alertText(a, "https://sb.example", prague) ||
		jobs[0].Attempt != 0 || jobs[0].Done == nil {
		t.Fatalf("job: %+v", jobs)
	}
	jobs[0].Done(telegram.Result{Err: &telegram.APIError{Code: 500}})
	if _, ok, _ := svc.TelegramLink(uid); !ok {
		t.Fatal("a 500 removed the link")
	}
	jobs[0].Done(telegram.Result{Err: &telegram.APIError{Code: 403, Description: "Forbidden: bot was blocked by the user"}})
	if _, ok, _ := svc.TelegramLink(uid); ok {
		t.Fatal("a 403 kept the link")
	}
}

func TestTelegramAlerts_403ForAnOlderChatKeepsTheNewLink(t *testing.T) {
	svc, uid := newAlertsAccounts(t)
	out := &fakeOutbox{}
	tg := &telegramAlerts{acc: svc, out: out, loc: prague, appURL: "https://sb.example"}
	if err := svc.SetTelegramLink(uid, 555); err != nil {
		t.Fatal(err)
	}
	tg.send(uid, botAlert{Kind: alertWindow, Severity: severityProblem, Title: "Blocked", CreatedAt: 1})
	if err := svc.SetTelegramLink(uid, 777); err != nil {
		t.Fatal(err)
	}
	out.take()[0].Done(telegram.Result{Err: &telegram.APIError{Code: 403}})
	if l, ok, _ := svc.TelegramLink(uid); !ok || l.ChatID != 777 {
		t.Fatalf("link: %+v %v", l, ok)
	}
}
