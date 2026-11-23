package accounts

import (
	"testing"
	"time"
)

// The start check's agents: non-revoked bot codes whose agent was seen since a time, every user's.
func TestRecentBotAgents(t *testing.T) {
	s, clk, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)
	old, _ := s.CreateCode(user.UserID, "Old box", KindBot)
	if err := s.TouchBotAgent(user.UserID, old.ID); err != nil {
		t.Fatal(err)
	}
	clk.advance(25 * time.Hour)
	home, _ := s.CreateCode(user.UserID, "Home PC", KindBot)
	gone, _ := s.CreateCode(user.UserID, "Revoked", KindBot)
	vps, _ := s.CreateCode(other.UserID, "VPS", KindBot)
	for _, a := range []struct{ user, code int64 }{{user.UserID, home.ID}, {user.UserID, gone.ID}, {other.UserID, vps.ID}} {
		if err := s.TouchBotAgent(a.user, a.code); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RevokeExtensionCode(user.UserID, gone.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecentBotAgents(clk.now().Add(-24 * time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	want := []RecentBotAgent{{UserID: user.UserID, CodeID: home.ID, Name: "Home PC"}, {UserID: other.UserID, CodeID: vps.ID, Name: "VPS"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("recent = %+v, want %+v (not the one seen 25 h ago, not the revoked one)", got, want)
	}
}

// A disabled user's agents are not in the start check; UserDisabled says so.
func TestRecentBotAgents_SkipsDisabledUsers(t *testing.T) {
	s, _, _ := newTestService(t)
	admin, _ := mustUser(t, s, "admin@example.com", RoleAdmin)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	code, _ := s.CreateCode(user.UserID, "Home PC", KindBot)
	if err := s.TouchBotAgent(user.UserID, code.ID); err != nil {
		t.Fatal(err)
	}
	if d, err := s.UserDisabled(user.UserID); err != nil || d {
		t.Fatalf("enabled: %v %v", d, err)
	}
	if err := s.SetDisabled(admin.UserID, user.UserID, true); err != nil {
		t.Fatal(err)
	}
	if d, err := s.UserDisabled(user.UserID); err != nil || !d {
		t.Fatalf("disabled: %v %v", d, err)
	}
	if got, err := s.RecentBotAgents(0); err != nil || len(got) != 0 {
		t.Fatalf("recent = %+v (err %v), want none", got, err)
	}
}
