package accounts

import (
	"errors"
	"testing"
	"time"
)

func TestInvite_LifecycleAndRegister(t *testing.T) {
	s, clk, _ := newTestService(t)
	inv, err := s.CreateInvite(0, RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Path != "/app/invite?code="+inv.Code || inv.ExpiresAt != clk.now().Add(InviteTTL).UnixMilli() {
		t.Fatalf("unexpected invite %+v", inv)
	}
	if st, _ := s.CheckInvite("1.1.1.1", inv.Code); st != InviteValid {
		t.Fatalf("status = %s, want valid", st)
	}

	p, token, err := s.Register("1.1.1.1", " "+inv.Code+" ", " New.User@Example.com ", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if p.Email != "new.user@example.com" || p.Role != RoleUser || p.Onboarded || p.SessionID == 0 || token == "" {
		t.Fatalf("unexpected principal %+v token %q", p, token)
	}
	if st, _ := s.CheckInvite("1.1.1.1", inv.Code); st != InviteUsed {
		t.Fatalf("status after use = %s, want used", st)
	}
	var ie *InviteError
	if _, _, err := s.Register("1.1.1.1", inv.Code, "other@example.com", testPassword); !errors.As(err, &ie) || ie.Status != InviteUsed {
		t.Fatalf("reusing an invite: %v", err)
	}

	inv2, _ := s.CreateInvite(0, RoleUser)
	if _, _, err := s.Register("1.1.1.1", inv2.Code, "new.user@example.com", testPassword); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email: %v", err)
	}
	if _, _, err := s.Register("1.1.1.1", inv2.Code, "x@example.com", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("short password: %v", err)
	}
	if _, _, err := s.Register("1.1.1.1", inv2.Code, "not-an-email", testPassword); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("bad email: %v", err)
	}

	clk.advance(InviteTTL)
	if st, _ := s.CheckInvite("1.1.1.1", inv2.Code); st != InviteExpired {
		t.Fatalf("status after 14 days = %s, want expired", st)
	}
}

func TestInvite_RevokeAndList(t *testing.T) {
	s, _, _ := newTestService(t)
	admin, _ := mustUser(t, s, "admin@example.com", RoleAdmin)
	inv, _ := s.CreateInvite(admin.UserID, RoleUser)
	if err := s.RevokeInvite(inv.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.CheckInvite("1.1.1.1", inv.Code); st != InviteRevoked {
		t.Fatalf("status = %s, want revoked", st)
	}
	if err := s.RevokeInvite(inv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking twice: %v", err)
	}
	list, err := s.ListInvites()
	if err != nil {
		t.Fatal(err)
	}
	// newest first: the revoked one, then the one mustUser used
	if len(list) != 2 || list[0].Status != InviteRevoked || list[1].Status != InviteUsed ||
		list[1].UsedBy == nil || *list[1].UsedBy != "admin@example.com" || list[0].Hint != hint(inv.Code) {
		t.Fatalf("unexpected list %+v", list)
	}
}

func TestInvite_UnknownCodesLockTheIP(t *testing.T) {
	s, clk, _ := newTestService(t)
	for i := 0; i < 5; i++ {
		if st, err := s.CheckInvite("6.6.6.6", "AAAA-BBBB-CCCC"); err != nil || st != InviteUnknown {
			t.Fatalf("check %d: %s %v", i, st, err)
		}
	}
	var locked *LockedError
	if _, err := s.CheckInvite("6.6.6.6", "AAAA-BBBB-CCCC"); !errors.As(err, &locked) {
		t.Fatalf("6th try must be locked, got %v", err)
	}
	clk.advance(15 * time.Minute)
	if _, err := s.CheckInvite("6.6.6.6", "AAAA-BBBB-CCCC"); err != nil {
		t.Fatalf("lock must end: %v", err)
	}
}

func TestInvite_Bootstrap(t *testing.T) {
	s, _, _ := newTestService(t)
	first, created, err := s.EnsureBootstrapInvite()
	if err != nil || !created {
		t.Fatalf("first bootstrap: %v %v", created, err)
	}
	second, created, err := s.EnsureBootstrapInvite()
	if err != nil || !created {
		t.Fatalf("second bootstrap: %v %v", created, err)
	}
	if st, _ := s.CheckInvite("1.1.1.1", first.Code); st != InviteRevoked {
		t.Fatalf("an earlier bootstrap invite must be revoked, got %s", st)
	}
	p, _, err := s.Register("1.1.1.1", second.Code, "owner@example.com", testPassword)
	if err != nil || p.Role != RoleAdmin {
		t.Fatalf("bootstrap invite must create an admin: %+v %v", p, err)
	}
	if _, created, err := s.EnsureBootstrapInvite(); err != nil || created {
		t.Fatalf("no bootstrap invite once a user exists: %v %v", created, err)
	}
}
