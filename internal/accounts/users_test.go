package accounts

import (
	"errors"
	"testing"
)

func TestUsers_ListDisableEnable(t *testing.T) {
	s, _, rec := newTestService(t)
	admin, _ := mustUser(t, s, "admin@example.com", RoleAdmin)
	user, token := mustUser(t, s, "user@example.com", RoleUser)

	list, err := s.ListUsers()
	if err != nil || len(list) != 2 || list[0].Email != "admin@example.com" || list[1].Role != RoleUser || list[1].DisabledAt != nil {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := s.SetDisabled(admin.UserID, admin.UserID, true); !errors.Is(err, ErrSelf) {
		t.Fatalf("disabling yourself: %v", err)
	}
	if err := s.SetDisabled(admin.UserID, 999, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}

	if err := s.SetDisabled(admin.UserID, user.UserID, true); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.AuthenticateSession(token); ok {
		t.Fatal("a disabled user's session must stop working")
	}
	if _, _, err := s.Login("1.1.1.1", "user@example.com", testPassword); !errors.Is(err, ErrDisabled) {
		t.Fatalf("login while disabled: %v", err)
	}
	if len(rec.users) != 1 || rec.users[0] != user.UserID {
		t.Fatalf("disabling must close the user's sockets, got %v", rec.users)
	}

	if err := s.SetDisabled(admin.UserID, user.UserID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Login("1.1.1.1", "user@example.com", testPassword); err != nil {
		t.Fatalf("login after enable: %v", err)
	}
}

func TestTicket_InvalidAfterDisableOrRevoke(t *testing.T) {
	s, _, _ := newTestService(t)
	admin, _ := mustUser(t, s, "admin@example.com", RoleAdmin)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	c, _ := s.CreateExtensionCode(user.UserID, "Laptop")
	codeP, _, _ := s.AuthenticateCode(c.Code)

	if _, ok := s.RedeemTicket(s.IssueTicket(user)); !ok {
		t.Fatal("a session ticket must work")
	}
	revoked := s.IssueTicket(codeP)
	s.RevokeExtensionCode(user.UserID, c.ID)
	if _, ok := s.RedeemTicket(revoked); ok {
		t.Fatal("a ticket must not outlive its revoked code")
	}
	disabled := s.IssueTicket(user)
	s.SetDisabled(admin.UserID, user.UserID, true)
	if _, ok := s.RedeemTicket(disabled); ok {
		t.Fatal("a ticket must not work for a user disabled after it was issued")
	}
}

func TestReset_DisabledUserCodeIsNotUsable(t *testing.T) {
	s, _, _ := newTestService(t)
	admin, _ := mustUser(t, s, "admin@example.com", RoleAdmin)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	r, _ := s.CreateReset(admin.UserID, user.UserID)
	s.SetDisabled(admin.UserID, user.UserID, true)
	if _, ok, _ := s.CheckReset("1.1.1.1", r.Code); ok {
		t.Fatal("a disabled user's reset code must not check out")
	}
	if err := s.SetDisabled(admin.UserID, user.UserID, true); err != nil {
		t.Fatalf("disabling twice: %v", err)
	}
}
