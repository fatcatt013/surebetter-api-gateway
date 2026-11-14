package accounts

import (
	"database/sql"
	"errors"
)

// BotSettings is a user's bots controller settings: the controller-wide mode, pause and limits.
// Kč amounts are whole crowns; 0 = no limit.
type BotSettings struct {
	DryRun         bool
	Paused         bool
	PausedReason   string
	MaxOrder       int64 // whole order (both legs)
	MaxLeg         int64 // one leg
	PerDay         int64 // live stake per Prague calendar day
	OrdersAtOnce   int   // 1–20
	PauseOnUnknown bool
	RulesOn        string // the rules switched on: a JSON array of rule ids, e.g. ["totals-over-first"]; "[]" = none
	OddsDrop       string // when a leg's odds drop: "strict" (stop the pair) or "rearb" (keep it while still an arb)
}

// DefaultBotSettings is what a user without a bot_settings row has: dry run, not paused, no limits,
// one order at once, pause when an outcome is unknown, no rule switched on, strict on an odds drop.
func DefaultBotSettings() BotSettings {
	return BotSettings{DryRun: true, OrdersAtOnce: 1, PauseOnUnknown: true, RulesOn: "[]", OddsDrop: "strict"}
}

// BotSettings returns userID's settings (the defaults when it has no row).
func (s *Service) BotSettings(userID int64) (BotSettings, error) {
	var st BotSettings
	err := s.db.QueryRow(`SELECT dry_run, paused, paused_reason, max_order, max_leg, per_day, orders_at_once, pause_on_unknown, rules_on, odds_drop
		FROM bot_settings WHERE user_id = ?`, userID).
		Scan(&st.DryRun, &st.Paused, &st.PausedReason, &st.MaxOrder, &st.MaxLeg, &st.PerDay, &st.OrdersAtOnce, &st.PauseOnUnknown, &st.RulesOn, &st.OddsDrop)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultBotSettings(), nil
	}
	return st, err
}

// SaveBotSettings stores userID's settings (all fields) with updated_at = now.
func (s *Service) SaveBotSettings(userID int64, st BotSettings) error {
	_, err := s.db.Exec(`INSERT INTO bot_settings (user_id, dry_run, paused, paused_reason, max_order, max_leg, per_day, orders_at_once,
			pause_on_unknown, rules_on, odds_drop, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET dry_run = excluded.dry_run, paused = excluded.paused, paused_reason = excluded.paused_reason,
			max_order = excluded.max_order, max_leg = excluded.max_leg, per_day = excluded.per_day,
			orders_at_once = excluded.orders_at_once, pause_on_unknown = excluded.pause_on_unknown, rules_on = excluded.rules_on,
			odds_drop = excluded.odds_drop, updated_at = excluded.updated_at`,
		userID, st.DryRun, st.Paused, st.PausedReason, st.MaxOrder, st.MaxLeg, st.PerDay, st.OrdersAtOnce, st.PauseOnUnknown, st.RulesOn, st.OddsDrop, s.nowMs())
	return err
}
