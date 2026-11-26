package accounts

import (
	"testing"
)

func TestBotLedger_Migration(t *testing.T) {
	s, _, _ := newTestService(t)
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version %d (%v), want %d", v, err, len(migrations))
	}
	for _, name := range []string{"bot_balances", "bot_accounts", "bot_ledger_legs"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s: %d %v", name, n, err)
		}
	}
}

// Every read is a row; the first one of an account is its point 0, never changed afterwards.
func TestBotBalance_InsertAndPointZero(t *testing.T) {
	s, _, _ := newTestService(t)
	u, _ := mustUser(t, s, "u@example.com", RoleUser)
	other, _ := mustUser(t, s, "o@example.com", RoleUser)
	rows := []BotBalanceRow{
		{UserID: u.UserID, CodeID: 1, WindowID: "tipsport-1", Bookmaker: "tipsport", Amount: 5000, At: 1000},
		{UserID: u.UserID, CodeID: 1, WindowID: "tipsport-1", Bookmaker: "tipsport", Amount: 4800.5, At: 2000},
		{UserID: u.UserID, CodeID: 1, WindowID: "betano-1", Bookmaker: "betano", Amount: 300, At: 1500},
		{UserID: u.UserID, CodeID: 2, WindowID: "tipsport-1", Bookmaker: "tipsport", Amount: 10, At: 1700},
		{UserID: other.UserID, CodeID: 3, WindowID: "fortuna-1", Bookmaker: "fortuna", Amount: 1, At: 1},
	}
	for _, r := range rows {
		if err := s.InsertBotBalance(r); err != nil {
			t.Fatal(err)
		}
	}
	acc, err := s.BotAccounts(u.UserID)
	if err != nil || len(acc) != 3 {
		t.Fatalf("accounts: %+v %v", acc, err)
	}
	want := BotAccountRow{UserID: u.UserID, CodeID: 1, WindowID: "tipsport-1", Bookmaker: "tipsport", StartAmount: 5000, StartAt: 1000}
	if acc[1] != want { // by code, window
		t.Fatalf("point 0 %+v, want %+v", acc[1], want)
	}
	latest, err := s.LatestBotBalances(u.UserID, 1)
	if err != nil || len(latest) != 2 || latest[0].WindowID != "betano-1" || latest[1].Amount != 4800.5 || latest[1].At != 2000 {
		t.Fatalf("latest of code 1: %+v %v", latest, err)
	}
	if all, err := s.LatestBotBalances(u.UserID, 0); err != nil || len(all) != 3 {
		t.Fatalf("latest of every code: %+v %v", all, err)
	}
	hist, err := s.BotBalances(u.UserID)
	if err != nil || len(hist) != 4 || hist[0].At != 1000 || hist[3].At != 2000 {
		t.Fatalf("history: %+v %v", hist, err)
	}
}

func ledgerLeg(user int64, order string, leg int) BotLedgerLeg {
	line := 2.5
	return BotLedgerLeg{OrderID: order, Leg: leg, UserID: user, CodeID: 1, WindowID: "tipsport-1", Bookmaker: "tipsport", Source: "auto",
		Match: "A – B", Market: "Total 2.5", MarketType: "TOTAL_GOALS", Line: &line, Label: "Over 2.5", Odds: 2.1, Stake: 100,
		PlacedAt: 5000, Result: "open", UpdatedAt: 5000}
}

