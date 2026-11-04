package telegram

import "time"

// SampleArb is the fixed arb the test message (POST /api/telegram/test) and the render golden
// tests use. First detection is 38s before now; the match started 2m05s before that.
// Odds 2.10/2.02 → 1/2.10+1/2.02 = 0.9712 → +2.96%; stake split 49.03/50.97.
func SampleArb(now time.Time) Arb {
	first := now.Add(-38 * time.Second)
	return Arb{
		EventID:    "tennis-live:sample",
		Sport:      "tennis-live",
		MarketType: "GAME_HANDICAP",
		Line:       3.5,
		ProfitPct:  2.96,
		Stakes: []Stake{
			{Bookmaker: "betano", OutcomeID: "cody benton:+3.5", Label: "Cody Benton +3.5", DecimalOdds: 2.10, StakePct: 49.03, MatchURL: "https://www.betano.cz/"},
			{Bookmaker: "tipsport", OutcomeID: "cody benton:-3.5", Label: "Kai-Luca Ampaw -3.5", DecimalOdds: 2.02, StakePct: 50.97, MatchURL: "https://www.tipsport.cz/"},
		},
		DetectedAt:      now.UnixMilli(),
		FirstDetectedAt: first.UnixMilli(),
		LastChangedAt:   first.UnixMilli(),
		NameHome:        "Cody Benton",
		NameAway:        "Kai-Luca Ampaw",
		League:          "ITF M25 Monastir",
		MarketName:      "Game handicap",
		StartTime:       first.Add(-(2*time.Minute + 5*time.Second)).UnixMilli(),
	}
}
