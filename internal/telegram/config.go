package telegram

import (
	"errors"
	"fmt"
)

// PresetRef names the frontend filter preset the bot follows.
type PresetRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Filter is the frontend FilterState as the bot applies it. `markets` is pre-resolved by the
// frontend into BlockedMarkets (base sport → canonical market types it blocks); `sort` is dropped.
// A nil slice means "don't constrain"; an empty, non-nil Bookmakers slice keeps nothing (as in the UI).
type Filter struct {
	Bookmakers     []string            `json:"bookmakers"`
	Sports         []string            `json:"sports"`
	LiveState      string              `json:"live_state"` // all | live | prematch
	ProfitMin      *float64            `json:"profit_min"`
	ProfitMax      *float64            `json:"profit_max"`
	MinAgeSec      int                 `json:"min_age_sec"`
	ThreeLeg       string              `json:"three_leg"` // include | exclude: arbs with 3+ legs
	BlockedMarkets map[string][]string `json:"blocked_markets"`
}

// Bankroll mirrors the frontend BankrollSettings (snake_case).
type Bankroll struct {
	Mode       string  `json:"mode"` // split | maxleg
	Total      float64 `json:"total"`
	MaxLeg     float64 `json:"max_leg"`
	Currency   string  `json:"currency"` // CZK | EUR
	AnchorBook *string `json:"anchor_book"`
	Rounding   int     `json:"rounding"` // 1 | 10 | 100
}

// DefaultBankroll matches the frontend DEFAULT_BANKROLL; used for the test message when no config is stored.
var DefaultBankroll = Bankroll{Mode: "split", Total: 10000, MaxLeg: 3000, Currency: "CZK", Rounding: 1}

// BotConfig is stored in Redis (telegram:bot_config) and edited through PUT /api/telegram/config.
type BotConfig struct {
	Enabled   bool       `json:"enabled"`
	Preset    *PresetRef `json:"preset"`
	Filter    Filter     `json:"filter"`
	Bankroll  Bankroll   `json:"bankroll"`
	UpdatedAt int64      `json:"updated_at"`
	UpdatedBy int64      `json:"updated_by,omitempty"` // user id of the admin who saved it (0 = unknown); set by the server
}

// ValidationError marks a config the caller got wrong (HTTP 400), as opposed to a storage failure.
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// Normalize fills the nil map so the stored JSON says {} like the frontend payload, and defaults a
// missing three_leg (configs stored before it existed) to "include", which keeps the frontend's
// "is the server copy up to date?" comparison exact.
func (c *BotConfig) Normalize() {
	if c.Filter.ThreeLeg == "" {
		c.Filter.ThreeLeg = "include"
	}
	if c.Filter.BlockedMarkets == nil {
		c.Filter.BlockedMarkets = map[string][]string{}
	}
}

// Validate checks an enabled config. A disabled config is always accepted (it may be empty).
func (c BotConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Preset == nil || c.Preset.ID == "" {
		return errors.New("an enabled config needs a preset")
	}
	switch c.Filter.LiveState {
	case "all", "live", "prematch":
	default:
		return fmt.Errorf("filter.live_state %q: want all, live or prematch", c.Filter.LiveState)
	}
	if c.Filter.ThreeLeg != "include" && c.Filter.ThreeLeg != "exclude" {
		return fmt.Errorf("filter.three_leg %q: want include or exclude", c.Filter.ThreeLeg)
	}
	if c.Filter.MinAgeSec < 0 {
		return errors.New("filter.min_age_sec must be >= 0")
	}
	b := c.Bankroll
	switch b.Mode {
	case "split":
		if b.Total <= 0 {
			return errors.New("bankroll.total must be > 0 in split mode")
		}
	case "maxleg":
		if b.MaxLeg <= 0 {
			return errors.New("bankroll.max_leg must be > 0 in maxleg mode")
		}
	default:
		return fmt.Errorf("bankroll.mode %q: want split or maxleg", b.Mode)
	}
	if b.Currency != "CZK" && b.Currency != "EUR" {
		return fmt.Errorf("bankroll.currency %q: want CZK or EUR", b.Currency)
	}
	if b.Rounding != 1 && b.Rounding != 10 && b.Rounding != 100 {
		return fmt.Errorf("bankroll.rounding %d: want 1, 10 or 100", b.Rounding)
	}
	return nil
}