func TestBotLedger_InsertIgnoreAndResult(t *testing.T) {
	s, _, _ := newTestService(t)
	u, _ := mustUser(t, s, "u@example.com", RoleUser)
	other, _ := mustUser(t, s, "o@example.com", RoleUser)
	first := ledgerLeg(u.UserID, "o1", 0)
	second := ledgerLeg(u.UserID, "o1", 1)
	second.Bookmaker, second.WindowID, second.Line, second.Voidable = "betano", "betano-1", nil, true
	if err := s.InsertBotLedgerLegs([]BotLedgerLeg{first, second}); err != nil {
		t.Fatal(err)
	}
	again := first
	again.Stake = 999
	if err := s.InsertBotLedgerLegs([]BotLedgerLeg{again}); err != nil {
		t.Fatal(err)
	}
	legs, err := s.BotLedgerLegs(u.UserID)
	if err != nil || len(legs) != 2 || legs[0].Stake != 100 || *legs[0].Line != 2.5 || legs[1].Line != nil || !legs[1].Voidable ||
		legs[0].Payout != nil || legs[0].SettledAt != nil || legs[0].ResultSource != "" || legs[0].TicketID != "" {
		t.Fatalf("legs: %+v %v", legs, err)
	}
	payout, at := 210.0, int64(9000)
	if ok, err := s.SetBotLedgerResult(other.UserID, "o1", 0, "won", &payout, &at, "manual", 9100); ok || err != nil {
		t.Fatalf("other user's leg: %v %v", ok, err)
	}
	if ok, err := s.SetBotLedgerResult(u.UserID, "o1", 0, "won", &payout, &at, "manual", 9100); !ok || err != nil {
		t.Fatalf("set: %v %v", ok, err)
	}
	legs, _ = s.BotLedgerLegs(u.UserID)
	if l := legs[0]; l.Result != "won" || *l.Payout != 210 || *l.SettledAt != 9000 || l.ResultSource != "manual" || l.UpdatedAt != 9100 {
		t.Fatalf("after set: %+v", l)
	}
	if ok, err := s.SetBotLedgerResult(u.UserID, "o1", 0, "open", nil, nil, "", 9200); !ok || err != nil {
		t.Fatalf("reset: %v %v", ok, err)
	}
	legs, _ = s.BotLedgerLegs(u.UserID)
	if l := legs[0]; l.Result != "open" || l.Payout != nil || l.SettledAt != nil || l.ResultSource != "" || l.UpdatedAt != 9200 {
		t.Fatalf("after reset: %+v", l)
	}
	if ok, _ := s.SetBotLedgerResult(u.UserID, "o1", 5, "won", &payout, &at, "manual", 1); ok {
		t.Fatal("unknown leg updated")
	}
}

// The orders the ledger reads: non-dry with a ledger row, ended outcome_unknown, or whose last flow is stuck.
func TestBotLedger_OrdersAndBackfillSource(t *testing.T) {
	s, _, _ := newTestService(t)
	u, _ := mustUser(t, s, "u@example.com", RoleUser)
	ins := func(id, state string, dry bool, legs, flow string) {
		t.Helper()
		if err := s.InsertBotOrder(BotOrderRow{ID: id, UserID: u.UserID, ArbKey: "k", State: state, DryRun: dry, InfoJSON: `{}`,
			LegsJSON: legs, FlowJSON: flow, CreatedAt: 1, OddsDrop: "strict"}); err != nil {
			t.Fatal(err)
		}
	}
	placed := `[{"bookmaker":"tipsport","placed":true},{"bookmaker":"betano","placed":false}]`
	ins("a-ledger", "placed", false, placed, "")
	ins("b-unknown", "outcome_unknown", false, `[]`, "")
	ins("c-stuck", "stopped", false, `[]`, `{"rule":"r","playbook":"p","phase":1,"stuck":{"at":1}}`)
	ins("d-dry", "outcome_unknown", true, placed, "")
	ins("e-plain", "stopped", false, `[{"bookmaker":"tipsport","placed":false}]`, `{"rule":"r","playbook":"p","phase":1,"stuck":null}`)
	if err := s.InsertBotLedgerLegs([]BotLedgerLeg{ledgerLeg(u.UserID, "a-ledger", 0)}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.BotLedgerOrders(u.UserID)
	if err != nil || len(rows) != 3 || rows[0].ID != "a-ledger" || rows[1].ID != "b-unknown" || rows[2].ID != "c-stuck" {
		t.Fatalf("ledger orders: %v %v", botOrderIDs(rows), err)
	}
	rows, err = s.BotOrdersWithPlacedLegs()
	if err != nil || len(rows) != 1 || rows[0].ID != "a-ledger" {
		t.Fatalf("with placed legs: %v %v", botOrderIDs(rows), err)
	}
}
