// Package arbfilter is the app's arb filter, re-pick and stake sizing ported to Go, shared by the
// Telegram arbs bot (internal/telegram) and the bots' automatic mode (cmd/gateway bot_auto*.go).
// Ports: filterArbs steps 2–7 (surebetter-frontend util/apply-filters.ts), repickArb (util/repick.ts)
// and computeStakes / losesAfterRounding (util/bankroll.ts). Keep each in step with the app.
package arbfilter

// Arb is the gateway's ArbOpportunity (cmd/gateway/main.go) reduced to the fields the filter, the
// re-pick, the stakes and the Telegram notifier read. The JSON tags are what the Telegram notifier
// persists (telegram:messages); the fields the Telegram path never sets are omitempty, so that JSON
// is unchanged.
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
	// MatchState, MatchStateText, MatchStateSince: the match's state ("running", "break", "interrupted",
	// "unknown"; "" = unknown), the book's own text and since when (ms) — the arb's (the engine's pick among the
	// stakes; Repick picks again among the re-picked stakes). Information only: no filter step reads them.
	MatchState      string `json:"match_state,omitempty"`
	MatchStateText  string `json:"match_state_text,omitempty"`
	MatchStateSince int64  `json:"match_state_since,omitempty"`
}

// Stake is one leg of an arb.
type Stake struct {
	Bookmaker   string  `json:"bookmaker"`
	OutcomeID   string  `json:"outcome_id"`
	Label       string  `json:"label"`
	DecimalOdds float64 `json:"decimal_odds"`
	StakePct    float64 `json:"stake_pct"` // percent, e.g. 49.03
	MatchURL    string  `json:"match_url"`
	MarketName  string  `json:"market_name"` // the bookmaker's own market label for this leg
	// SourceID, SourceMarketRef and FairOdds are what a bot order needs of the leg (the
	// bookmaker's selection id, its market container id, the consensus fair odds; nil = none).
	// The Telegram path leaves them empty.
	SourceID        string   `json:"source_id,omitempty"`
	SourceMarketRef string   `json:"source_market_ref,omitempty"`
	FairOdds        *float64 `json:"fair_odds,omitempty"`
	// Alternatives: the other bookmakers' prices for this outcome, best first (the engine's
	// stakes[].alternatives). Read only by Repick; a re-picked arb carries none.
	Alternatives []Quote `json:"alternatives,omitempty"`
	// MatchState, MatchStateText, MatchStateSince: the match's state at this leg's bookmaker (see Arb).
	MatchState      string `json:"match_state,omitempty"`
	MatchStateText  string `json:"match_state_text,omitempty"`
	MatchStateSince int64  `json:"match_state_since,omitempty"`
}

// Quote is another bookmaker's price for a stake's outcome; the fields mean what Stake's do.
type Quote struct {
	Bookmaker       string   `json:"bookmaker"`
	Label           string   `json:"label"`
	DecimalOdds     float64  `json:"decimal_odds"`
	MatchURL        string   `json:"match_url"`
	MarketName      string   `json:"market_name"`
	SourceID        string   `json:"source_id,omitempty"`
	SourceMarketRef string   `json:"source_market_ref,omitempty"`
	FairOdds        *float64 `json:"fair_odds,omitempty"`
	MatchState      string   `json:"match_state,omitempty"`
	MatchStateText  string   `json:"match_state_text,omitempty"`
	MatchStateSince int64    `json:"match_state_since,omitempty"`
}

// FirstSeen is the ms epoch the arb was first detected (first_detected_at, else detected_at, else 0).
func (a Arb) FirstSeen() int64 {
	if a.FirstDetectedAt != 0 {
		return a.FirstDetectedAt
	}
	return a.DetectedAt
}

// Filter is the app's FilterState as a server applies it (the Telegram bot's preset, the bots'
// auto settings). `markets` is pre-resolved by the app into BlockedMarkets (base sport → canonical
// market types it blocks); `sort` is dropped. A nil slice means "don't constrain"; an empty,
// non-nil Bookmakers slice keeps nothing (as in the UI).
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

// Bankroll mirrors the app's BankrollSettings (snake_case).
type Bankroll struct {
	Mode       string  `json:"mode"` // split | maxleg
	Total      float64 `json:"total"`
	MaxLeg     float64 `json:"max_leg"`
	Currency   string  `json:"currency"` // CZK | EUR
	AnchorBook *string `json:"anchor_book"`
	Rounding   int     `json:"rounding"` // 1 | 10 | 100
}
