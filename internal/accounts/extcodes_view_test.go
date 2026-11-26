package accounts

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// fakeCodeCipher "seals" as <user>:<code id>:<code> reversed, so a value moved to another row does not open.
type fakeCodeCipher struct{ failSeal bool }

func (f fakeCodeCipher) SealCode(userID, codeID int64, code string) ([]byte, error) {
	if f.failSeal {
		return nil, errors.New("seal failed")
	}
	b := []byte(fmt.Sprintf("%d:%d:%s", userID, codeID, code))
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b, nil
}

func (f fakeCodeCipher) OpenCode(userID, codeID int64, sealed []byte) (string, error) {
	b := bytes.Clone(sealed)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	prefix := fmt.Sprintf("%d:%d:", userID, codeID)
	if !strings.HasPrefix(string(b), prefix) {
		return "", errors.New("does not open")
	}
	return strings.TrimPrefix(string(b), prefix), nil
}

func viewableOf(t *testing.T, s *Service, userID, id int64) bool {
	t.Helper()
	list, err := s.ListExtensionCodes(userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.ID == id {
			return c.Viewable
		}
	}
	t.Fatalf("code %d not listed", id)
	return false
}

func TestExtensionCodes_ViewAgain(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)

	// No cipher: created, not viewable.
	plain, err := s.CreateCode(user.UserID, "Plain", KindExtension)
	if err != nil {
		t.Fatal(err)
	}
	if viewableOf(t, s, user.UserID, plain.ID) {
		t.Fatal("a code made without a cipher is viewable")
	}
	s.SetCodeCipher(fakeCodeCipher{}, false)
	if _, err := s.RevealExtensionCode(user.UserID, plain.ID); !errors.Is(err, ErrNotViewable) {
		t.Fatalf("unsealed code: %v", err)
	}

	c, err := s.CreateCode(user.UserID, "Laptop", KindBot)
	if err != nil {
		t.Fatal(err)
	}
	if !viewableOf(t, s, user.UserID, c.ID) {
		t.Fatal("a sealed code is not viewable")
	}
	if got, err := s.RevealExtensionCode(user.UserID, c.ID); err != nil || got != c.Code {
		t.Fatalf("reveal: %q %v", got, err)
	}
	if _, err := s.RevealExtensionCode(other.UserID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's code: %v", err)
	}
	if _, err := s.RevealExtensionCode(user.UserID, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such code: %v", err)
	}

	// A value of another row does not open here.
	d, _ := s.CreateCode(user.UserID, "Home", KindExtension)
	if _, err := s.db.Exec(`UPDATE extension_codes SET code_enc = (SELECT code_enc FROM extension_codes WHERE id = ?) WHERE id = ?`, c.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if viewableOf(t, s, user.UserID, d.ID) {
		t.Fatal("a swapped value is viewable")
	}
	if _, err := s.RevealExtensionCode(user.UserID, d.ID); !errors.Is(err, ErrNotViewable) {
		t.Fatalf("swapped value: %v", err)
	}

	// The cipher gone (no key at the next start): nothing is viewable.
	s.SetCodeCipher(nil, false)
	if viewableOf(t, s, user.UserID, c.ID) {
		t.Fatal("viewable without a cipher")
	}
	s.SetCodeCipher(fakeCodeCipher{}, false)

	// Revoke forgets the sealed copy.
	if err := s.RevokeExtensionCode(user.UserID, c.ID); err != nil {
		t.Fatal(err)
	}
	if viewableOf(t, s, user.UserID, c.ID) {
		t.Fatal("a revoked code is viewable")
	}
	if _, err := s.RevealExtensionCode(user.UserID, c.ID); !errors.Is(err, ErrCodeRevoked) {
		t.Fatalf("revoked: %v", err)
	}
	var cleared bool
	if err := s.db.QueryRow(`SELECT code_enc IS NULL FROM extension_codes WHERE id = ?`, c.ID).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("code_enc after revoke: cleared=%v err=%v", cleared, err)
	}

	// A sealing failure never fails creation.
	s.SetCodeCipher(fakeCodeCipher{failSeal: true}, false)
	e, err := s.CreateCode(user.UserID, "NoSeal", KindExtension)
	if err != nil || viewableOf(t, s, user.UserID, e.ID) {
		t.Fatalf("seal failure: %v", err)
	}
}

