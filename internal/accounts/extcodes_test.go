package accounts

import (
	"errors"
	"strings"
	"testing"
)

func TestExtensionCodes(t *testing.T) {
	s, _, rec := newTestService(t)
	admin, _ := mustUser(t, s, "admin@example.com", RoleAdmin)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)

	c, err := s.CreateExtensionCode(user.UserID, "  Laptop ")
	if err != nil || c.Name != "Laptop" || !strings.HasPrefix(c.Code, "SBX-") || c.Hint != hint(c.Code) {
		t.Fatalf("create: %+v %v", c, err)
	}
	if d, _ := s.CreateExtensionCode(user.UserID, ""); d.Name != "Extension" {
		t.Fatalf("empty name must default to Extension, got %q", d.Name)
	}
	if _, err := s.CreateExtensionCode(user.UserID, strings.Repeat("x", 41)); !errors.Is(err, ErrBadName) {
		t.Fatalf("41-char name: %v", err)
	}

	p, ok, err := s.AuthenticateCode(" " + strings.ToLower(c.Code) + "\n")
	if err != nil || !ok || p.UserID != user.UserID || p.CodeID != c.ID || p.CodeName != "Laptop" || p.SessionID != 0 {
		t.Fatalf("authenticate: %+v %v %v", p, ok, err)
	}
	if _, ok, _ := s.AuthenticateCode("K7QX-2M9P-H4TD"); ok {
		t.Fatal("a non-SBX code must not authenticate")
	}

	list, err := s.ListExtensionCodes(user.UserID)
	if err != nil || len(list) != 2 || list[1].LastUsedAt == nil || list[1].Name != "Laptop" {
		t.Fatalf("list (newest first, Laptop used): %+v %v", list, err)
	}

	if err := s.RevokeExtensionCode(admin.UserID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user must not revoke it: %v", err)
	}
	if err := s.RevokeExtensionCode(user.UserID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.AuthenticateCode(c.Code); ok {
		t.Fatal("a revoked code must not authenticate")
	}
	if len(rec.codes) != 1 || rec.codes[0] != c.ID {
		t.Fatalf("revoke must close the code's sockets, got %v", rec.codes)
	}

	c2, _ := s.CreateExtensionCode(user.UserID, "Home")
	s.SetDisabled(admin.UserID, user.UserID, true)
	if _, ok, _ := s.AuthenticateCode(c2.Code); ok {
		t.Fatal("a disabled user's code must not authenticate")
	}
}

func TestExtensionCodes_Kind(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)

	ext, err := s.CreateCode(user.UserID, "", "")
	if err != nil || ext.Kind != KindExtension || ext.Name != "Extension" {
		t.Fatalf("default kind: %+v %v", ext, err)
	}
	bot, err := s.CreateCode(user.UserID, "", KindBot)
	if err != nil || bot.Kind != KindBot || bot.Name != "Bot" || !strings.HasPrefix(bot.Code, "SBX-") {
		t.Fatalf("bot code: %+v %v", bot, err)
	}
	if _, err := s.CreateCode(user.UserID, "X", "robot"); !errors.Is(err, ErrBadKind) {
		t.Fatalf("unknown kind: %v", err)
	}
	if old, _ := s.CreateExtensionCode(user.UserID, "Laptop"); old.Kind != KindExtension {
		t.Fatalf("CreateExtensionCode must make an extension code, got %q", old.Kind)
	}

	list, err := s.ListExtensionCodes(user.UserID)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %+v %v", list, err)
	}
	kinds := map[int64]string{}
	for _, c := range list {
		kinds[c.ID] = c.Kind
	}
	if kinds[ext.ID] != KindExtension || kinds[bot.ID] != KindBot {
		t.Fatalf("listed kinds: %v", kinds)
	}
}

func TestCodeKind_PrincipalAndTicket(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	bot, _ := s.CreateCode(user.UserID, "Bot", KindBot)
	ext, _ := s.CreateCode(user.UserID, "Laptop", KindExtension)

	p, ok, err := s.AuthenticateCode(bot.Code)
	if err != nil || !ok || p.CodeKind != KindBot || p.CodeID != bot.ID {
		t.Fatalf("bot code principal: %+v %v %v", p, ok, err)
	}
	if e, _, _ := s.AuthenticateCode(ext.Code); e.CodeKind != KindExtension {
		t.Fatalf("extension code principal kind = %q", e.CodeKind)
	}
	if user.CodeKind != "" {
		t.Fatalf("a session principal has no code kind, got %q", user.CodeKind)
	}
	got, ok := s.RedeemTicket(s.IssueTicket(p))
	if !ok || got.CodeKind != KindBot || got.CodeID != bot.ID {
		t.Fatalf("a ticket must remember the code kind: %+v %v", got, ok)
	}
}
