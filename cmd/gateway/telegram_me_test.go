package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
	"github.com/surebetter/api-gateway/internal/telegram"
)

// chanUpdates hands the linker one batch per channel receive.
type chanUpdates struct {
	batches chan []telegram.Update
	me      string
}

func (c *chanUpdates) GetUpdates(ctx context.Context, _ int64, _ int) ([]telegram.Update, error) {
	select {
	case b := <-c.batches:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *chanUpdates) GetMe(context.Context) (string, error) {
	if c.me == "" {
		return "", errors.New("telegram down")
	}
	return c.me, nil
}

type telegramRig struct {
	srv    *httptest.Server
	svc    *accounts.Service
	out    *fakeOutbox
	up     *chanUpdates
	linker *telegram.Linker
}

// newTelegramServer is newBotServer with Telegram alerts configured (withTelegram) or not.
func newTelegramServer(t *testing.T, withTelegram bool) telegramRig {
	t.Helper()
	db, err := accounts.OpenDB(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := accounts.NewService(db, nil)
	reg := newConnRegistry()
	svc.SetConnections(reg)
	bots := newBotHub(reg, svc)
	rig := telegramRig{svc: svc, out: &fakeOutbox{}, up: &chanUpdates{batches: make(chan []telegram.Update, 1), me: "sb_alerts_bot"}}
	if withTelegram {
		rig.linker = telegram.NewLinker(rig.up, rig.out, svc)
		bots.tg = &telegramAlerts{acc: svc, out: rig.out, linker: rig.linker, loc: prague, appURL: "https://sb.example"}
	}
	cfg := Config{SnapshotCacheMs: 500, MaxClients: 10}
	rig.srv = httptest.NewServer(newMux(NewHub(cfg), NewValueHub(cfg), nil, &wsGate{acc: svc, reg: reg, required: true, bots: bots}))
	t.Cleanup(rig.srv.Close)
	return rig
}

func TestTelegramMe_Unavailable(t *testing.T) {
	r := newTelegramServer(t, false)
	token := newUser(t, r.svc, "u@example.com", accounts.RoleUser)
	if s, b := request(t, r.srv, "GET", "/api/me/telegram", token, "", nil); s != 200 || strings.TrimSpace(b) != `{"available":false,"linked":false}` {
		t.Fatalf("GET: %d %s", s, b)
	}
	for _, p := range []string{"/api/me/telegram/link", "/api/me/telegram/test"} {
		if s, b := request(t, r.srv, "POST", p, token, `{}`, nil); s != 503 || !strings.Contains(b, `"telegram_unavailable"`) {
			t.Fatalf("POST %s: %d %s", p, s, b)
		}
	}
	if s, _ := request(t, r.srv, "DELETE", "/api/me/telegram", token, "", nil); s != 204 {
		t.Fatalf("DELETE: %d", s)
	}
	if s, _ := request(t, r.srv, "GET", "/api/me/telegram", "", "", nil); s != 401 {
		t.Fatalf("no session: %d", s)
	}
}

func TestTelegramMe_LinkFlow(t *testing.T) {
	r := newTelegramServer(t, true)
	token := newUser(t, r.svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, r.svc, token)
	s, b := request(t, r.srv, "POST", "/api/me/telegram/link", token, `{}`, nil)
	var l struct {
		URL       string `json:"url"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if s != 200 || json.Unmarshal([]byte(b), &l) != nil || !strings.HasPrefix(l.URL, "https://t.me/sb_alerts_bot?start=") ||
		len(strings.TrimPrefix(l.URL, "https://t.me/sb_alerts_bot?start=")) != 32 || l.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("link: %d %s", s, b)
	}
	code := strings.TrimPrefix(l.URL, "https://t.me/sb_alerts_bot?start=")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.linker.Run(ctx)
	r.up.batches <- []telegram.Update{{UpdateID: 1, Message: &telegram.Message{Chat: telegram.Chat{ID: 555, Type: "private"}, Text: "/start " + code}}}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if link, ok, _ := r.svc.TelegramLink(uid); ok {
			if link.ChatID != 555 {
				t.Fatalf("link: %+v", link)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never linked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s, b = request(t, r.srv, "GET", "/api/me/telegram", token, "", nil)
	var v struct {
		Available, Linked bool
		LinkedAt          int64
	}
	if s != 200 || json.Unmarshal([]byte(b), &v) != nil || !v.Available || !v.Linked || v.LinkedAt == 0 {
		t.Fatalf("GET after linking: %d %s", s, b)
	}
	if s, _ := request(t, r.srv, "DELETE", "/api/me/telegram", token, "", nil); s != 204 {
		t.Fatalf("DELETE: %d", s)
	}
	if _, ok, _ := r.svc.TelegramLink(uid); ok {
		t.Fatal("still linked")
	}
}

func TestTelegramMe_LinkWhileGetMeFails(t *testing.T) {
	r := newTelegramServer(t, true)
	r.up.me = ""
	token := newUser(t, r.svc, "u@example.com", accounts.RoleUser)
	if s, b := request(t, r.srv, "POST", "/api/me/telegram/link", token, `{}`, nil); s != 502 || !strings.Contains(b, `"telegram_error"`) {
		t.Fatalf("link: %d %s", s, b)
	}
}

func TestTelegramMe_SendTest(t *testing.T) {
	r := newTelegramServer(t, true)
	token := newUser(t, r.svc, "u@example.com", accounts.RoleUser)
	uid := userIDOf(t, r.svc, token)
	if s, b := request(t, r.srv, "POST", "/api/me/telegram/test", token, `{}`, nil); s != 409 || !strings.Contains(b, `"not_linked"`) {
		t.Fatalf("not linked: %d %s", s, b)
	}
	if err := r.svc.SetTelegramLink(uid, 555); err != nil {
		t.Fatal(err)
	}
	r.out.answer = func(j telegram.Job) telegram.Result { return telegram.Result{Job: j, MessageID: 9} }
	if s, b := request(t, r.srv, "POST", "/api/me/telegram/test", token, `{}`, nil); s != 200 || strings.TrimSpace(b) != `{"ok":true}` {
		t.Fatalf("test: %d %s", s, b)
	}
	jobs := r.out.take()
	if len(jobs) != 1 || jobs[0].ChatID != 555 || jobs[0].Attempt != telegram.AlertNoRetry ||
		jobs[0].Text != "🧪 Test · SureBetter bot alerts come here\nhttps://sb.example/app/bots/settings" {
		t.Fatalf("job: %+v", jobs)
	}
	r.out.answer = func(j telegram.Job) telegram.Result {
		return telegram.Result{Job: j, Err: &telegram.APIError{Code: 502}}
	}
	if s, b := request(t, r.srv, "POST", "/api/me/telegram/test", token, `{}`, nil); s != 502 || !strings.Contains(b, `"telegram_error"`) {
		t.Fatalf("5xx: %d %s", s, b)
	}
	r.out.answer = func(j telegram.Job) telegram.Result {
		return telegram.Result{Job: j, Err: &telegram.APIError{Code: 403}}
	}
	if s, b := request(t, r.srv, "POST", "/api/me/telegram/test", token, `{}`, nil); s != 409 || !strings.Contains(b, `"not_linked"`) {
		t.Fatalf("403: %d %s", s, b)
	}
	if _, ok, _ := r.svc.TelegramLink(uid); ok {
		t.Fatal("a 403 kept the link")
	}
	if err := r.svc.SetTelegramLink(uid, 555); err != nil {
		t.Fatal(err)
	}
	r.out.answer = nil // Telegram never answers
	defer func(d time.Duration) { telegramTestWait = d }(telegramTestWait)
	telegramTestWait = 50 * time.Millisecond
	if s, b := request(t, r.srv, "POST", "/api/me/telegram/test", token, `{}`, nil); s != 504 || !strings.Contains(b, `"telegram_timeout"`) {
		t.Fatalf("timeout: %d %s", s, b)
	}
}
