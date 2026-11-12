package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// jpeg is a fake JPEG of n bytes: the FF D8 FF magic, then filler.
func jpeg(n int) []byte {
	b := bytes.Repeat([]byte{0x42}, n)
	copy(b, []byte{0xFF, 0xD8, 0xFF, 0xE0})
	return b
}

func postShot(t *testing.T, srv *httptest.Server, code string, body []byte) (int, string) {
	t.Helper()
	return request(t, srv, "POST", "/api/me/bot/screenshots?window=fortuna-1&order=c81e55a0deadbeef", "", string(body),
		map[string]string{"Authorization": "Bearer " + code, "Content-Type": "image/jpeg"})
}

// getShot fetches a screenshot with a session and returns status, Content-Type and body.
func getShot(t *testing.T, srv *httptest.Server, token, id string) (int, string, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/api/me/bot/screenshots/"+id, nil)
	req.Header.Set("Cookie", accounts.CookieName+"="+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), b
}

func TestBotShots_UploadAndFetch(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	img := jpeg(1000)

	s, body := postShot(t, srv, code, img) // no bot socket needed
	var res struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(body), &res); s != 201 || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(res.ID) {
		t.Fatalf("upload: %d %s", s, body)
	}
	onDisk, err := os.ReadFile(filepath.Join(bots.shotsDir, fmt.Sprint(userIDOf(t, svc, token)), res.ID+".jpg"))
	if err != nil || !bytes.Equal(onDisk, img) {
		t.Fatalf("stored file: %v (%d bytes)", err, len(onDisk))
	}
	if s, ct, got := getShot(t, srv, token, res.ID); s != 200 || ct != "image/jpeg" || !bytes.Equal(got, img) {
		t.Fatalf("fetch: %d %s (%d bytes)", s, ct, len(got))
	}

	otherToken := newUser(t, svc, "o@example.com", accounts.RoleUser)
	if s, _, _ := getShot(t, srv, otherToken, res.ID); s != 404 {
		t.Fatalf("another user's screenshot: %d, want 404", s)
	}
	for _, id := range []string{"0123456789abcdef", "ABCDEF0123456789", "0123456789abcde"} {
		if s, _, _ := getShot(t, srv, token, id); s != 404 {
			t.Errorf("id %q: %d, want 404", id, s)
		}
	}
	if s, b := request(t, srv, "GET", "/api/me/bot/screenshots/"+res.ID, "", "", bearer(code)); s != 403 || !strings.Contains(b, `"session_required"`) {
		t.Fatalf("fetch with a bot code: %d %s", s, b)
	}
}

func TestBotShots_UploadErrors(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")

	if s, b := postShot(t, srv, code, []byte("\x89PNG\r\n\x1a\n")); s != 400 || !strings.Contains(b, `"bad_image"`) {
		t.Fatalf("PNG: %d %s", s, b)
	}
	if s, b := postShot(t, srv, code, nil); s != 400 || !strings.Contains(b, `"bad_image"`) {
		t.Fatalf("empty body: %d %s", s, b)
	}
	if s, b := postShot(t, srv, code, jpeg(maxShotBytes+1)); s != 413 || !strings.Contains(b, `"too_large"`) {
		t.Fatalf("1 500 001 bytes: %d %s", s, b)
	}
	if s, b := postShot(t, srv, code, jpeg(maxShotBytes)); s != 201 {
		t.Fatalf("1 500 000 bytes: %d %s", s, b)
	}
	if s, b := request(t, srv, "POST", "/api/me/bot/screenshots", token, string(jpeg(10)), nil); s != 403 || !strings.Contains(b, `"bot_code_required"`) {
		t.Fatalf("upload with a session: %d %s", s, b)
	}
}

func TestBotShots_RateLimitPerUser(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	clk := fixClock(bots)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	sameUser := newCode(t, srv, token, "bot")
	otherUser := newCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "bot")

	for i := 0; i < 30; i++ {
		c := code
		if i%2 == 1 {
			c = sameUser
		}
		if s, b := postShot(t, srv, c, jpeg(100)); s != 201 {
			t.Fatalf("shot %d: %d %s", i, s, b)
		}
	}
	clk.advance(59 * time.Second)
	if s, b := postShot(t, srv, sameUser, jpeg(100)); s != 429 || !strings.Contains(b, `"too_many_shots"`) {
		t.Fatalf("shot 31: %d %s", s, b)
	}
	if s, b := postShot(t, srv, otherUser, jpeg(100)); s != 201 {
		t.Fatalf("another user has its own budget: %d %s", s, b)
	}
	clk.advance(time.Second)
	if s, b := postShot(t, srv, code, jpeg(100)); s != 201 {
		t.Fatalf("next minute: %d %s", s, b)
	}
}

func TestBotShots_UserDirCap(t *testing.T) {
	srv, svc, _ := newBotServer(t)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	other := newCode(t, srv, newUser(t, svc, "o@example.com", accounts.RoleUser), "bot")
	defer func(n int, b int64) { maxShotFiles, maxShotDirBytes = n, b }(maxShotFiles, maxShotDirBytes)

	maxShotFiles, maxShotDirBytes = 3, 1<<30
	for i := 0; i < 3; i++ {
		if s, b := postShot(t, srv, code, jpeg(100)); s != 201 {
			t.Fatalf("file %d: %d %s", i+1, s, b)
		}
	}
	if s, b := postShot(t, srv, code, jpeg(100)); s != 429 || !strings.Contains(b, `"shots_full"`) {
		t.Fatalf("file 4: %d %s", s, b)
	}
	if s, b := postShot(t, srv, other, jpeg(100)); s != 201 {
		t.Fatalf("another user's directory: %d %s", s, b)
	}

	maxShotFiles, maxShotDirBytes = 100, 300+250 // 3 × 100 bytes stored
	if s, b := postShot(t, srv, code, jpeg(251)); s != 429 || !strings.Contains(b, `"shots_full"`) {
		t.Fatalf("over the byte cap: %d %s", s, b)
	}
	if s, b := postShot(t, srv, code, jpeg(250)); s != 201 {
		t.Fatalf("up to the byte cap: %d %s", s, b)
	}
}

// A screenshot that could not be stored does not count against the minute's budget.
func TestBotShots_FailedWriteIsNotCounted(t *testing.T) {
	srv, svc, bots := newBotServer(t)
	fixClock(bots)
	token := newUser(t, svc, "u@example.com", accounts.RoleUser)
	code := newCode(t, srv, token, "bot")
	if err := os.WriteFile(bots.shotsDir, nil, 0o600); err != nil { // a file where the directory goes
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if s, b := postShot(t, srv, code, jpeg(100)); s != 500 {
			t.Fatalf("failing write %d: %d %s", i, s, b)
		}
	}
	if err := os.Remove(bots.shotsDir); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if s, b := postShot(t, srv, code, jpeg(100)); s != 201 {
			t.Fatalf("shot %d after the failures: %d %s", i, s, b)
		}
	}
}

// A write that fails after its temporary file was created removes that file.
func TestBotShots_FailedWriteRemovesTmp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "0123456789abcdef.jpg")
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil { // the rename target is a non-empty directory
		t.Fatal(err)
	}
	if err := writeShot(path, jpeg(10)); err == nil {
		t.Fatal("writeShot over a directory must fail")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("the temporary file is left: %v", err)
	}
}
