package telegram

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

// Settings come from the gateway's environment.
type Settings struct {
	Token        string
	ChatID       string
	EditInterval time.Duration
	RatePerMin   int
	TZ           string
	StartupGrace time.Duration
	GoneDelete   time.Duration // TELEGRAM_GONE_DELETE_AFTER_S; 0 = delete at once, no GONE edit
	Present      func(key string) bool
}

// Start builds the notifier and outbox, loads state from Redis and launches both goroutines.
// A Redis load failure is logged and leaves the bot disabled until a config is saved.
func Start(ctx context.Context, s Settings, rdb *redis.Client) (*Notifier, error) {
	render, err := NewRenderer(s.TZ)
	if err != nil {
		return nil, err
	}
	var n *Notifier
	out := NewOutbox(NewHTTPClient(s.Token, s.ChatID), s.RatePerMin, func(r Result) { n.Deliver(r) })
	n = NewNotifier(NewRedisStore(rdb), out, render, Options{EditInterval: s.EditInterval, StartupGrace: s.StartupGrace, GoneDeleteAfter: s.GoneDelete, Present: s.Present})
	if err := n.Load(ctx); err != nil {
		log.Error().Err(err).Msg("telegram: loading state from Redis failed; the bot stays disabled until a config is saved")
	}
	go out.Run(ctx)
	go n.Run(ctx)
	log.Info().Str("chat_id", s.ChatID).Bool("enabled", n.Config().Enabled).Msg("Telegram notifier started")
	return n, nil
}
