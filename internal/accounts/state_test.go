package accounts

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestState_PutGetNotify(t *testing.T) {
	s, _, rec := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)

	if err := s.PutState(user, "pins", json.RawMessage(`[{"key":"a"}]`)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutState(user, "pins", json.RawMessage(`[{"key":"b"}]`)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutState(user, "profile", json.RawMessage(`{"bookmakers":["tipsport","betano"]}`)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetState(user.UserID)
	if err != nil || len(got) != 2 || string(got["pins"]) != `[{"key":"b"}]` {
		t.Fatalf("get: %v %v", got, err)
	}
	if empty, _ := s.GetState(other.UserID); len(empty) != 0 {
		t.Fatalf("users must not see each other's state: %v", empty)
	}
	want := []string{
		"1:pins:" + itoa(user.SessionID),
		"1:pins:" + itoa(user.SessionID),
		"1:profile:" + itoa(user.SessionID),
	}
	if strings.Join(rec.states, ",") != strings.Join(want, ",") {
		t.Fatalf("notifications = %v, want %v", rec.states, want)
	}

	if err := s.PutState(user, "sb.theme", json.RawMessage(`"dark"`)); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("theme is not an account key: %v", err)
	}
	if err := s.PutState(user, "pins", json.RawMessage(`{broken`)); !errors.Is(err, ErrBadValue) {
		t.Fatalf("invalid JSON: %v", err)
	}
	if err := s.PutState(user, "pins", nil); !errors.Is(err, ErrBadValue) {
		t.Fatalf("missing value: %v", err)
	}
	big := json.RawMessage(`"` + strings.Repeat("x", MaxStateBytes) + `"`)
	if err := s.PutState(user, "pins", big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over 256 KB: %v", err)
	}
}

func TestState_Onboarded(t *testing.T) {
	s, _, _ := newTestService(t)
	user, token := mustUser(t, s, "user@example.com", RoleUser)
	if err := s.SetOnboarded(user.UserID); err != nil {
		t.Fatal(err)
	}
	p, _, _ := s.AuthenticateSession(token)
	if !p.Onboarded {
		t.Fatal("onboarded flag must be set")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
