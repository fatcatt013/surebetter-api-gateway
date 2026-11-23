package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// LinkStore keeps a user's private chat for bot alerts (the accounts DB).
type LinkStore interface {
	SetTelegramLink(userID, chatID int64) error
}

// UpdatesClient is the slice of the Bot API the linker reads with.
type UpdatesClient interface {
	GetUpdates(ctx context.Context, offset int64, timeoutS int) ([]Update, error)
	GetMe(ctx context.Context) (string, error)
}

// LinkCodeTTL is how long a Connect Telegram code works.
const LinkCodeTTL = 10 * time.Minute

const (
	linkPollTimeoutS = 10               // getUpdates long poll (below the client's 15 s HTTP timeout)
	linkRetryWait    = 5 * time.Second  // after a failed getUpdates
	linkConflictWait = 30 * time.Second // after a 409: another getUpdates reader, or a webhook, holds this bot
)

// The linker's one-line answers in a private chat.
const (
	linkedText     = "Connected: SureBetter bot alerts will come here."
	badCodeText    = "This link has expired. Open SureBetter › Bots › Settings and connect Telegram again."
	saveFailedText = "Not connected: please try again."
	onlyAlertsText = "This bot only sends SureBetter alerts."
)

type linkCode struct {
	userID  int64
	expires time.Time
}

// Linker links users' private chats for bot alerts: the app gets a one-time code (NewCode) and opens
// t.me/<bot>?start=<code>; the bot's updates are read with getUpdates long polling (one reader, no webhook), and a
// private-chat "/start <code>" with a valid code stores the link. Group messages are ignored; any other private
// message gets a one-line answer. Codes live in memory only.
type Linker struct {
	client UpdatesClient
	out    enqueuer
	store  LinkStore
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error

	mu       sync.Mutex
	codes    map[string]linkCode
	username string
	offset   int64 // next update id (Run's goroutine only)
}

func NewLinker(c UpdatesClient, out enqueuer, store LinkStore) *Linker {
	return &Linker{client: c, out: out, store: store, now: time.Now, sleep: sleepCtx, codes: map[string]linkCode{}}
}

// NewCode makes a one-time code (32 hex) for userID, valid LinkCodeTTL; the user's older code and expired codes stop
// working.
func (l *Linker) NewCode(userID int64) (string, time.Time) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	code := hex.EncodeToString(b)
	now := l.now()
	exp := now.Add(LinkCodeTTL)
	l.mu.Lock()
	for c, lc := range l.codes {
		if lc.userID == userID || !now.Before(lc.expires) {
			delete(l.codes, c)
		}
	}
	l.codes[code] = linkCode{userID: userID, expires: exp}
	l.mu.Unlock()
	return code, exp
}

// take uses up code; ok is false when it is unknown or expired.
func (l *Linker) take(code string) (linkCode, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lc, ok := l.codes[code]
	if !ok {
		return linkCode{}, false
	}
	delete(l.codes, code)
	return lc, l.now().Before(lc.expires)
}

// BotUsername is the bot's username (getMe, kept after the first success).
func (l *Linker) BotUsername(ctx context.Context) (string, error) {
	l.mu.Lock()
	u := l.username
	l.mu.Unlock()
	if u != "" {
		return u, nil
	}
	u, err := l.client.GetMe(ctx)
	if err != nil {
		return "", err
	}
	if u == "" {
		return "", errors.New("telegram getMe: no username")
	}
	l.mu.Lock()
	l.username = u
	l.mu.Unlock()
	return u, nil
}

// Run reads the bot's updates until ctx ends.
func (l *Linker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := l.pollOnce(ctx)
		if err == nil || ctx.Err() != nil {
			continue
		}
		wait := linkRetryWait
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == 409 {
			wait = linkConflictWait
			log.Warn().Err(err).Msg("telegram: another getUpdates reader or a webhook holds this bot; alert links wait")
		} else {
			log.Warn().Err(err).Msg("telegram getUpdates failed")
		}
		if l.sleep(ctx, wait) != nil {
			return
		}
	}
}

// pollOnce reads one batch of updates and handles them.
func (l *Linker) pollOnce(ctx context.Context) error {
	ups, err := l.client.GetUpdates(ctx, l.offset, linkPollTimeoutS)
	if err != nil {
		return err
	}
	for _, u := range ups {
		if u.UpdateID >= l.offset {
			l.offset = u.UpdateID + 1
		}
		l.handle(u)
	}
	return nil
}

// handle answers one update: a private "/start <code>" links (or says the link expired), any other private message
// gets onlyAlertsText, and everything else is ignored.
func (l *Linker) handle(u Update) {
	m := u.Message
	if m == nil || m.Chat.Type != "private" {
		return
	}
	text := strings.TrimSpace(m.Text)
	rest, isStart := strings.CutPrefix(text, "/start")
	if !isStart || (rest != "" && rest[0] != ' ') {
		l.reply(m.Chat.ID, onlyAlertsText)
		return
	}
	lc, ok := l.take(strings.TrimSpace(rest))
	if !ok {
		l.reply(m.Chat.ID, badCodeText)
		return
	}
	if err := l.store.SetTelegramLink(lc.userID, m.Chat.ID); err != nil {
		log.Error().Err(err).Int64("user", lc.userID).Msg("telegram link save failed")
		l.reply(m.Chat.ID, saveFailedText)
		return
	}
	log.Info().Int64("user", lc.userID).Msg("telegram alerts linked")
	l.reply(m.Chat.ID, linkedText)
}

func (l *Linker) reply(chatID int64, text string) {
	l.out.Enqueue(Job{Kind: JobAlert, ChatID: chatID, Text: text})
}
