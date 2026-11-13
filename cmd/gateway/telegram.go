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
func setupTelegram(ctx context.Context, cfg Config, hub *Hub, rdb *redis.Client, acc *accounts.Service) *telegram.Notifier {
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
	}, rdb)
	if err != nil {
		log.Error().Err(err).Msg("Telegram notifier failed to start; feature off")
		return nil
	}
	hub.onChange = func(key string, opp ArbOpportunity, removed, evicted bool) {
		if removed {
			n.Observe(telegram.Event{Key: key, Removed: true, Evicted: evicted})
			return
		}
		n.Observe(telegram.Event{Key: key, Arb: toTelegramArb(opp)})
	}
	return n
}

// toTelegramArb copies the fields the notifier reads.
func toTelegramArb(o ArbOpportunity) telegram.Arb {
	stakes := make([]telegram.Stake, len(o.Stakes))
	for i, s := range o.Stakes {
		stakes[i] = telegram.Stake{Bookmaker: s.Bookmaker, OutcomeID: s.OutcomeID, Label: s.Label, DecimalOdds: s.DecimalOdds, StakePct: s.StakePct, MatchURL: s.MatchURL, MarketName: s.MarketName}
	}
	return telegram.Arb{
		EventID: o.EventID, Sport: o.Sport, MarketType: o.MarketType, Line: o.Line, ProfitPct: o.ProfitPct, Stakes: stakes,
		DetectedAt: o.DetectedAt, FirstDetectedAt: o.FirstDetectedAt, LastChangedAt: o.LastChangedAt,
		NameHome: o.NameHome, NameAway: o.NameAway, League: o.League, MarketName: o.MarketName, StartTime: o.StartTime,
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
