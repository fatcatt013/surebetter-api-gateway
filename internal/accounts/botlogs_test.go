package accounts

import (
	"slices"
	"strings"
	"testing"
)

// logIDs lists the ids of logs, in order.
func logIDs(logs []BotLog) []int64 {
	ids := []int64{}
	for _, l := range logs {
		ids = append(ids, l.ID)
	}
	return ids
}

func TestBotLogs_AddQueryPrune(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)
	home, _ := s.CreateCode(user.UserID, "Home PC", KindBot)
	vps, _ := s.CreateCode(user.UserID, "VPS Praha", KindBot)
	otherBot, _ := s.CreateCode(other.UserID, "Other", KindBot)

	if err := s.AddBotLogs(user.UserID, home.ID, []BotLogLine{
		{TS: 1000, Level: "info", Step: "agent", Msg: "agent connected"},                                                    // id 1
		{TS: 2000, Level: "info", WindowID: "fortuna-1", OrderID: "c81e55a0deadbeef", Step: "navigate", Msg: "Leg started"}, // id 2
		{TS: 3000, Level: "warn", WindowID: "fortuna-1", OrderID: "c81e55a0deadbeef", Step: "checkOdds",
			Msg: "odds dropped 2.10 → 2.05 · pair stopped", Detail: `{"onSlip":2.05,"ordered":2.1}`}, // id 3
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBotLogs(user.UserID, vps.ID, []BotLogLine{
		{TS: 4000, Level: "error", WindowID: "tipsport-1", Step: "placeBet", Msg: "outcome unknown"}, // id 4
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBotLogs(other.UserID, otherBot.ID, []BotLogLine{{TS: 5000, Level: "error", Msg: "not yours"}}); err != nil { // id 5
		t.Fatal(err)
	}

	all, err := s.QueryBotLogs(user.UserID, BotLogFilter{})
	if err != nil || !slices.Equal(logIDs(all), []int64{4, 3, 2, 1}) {
		t.Fatalf("all, newest first: %v %v", logIDs(all), err)
	}
	if l := all[1]; l.AgentID != home.ID || l.Agent != "Home PC" || l.TS != 3000 || l.Level != "warn" || l.WindowID != "fortuna-1" ||
		l.OrderID != "c81e55a0deadbeef" || l.Step != "checkOdds" || l.Msg != "odds dropped 2.10 → 2.05 · pair stopped" ||
		string(l.Detail) != `{"onSlip":2.05,"ordered":2.1}` {
		t.Fatalf("line 3: %+v", l)
	}
	if all[0].Agent != "VPS Praha" || all[0].Detail != nil {
		t.Fatalf("line 4: %+v", all[0])
	}

	cases := map[string]struct {
		f    BotLogFilter
		want []int64
	}{
		"agent":                {BotLogFilter{AgentID: home.ID}, []int64{3, 2, 1}},
		"another user's agent": {BotLogFilter{AgentID: otherBot.ID}, []int64{}},
		"window":               {BotLogFilter{WindowID: "fortuna-1"}, []int64{3, 2}},
		"order":                {BotLogFilter{OrderID: "c81e55a0deadbeef"}, []int64{3, 2}},
		"problems":             {BotLogFilter{Problems: true}, []int64{4, 3}},
		"query in msg":         {BotLogFilter{Query: "ODDS DROPPED"}, []int64{3}},
		"query in step":        {BotLogFilter{Query: "placebet"}, []int64{4}},
		"after, oldest first":  {BotLogFilter{After: 2, Ascending: true}, []int64{3, 4}},
		"before":               {BotLogFilter{Before: 3}, []int64{2, 1}},
		"limit":                {BotLogFilter{Limit: 2}, []int64{4, 3}},
	}
	for name, c := range cases {
		got, err := s.QueryBotLogs(user.UserID, c.f)
		if err != nil || !slices.Equal(logIDs(got), c.want) {
			t.Errorf("%s: %v %v, want %v", name, logIDs(got), err, c.want)
		}
	}

	n, err := s.PruneBotLogs(3000)
	if err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if got, _ := s.QueryBotLogs(user.UserID, BotLogFilter{}); !slices.Equal(logIDs(got), []int64{4, 3}) {
		t.Fatalf("after prune: %v", logIDs(got))
	}
}

// queryPlan is SQLite's EXPLAIN QUERY PLAN for q, its detail column joined.
func queryPlan(t *testing.T, s *Service, q string, args []any) string {
	t.Helper()
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	return strings.Join(out, "; ")
}

// The problems filter reads the partial index of warn and error lines, not every line of the user.
func TestBotLogs_ProblemsUseThePartialIndex(t *testing.T) {
	s, _, _ := newTestService(t)
	for _, f := range []BotLogFilter{{Problems: true, Limit: 10}, {Problems: true, Before: 99, Limit: 10}, {Problems: true, After: 5, Ascending: true}} {
		q, args := botLogsQuery(7, f)
		if plan := queryPlan(t, s, q, args); !strings.Contains(plan, "bot_logs_problems") {
			t.Errorf("%+v: plan %q does not use bot_logs_problems", f, plan)
		}
	}
}

func TestBotLogs_SinceFilter(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	home, _ := s.CreateCode(user.UserID, "Home PC", KindBot)
	if err := s.AddBotLogs(user.UserID, home.ID, []BotLogLine{
		{TS: 1000, Level: "info", Msg: "odds old"}, {TS: 2000, Level: "info", Msg: "odds edge"}, {TS: 3000, Level: "info", Msg: "odds new"},
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.QueryBotLogs(user.UserID, BotLogFilter{Query: "odds", Since: 2000}); err != nil || !slices.Equal(logIDs(got), []int64{3, 2}) {
		t.Fatalf("since 2000: %v %v", logIDs(got), err)
	}
}

func TestBotLogs_PruneInBatches(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	home, _ := s.CreateCode(user.UserID, "Home PC", KindBot)
	lines := []BotLogLine{}
	for ts := int64(1); ts <= 7; ts++ {
		lines = append(lines, BotLogLine{TS: ts * 1000, Level: "info", Msg: "x"})
	}
	if err := s.AddBotLogs(user.UserID, home.ID, lines); err != nil {
		t.Fatal(err)
	}
	defer func(n int) { pruneBatch = n }(pruneBatch)
	pruneBatch = 2
	if n, err := s.PruneBotLogs(6000); err != nil || n != 5 {
		t.Fatalf("prune: %d %v, want 5 across batches", n, err)
	}
	if got, _ := s.QueryBotLogs(user.UserID, BotLogFilter{}); !slices.Equal(logIDs(got), []int64{7, 6}) {
		t.Fatalf("after prune: %v", logIDs(got))
	}
}
