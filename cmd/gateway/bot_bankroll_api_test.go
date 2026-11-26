package main

import (
	"database/sql"
	"strconv"
	"testing"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// balanceRows is every bot_balances row of window as "amount@at", oldest first.
func balanceRows(t *testing.T, db *sql.DB, window string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT amount, at FROM bot_balances WHERE window_id = ? ORDER BY at, rowid`, window)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var amount float64
		var at int64
		rows.Scan(&amount, &at)
		out = append(out, strconv.FormatFloat(amount, 'f', -1, 64)+"@"+strconv.FormatInt(at, 10))
	}
	return out
}

// A balance read is stored when the window has none yet, when the amount changed (at 2 decimals), or when the last
// stored row is at least an hour older than the read; null and protocol-6 balances are never stored. The first stored
// read is the account's point 0, never changed. A new hub over the same database does not store the last read again.
func TestBankroll_BalanceRows(t *testing.T) {
	srv, svc, bots, db := newBotServerDB(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	userID := userIDOf(t, svc, token)
	code, codeID := newBotCode(t, srv, token, "PC")
	dialBot(t, srv, code)
	waitBot(t, bots, userID, true)
	const t0, minute, hour = int64(1795710000000), int64(60000), int64(3600000)
	post := func(proto int, balance string) {
		t.Helper()
		if s, b := postStatus(t, srv, code, protoStatus(proto, winBal("tipsport", 1, "idle", balance), winBal("betano", 1, "idle", "null"))); s != 204 {
			t.Fatalf("status: %d %s", s, b)
		}
	}
	steps := []struct {
		proto   int
		balance string
		want    int // rows of tipsport-1 after it
	}{
		{7, bal(5000, t0), 1},                  // first
		{7, bal(5000, t0+10*minute), 1},        // same, within the hour
		{7, bal(5000.004, t0+20*minute), 1},    // same at 2 decimals
		{7, "null", 1},                         // unknown
		{6, bal(9999, t0+30*minute), 1},        // protocol 6: no balance
		{7, bal(4800, t0+40*minute), 2},        // changed
		{7, bal(4800, t0+40*minute+hour-1), 2}, // same, 1 h − 1 ms after the last row
		{7, bal(4800, t0+40*minute+hour), 3},   // same, 1 h after
		{7, bal(4800, t0+40*minute+hour), 3},   // the same read again
	}
	for i, s := range steps {
		post(s.proto, s.balance)
		if got := balanceRows(t, db, "tipsport-1"); len(got) != s.want {
			t.Fatalf("step %d (%s): rows %v, want %d", i, s.balance, got, s.want)
		}
	}
	if got := balanceRows(t, db, "betano-1"); len(got) != 0 {
		t.Fatalf("betano-1 (always null): %v", got)
	}
	accs, err := svc.BotAccounts(userID)
	if err != nil || len(accs) != 1 || accs[0].CodeID != codeID || accs[0].WindowID != "tipsport-1" || accs[0].Bookmaker != "tipsport" ||
		accs[0].StartAmount != 5000 || accs[0].StartAt != t0 {
		t.Fatalf("point 0: %+v %v", accs, err)
	}

	// A new process: the marks come from the database (no row for the same amount within the hour).
	b2 := newBotHub(newConnRegistry(), svc)
	ws := []botWindow{{ID: "tipsport-1", Bookmaker: "tipsport", Balance: &botBalance{Amount: 4800, At: t0 + 40*minute + hour + 5*minute}}}
	record := func() {
		b2.loadBalanceMarks(userID, codeID)
		b2.mu.Lock()
		fx := &effects{}
		b2.recordBalancesLocked(userID, codeID, 7, ws, fx)
		b2.release(fx)
	}
	record()
	if got := balanceRows(t, db, "tipsport-1"); len(got) != 3 {
		t.Fatalf("after a restart: %v", got)
	}
	ws[0].Balance = &botBalance{Amount: 4700, At: t0 + 2*hour}
	record()
	if got := balanceRows(t, db, "tipsport-1"); len(got) != 4 || got[3] != "4700@"+strconv.FormatInt(t0+2*hour, 10) {
		t.Fatalf("a change after a restart: %v", got)
	}
	if accs, _ := svc.BotAccounts(userID); len(accs) != 1 || accs[0].StartAmount != 5000 {
		t.Fatalf("point 0 changed: %+v", accs)
	}
}
