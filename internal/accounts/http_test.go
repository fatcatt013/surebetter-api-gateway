package accounts

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

type api struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newAPI(t *testing.T, s *Service) *api {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &api{t: t, base: srv.URL, c: newClient()}
}

func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

// call sends a same-origin JSON request (Origin = server URL). hdr overrides headers;
// an empty value deletes the header.
func (a *api) call(c *http.Client, method, path string, body any, hdr map[string]string) (int, []byte) {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, rd)
	req.Header.Set("Origin", a.base)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func obj(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not a JSON object: %s", b)
	}
	return m
}

func TestHTTP_RegisterMeLogout(t *testing.T) {
	s, _, _ := newTestService(t)
	a := newAPI(t, s)
	inv, _ := s.CreateInvite(0, RoleUser)
	c := newClient()

	if code, b := a.call(c, "POST", "/api/auth/invite/check", map[string]string{"code": inv.Code}, nil); code != 200 || obj(t, b)["valid"] != true {
		t.Fatalf("invite check: %d %s", code, b)
	}
	if code, b := a.call(c, "POST", "/api/auth/invite/check", map[string]string{"code": "AAAA-BBBB-CCCC"}, nil); code != 200 || obj(t, b)["reason"] != "unknown" {
		t.Fatalf("unknown invite: %d %s", code, b)
	}
	code, b := a.call(c, "POST", "/api/auth/register", map[string]string{"code": inv.Code, "email": "u@example.com", "password": testPassword}, nil)
	if code != 200 || obj(t, b)["email"] != "u@example.com" || obj(t, b)["onboarded"] != false {
		t.Fatalf("register: %d %s", code, b)
	}
	if code, b := a.call(c, "GET", "/api/auth/me", nil, nil); code != 200 || obj(t, b)["role"] != "user" {
		t.Fatalf("me: %d %s", code, b)
	}
	if code, _ := a.call(c, "POST", "/api/me/onboarded", map[string]any{}, nil); code != 204 {
		t.Fatalf("onboarded: %d", code)
	}
	if code, b := a.call(c, "GET", "/api/auth/me", nil, nil); obj(t, b)["onboarded"] != true {
		t.Fatalf("me after onboarding: %d %s", code, b)
	}
	if code, _ := a.call(c, "POST", "/api/auth/logout", map[string]any{}, nil); code != 204 {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := a.call(c, "GET", "/api/auth/me", nil, nil); code != 401 {
		t.Fatalf("me after logout: %d", code)
	}
	code, b = a.call(c, "POST", "/api/auth/register", map[string]string{"code": inv.Code, "email": "v@example.com", "password": testPassword}, nil)
	if code != 400 || obj(t, b)["reason"] != "used" {
		t.Fatalf("used invite: %d %s", code, b)
	}
}

func TestHTTP_LoginErrorsAndLock(t *testing.T) {
	s, _, _ := newTestService(t)
	a := newAPI(t, s)
	mustUser(t, s, "u@example.com", RoleUser)
	c := newClient()
	for i := 0; i < 5; i++ {
		if code, b := a.call(c, "POST", "/api/auth/login", map[string]string{"email": "u@example.com", "password": "wrong password"}, nil); code != 401 || obj(t, b)["error"] != "invalid_credentials" {
			t.Fatalf("wrong password %d: %d %s", i, code, b)
		}
	}
	code, b := a.call(c, "POST", "/api/auth/login", map[string]string{"email": "u@example.com", "password": testPassword}, nil)
	if code != 429 || obj(t, b)["error"] != "locked" || obj(t, b)["retry_after"] != float64(900) {
		t.Fatalf("locked: %d %s", code, b)
	}
}

func TestHTTP_CSRF(t *testing.T) {
	s, _, _ := newTestService(t)
	a := newAPI(t, s)
	c := newClient()
	body := map[string]string{"email": "u@example.com", "password": testPassword}
	if code, b := a.call(c, "POST", "/api/auth/login", body, map[string]string{"Origin": ""}); code != 403 || obj(t, b)["error"] != "bad_origin" {
		t.Fatalf("no Origin: %d %s", code, b)
	}
	if code, _ := a.call(c, "POST", "/api/auth/login", body, map[string]string{"Origin": "http://evil.example"}); code != 403 {
		t.Fatalf("foreign Origin: %d", code)
	}
	if code, _ := a.call(c, "POST", "/api/auth/login", body, map[string]string{"Content-Type": "text/plain"}); code != 415 {
		t.Fatalf("non-JSON body: %d", code)
	}
}

func TestHTTP_StateAndExtensionCodes(t *testing.T) {
	s, _, _ := newTestService(t)
	a := newAPI(t, s)
	c := newClient()
	inv, _ := s.CreateInvite(0, RoleUser)
	a.call(c, "POST", "/api/auth/register", map[string]string{"code": inv.Code, "email": "u@example.com", "password": testPassword}, nil)

	if code, b := a.call(c, "PUT", "/api/me/state/pins", map[string]any{"value": []int{1, 2}}, nil); code != 204 {
		t.Fatalf("put: %d %s", code, b)
	}
	if code, b := a.call(c, "GET", "/api/me/state", nil, nil); code != 200 || string(b) != `{"pins":[1,2]}`+"\n" {
		t.Fatalf("get: %d %q", code, b)
	}
	if code, b := a.call(c, "PUT", "/api/me/state/nope", map[string]any{"value": 1}, nil); code != 400 || obj(t, b)["error"] != "unknown_key" {
		t.Fatalf("unknown key: %d %s", code, b)
	}

	code, b := a.call(c, "POST", "/api/me/extension-codes", map[string]string{"name": "Laptop"}, nil)
	if code != 201 {
		t.Fatalf("create code: %d %s", code, b)
	}
	ext := obj(t, b)["code"].(string)
	if code, b := a.call(c, "GET", "/api/me/extension-codes", nil, nil); code != 200 || !strings.Contains(string(b), `"name":"Laptop"`) || strings.Contains(string(b), ext) {
		t.Fatalf("list must show the name but never the code: %d %s", code, b)
	}

	// The extension: no cookies, Bearer code, Origin of its own.
	bare := &http.Client{}
	auth := map[string]string{"Authorization": "Bearer " + ext, "Origin": "chrome-extension://abc"}
	if code, b := a.call(bare, "GET", "/api/auth/me", nil, auth); code != 200 || obj(t, b)["code_name"] != "Laptop" {
		t.Fatalf("me with code: %d %s", code, b)
	}
	if code, b := a.call(bare, "GET", "/api/me/state", nil, auth); code != 200 || !strings.Contains(string(b), `"pins"`) {
		t.Fatalf("state with code: %d %s", code, b)
	}
	if code, b := a.call(bare, "PUT", "/api/me/state/pins", map[string]any{"value": 1}, auth); code != 403 || obj(t, b)["error"] != "session_required" {
		t.Fatalf("codes are read-only: %d %s", code, b)
	}
	if code, b := a.call(bare, "POST", "/api/auth/ws-ticket", map[string]any{}, auth); code != 200 || obj(t, b)["ticket"] == "" {
		t.Fatalf("ticket: %d %s", code, b)
	}
	if code, _ := a.call(bare, "GET", "/api/auth/me", nil, map[string]string{"Authorization": "Bearer SBX-0000-0000-0000-0000-0000"}); code != 401 {
		t.Fatalf("bad code: %d", code)
	}
}

func TestHTTP_AdminRoutes(t *testing.T) {
	s, _, _ := newTestService(t)
	a := newAPI(t, s)
	admin, user := newClient(), newClient()
	invA, _ := s.CreateInvite(0, RoleAdmin)
	invU, _ := s.CreateInvite(0, RoleUser)
	a.call(admin, "POST", "/api/auth/register", map[string]string{"code": invA.Code, "email": "admin@example.com", "password": testPassword}, nil)
	a.call(user, "POST", "/api/auth/register", map[string]string{"code": invU.Code, "email": "user@example.com", "password": testPassword}, nil)

	if code, _ := a.call(user, "GET", "/api/admin/users", nil, nil); code != 403 {
		t.Fatalf("user on admin route: %d", code)
	}
	code, b := a.call(admin, "GET", "/api/admin/users", nil, nil)
	var users []UserInfo
	if code != 200 || json.Unmarshal(b, &users) != nil || len(users) != 2 {
		t.Fatalf("users: %d %s", code, b)
	}
	if code, b := a.call(admin, "POST", "/api/admin/invites", map[string]any{}, nil); code != 201 || !strings.HasPrefix(obj(t, b)["path"].(string), "/app/invite?code=") {
		t.Fatalf("new invite: %d %s", code, b)
	}
	if code, b := a.call(admin, "POST", "/api/admin/users/"+itoa(users[0].ID)+"/disable", map[string]any{}, nil); code != 400 || obj(t, b)["error"] != "cannot_change_self" {
		t.Fatalf("disable self: %d %s", code, b)
	}
	if code, b := a.call(admin, "POST", "/api/admin/users/"+itoa(users[1].ID)+"/reset", map[string]any{}, nil); code != 201 || !strings.HasPrefix(obj(t, b)["path"].(string), "/app/reset?code=") {
		t.Fatalf("reset: %d %s", code, b)
	}
	if code, _ := a.call(admin, "POST", "/api/admin/users/"+itoa(users[1].ID)+"/disable", map[string]any{}, nil); code != 204 {
		t.Fatalf("disable: %d", code)
	}
	if code, _ := a.call(user, "GET", "/api/auth/me", nil, nil); code != 401 {
		t.Fatalf("disabled user's session: %d", code)
	}
	if code, _ := a.call(admin, "DELETE", "/api/admin/invites/999", nil, nil); code != 404 {
		t.Fatalf("revoke unknown invite: %d", code)
	}
}
