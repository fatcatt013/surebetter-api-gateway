package accounts

import (
	"testing"
	"time"
)

func TestBotAgents_TouchSaveList(t *testing.T) {
	s, clk, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)
	vps, _ := s.CreateCode(user.UserID, "VPS Praha", KindBot)
	home, _ := s.CreateCode(user.UserID, "Home PC", KindBot)
	if _, err := s.CreateCode(user.UserID, "Never connected", KindBot); err != nil {
		t.Fatal(err)
	}
	otherBot, _ := s.CreateCode(other.UserID, "Other", KindBot)

	t0 := clk.now().UnixMilli()
	if err := s.TouchBotAgent(user.UserID, vps.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchBotAgent(other.UserID, otherBot.ID); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	if err := s.SaveBotAgentStatus(user.UserID, home.ID, BotAgentStatus{Version: "0.9.0+e9ed9d2", HostName: "home-pc",
		WindowsJSON: `[{"id":"tipsport-1"}]`}); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	if err := s.TouchBotAgent(user.UserID, vps.ID); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListBotAgents(user.UserID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	h, v := list[0], list[1] // by name: Home PC, VPS Praha
	if h.CodeID != home.ID || h.Name != "Home PC" || h.Version != "0.9.0+e9ed9d2" || h.HostName != "home-pc" ||
		h.WindowsJSON != `[{"id":"tipsport-1"}]` || h.FirstSeenAt != t0+60_000 || h.LastSeenAt != t0+60_000 {
		t.Fatalf("home: %+v", h)
	}
	if v.CodeID != vps.ID || v.Name != "VPS Praha" || v.Version != "" || v.WindowsJSON != "[]" ||
		v.FirstSeenAt != t0 || v.LastSeenAt != t0+120_000 {
		t.Fatalf("vps: %+v", v)
	}

	// A status keeps first seen; an empty windows value is stored as [].
	if err := s.SaveBotAgentStatus(user.UserID, vps.ID, BotAgentStatus{Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListBotAgents(user.UserID)
	if v := list[1]; v.Version != "1.0.0" || v.WindowsJSON != "[]" || v.FirstSeenAt != t0 {
		t.Fatalf("vps after status: %+v", v)
	}

	// A revoked code's agent is not listed.
	if err := s.RevokeExtensionCode(user.UserID, vps.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListBotAgents(user.UserID); len(list) != 1 || list[0].CodeID != home.ID {
		t.Fatalf("after revoke: %+v", list)
	}
}
