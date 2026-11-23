package main

import (
	"context"
	"testing"
)

func TestSetupTelegramAlerts_OffWithoutTokenOrPoll(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no token":    {TelegramChatID: "-5306930735", TelegramPoll: true},
		"poll not on": {TelegramToken: "T", TelegramChatID: "-5306930735"},
	} {
		if tg, out := setupTelegramAlerts(context.Background(), cfg, nil); tg != nil || out != nil {
			t.Fatalf("%s: %v %v", name, tg, out)
		}
	}
}

// The token and TELEGRAM_POLL=1 are enough (no TELEGRAM_CHAT_ID); APP_PUBLIC_URL goes into the messages' links.
func TestSetupTelegramAlerts_TokenAndPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the outbox and the poller stop at once
	tg, out := setupTelegramAlerts(ctx, Config{TelegramToken: "T", TelegramPoll: true, TelegramTZ: "Nowhere/Nothing", TelegramRatePerMin: 18,
		AppPublicURL: "https://sb.example"}, nil)
	if tg == nil || out == nil || tg.linker == nil || tg.loc != prague || tg.appURL != "https://sb.example" {
		t.Fatalf("token and poll: %+v %v", tg, out)
	}
}

func TestConfig_TelegramPollAndAppPublicURL(t *testing.T) {
	if c := configFromEnv(); c.TelegramPoll || c.AppPublicURL != "" {
		t.Fatalf("defaults: poll %v url %q", c.TelegramPoll, c.AppPublicURL)
	}
	t.Setenv("TELEGRAM_POLL", "1")
	t.Setenv("APP_PUBLIC_URL", " https://sb.example/ ")
	if c := configFromEnv(); !c.TelegramPoll || c.AppPublicURL != "https://sb.example" {
		t.Fatalf("set: poll %v url %q", c.TelegramPoll, c.AppPublicURL)
	}
	t.Setenv("TELEGRAM_POLL", "true") // only 1 turns it on
	if configFromEnv().TelegramPoll {
		t.Fatal("TELEGRAM_POLL=true turned the poller on")
	}
}
