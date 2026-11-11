package accounts

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestLogin_SessionLifecycle(t *testing.T) {
	s, clk, rec := newTestService(t)
	mustUser(t, s, "a@example.com", RoleUser)

	p, token, err := s.Login("1.1.1.1", " A@Example.com ", testPassword)
	if err != nil || p.Email != "a@example.com" || p.SessionID == 0 || token == "" {
		t.Fatalf("login: %+v %v", p, err)
	}
	got, ok, err := s.AuthenticateSession(token)
	if err != nil || !ok || got.UserID != p.UserID || got.SessionID != p.SessionID {
		t.Fatalf("authenticate: %+v %v %v", got, ok, err)
	}

	// Sliding expiry: use within 30 days keeps the session alive.
	clk.advance(29 * 24 * time.Hour)
	if _, ok, _ := s.AuthenticateSession(token); !ok {
		t.Fatal("used after 29 days: must still be valid")
	}
	clk.advance(29 * 24 * time.Hour)
	if _, ok, _ := s.AuthenticateSession(token); !ok {
		t.Fatal("used again 29 days later: must still be valid")
	}
	clk.advance(31 * 24 * time.Hour)
	if _, ok, _ := s.AuthenticateSession(token); ok {
		t.Fatal("unused for 31 days: must be expired")
	}

	_, token2, _ := s.Login("1.1.1.1", "a@example.com", testPassword)
	p2, _, _ := s.AuthenticateSession(token2)
	if err := s.Logout(p2.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.AuthenticateSession(token2); ok {
		t.Fatal("logged-out session must not authenticate")
	}
	if len(rec.sessions) != 1 || rec.sessions[0] != p2.SessionID {
		t.Fatalf("logout must close the session's sockets, got %v", rec.sessions)
	}
	if _, ok, _ := s.AuthenticateSession(""); ok {
		t.Fatal("empty token must not authenticate")
	}
}

func TestLogin_WrongPasswordAndUnknownEmail(t *testing.T) {
	s, _, _ := newTestService(t)
	mustUser(t, s, "a@example.com", RoleUser)
	if _, _, err := s.Login("1.1.1.1", "a@example.com", "wrong password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, _, err := s.Login("1.1.1.1", "nobody@example.com", testPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown email: %v", err)
	}
}

func TestLogin_LocksPerEmailAcrossIPs(t *testing.T) {
	s, clk, _ := newTestService(t)
	mustUser(t, s, "a@example.com", RoleUser)
	for i := 0; i < 5; i++ {
		s.Login(fmt.Sprintf("2.2.2.%d", i), "a@example.com", "wrong password")
	}
	var locked *LockedError
	if _, _, err := s.Login("3.3.3.3", "a@example.com", testPassword); !errors.As(err, &locked) || locked.RetryAfter != 15*time.Minute {
		t.Fatalf("email must be locked for 15m, got %v", err)
	}
	clk.advance(15 * time.Minute)
	if _, _, err := s.Login("3.3.3.3", "a@example.com", testPassword); err != nil {
		t.Fatalf("after the lock: %v", err)
	}
}

func TestLogin_LocksPerIP(t *testing.T) {
	s, _, _ := newTestService(t)
	mustUser(t, s, "a@example.com", RoleUser)
	for i := 0; i < 5; i++ {
		s.Login("4.4.4.4", fmt.Sprintf("x%d@example.com", i), "wrong password")
	}
	var locked *LockedError
	if _, _, err := s.Login("4.4.4.4", "a@example.com", testPassword); !errors.As(err, &locked) {
		t.Fatalf("IP must be locked, got %v", err)
	}
}
