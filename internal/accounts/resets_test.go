package accounts

import (
	"errors"
	"testing"
)

func TestReset_SetsPasswordAndEndsSessions(t *testing.T) {
	s, clk, rec := newTestService(t)
	admin, _ := mustUser(t, s, "admin@example.com", RoleAdmin)
	user, oldToken := mustUser(t, s, "user@example.com", RoleUser)

	r, err := s.CreateReset(admin.UserID, user.UserID)
	if err != nil || r.Path != "/app/reset?code="+r.Code {
		t.Fatalf("create: %+v %v", r, err)
	}
	if email, ok, err := s.CheckReset("1.1.1.1", r.Code); err != nil || !ok || email != "user@example.com" {
		t.Fatalf("check: %q %v %v", email, ok, err)
	}
	if _, _, err := s.ResetPassword("1.1.1.1", r.Code, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("short password: %v", err)
	}

	p, token, err := s.ResetPassword("1.1.1.1", r.Code, "a brand new password")
	if err != nil || p.UserID != user.UserID || token == "" {
		t.Fatalf("reset: %+v %v", p, err)
	}
	if _, ok, _ := s.AuthenticateSession(oldToken); ok {
		t.Fatal("old sessions must end")
	}
	if _, ok, _ := s.AuthenticateSession(token); !ok {
		t.Fatal("the new session must work")
	}
	if _, _, err := s.Login("1.1.1.1", "user@example.com", testPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password: %v", err)
	}
	if _, _, err := s.Login("1.1.1.1", "user@example.com", "a brand new password"); err != nil {
		t.Fatalf("new password: %v", err)
	}
	if len(rec.users) != 1 || rec.users[0] != user.UserID {
		t.Fatalf("reset must close the user's sockets, got %v", rec.users)
	}
	if _, ok, _ := s.CheckReset("1.1.1.1", r.Code); ok {
		t.Fatal("a used reset code must not check out")
	}
	if _, _, err := s.ResetPassword("1.1.1.1", r.Code, "another new password"); !errors.Is(err, ErrResetInvalid) {
		t.Fatalf("reuse: %v", err)
	}

	r2, _ := s.CreateReset(admin.UserID, user.UserID)
	clk.advance(ResetTTL)
	if _, ok, _ := s.CheckReset("1.1.1.1", r2.Code); ok {
		t.Fatal("reset codes expire after 24h")
	}
	if _, err := s.CreateReset(admin.UserID, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestReset_NewCodeSupersedesOlder(t *testing.T) {
	s, _, _ := newTestService(t)
	admin, _ := mustUser(t, s, "admin@example.com", RoleAdmin)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)
	ro, _ := s.CreateReset(admin.UserID, other.UserID)
	r1, _ := s.CreateReset(admin.UserID, user.UserID)
	r2, err := s.CreateReset(admin.UserID, user.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.CheckReset("1.1.1.1", r1.Code); ok {
		t.Fatal("older code must be unusable")
	}
	if _, ok, _ := s.CheckReset("1.1.1.1", r2.Code); !ok {
		t.Fatal("newest code must be usable")
	}
	if _, ok, _ := s.CheckReset("1.1.1.1", ro.Code); !ok {
		t.Fatal("another user's code must be unaffected")
	}
}
