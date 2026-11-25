// Package telegram posts arbitrage opportunities to a Telegram chat and keeps each message
// in step with its arb (edit on change, GONE on removal, deleted a while later). See
// surebetter-common/docs/superpowers/specs/2026-11-04-telegram-bot-design.md.
package telegram

import "github.com/surebetter/api-gateway/internal/arbfilter"

// Arb is the gateway's ArbOpportunity (cmd/gateway/main.go) reduced to the fields the notifier
// reads; cmd/gateway/telegram.go converts. The JSON tags are only used to persist the last-seen
// arb in telegram:messages so a GONE edit can still be rendered after a restart. The type lives in
// internal/arbfilter (shared with the bots' auto mode).
type Arb = arbfilter.Arb

// Stake is one leg of an arb.
type Stake = arbfilter.Stake

// Quote is another bookmaker's price for a stake's outcome; the fields mean what Stake's do.
type Quote = arbfilter.Quote
