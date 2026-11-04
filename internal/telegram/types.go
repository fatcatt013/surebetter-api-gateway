// Package telegram posts arbitrage opportunities to a Telegram chat and keeps each message
// in step with its arb (edit on change, GONE on removal). See
// surebetter-common/docs/superpowers/specs/2026-11-04-telegram-bot-design.md.
package telegram

// Arb is the gateway's ArbOpportunity (cmd/gateway/main.go) reduced to the fields the notifier
// reads; cmd/gateway/telegram.go converts. The JSON tags are only used to persist the last-seen
// arb in telegram:messages so a GONE edit can still be rendered after a restart.
type Arb struct {
	EventID         string  `json:"event_id"`
	Sport           string  `json:"sport"`
	MarketType      string  `json:"market_type"`
	Line            float64 `json:"line"`
	ProfitPct       float64 `json:"profit_pct"`
	Stakes          []Stake `json:"stakes"`
	DetectedAt      int64   `json:"detected_at"`
	FirstDetectedAt int64   `json:"first_detected_at"`
	LastChangedAt   int64   `json:"last_changed_at"`
	NameHome        string  `json:"name_home"`
	NameAway        string  `json:"name_away"`
	League          string  `json:"league"`
	MarketName      string  `json:"market_name"`
	StartTime       int64   `json:"start_time"`
}

// Stake is one leg of an arb.
type Stake struct {
	Bookmaker   string  `json:"bookmaker"`
	OutcomeID   string  `json:"outcome_id"`
	Label       string  `json:"label"`
	DecimalOdds float64 `json:"decimal_odds"`
	StakePct    float64 `json:"stake_pct"` // percent, e.g. 49.03
	MatchURL    string  `json:"match_url"`
}

// firstSeen is the ms epoch the arb was first detected (first_detected_at, else detected_at, else 0).
func (a Arb) firstSeen() int64 {
	if a.FirstDetectedAt != 0 {
		return a.FirstDetectedAt
	}
	return a.DetectedAt
}
