package main

import (
	"encoding/json"
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

// ledgerLeg is leg i (l) of order orderID as a ledger row: the leg's window, bookmaker, label, odds and stake (after a
// final event: what the agent reported placing), the order's source, match and market, placed at placedAt, open.
func ledgerLeg(orderID string, userID, codeID int64, source string, info orderInfo, i int, l storedLeg, placedAt int64) accounts.BotLedgerLeg {
	return accounts.BotLedgerLeg{OrderID: orderID, Leg: i, UserID: userID, CodeID: codeID, WindowID: l.WindowID, Bookmaker: l.Bookmaker,
		Source: rowSource(source), Match: info.Match, Market: info.Market, MarketType: info.MarketType, Line: info.Line, Label: l.Label,
		Odds: l.Odds, Stake: l.Stake, PlacedAt: placedAt, Voidable: voidable(info.MarketType, info.Line), Result: "open", UpdatedAt: placedAt}
}

// placedLedgerLegs are the ledger rows of the legs reported placed of an order.
func placedLedgerLegs(orderID string, userID, codeID int64, source string, info orderInfo, legs []storedLeg, placedAt int64) []accounts.BotLedgerLeg {
	var out []accounts.BotLedgerLeg
	for i, l := range legs {
		if l.Placed != nil && *l.Placed {
			out = append(out, ledgerLeg(orderID, userID, codeID, source, info, i, l, placedAt))
		}
	}
	return out
}

// ledgerOnEndLocked queues the ledger rows of a live order that ended with legs reported placed (placed_at = now, the
// final event's time), after its row's end. A dry run adds none. b.mu must be held.
func (b *botHub) ledgerOnEndLocked(o *botOrder, now int64, fx *effects) {
	if o.dryRun || b.acc == nil {
		return
	}
	legs := placedLedgerLegs(o.id, o.userID, o.codeID, o.sourceOrApp(), o.info, o.legs, now)
	if len(legs) > 0 {
		fx.write(func() error { return b.acc.InsertBotLedgerLegs(legs) })
	}
}

// orderInfoOf decodes a stored order's info and legs (unreadable → empty).
func orderInfoOf(r accounts.BotOrderRow) (orderInfo, []storedLeg) {
	var info orderInfo
	var legs []storedLeg
	json.Unmarshal([]byte(r.InfoJSON), &info)
	json.Unmarshal([]byte(r.LegsJSON), &legs)
	return info, legs
}

// orderPlacedAt is a stored order's time for legs that have no event time: its end, else its send.
func orderPlacedAt(r accounts.BotOrderRow) int64 {
	if r.EndedAt > 0 {
		return r.EndedAt
	}
	return r.SentAt
}

// backfillLedger gives every stored live order with legs reported placed its ledger rows (placed_at = the order's end,
// else its send). Rows already there are kept, so running it at every start adds each leg once. Call at start, after
// recoverOrders.
func (b *botHub) backfillLedger() {
	if b.acc == nil {
		return
	}
	rows, err := b.acc.BotOrdersWithPlacedLegs()
	if err != nil {
		log.Error().Err(err).Msg("bot ledger backfill read failed")
		return
	}
	var legs []accounts.BotLedgerLeg
	for _, r := range rows {
		info, stored := orderInfoOf(r)
		legs = append(legs, placedLedgerLegs(r.ID, r.UserID, r.CodeID, r.Source, info, stored, orderPlacedAt(r))...)
	}
	b.dbMu.Lock()
	err = b.acc.InsertBotLedgerLegs(legs)
	b.dbMu.Unlock()
	if err != nil {
		log.Error().Err(err).Msg("bot ledger backfill failed")
		return
	}
	log.Info().Int("orders", len(rows)).Msg("bot ledger backfilled")
}
