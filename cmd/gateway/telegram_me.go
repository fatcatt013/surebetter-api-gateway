package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
	"github.com/surebetter/api-gateway/internal/telegram"
)

// telegramTestWait bounds how long POST /api/me/telegram/test waits for Telegram (a var so a test can shorten it).
var telegramTestWait = 20 * time.Second

// telegramTestEvery: one Send test per user this often (429 too_many otherwise; a var so a test can change it).
var telegramTestEvery = 10 * time.Second

const telegramTestText = "🧪 Test · SureBetter bot alerts come here"

// telegramMeAPI serves /api/me/telegram*: the user's private chat for bot alerts.
type telegramMeAPI struct {
	acc *accounts.Service
	tg  *telegramAlerts // nil: Telegram alerts off (no TELEGRAM_TOKEN, or TELEGRAM_POLL not 1)

	mu       sync.Mutex
	lastTest map[int64]time.Time // user → their last Send test
}

func newTelegramMeAPI(acc *accounts.Service, tg *telegramAlerts) http.Handler {
	a := &telegramMeAPI{acc: acc, tg: tg, lastTest: map[int64]time.Time{}}
	mux := http.NewServeMux()
	mux.Handle("GET /api/me/telegram", acc.RequireSession(http.HandlerFunc(a.handleGet)))
	mux.Handle("DELETE /api/me/telegram", acc.RequireSession(http.HandlerFunc(a.handleUnlink)))
	mux.Handle("POST /api/me/telegram/link", acc.RequireSession(http.HandlerFunc(a.handleLink)))
	mux.Handle("POST /api/me/telegram/test", acc.RequireSession(http.HandlerFunc(a.handleTest)))
	return accounts.CSRF(mux)
}

func (a *telegramMeAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	link, ok, err := a.acc.TelegramLink(principalOf(r).UserID)
	if err != nil {
		log.Error().Err(err).Msg("telegram link read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	v := struct {
		Available bool  `json:"available"`
		Linked    bool  `json:"linked"`
		LinkedAt  int64 `json:"linkedAt,omitempty"`
	}{Available: a.tg != nil, Linked: ok}
	if ok {
		v.LinkedAt = link.LinkedAt
	}
	accounts.WriteJSON(w, http.StatusOK, v)
}

func (a *telegramMeAPI) handleLink(w http.ResponseWriter, r *http.Request) {
	if a.tg == nil {
		accounts.WriteError(w, http.StatusServiceUnavailable, "telegram_unavailable")
		return
	}
	username, err := a.tg.linker.BotUsername(r.Context())
	if err != nil {
		log.Warn().Err(err).Msg("telegram getMe failed")
		accounts.WriteError(w, http.StatusBadGateway, "telegram_error")
		return
	}
	code, exp := a.tg.linker.NewCode(principalOf(r).UserID)
	accounts.WriteJSON(w, http.StatusOK, map[string]any{"url": "https://t.me/" + username + "?start=" + code, "expiresAt": exp.UnixMilli()})
}

func (a *telegramMeAPI) handleUnlink(w http.ResponseWriter, r *http.Request) {
	if err := a.acc.DeleteTelegramLink(principalOf(r).UserID); err != nil {
		log.Error().Err(err).Msg("telegram unlink failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTest sends one test message to the linked chat (no retries) and waits for Telegram's answer.
func (a *telegramMeAPI) handleTest(w http.ResponseWriter, r *http.Request) {
	if a.tg == nil {
		accounts.WriteError(w, http.StatusServiceUnavailable, "telegram_unavailable")
		return
	}
	userID := principalOf(r).UserID
	link, ok, err := a.acc.TelegramLink(userID)
	if err != nil {
		log.Error().Err(err).Msg("telegram link read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !ok {
		accounts.WriteError(w, http.StatusConflict, "not_linked")
		return
	}
	if !a.testAllowed(userID, time.Now()) {
		accounts.WriteError(w, http.StatusTooManyRequests, "too_many")
		return
	}
	text := telegramTestText
	if a.tg.appURL != "" {
		text += "\n" + a.tg.appURL + "/app/bots/settings"
	}
	done := make(chan telegram.Result, 1)
	a.tg.out.Enqueue(telegram.Job{Kind: telegram.JobAlert, ChatID: link.ChatID, Text: text, Attempt: telegram.AlertNoRetry,
		Done: func(res telegram.Result) { done <- res }})
	select {
	case res := <-done:
		switch {
		case res.Err == nil:
			accounts.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
		case telegramForbidden(res.Err):
			if _, err := a.acc.DeleteTelegramLinkIfChat(userID, link.ChatID); err != nil {
				log.Error().Err(err).Msg("telegram unlink failed")
			}
			accounts.WriteError(w, http.StatusConflict, "not_linked")
		default:
			log.Warn().Err(res.Err).Msg("telegram test failed")
			accounts.WriteError(w, http.StatusBadGateway, "telegram_error")
		}
	case <-time.After(telegramTestWait):
		accounts.WriteError(w, http.StatusGatewayTimeout, "telegram_timeout")
	case <-r.Context().Done():
	}
}

// testAllowed: userID may send a test now (none in the last telegramTestEvery); it then counts as their last.
func (a *telegramMeAPI) testAllowed(userID int64, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, ok := a.lastTest[userID]; ok && now.Sub(last) < telegramTestEvery {
		return false
	}
	if len(a.lastTest) >= 1000 {
		for u, t := range a.lastTest {
			if now.Sub(t) >= telegramTestEvery {
				delete(a.lastTest, u)
			}
		}
	}
	a.lastTest[userID] = now
	return true
}
