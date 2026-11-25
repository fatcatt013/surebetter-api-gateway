package accounts

import (
	"testing"
	"time"
)

func alertRow(id string, userID int64, kind, severity string, created int64) BotAlertRow {
	return BotAlertRow{ID: id, UserID: userID, Kind: kind, Severity: severity, Title: "T " + id, Detail: "D " + id, CreatedAt: created}
}

func insertAlerts(t *testing.T, s *Service, rows ...BotAlertRow) {
	t.Helper()
	for _, a := range rows {
		if err := s.InsertBotAlert(a); err != nil {
			t.Fatalf("insert %s: %v", a.ID, err)
		}
	}
}

func alertIDs(rows []BotAlertRow) []string {
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestBotAlerts_ListOpenFirstNewestFirst(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)
	insertAlerts(t, s,
		alertRow("a1", user.UserID, "window", "problem", 100),
		alertRow("a2", user.UserID, "order", "info", 200),
		alertRow("a3", user.UserID, "attention", "attention", 300),
		alertRow("a4", user.UserID, "window", "problem", 400),
		alertRow("b1", other.UserID, "window", "problem", 500))
	if err := s.ResolveBotAlert("a4", 450); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveBotAlert("a4", 999); err != nil { // the first time stays
		t.Fatal(err)
	}
	if _, _, err := s.DismissBotAlert(user.UserID, "a1", 460); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListBotAlerts(user.UserID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := alertIDs(list); len(got) != 3 || got[0] != "a3" || got[1] != "a2" || got[2] != "a4" {
		t.Fatalf("list = %v, want [a3 a2 a4] (open newest first, then resolved; dismissed and other users' left out)", got)
	}
	if list[2].ResolvedAt != 450 || list[0].Title != "T a3" || list[0].Detail != "D a3" || list[0].Kind != "attention" {
		t.Fatalf("rows: %+v", list)
	}
	if limited, _ := s.ListBotAlerts(user.UserID, 1); len(limited) != 1 || limited[0].ID != "a3" {
		t.Fatalf("limit 1: %+v", limited)
	}
	if n, err := s.CountOpenBotAlerts(user.UserID); err != nil || n != 1 {
		t.Fatalf("open = %d (err %v), want 1 (a3; a2 is info, a4 resolved, a1 dismissed)", n, err)
	}
}

func TestBotAlerts_DismissOnlyYourOwnOnce(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)
	insertAlerts(t, s, alertRow("a1", user.UserID, "window", "problem", 100), alertRow("b1", other.UserID, "window", "problem", 100))
	got, found, err := s.DismissBotAlert(user.UserID, "a1", 500)
	if err != nil || !found || got.DismissedAt != 500 || got.ID != "a1" {
		t.Fatalf("dismiss: %+v %v %v", got, found, err)
	}
	if again, _, _ := s.DismissBotAlert(user.UserID, "a1", 900); again.DismissedAt != 500 {
		t.Fatalf("second dismiss moved the time: %+v", again)
	}
	if _, found, err := s.DismissBotAlert(user.UserID, "b1", 500); err != nil || found {
		t.Fatalf("another user's alert: found %v err %v", found, err)
	}
	if list, _ := s.ListBotAlerts(other.UserID, 10); len(list) != 1 {
		t.Fatalf("the other user's alert was touched: %+v", list)
	}
}

