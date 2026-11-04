package telegram

import (
	"encoding/json"
	"testing"
)

func TestValidate_AcceptsValidAndDisabled(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := (BotConfig{}).Validate(); err != nil {
		t.Fatalf("disabled empty config rejected: %v", err)
	}
}

func TestValidate_RejectsBadFields(t *testing.T) {
	cases := map[string]func(c *BotConfig){
		"no preset":    func(c *BotConfig) { c.Preset = nil },
		"empty preset": func(c *BotConfig) { c.Preset = &PresetRef{} },
		"live state":   func(c *BotConfig) { c.Filter.LiveState = "sometimes" },
		"min age":      func(c *BotConfig) { c.Filter.MinAgeSec = -1 },
		"mode":         func(c *BotConfig) { c.Bankroll.Mode = "yolo" },
		"split total":  func(c *BotConfig) { c.Bankroll.Total = 0 },
		"maxleg max":   func(c *BotConfig) { c.Bankroll.Mode = "maxleg"; c.Bankroll.MaxLeg = 0 },
		"currency":     func(c *BotConfig) { c.Bankroll.Currency = "USD" },
		"rounding":     func(c *BotConfig) { c.Bankroll.Rounding = 5 },
	}
	for name, mutate := range cases {
		c := validConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}

func TestNormalize_FillsBlockedMarkets(t *testing.T) {
	var c BotConfig
	c.Normalize()
	raw, _ := json.Marshal(c.Filter)
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if _, ok := back["blocked_markets"].(map[string]any); !ok {
		t.Fatalf("blocked_markets = %v, want {}", back["blocked_markets"])
	}
}

func TestBotConfig_JSONRoundTrip_KeepsNullVsEmptyBookmakers(t *testing.T) {
	var c BotConfig
	_ = json.Unmarshal([]byte(`{"filter":{"bookmakers":[],"sports":null}}`), &c)
	if c.Filter.Bookmakers == nil || len(c.Filter.Bookmakers) != 0 {
		t.Fatalf("[] must decode to an empty, non-nil slice")
	}
	if c.Filter.Sports != nil {
		t.Fatalf("null must decode to nil")
	}
}
