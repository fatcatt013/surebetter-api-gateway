package main

import (
	"bytes"
	"encoding/json"
)

// Each window's balance (bot balance contract 2026-11-26): a protocol-7 agent reports the money its bookmaker's header
// shows for every window; the window pick sends a leg only to a window whose fresh balance covers the stake. Older
// agents are exempt.

// balanceProto is the agent protocol that reports balances; an agent below it is exempt from the check.
const balanceProto = 7

// balanceFreshMs is how old (ms) a balance read may be and still count (the app uses the same constant).
const balanceFreshMs = 720000

// botBalance is a window's last balance read: amount in Kč, at = Unix ms when the read finished; Null = the agent
// sent null (no read). On a botWindow a nil *botBalance means "not supported" and the field is left out, so the app
// tells absent (an older agent), null (unknown) and an object (a read) apart.
type botBalance struct {
	Amount float64
	At     int64
	Null   bool
}

type botBalanceJSON struct {
	Amount float64 `json:"amount"`
	At     int64   `json:"at"`
}

func (b botBalance) MarshalJSON() ([]byte, error) {
	if b.Null {
		return []byte("null"), nil
	}
	return json.Marshal(botBalanceJSON{b.Amount, b.At})
}

// UnmarshalJSON reads an object (a JSON null on a *botBalance field leaves it nil: balancesOf makes it Null).
func (b *botBalance) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*b = botBalance{Null: true}
		return nil
	}
	var v botBalanceJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*b = botBalance{Amount: v.Amount, At: v.At}
	return nil
}

// validBalance: none, null, or an amount ≥ 0 read at a time ≥ 0.
func validBalance(b *botBalance) bool { return b == nil || b.Null || (b.Amount >= 0 && b.At >= 0) }

// balancesOf sets ws's balances for an agent of protocol proto: below balanceProto no window has one (the field is
// left out); from it a window without one is null (unknown).
func balancesOf(ws []botWindow, proto int) {
	for i := range ws {
		switch {
		case proto < balanceProto:
			ws[i].Balance = nil
		case ws[i].Balance == nil:
			ws[i].Balance = &botBalance{Null: true}
		}
	}
}
