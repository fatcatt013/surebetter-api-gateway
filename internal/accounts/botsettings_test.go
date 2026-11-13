package accounts

import (
	"testing"
	"time"
)

func TestBotSettings_DefaultsSaveAndLoad(t *testing.T) {
	s, clk, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)

	got, err := s.BotSettings(user.UserID)
	if err != nil || got != DefaultBotSettings() {
		t.Fatalf("no row: %+v %v, want the defaults", got, err)
	}
	if d := DefaultBotSettings(); !d.DryRun || d.Paused || d.PausedReason != "" || d.MaxOrder != 0 || d.MaxLeg != 0 || d.PerDay != 0 ||
		d.OrdersAtOnce != 1 || !d.PauseOnUnknown {
		t.Fatalf("defaults: %+v", d)
	}

	want := BotSettings{DryRun: false, Paused: true, PausedReason: "Outcome unknown · order 0123abcd", MaxOrder: 5000, MaxLeg: 3000,
		PerDay: 20000, OrdersAtOnce: 3, PauseOnUnknown: false}
	if err := s.SaveBotSettings(user.UserID, want); err != nil {
		t.Fatal(err)
	}
	if got, err := s.BotSettings(user.UserID); err != nil || got != want {
		t.Fatalf("after save: %+v %v", got, err)
	}
	clk.advance(time.Minute)
	want.Paused, want.PausedReason = false, ""
	if err := s.SaveBotSettings(user.UserID, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.BotSettings(user.UserID); got != want {
		t.Fatalf("after second save: %+v", got)
	}
	var updated int64
	if err := s.db.QueryRow(`SELECT updated_at FROM bot_settings WHERE user_id = ?`, user.UserID).Scan(&updated); err != nil || updated != clk.now().UnixMilli() {
		t.Fatalf("updated_at = %d (err %v)", updated, err)
	}
	if got, _ := s.BotSettings(other.UserID); got != DefaultBotSettings() {
		t.Fatalf("another user's settings: %+v", got)
	}
}
