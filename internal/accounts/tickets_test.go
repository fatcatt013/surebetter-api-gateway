package accounts

import (
	"testing"
	"time"
)

func TestTickets_SingleUseAndExpiry(t *testing.T) {
	clk := newClock()
	tk := newTickets(30*time.Second, clk.now)
	p := Principal{UserID: 7, Email: "a@example.com"}

	tok := tk.issue(p)
	got, ok := tk.redeem(tok)
	if !ok || got.UserID != 7 {
		t.Fatalf("first redeem = %+v, %v", got, ok)
	}
	if _, ok := tk.redeem(tok); ok {
		t.Fatal("a ticket must work only once")
	}

	tok = tk.issue(p)
	clk.advance(30 * time.Second)
	if _, ok := tk.redeem(tok); ok {
		t.Fatal("a ticket must expire after 30s")
	}
	if _, ok := tk.redeem("nope"); ok {
		t.Fatal("unknown tickets must fail")
	}
}

func TestStillValid(t *testing.T) {
	s, _, _ := newTestService(t)
	p, _ := mustUser(t, s, "u@example.com", RoleUser)
	if !s.StillValid(p) {
		t.Fatal("fresh principal must be valid")
	}
	if err := s.Logout(p.SessionID); err != nil {
		t.Fatal(err)
	}
	if s.StillValid(p) {
		t.Fatal("ended session must be invalid")
	}
}
