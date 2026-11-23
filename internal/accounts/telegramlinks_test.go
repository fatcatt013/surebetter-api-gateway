package accounts

import "testing"

func TestTelegramLinks(t *testing.T) {
	s, clk, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	if _, ok, err := s.TelegramLink(user.UserID); err != nil || ok {
		t.Fatalf("no link: ok %v err %v", ok, err)
	}
	if err := s.SetTelegramLink(user.UserID, 555); err != nil {
		t.Fatal(err)
	}
	l, ok, err := s.TelegramLink(user.UserID)
	if err != nil || !ok || l.ChatID != 555 || l.LinkedAt != clk.now().UnixMilli() {
		t.Fatalf("link: %+v %v %v", l, ok, err)
	}
	if err := s.SetTelegramLink(user.UserID, 777); err != nil { // replaces
		t.Fatal(err)
	}
	if removed, err := s.DeleteTelegramLinkIfChat(user.UserID, 555); err != nil || removed {
		t.Fatalf("an older chat removed the newer link: %v %v", removed, err)
	}
	if l, _, _ := s.TelegramLink(user.UserID); l.ChatID != 777 {
		t.Fatalf("after relink: %+v", l)
	}
	if removed, err := s.DeleteTelegramLinkIfChat(user.UserID, 777); err != nil || !removed {
		t.Fatalf("delete if chat: %v %v", removed, err)
	}
	if err := s.SetTelegramLink(user.UserID, 555); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTelegramLink(user.UserID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTelegramLink(user.UserID); err != nil { // not linked: fine
		t.Fatal(err)
	}
	if _, ok, _ := s.TelegramLink(user.UserID); ok {
		t.Fatal("still linked after delete")
	}
}
