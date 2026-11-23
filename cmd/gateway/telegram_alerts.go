package main

import (
	"errors"
	"net/url"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
	"github.com/surebetter/api-gateway/internal/telegram"
)

// alertOutbox is the Telegram outbox as the alerts use it (a fake in tests).
type alertOutbox interface{ Enqueue(telegram.Job) }

// telegramAlerts sends bot alerts to each user's linked private chat through the bot's shared outbox and links chats
// (telegram.Linker). nil on the hub = no TELEGRAM_TOKEN.
type telegramAlerts struct {
	acc    *accounts.Service
	out    alertOutbox
	linker *telegram.Linker
	loc    *time.Location // TELEGRAM_TZ
	appURL string         // APP_PUBLIC_URL without a trailing slash; "" = messages without a link
}

// severityIcons start each alert message.
var severityIcons = map[string]string{severityAttention: "⚠️", severityProblem: "🔴", severityInfo: "🟢"}

// alertPath is where an alert opens in the app: its order (Orders) or the Fleet.
func alertPath(a botAlert) string {
	if a.OrderID != "" {
		return "/app/bots/orders?order=" + url.QueryEscape(a.OrderID)
	}
	return "/app/bots/fleet"
}

// alertText is a as its Telegram message: one line (title, detail, the time in loc; an outcome unknown adds what to
// do), then the link into the app at appURL (APP_PUBLIC_URL; none when it is not set).
func alertText(a botAlert, appURL string, loc *time.Location) string {
	line := severityIcons[a.Severity] + " " + joinDetail(a.Title, a.Detail, time.UnixMilli(a.CreatedAt).In(loc).Format("15:04"))
	if a.Title == titleOutcomeUnknown {
		line += " — check the bookmaker before retrying"
	}
	if appURL != "" {
		line += "\n" + appURL + alertPath(a)
	}
	return line
}

// send queues a for userID's linked chat; nothing when the user has no link. Call it outside the hub's locks (it reads
// the link). A 403 (bot blocked, chat gone) removes the link; other failures are logged (the alert stays in the app).
func (t *telegramAlerts) send(userID int64, a botAlert) {
	link, ok, err := t.acc.TelegramLink(userID)
	if err != nil {
		log.Error().Err(err).Int64("user", userID).Msg("telegram link read failed; alert not sent")
		return
	}
	if !ok {
		return
	}
	chatID := link.ChatID
	t.out.Enqueue(telegram.Job{Kind: telegram.JobAlert, ChatID: chatID, Text: alertText(a, t.appURL, t.loc),
		Done: func(r telegram.Result) { t.done(userID, chatID, r) }})
}

func (t *telegramAlerts) done(userID, chatID int64, r telegram.Result) {
	if r.Err == nil {
		return
	}
	if telegramForbidden(r.Err) {
		if removed, err := t.acc.DeleteTelegramLinkIfChat(userID, chatID); err != nil {
			log.Error().Err(err).Int64("user", userID).Msg("telegram unlink failed")
		} else if removed {
			log.Warn().Int64("user", userID).Msg("telegram alerts unlinked: the bot is blocked or the chat is gone")
		}
		return
	}
	log.Warn().Err(r.Err).Int64("user", userID).Msg("telegram alert dropped")
}

// telegramForbidden: Telegram answered 403 (the user blocked the bot, or the chat is gone).
func telegramForbidden(err error) bool {
	var ae *telegram.APIError
	return errors.As(err, &ae) && ae.Code == 403
}
