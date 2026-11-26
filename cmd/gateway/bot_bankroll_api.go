package main

import (
	"math"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// The bot bankroll's hub side (bankroll Phase 1 contract): balance reads stored from the agents' statuses, ledger rows
// for the legs that were really placed, the backfill at start and the /api/me/bot/bankroll* routes. The money logic is
// in bot_bankroll.go.

// balanceEveryMs: an unchanged balance is stored again once its last stored row is this old (by the reads' times).
const balanceEveryMs = 3600 * 1000

// balMark is an account's last stored balance read.
type balMark struct {
	amount float64
	at     int64
}

// loadBalanceMarks reads codeID's last stored balance per window into b.balMarks, once per code (call without b.mu).
// A failed read is logged and tried again on the next status.
func (b *botHub) loadBalanceMarks(userID, codeID int64) {
	if b.acc == nil {
		return
	}
	b.mu.Lock()
	loaded := b.balLoaded[codeID]
	b.mu.Unlock()
	if loaded {
		return
	}
	rows, err := b.acc.LatestBotBalances(userID, codeID)
	if err != nil {
		log.Error().Err(err).Int64("code", codeID).Msg("bot balances read failed")
		return
	}
	b.mu.Lock()
	if !b.balLoaded[codeID] {
		for _, r := range rows {
			k := acctKey{r.CodeID, r.WindowID}
			if _, ok := b.balMarks[k]; !ok {
				b.balMarks[k] = balMark{r.Amount, r.At}
			}
		}
		b.balLoaded[codeID] = true
	}
	b.mu.Unlock()
}

// cents is v at 2 decimals, for comparing amounts.
func cents(v float64) int64 { return int64(math.Round(v * 100)) }

// recordBalancesLocked stores the balance reads of a protocol-7 status: a window's read becomes a row (with the
// account's point 0 the first time) when the window has no stored row yet, when the amount differs from the last
// stored one at 2 decimals, or when the last stored row is at least an hour older than the read. A read not newer
// than the last stored one, a null balance and an agent below protocol 7 store nothing. b.mu must be held.
func (b *botHub) recordBalancesLocked(userID, codeID int64, proto int, windows []botWindow, fx *effects) {
	if proto < balanceProto || b.acc == nil {
		return
	}
	for _, w := range windows {
		if w.Balance == nil || w.Balance.Null {
			continue
		}
		k := acctKey{codeID, w.ID}
		amount, at := w.Balance.Amount, w.Balance.At
		if m, ok := b.balMarks[k]; ok && (at <= m.at || (cents(amount) == cents(m.amount) && at-m.at < balanceEveryMs)) {
			continue
		}
		b.balMarks[k] = balMark{amount, at}
		row := accounts.BotBalanceRow{UserID: userID, CodeID: codeID, WindowID: w.ID, Bookmaker: w.Bookmaker, Amount: amount, At: at}
		fx.write(func() error { return b.acc.InsertBotBalance(row) })
	}
}
