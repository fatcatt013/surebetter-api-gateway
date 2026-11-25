package accounts

import (
	"bytes"
	"errors"
	"testing"
)

func loginRow(uid, code int64, window, book, tag string) BotLoginRow {
	return BotLoginRow{UserID: uid, CodeID: code, WindowID: window, Bookmaker: book, Secret: []byte("s-" + window), KeyID: "k1",
		UserTag: tag, UpdatedAt: 100}
}

func TestBotLogins_SaveKeepsLastResultAndClearsFailing(t *testing.T) {
	s, _, _ := newTestService(t)
	p, _ := mustUser(t, s, "u@example.com", RoleUser)
	code, _ := s.CreateCode(p.UserID, "Home PC", KindBot)
	if err := s.SaveBotLogin(loginRow(p.UserID, code.ID, "tipsport-1", "tipsport", "t1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetBotLoginResult(code.ID, "tipsport-1", "ok", 200, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetBotLoginResult(code.ID, "tipsport-1", "wrong_password", 300, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RevealBotLogin(code.ID, "tipsport-1", "2026-11-25", func(BotLoginRow, int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	r := loginRow(p.UserID, code.ID, "tipsport-1", "tipsport", "t2")
	r.Secret, r.KeyID, r.UpdatedAt = []byte("new"), "k2", 400
	if err := s.SaveBotLogin(r); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.BotLogin(code.ID, "tipsport-1")
	if err != nil || !found {
		t.Fatalf("get: %v %v", found, err)
	}
	if !bytes.Equal(got.Secret, []byte("new")) || got.KeyID != "k2" || got.UserTag != "t2" || got.UpdatedAt != 400 || got.Failing != "" ||
		got.Reveals != 0 || got.LastResult != "wrong_password" || got.LastAt != 300 || got.LastOkAt != 200 {
		t.Fatalf("after save: %+v", got)
	}
	if _, found, _ := s.BotLogin(code.ID, "tipsport-2"); found {
		t.Fatal("found a window without a login")
	}
}

func TestBotLogins_ListsAndDelete(t *testing.T) {
	s, _, _ := newTestService(t)
	p, _ := mustUser(t, s, "u@example.com", RoleUser)
	o, _ := mustUser(t, s, "o@example.com", RoleUser)
	a, _ := s.CreateCode(p.UserID, "A", KindBot)
	b, _ := s.CreateCode(p.UserID, "B", KindBot)
	other, _ := s.CreateCode(o.UserID, "O", KindBot)
	for _, r := range []BotLoginRow{loginRow(p.UserID, b.ID, "betano-1", "betano", "t"), loginRow(p.UserID, a.ID, "tipsport-2", "tipsport", "t"),
		loginRow(p.UserID, a.ID, "betano-1", "betano", "t"), loginRow(o.UserID, other.ID, "tipsport-1", "tipsport", "t")} {
		if err := s.SaveBotLogin(r); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListBotLogins(p.UserID)
	if err != nil || len(list) != 3 || list[0].CodeID != a.ID || list[0].WindowID != "betano-1" || list[1].WindowID != "tipsport-2" ||
		list[2].CodeID != b.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	mine, err := s.ListBotLoginsOfCode(a.ID)
	if err != nil || len(mine) != 2 {
		t.Fatalf("of code: %+v %v", mine, err)
	}
	if ok, err := s.DeleteBotLogin(a.ID, "betano-1"); !ok || err != nil {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, _ := s.DeleteBotLogin(a.ID, "betano-1"); ok {
		t.Fatal("deleted twice")
	}
	// Revoking a code deletes its logins; disabling a user deletes all of theirs.
	if err := s.RevokeExtensionCode(p.UserID, b.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListBotLoginsOfCode(b.ID); len(l) != 0 {
		t.Fatalf("revoked code's logins: %+v", l)
	}
	if l, _ := s.ListBotLogins(p.UserID); len(l) != 1 {
		t.Fatalf("after revoke: %+v", l)
	}
	admin, _ := mustUser(t, s, "admin@example.com", RoleAdmin)
	if err := s.SetDisabled(admin.UserID, p.UserID, true); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListBotLoginsOfCode(a.ID); len(l) != 0 {
		t.Fatalf("disabled user's logins: %+v", l)
	}
	if l, _ := s.ListBotLogins(o.UserID); len(l) != 1 {
		t.Fatalf("another user's logins: %+v", l)
	}
}

func TestBotLogins_ResultSpreadsFailingByTag(t *testing.T) {
	s, _, _ := newTestService(t)
	p, _ := mustUser(t, s, "u@example.com", RoleUser)
	a, _ := s.CreateCode(p.UserID, "A", KindBot)
	b, _ := s.CreateCode(p.UserID, "B", KindBot)
	for _, r := range []BotLoginRow{
		loginRow(p.UserID, a.ID, "tipsport-1", "tipsport", "same"),
		loginRow(p.UserID, a.ID, "tipsport-2", "tipsport", "same"),
		loginRow(p.UserID, a.ID, "tipsport-3", "tipsport", "other"),
		loginRow(p.UserID, a.ID, "betano-1", "betano", "same"),
		loginRow(p.UserID, b.ID, "tipsport-1", "tipsport", "same"),
	} {
		if err := s.SaveBotLogin(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, n, err := s.SetBotLoginResult(a.ID, "tipsport-1", "code", 5, nil); err != nil || n != 0 {
		t.Fatalf("code: %d %v", n, err)
	}
	if _, n, err := s.SetBotLoginResult(a.ID, "tipsport-1", "blocked", 6, nil); err != nil || n != 2 {
		t.Fatalf("blocked spread: %d %v", n, err)
	}
	want := map[string]string{"tipsport-1": "blocked", "tipsport-2": "blocked", "tipsport-3": "", "betano-1": ""}
	for w, f := range want {
		r, _, _ := s.BotLogin(a.ID, w)
		if r.Failing != f {
			t.Fatalf("%s failing %q, want %q", w, r.Failing, f)
		}
	}
	if r, _, _ := s.BotLogin(b.ID, "tipsport-1"); r.Failing != "" {
		t.Fatal("spread to another agent")
	}
	if r, _, _ := s.BotLogin(a.ID, "tipsport-2"); r.LastResult != "" || r.LastAt != 0 {
		t.Fatalf("the other row's last result changed: %+v", r)
	}
	if _, _, err := s.SetBotLoginResult(a.ID, "fortuna-1", "ok", 7, nil); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("no row: %v", err)
	}
}

func TestBotLogins_RevealCounterAndDay(t *testing.T) {
	s, _, _ := newTestService(t)
	p, _ := mustUser(t, s, "u@example.com", RoleUser)
	a, _ := s.CreateCode(p.UserID, "A", KindBot)
	s.SaveBotLogin(loginRow(p.UserID, a.ID, "tipsport-1", "tipsport", "t"))
	var seen []int
	use := func(_ BotLoginRow, today int) error { seen = append(seen, today); return nil }
	for i := 0; i < 3; i++ {
		if err := s.RevealBotLogin(a.ID, "tipsport-1", "2026-11-25", use); err != nil {
			t.Fatal(err)
		}
	}
	refuse := errors.New("refused")
	if err := s.RevealBotLogin(a.ID, "tipsport-1", "2026-11-25", func(BotLoginRow, int) error { return refuse }); err != refuse {
		t.Fatalf("refusal: %v", err)
	}
	s.RevealBotLogin(a.ID, "tipsport-1", "2026-11-25", use) // the refusal counted nothing
	s.RevealBotLogin(a.ID, "tipsport-1", "2026-11-26", use) // a new day starts at 0
	if len(seen) != 5 || seen[0] != 0 || seen[2] != 2 || seen[3] != 3 || seen[4] != 0 {
		t.Fatalf("counts: %v", seen)
	}
	if r, _, _ := s.BotLogin(a.ID, "tipsport-1"); r.Reveals != 1 || r.RevealsDay != "2026-11-26" {
		t.Fatalf("row: %+v", r)
	}
	if err := s.RevealBotLogin(a.ID, "betano-1", "2026-11-26", use); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("no row: %v", err)
	}
}

func TestBotLogins_Rekey(t *testing.T) {
	s, _, _ := newTestService(t)
	p, _ := mustUser(t, s, "u@example.com", RoleUser)
	a, _ := s.CreateCode(p.UserID, "A", KindBot)
	old1, old2, lost := loginRow(p.UserID, a.ID, "tipsport-1", "tipsport", "t"), loginRow(p.UserID, a.ID, "tipsport-2", "tipsport", "t"),
		loginRow(p.UserID, a.ID, "betano-1", "betano", "t")
	old1.KeyID, old2.KeyID, lost.KeyID = "old", "old", "lost"
	old2.Secret = []byte("bad")
	for _, r := range []BotLoginRow{old1, old2, lost} {
		s.SaveBotLogin(r)
	}
	reseal := func(r BotLoginRow) ([]byte, string, string, error) {
		if string(r.Secret) == "bad" {
			return nil, "", "", errors.New("cannot decrypt")
		}
		return append([]byte("re-"), r.Secret...), "new", "newtag", nil
	}
	done, skipped, err := s.RekeyBotLogins("old", reseal)
	if err != nil || done != 1 || skipped != 1 {
		t.Fatalf("rekey: %d %d %v", done, skipped, err)
	}
	if r, _, _ := s.BotLogin(a.ID, "tipsport-1"); string(r.Secret) != "re-s-tipsport-1" || r.KeyID != "new" || r.UserTag != "newtag" {
		t.Fatalf("resealed: %+v", r)
	}
	if r, _, _ := s.BotLogin(a.ID, "betano-1"); r.KeyID != "lost" {
		t.Fatal("unknown key touched")
	}
	if done, _, _ := s.RekeyBotLogins("new", func(BotLoginRow) ([]byte, string, string, error) { return nil, "", "", errors.New("x") }); done != 0 {
		t.Fatal("the current key's rows resealed")
	}
}

func TestBotAlerts_OpenLoginAlerts(t *testing.T) {
	s, _, _ := newTestService(t)
	p, _ := mustUser(t, s, "u@example.com", RoleUser)
	for _, a := range []BotAlertRow{
		{ID: "login", Kind: "attention", Severity: "attention", AgentCodeID: 3, WindowID: "tipsport-1", Title: "Log in needed", CreatedAt: 1},
		{ID: "order", Kind: "attention", Severity: "attention", AgentCodeID: 3, OrderID: "o1", Title: "Stuck order", CreatedAt: 2},
		{ID: "window", Kind: "window", Severity: "problem", AgentCodeID: 3, WindowID: "tipsport-1", Title: "Logged out", CreatedAt: 3},
		{ID: "resolved", Kind: "attention", Severity: "attention", AgentCodeID: 3, WindowID: "tipsport-2", Title: "Log in needed", CreatedAt: 4, ResolvedAt: 5},
		{ID: "dismissed", Kind: "attention", Severity: "attention", AgentCodeID: 3, WindowID: "tipsport-3", Title: "Login refused", CreatedAt: 4, DismissedAt: 5},
	} {
		a.UserID = p.UserID
		if err := s.InsertBotAlert(a); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.OpenBotLoginAlerts()
	if err != nil || len(rows) != 1 || rows[0].ID != "login" {
		t.Fatalf("open login alerts: %+v %v", rows, err)
	}
}

func TestBotLogins_ResultOfAnOlderSave(t *testing.T) {
	s, _, _ := newTestService(t)
	p, _ := mustUser(t, s, "u@example.com", RoleUser)
	a, _ := s.CreateCode(p.UserID, "A", KindBot)
	s.SaveBotLogin(loginRow(p.UserID, a.ID, "tipsport-1", "tipsport", "t"))
	s.SaveBotLogin(loginRow(p.UserID, a.ID, "tipsport-2", "tipsport", "t"))
	stale, cur := int64(99), int64(100)
	row, n, err := s.SetBotLoginResult(a.ID, "tipsport-1", "blocked", 5, &stale)
	if !errors.Is(err, ErrStaleLogin) || n != 0 || row.UpdatedAt != 100 {
		t.Fatalf("stale: %+v %d %v", row, n, err)
	}
	for _, w := range []string{"tipsport-1", "tipsport-2"} {
		if r, _, _ := s.BotLogin(a.ID, w); r.Failing != "" || r.LastResult != "" || r.LastAt != 0 {
			t.Fatalf("%s changed: %+v", w, r)
		}
	}
	if _, n, err := s.SetBotLoginResult(a.ID, "tipsport-1", "blocked", 5, &cur); err != nil || n != 2 {
		t.Fatalf("current: %d %v", n, err)
	}
	if _, _, err := s.SetBotLoginResult(a.ID, "fortuna-1", "ok", 5, &cur); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("no row first: %v", err)
	}
}
