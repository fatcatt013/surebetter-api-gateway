package accounts

import (
	"testing"
)

func botOrderIDs(rows []BotOrderRow) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func TestBotOrders_Lifecycle(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	code, _ := s.CreateCode(user.UserID, "Home PC", KindBot)

	w := BotOrderRow{ID: "000000000000000a", UserID: user.UserID, ArbKey: "k1", State: "waiting", DryRun: true, Stake: 202,
		InfoJSON: `{"match":"A – B"}`, LegsJSON: `[{"bookmaker":"tipsport"}]`, CreatedAt: 1000}
	if err := s.InsertBotOrder(w); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBotOrderSent(w.ID, code.ID, false, `[{"bookmaker":"tipsport","windowId":"tipsport-2"}]`, 1500); err != nil {
		t.Fatal(err)
	}
	if err := s.EndBotOrder(w.ID, "placed", "", `[{"bookmaker":"tipsport","windowId":"tipsport-2","step":"placeBet"}]`, 202, 9000); err != nil {
		t.Fatal(err)
	}
	// A row that already ended is not changed again.
	if err := s.EndBotOrder(w.ID, "stopped", "late", `[]`, 1, 9999); err != nil {
		t.Fatal(err)
	}
	rows, err := s.EndedBotOrders(user.UserID, 0, 2000, 500)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ended: %+v %v", rows, err)
	}
	want := BotOrderRow{ID: w.ID, UserID: user.UserID, ArbKey: "k1", CodeID: code.ID, Agent: "Home PC", State: "placed", DryRun: false, Stake: 202,
		InfoJSON: `{"match":"A – B"}`, LegsJSON: `[{"bookmaker":"tipsport","windowId":"tipsport-2","step":"placeBet"}]`,
		CreatedAt: 1000, SentAt: 1500, EndedAt: 9000}
	if rows[0] != want {
		t.Fatalf("row:\n got %+v\nwant %+v", rows[0], want)
	}
}