func TestExtensionCodes_Reseal(t *testing.T) {
	s, _, _ := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	s.SetCodeCipher(fakeCodeCipher{}, false)
	a, _ := s.CreateCode(user.UserID, "A", KindBot)
	b, _ := s.CreateCode(user.UserID, "B", KindBot)
	r, _ := s.CreateCode(user.UserID, "R", KindBot)
	s.RevokeExtensionCode(user.UserID, r.ID)
	var seen []int64
	n, err := s.ResealCodes(func(c SealedCode) ([]byte, bool) {
		seen = append(seen, c.CodeID)
		if c.CodeID == b.ID {
			return nil, false
		}
		enc, _ := fakeCodeCipher{}.SealCode(c.UserID, c.CodeID, "SBX-NEW")
		return enc, true
	})
	if err != nil || n != 1 || len(seen) != 2 {
		t.Fatalf("reseal: %d %v %v", n, err, seen)
	}
	if got, _ := s.RevealExtensionCode(user.UserID, a.ID); got != "SBX-NEW" {
		t.Fatalf("a after reseal: %q", got)
	}
	if got, _ := s.RevealExtensionCode(user.UserID, b.ID); got != b.Code {
		t.Fatalf("b after reseal: %q", got)
	}
}

func TestExtensionCodes_DeleteForGood(t *testing.T) {
	s, _, rec := newTestService(t)
	user, _ := mustUser(t, s, "user@example.com", RoleUser)
	other, _ := mustUser(t, s, "other@example.com", RoleUser)
	s.SetCodeCipher(fakeCodeCipher{}, false)
	bot, _ := s.CreateCode(user.UserID, "PC", KindBot)
	keep, _ := s.CreateCode(user.UserID, "Keep", KindBot)
	for _, id := range []int64{bot.ID, keep.ID} {
		if err := s.TouchBotAgent(user.UserID, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveBotRecording(user.UserID, id, "mouse", `[]`, 0); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveBotLogin(BotLoginRow{UserID: user.UserID, CodeID: id, WindowID: "tipsport-1", Bookmaker: "tipsport", Secret: []byte{1}, KeyID: "k", UserTag: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertBotOrder(BotOrderRow{ID: "o1", UserID: user.UserID, ArbKey: "k", CodeID: bot.ID, State: "placed", InfoJSON: "{}", LegsJSON: "[]", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBotLogs(user.UserID, bot.ID, []BotLogLine{{TS: 1, Level: "info", Msg: "hello"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBotAlert(BotAlertRow{ID: "a1", UserID: user.UserID, Kind: "agent", Severity: "problem", AgentCodeID: bot.ID, Title: "x", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteExtensionCode(other.UserID, bot.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's code: %v", err)
	}
	if err := s.DeleteExtensionCode(user.UserID, bot.ID); err != nil {
		t.Fatalf("delete an active code: %v", err)
	}
	if len(rec.codes) != 1 || rec.codes[0] != bot.ID {
		t.Fatalf("delete must close the code's sockets (revoke hooks): %v", rec.codes)
	}
	if _, ok, _ := s.AuthenticateCode(bot.Code); ok {
		t.Fatal("a deleted code authenticates")
	}
	if err := s.DeleteExtensionCode(user.UserID, bot.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := s.RevokeExtensionCode(user.UserID, bot.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke a deleted code: %v", err)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, tbl := range []string{"bot_agents", "bot_recordings", "bot_logins"} {
		if n := count(`SELECT COUNT(*) FROM `+tbl+` WHERE code_id = ?`, bot.ID); n != 0 {
			t.Fatalf("%s rows of the deleted code: %d", tbl, n)
		}
		if n := count(`SELECT COUNT(*) FROM `+tbl+` WHERE code_id = ?`, keep.ID); n != 1 {
			t.Fatalf("%s rows of the other code: %d", tbl, n)
		}
	}
	// History stays.
	if o, ok, err := s.BotOrderByID(user.UserID, "o1"); err != nil || !ok || o.CodeID != bot.ID || o.Agent != "" {
		t.Fatalf("order after delete: %+v %v %v", o, ok, err)
	}
	if n := count(`SELECT COUNT(*) FROM bot_logs WHERE code_id = ?`, bot.ID) + count(`SELECT COUNT(*) FROM bot_alerts WHERE agent_code_id = ?`, bot.ID); n != 2 {
		t.Fatalf("logs and alerts after delete: %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("foreign key violations: %d", n)
	}

	// A revoked code deletes too.
	if err := s.RevokeExtensionCode(user.UserID, keep.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteExtensionCode(user.UserID, keep.ID); err != nil {
		t.Fatalf("delete a revoked code: %v", err)
	}
	if list, _ := s.ListExtensionCodes(user.UserID); len(list) != 0 {
		t.Fatalf("list after deletes: %+v", list)
	}

	// The highest id was deleted: the next code never gets it again.
	next, err := s.CreateCode(user.UserID, "Next", KindBot)
	if err != nil || next.ID <= keep.ID {
		t.Fatalf("next id %d after deleting %d: %v", next.ID, keep.ID, err)
	}
}

func TestHTTP_ExtensionCodeViewAndDelete(t *testing.T) {
	s, _, rec := newTestService(t)
	s.SetCodeCipher(fakeCodeCipher{}, false)
	a := newAPI(t, s)
	c := newClient()
	inv, _ := s.CreateInvite(0, RoleUser)
	a.call(c, "POST", "/api/auth/register", map[string]string{"code": inv.Code, "email": "u@example.com", "password": testPassword}, nil)
	https := map[string]string{"X-Forwarded-Proto": "https"}

	_, b := a.call(c, "POST", "/api/me/extension-codes", map[string]string{"name": "Laptop"}, nil)
	created := obj(t, b)
	ext := created["code"].(string)
	id := int64(created["id"].(float64))
	path := fmt.Sprintf("/api/me/extension-codes/%d", id)

	if code, b := a.call(c, "GET", "/api/me/extension-codes", nil, nil); code != 200 || !strings.Contains(string(b), `"viewable":true`) || strings.Contains(string(b), ext) {
		t.Fatalf("list: %d %s", code, b)
	}
	// HTTPS first.
	if code, b := a.call(c, "GET", path+"/code", nil, nil); code != 403 || obj(t, b)["error"] != "https_required" {
		t.Fatalf("plain http: %d %s", code, b)
	}
	if code, b := a.call(c, "GET", path+"/code", nil, map[string]string{"X-Forwarded-Proto": "http"}); code != 403 || obj(t, b)["error"] != "https_required" {
		t.Fatalf("forwarded http: %d %s", code, b)
	}
	req, _ := http.NewRequest("GET", a.base+path+"/code", nil)
	req.Header.Add("X-Forwarded-Proto", "https")
	req.Header.Add("X-Forwarded-Proto", "https")
	if resp, err := c.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("two forwarded values: %v %v", resp, err)
	}
	// Session only.
	bare := &http.Client{}
	auth := map[string]string{"Authorization": "Bearer " + ext, "Origin": "chrome-extension://abc", "X-Forwarded-Proto": "https"}
	if code, b := a.call(bare, "GET", path+"/code", nil, auth); code != 403 || obj(t, b)["error"] != "session_required" {
		t.Fatalf("code bearer: %d %s", code, b)
	}
	if code, _ := a.call(bare, "GET", path+"/code", nil, https); code != 401 {
		t.Fatalf("no credential: %d", code)
	}
	req, _ = http.NewRequest("GET", a.base+path+"/code", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("reveal: %v %v", resp, err)
	}
	resp.Body.Close()
	if code, b := a.call(c, "GET", path+"/code", nil, https); code != 200 || obj(t, b)["code"] != ext {
		t.Fatalf("reveal body: %d %s", code, b)
	}
	if code, b := a.call(c, "GET", "/api/me/extension-codes/999/code", nil, https); code != 404 || obj(t, b)["error"] != "not_found" {
		t.Fatalf("unknown id: %d %s", code, b)
	}
	if code, b := a.call(c, "GET", "/api/me/extension-codes/x/code", nil, https); code != 404 {
		t.Fatalf("bad id: %d %s", code, b)
	}

	// A code made before the key: 409 not_viewable.
	s.SetCodeCipher(nil, false)
	_, b = a.call(c, "POST", "/api/me/extension-codes", map[string]string{"name": "Old"}, nil)
	oldID := int64(obj(t, b)["id"].(float64))
	s.SetCodeCipher(fakeCodeCipher{}, false)
	if code, b := a.call(c, "GET", fmt.Sprintf("/api/me/extension-codes/%d/code", oldID), nil, https); code != 409 || obj(t, b)["error"] != "not_viewable" {
		t.Fatalf("not viewable: %d %s", code, b)
	}

	// Revoke: 409 revoked.
	if code, _ := a.call(c, "DELETE", path, nil, nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code, b := a.call(c, "GET", path+"/code", nil, https); code != 409 || obj(t, b)["error"] != "revoked" {
		t.Fatalf("revoked: %d %s", code, b)
	}
	if code, b := a.call(c, "DELETE", path+"?permanent=yes", nil, nil); code != 400 || obj(t, b)["error"] != "bad_query" {
		t.Fatalf("bad permanent: %d %s", code, b)
	}
	if code, _ := a.call(c, "DELETE", path+"?permanent=1", nil, nil); code != 204 {
		t.Fatalf("delete a revoked code: %d", code)
	}
	if code, _ := a.call(c, "DELETE", path+"?permanent=1", nil, nil); code != 404 {
		t.Fatalf("delete twice: %d", code)
	}
	if code, _ := a.call(c, "GET", path+"/code", nil, https); code != 404 {
		t.Fatalf("reveal a deleted code: %d", code)
	}
	// An active code: revoked first (sockets closed), then gone; a code bearer cannot delete.
	oldPath := fmt.Sprintf("/api/me/extension-codes/%d", oldID)
	_, b = a.call(c, "POST", "/api/me/extension-codes", map[string]string{"name": "Third"}, nil)
	third := obj(t, b)["code"].(string)
	if code, b := a.call(bare, "DELETE", oldPath+"?permanent=1", nil, map[string]string{"Authorization": "Bearer " + third}); code != 403 || obj(t, b)["error"] != "session_required" {
		t.Fatalf("code bearer delete: %d %s", code, b)
	}
	before := len(rec.codes)
	if code, _ := a.call(c, "DELETE", oldPath+"?permanent=1", nil, nil); code != 204 {
		t.Fatalf("delete an active code: %d", code)
	}
	if len(rec.codes) != before+1 || rec.codes[before] != oldID {
		t.Fatalf("sockets of the deleted code: %v", rec.codes)
	}
	if code, b := a.call(c, "GET", "/api/me/extension-codes", nil, nil); code != 200 || strings.Contains(string(b), `"Old"`) || strings.Contains(string(b), `"Laptop"`) {
		t.Fatalf("list after delete: %d %s", code, b)
	}

	// BOT_LOGINS_ALLOW_HTTP: plain http answers.
	s.SetCodeCipher(fakeCodeCipher{}, true)
	_, b = a.call(c, "POST", "/api/me/extension-codes", map[string]string{"name": "Dev"}, nil)
	dev := obj(t, b)
	if code, b := a.call(c, "GET", fmt.Sprintf("/api/me/extension-codes/%d/code", int64(dev["id"].(float64))), nil, nil); code != 200 || obj(t, b)["code"] != dev["code"] {
		t.Fatalf("allow http: %d %s", code, b)
	}
}