func TestBotAlerts_DismissAllAndResolveAnOrdersAttention(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	a1 := alertRow("a1", user.UserID, "attention", "attention", 100)
	a1.OrderID = "o1"
	a2 := alertRow("a2", user.UserID, "attention", "attention", 200)
	a2.OrderID = "o2"
	a3 := alertRow("a3", user.UserID, "order", "info", 300)
	a3.OrderID = "o1"
	insertAlerts(t, s, a1, a2, a3, alertRow("a4", user.UserID, "window", "problem", 400))
	resolved, err := s.ResolveBotOrderAlerts(user.UserID, "o1", 700)
	if err != nil || len(resolved) != 1 || resolved[0].ID != "a1" || resolved[0].ResolvedAt != 700 {
		t.Fatalf("resolve o1: %+v %v (only its attention alert)", resolved, err)
	}
	all, err := s.DismissAllBotAlerts(user.UserID, 800)
	if err != nil || len(all) != 4 {
		t.Fatalf("dismiss all: %+v %v", all, err)
	}
	for _, a := range all {
		if a.DismissedAt != 800 {
			t.Fatalf("dismissed_at: %+v", a)
		}
	}
	if list, _ := s.ListBotAlerts(user.UserID, 10); len(list) != 0 {
		t.Fatalf("list after dismiss all: %+v", list)
	}
	if again, err := s.DismissAllBotAlerts(user.UserID, 900); err != nil || len(again) != 0 {
		t.Fatalf("second dismiss all: %+v %v", again, err)
	}
}

func TestBotAlerts_ResolveOpenOfKindsAndPrune(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	insertAlerts(t, s,
		alertRow("a1", user.UserID, "window", "problem", 100),
		alertRow("a2", user.UserID, "agent", "problem", 200),
		alertRow("a3", user.UserID, "attention", "attention", 300),
		alertRow("a4", user.UserID, "window", "problem", 400))
	if err := s.ResolveBotAlert("a4", 50); err != nil {
		t.Fatal(err)
	}
	n, err := s.ResolveOpenBotAlerts([]string{"window", "agent"}, 900)
	if err != nil || n != 2 {
		t.Fatalf("resolved %d (err %v), want 2 (a1, a2)", n, err)
	}
	if c, _ := s.CountOpenBotAlerts(user.UserID); c != 1 {
		t.Fatalf("open after start resolve = %d, want 1 (a3)", c)
	}
	pruned, err := s.PruneBotAlerts(250)
	if err != nil || pruned != 2 {
		t.Fatalf("pruned %d (err %v), want 2", pruned, err)
	}
	if list, _ := s.ListBotAlerts(user.UserID, 10); len(list) != 2 {
		t.Fatalf("after prune: %+v", list)
	}
}

func TestBotAlertSettings_StoredJSON(t *testing.T) {
	s, clk, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	if _, found, err := s.BotAlertSettings(user.UserID); err != nil || found {
		t.Fatalf("no row: found %v err %v", found, err)
	}
	if err := s.SaveBotAlertSettings(user.UserID, `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	if err := s.SaveBotAlertSettings(user.UserID, `{"b":2}`); err != nil {
		t.Fatal(err)
	}
	js, found, err := s.BotAlertSettings(user.UserID)
	if err != nil || !found || js != `{"b":2}` {
		t.Fatalf("stored: %q %v %v", js, found, err)
	}
	var updated int64
	if err := s.db.QueryRow(`SELECT updated_at FROM bot_alert_settings WHERE user_id = ?`, user.UserID).Scan(&updated); err != nil ||
		updated != clk.now().UnixMilli() {
		t.Fatalf("updated_at = %d (err %v)", updated, err)
	}
}

func TestBotAlerts_ResolveTitled(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)
	a1 := alertRow("a1", user.UserID, "attention", "attention", 100)
	a1.Title = "Auto paused"
	a2 := alertRow("a2", user.UserID, "attention", "attention", 200)
	a2.Title, a2.DismissedAt = "Auto paused", 250
	a3 := alertRow("a3", other.UserID, "attention", "attention", 300)
	a3.Title = "Auto paused"
	insertAlerts(t, s, a1, a2, a3, alertRow("a4", user.UserID, "attention", "attention", 400))
	got, err := s.ResolveBotAlertsTitled(user.UserID, "Auto paused", 700)
	if err != nil || len(got) != 1 || got[0].ID != "a1" || got[0].ResolvedAt != 700 {
		t.Fatalf("resolved: %+v %v", got, err)
	}
}
