package main

import (
	"context"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
	"github.com/surebetter/api-gateway/internal/telegram"
)

// setupTelegram starts the Telegram notifier and hooks it to the arb hub. It returns nil (feature
// off) when TELEGRAM_TOKEN or TELEGRAM_CHAT_ID is unset or the notifier cannot start. Call it
// before the NATS bridge starts so no transition is missed.
func setupTelegram(ctx context.Context, cfg Config, hub *Hub, rdb *redis.Client, acc *accounts.Service, out *telegram.Outbox) *telegram.Notifier {
	if cfg.TelegramToken == "" || cfg.TelegramChatID == "" {
		log.Info().Msg("Telegram notifier off (TELEGRAM_TOKEN / TELEGRAM_CHAT_ID not set)")
		return nil
	}
	prematchTTL := time.Duration(cfg.EvictionTTLS) * time.Second
	if prematchTTL <= 0 {
		prematchTTL = 120 * time.Second
	}
	n, err := telegram.Start(ctx, telegram.Settings{
		Token:        cfg.TelegramToken,
		ChatID:       cfg.TelegramChatID,
		EditInterval: time.Duration(cfg.TelegramEditIntervalS) * time.Second,
		RatePerMin:   cfg.TelegramRatePerMin,
		TZ:           cfg.TelegramTZ,
		StartupGrace: prematchTTL + 5*time.Second,
		GoneDelete:   time.Duration(cfg.TelegramGoneDeleteS) * time.Second,
		Present:      hub.Has,
		Stats:        telegramStatsSink{acc},
		Outbox:       out, // shared with the bot alerts (setupTelegramAlerts)
	}, rdb)
	if err != nil {
		log.Error().Err(err).Msg("Telegram notifier failed to start; feature off")
		return nil
	}
	hub.listen(func(key string, opp ArbOpportunity, removed, evicted bool) {
		if removed {
			n.Observe(telegram.Event{Key: key, Removed: true, Evicted: evicted})
			return
		}
		n.Observe(telegram.Event{Key: key, Arb: toTelegramArb(opp)})
	})
	return n
}

// toTelegramArb copies the fields the notifier reads, and the match state (arb, stakes, alternatives: not shown yet).
func toTelegramArb(o ArbOpportunity) telegram.Arb {
	stakes := make([]telegram.Stake, len(o.Stakes))
	for i, s := range o.Stakes {
		stakes[i] = telegram.Stake{Bookmaker: s.Bookmaker, OutcomeID: s.OutcomeID, Label: s.Label, DecimalOdds: s.DecimalOdds, StakePct: s.StakePct, MatchURL: s.MatchURL, MarketName: s.MarketName,
			MatchState: s.MatchState, MatchStateText: s.MatchStateText, MatchStateSince: s.MatchStateSince}
		for _, q := range s.Alternatives {
			stakes[i].Alternatives = append(stakes[i].Alternatives, telegram.Quote{Bookmaker: q.Bookmaker, Label: q.Label, DecimalOdds: q.DecimalOdds, MatchURL: q.MatchURL, MarketName: q.MarketName,
				MatchState: q.MatchState, MatchStateText: q.MatchStateText, MatchStateSince: q.MatchStateSince})
		}
	}
	return telegram.Arb{
		EventID: o.EventID, Sport: o.Sport, MarketType: o.MarketType, Line: o.Line, ProfitPct: o.ProfitPct, Stakes: stakes,
		DetectedAt: o.DetectedAt, FirstDetectedAt: o.FirstDetectedAt, LastChangedAt: o.LastChangedAt,
		NameHome: o.NameHome, NameAway: o.NameAway, League: o.League, MarketName: o.MarketName, StartTime: o.StartTime,
		MatchState: o.MatchState, MatchStateText: o.MatchStateText, MatchStateSince: o.MatchStateSince,
	}
}

// requestUserID is the signed-in user of a request (0 when AUTH_REQUIRED=false let it through
// without one); a saved Telegram config records it as updated_by.
func requestUserID(r *http.Request) int64 {
	p, _ := accounts.PrincipalFrom(r.Context())
	return p.UserID
}

// telegramStatsSink stores the Telegram bot's sent-arb statistics in the accounts DB. It runs on
// the notifier's StatsWriter goroutine, never on the notifier's own.
type telegramStatsSink struct{ acc *accounts.Service }

func (s telegramStatsSink) InsertSent(a telegram.SentArb) error {
	legs := make([]accounts.TelegramSentLeg, len(a.Legs))
	for i, l := range a.Legs {
		legs[i] = accounts.TelegramSentLeg{Bookmaker: l.Bookmaker, OutcomeID: l.OutcomeID, Odds: l.Odds, Stake: l.Stake}
	}
	_, err := s.acc.InsertTelegramSent(accounts.TelegramSentRow{
		MessageID: a.MessageID, SentAt: a.SentAt, UserID: a.UserID, ArbKey: a.ArbKey, FirstDetectedAt: a.FirstDetectedAt,
		Sport: a.Sport, IsLive: a.IsLive, MarketType: a.MarketType, Line: a.Line, ProfitPct: a.ProfitPct,
		ShownProfitPct: a.ShownProfitPct, Currency: a.Currency, TotalStake: a.TotalStake, Payout: a.Payout, Profit: a.Profit, Legs: legs,
	})
	return err
}

func (s telegramStatsSink) EndSent(e telegram.SentEnd) error {
	return s.acc.EndTelegramSent(e.MessageID, e.EndedAt, e.Reason)
}

func (s telegramStatsSink) SweepSent(keep []int64) error {
	_, err := s.acc.SweepTelegramSent(keep)
	return err
}

// setupTelegramAlerts starts the bot's shared outbox and the alert linker (getUpdates) when TELEGRAM_TOKEN is set and
// TELEGRAM_POLL=1 (one getUpdates reader per token: only the VPS sets it); TELEGRAM_CHAT_ID is not needed for alerts.
// nil, nil: off (alerts stay in the app, GET /api/me/telegram says available false, the arb notifier keeps its own
// outbox).
func setupTelegramAlerts(ctx context.Context, cfg Config, acc *accounts.Service) (*telegramAlerts, *telegram.Outbox) {
	if cfg.TelegramToken == "" || !cfg.TelegramPoll {
		log.Info().Bool("token", cfg.TelegramToken != "").Bool("poll", cfg.TelegramPoll).
			Msg("Telegram alerts off (they need TELEGRAM_TOKEN and TELEGRAM_POLL=1)")
		return nil, nil
	}
	if cfg.AppPublicURL == "" {
		log.Warn().Msg("APP_PUBLIC_URL not set: Telegram alerts go out without a link into the app")
	}
	loc, err := time.LoadLocation(cfg.TelegramTZ)
	if err != nil {
		log.Warn().Err(err).Str("tz", cfg.TelegramTZ).Msg("TELEGRAM_TZ unknown; alert times use Europe/Prague")
		loc = prague
	}
	client := telegram.NewHTTPClient(cfg.TelegramToken, cfg.TelegramChatID)
	out := telegram.NewOutbox(client, cfg.TelegramRatePerMin, nil)
	go out.Run(ctx)
	var store telegram.LinkStore = acc
	linker := telegram.NewLinker(client, out, store)
	go linker.Run(ctx)
	log.Info().Msg("Telegram alerts on: private chats linked from the app")
	return &telegramAlerts{acc: acc, out: out, linker: linker, loc: loc, appURL: cfg.AppPublicURL}, out
}
