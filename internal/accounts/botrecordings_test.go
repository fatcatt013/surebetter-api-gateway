package accounts

import (
	"testing"
	"time"
)

func TestBotRecordings_PerAgentSaveReplaceAndRead(t *testing.T) {
	s, clk, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	home, _ := s.CreateCode(user.UserID, "Home PC", KindBot)
	vps, _ := s.CreateCode(user.UserID, "VPS Praha", KindBot)

	if list, err := s.BotRecordings(home.ID); err != nil || len(list) != 0 {
		t.Fatalf("none yet: %v %v", list, err)
	}
	if _, _, found, err := s.BotRecording(home.ID, "mouse"); err != nil || found {
		t.Fatalf("no mouse recording: %v %v", found, err)
	}
	t0 := clk.now().UnixMilli()
	if at, err := s.SaveBotRecording(user.UserID, home.ID, "mouse", `{"paths":[1]}`, 1); err != nil || at != t0 {
		t.Fatalf("save: %d %v", at, err)
	}
	clk.advance(time.Minute)
	if at, err := s.SaveBotRecording(user.UserID, home.ID, "mouse", `{"paths":[1,2]}`, 2); err != nil || at != t0+60_000 {
		t.Fatalf("replace: %d %v", at, err)
	}
	if _, err := s.SaveBotRecording(user.UserID, home.ID, "typing", `{"sessions":[1]}`, 1); err != nil {
		t.Fatal(err)
	}
	list, err := s.BotRecordings(home.ID)
	if err != nil || len(list) != 2 || list["mouse"] != (BotRecordingInfo{Count: 2, UpdatedAt: t0 + 60_000}) ||
		list["typing"] != (BotRecordingInfo{Count: 1, UpdatedAt: t0 + 60_000}) {
		t.Fatalf("summary: %+v %v", list, err)
	}
	body, at, found, err := s.BotRecording(home.ID, "mouse")
	if err != nil || !found || body != `{"paths":[1,2]}` || at != t0+60_000 {
		t.Fatalf("mouse: %q %d %v %v", body, at, found, err)
	}
	// Each agent has its own.
	if list, _ := s.BotRecordings(vps.ID); len(list) != 0 {
		t.Fatalf("another agent's recordings: %v", list)
	}
}
