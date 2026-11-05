package main

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/telegram"
)

// setupTelegram starts the Telegram notifier and hooks it to the arb hub. It returns nil (feature
// off) when TELEGRAM_TOKEN or TELEGRAM_CHAT_ID is unset or the notifier cannot start. Call it
// before the NATS bridge starts so no transition is missed.
func setupTelegram(ctx context.Context, cfg Config, hub *Hub, rdb *redis.Client) *telegram.Notifier {
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
		Present:      hub.Has,
	}, rdb)
	if err != nil {
		log.Error().Err(err).Msg("Telegram notifier failed to start; feature off")
		return nil
	}
	hub.onChange = func(key string, opp ArbOpportunity, removed bool) {
		if removed {
			n.Observe(telegram.Event{Key: key, Removed: true})
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