func TestBotOrders_EndedListAndDayStats(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)
	add := func(id string, userID int64, state string, dryRun bool, stake, created int64) {
		t.Helper()
		if err := s.InsertBotOrder(BotOrderRow{ID: id, UserID: userID, ArbKey: "k" + id, State: "waiting", DryRun: dryRun, Stake: stake,
			InfoJSON: "{}", LegsJSON: "[]", CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		switch state {
		case "running":
			if err := s.MarkBotOrderSent(id, 0, dryRun, "[]", created+1); err != nil {
				t.Fatal(err)
			}
		case "placed", "stopped", "outcome_unknown":
			if err := s.EndBotOrder(id, state, "", "[]", stake, created+5); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Day = [1000, 2000).
	add("a1", user.UserID, "placed", false, 100, 999)           // the day before
	add("a2", user.UserID, "placed", false, 200, 1000)          // live, counts
	add("a3", user.UserID, "stopped", false, 400, 1100)         // stopped: its stake is its placed legs, counts
	add("a4", user.UserID, "outcome_unknown", false, 800, 1200) // counts
	add("a5", user.UserID, "placed", true, 1600, 1300)          // dry run: no live stake
	add("a6", user.UserID, "running", false, 3200, 1400)        // counts
	add("a7", user.UserID, "waiting", false, 6400, 1500)        // counts
	add("a8", user.UserID, "placed", false, 12800, 2000)        // the next day
	add("b1", other.UserID, "placed", false, 25600, 1500)       // another user

	st, err := s.BotOrderDay(user.UserID, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if st != (BotOrderDayStats{Orders: 6, Placed: 2, LiveStake: 200 + 400 + 800 + 3200 + 6400}) {
		t.Fatalf("day stats: %+v", st)
	}
	rows, err := s.EndedBotOrders(user.UserID, 1000, 2000, 500)
	if err != nil {
		t.Fatal(err)
	}
	if got := botOrderIDs(rows); len(got) != 4 || got[0] != "a5" || got[1] != "a4" || got[2] != "a3" || got[3] != "a2" {
		t.Fatalf("ended, newest first: %v", got)
	}
	if rows, _ := s.EndedBotOrders(user.UserID, 1000, 2000, 2); len(rows) != 2 || rows[0].ID != "a5" {
		t.Fatalf("limit 2: %v", botOrderIDs(rows))
	}
	if rows[3].Agent != "" {
		t.Fatalf("an order never sent has no agent: %+v", rows[3])
	}
}

func TestBotOrders_CloseOpen(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	for _, id := range []string{"w1", "r1", "p1"} {
		if err := s.InsertBotOrder(BotOrderRow{ID: id, UserID: user.UserID, ArbKey: id, State: "waiting", DryRun: true, Stake: 1,
			InfoJSON: "{}", LegsJSON: "[]", CreatedAt: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkBotOrderSent("r1", 0, true, "[]", 1001); err != nil {
		t.Fatal(err)
	}
	if err := s.EndBotOrder("p1", "placed", "", "[]", 1, 1002); err != nil {
		t.Fatal(err)
	}
	// A live waiting order placed nothing: closed stopped, its stake becomes 0 (the day sum counts stopped orders).
	if err := s.InsertBotOrder(BotOrderRow{ID: "w2", UserID: user.UserID, ArbKey: "w2", State: "waiting", DryRun: false, Stake: 500,
		InfoJSON: "{}", LegsJSON: "[]", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	closed, err := s.CloseOpenBotOrders("Gateway restarted", 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 3 || closed[0].ID != "r1" || closed[0].State != "outcome_unknown" || closed[1].ID != "w1" || closed[1].State != "stopped" ||
		closed[2].ID != "w2" || closed[2].State != "stopped" {
		t.Fatalf("closed: %+v", closed)
	}
	if st, err := s.BotOrderDay(user.UserID, 0, 2000); err != nil || st.LiveStake != 0 {
		t.Fatalf("a live waiting order closed stopped still counts: %+v %v", st, err)
	}
	rows, _ := s.EndedBotOrders(user.UserID, 0, 2000, 500)
	states := map[string]string{}
	for _, r := range rows {
		states[r.ID] = r.State + "/" + r.Reason
		if r.ID != "p1" && r.EndedAt != 5000 {
			t.Fatalf("%s ended_at %d", r.ID, r.EndedAt)
		}
	}
	if states["w1"] != "stopped/Gateway restarted" || states["w2"] != "stopped/Gateway restarted" || states["r1"] != "outcome_unknown/Gateway restarted" || states["p1"] != "placed/" {
		t.Fatalf("states: %v", states)
	}
	if again, err := s.CloseOpenBotOrders("Gateway restarted", 6000); err != nil || len(again) != 0 {
		t.Fatalf("second close: %+v %v", again, err)
	}
}

// The last flow is stored while the order runs; a final state (cashed_out too) stores the real stake.
func TestBotOrders_FlowStakeAndCashedOut(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	if err := s.InsertBotOrder(BotOrderRow{ID: "00000000000000c1", UserID: user.UserID, ArbKey: "k", State: "waiting", DryRun: false, Stake: 2000,
		InfoJSON: "{}", LegsJSON: "[]", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	// Not running yet: the flow is not stored.
	if err := s.SetBotOrderFlow("00000000000000c1", `{"phase":0}`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBotOrderSent("00000000000000c1", 0, false, "[]", 1001); err != nil {
		t.Fatal(err)
	}
	flow := `{"rule":"totals-over-first","playbook":"overFirst","phase":1,"stuck":null}`
	if err := s.SetBotOrderFlow("00000000000000c1", flow); err != nil {
		t.Fatal(err)
	}
	if err := s.EndBotOrder("00000000000000c1", "cashed_out", "Over cashed out · 955 Kč of 1 000", `[{"bookmaker":"tipsport","placed":true}]`, 1000, 1500); err != nil {
		t.Fatal(err)
	}
	// Ended: neither the flow nor the row changes any more.
	if err := s.SetBotOrderFlow("00000000000000c1", `{"phase":2}`); err != nil {
		t.Fatal(err)
	}
	if err := s.EndBotOrder("00000000000000c1", "placed", "", "[]", 1, 1600); err != nil {
		t.Fatal(err)
	}
	rows, err := s.EndedBotOrders(user.UserID, 0, 2000, 500)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ended: %+v %v", rows, err)
	}
	if r := rows[0]; r.State != "cashed_out" || r.Reason != "Over cashed out · 955 Kč of 1 000" || r.Stake != 1000 || r.FlowJSON != flow ||
		r.LegsJSON != `[{"bookmaker":"tipsport","placed":true}]` || r.EndedAt != 1500 {
		t.Fatalf("row: %+v", r)
	}
	// A cashed-out order is not placed, but its live stake counts for the day.
	if st, err := s.BotOrderDay(user.UserID, 0, 2000); err != nil || st != (BotOrderDayStats{Orders: 1, Placed: 0, LiveStake: 1000}) {
		t.Fatalf("day: %+v %v", st, err)
	}
}
