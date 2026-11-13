package accounts

import (
	"testing"
	"time"
)

func TestBotFlows_SaveListAndRead(t *testing.T) {
	s, clk, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)
	home, _ := s.CreateCode(user.UserID, "Home PC", KindBot)
	vps, _ := s.CreateCode(user.UserID, "VPS Praha", KindBot)
	if err := s.TouchBotAgent(user.UserID, vps.ID); err != nil {
		t.Fatal(err)
	}

	// The first upload inserts the agent's row if it has none yet: a change.
	t0 := clk.now().UnixMilli()
	changed, err := s.SaveBotAgentFlows(user.UserID, home.ID, `{"steps":[1]}`)
	if err != nil || !changed {
		t.Fatalf("first upload: %v %v", changed, err)
	}
	clk.advance(time.Minute)
	if changed, err := s.SaveBotAgentFlows(user.UserID, home.ID, `{"steps":[1]}`); err != nil || changed {
		t.Fatalf("same flows again: %v %v, want no change", changed, err)
	}
	flows, at, err := s.BotAgentFlows(user.UserID, home.ID)
	if err != nil || flows != `{"steps":[1]}` || at != t0 {
		t.Fatalf("read: %q %d %v (an unchanged upload keeps flows_at)", flows, at, err)
	}
	if changed, err := s.SaveBotAgentFlows(user.UserID, home.ID, `{"steps":[2]}`); err != nil || !changed {
		t.Fatalf("new flows: %v %v", changed, err)
	}
	if flows, at, _ := s.BotAgentFlows(user.UserID, home.ID); flows != `{"steps":[2]}` || at != t0+60_000 {
		t.Fatalf("after change: %q %d", flows, at)
	}

	list, err := s.ListBotAgents(user.UserID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if h := list[0]; h.CodeID != home.ID || !h.HasFlows || h.FlowsAt != t0+60_000 {
		t.Fatalf("home: %+v", h)
	}
	if v := list[1]; v.CodeID != vps.ID || v.HasFlows || v.FlowsAt != 0 {
		t.Fatalf("vps: %+v", v)
	}
	// Another user's agent reads as none.
	if flows, at, err := s.BotAgentFlows(other.UserID, home.ID); err != nil || flows != "" || at != 0 {
		t.Fatalf("another user's read: %q %d %v", flows, at, err)
	}
}
