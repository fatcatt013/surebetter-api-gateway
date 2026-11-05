package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

const (
	ConfigKey   = "telegram:bot_config" // JSON BotConfig, no TTL
	MessagesKey = "telegram:messages"   // hash: oppStateKey (or "gone:<message_id>") → JSON Tracked
)

// Tracked is a posted message's persisted state, enough to keep editing it after a restart.
type Tracked struct {
	MessageID       int64    `json:"message_id"`
	SentAt          int64    `json:"sent_at"`
	FirstDetectedAt int64    `json:"first_detected_at"`
	PeakProfit      float64  `json:"peak_profit"`
	LastTextSHA     string   `json:"last_text_sha"`
	LastEditAt      int64    `json:"last_edit_at"`
	Arb             Arb      `json:"arb"`
	Bankroll        Bankroll `json:"bankroll"`          // as at send time; edits keep it
	PresetName      string   `json:"preset_name"`       // as at send time
	GoneAt          int64    `json:"gone_at,omitempty"` // set on "gone:<message_id>" fields: GONE shown, delete pending
}

// Store persists config and tracked messages.
type Store interface {
	LoadConfig(ctx context.Context) (BotConfig, error) // missing → zero config, nil
	SaveConfig(ctx context.Context, c BotConfig) error
	LoadMessages(ctx context.Context) (map[string]Tracked, error)
	SaveMessage(ctx context.Context, key string, t Tracked) error
	DeleteMessage(ctx context.Context, key string) error
}

type RedisStore struct{ rdb *redis.Client }

func NewRedisStore(rdb *redis.Client) *RedisStore { return &RedisStore{rdb: rdb} }

func (s *RedisStore) LoadConfig(ctx context.Context) (BotConfig, error) {
	raw, err := s.rdb.Get(ctx, ConfigKey).Bytes()
	if errors.Is(err, redis.Nil) {
		return BotConfig{}, nil
	}
	if err != nil {
		return BotConfig{}, err
	}
	var c BotConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return BotConfig{}, fmt.Errorf("%s: %w", ConfigKey, err)
	}
	return c, nil
}

func (s *RedisStore) SaveConfig(ctx context.Context, c BotConfig) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, ConfigKey, raw, 0).Err()
}

func (s *RedisStore) LoadMessages(ctx context.Context) (map[string]Tracked, error) {
	all, err := s.rdb.HGetAll(ctx, MessagesKey).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]Tracked, len(all))
	for k, v := range all {
		var t Tracked
		if err := json.Unmarshal([]byte(v), &t); err != nil {
			log.Warn().Str("key", k).Err(err).Msg("telegram: skipping undecodable tracked message")
			continue
		}
		out[k] = t
	}
	return out, nil
}

func (s *RedisStore) SaveMessage(ctx context.Context, key string, t Tracked) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.rdb.HSet(ctx, MessagesKey, key, raw).Err()
}

func (s *RedisStore) DeleteMessage(ctx context.Context, key string) error {
	return s.rdb.HDel(ctx, MessagesKey, key).Err()
}
