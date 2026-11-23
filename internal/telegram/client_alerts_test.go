package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A request that surely never reached Telegram (connection refused, DNS, a connect that timed out before the
// connection was made) is a NetError with NotSent; a timeout once connected is a NetError without it (Telegram may
// have taken it). The token never shows in the error.
func TestHTTPClient_NetErrorSaysWhetherItWasSent(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close() // nothing listens there any more: connection refused
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer slow.Close()
	// The name does not resolve (no real lookup: the dialer answers as DNS would).
	noSuchHost := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "bot.example", IsNotFound: true}
	}}
	// The TCP connect never completes: the client's timeout fires while dialing.
	hangingDial := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	for _, tc := range []struct {
		name, base string
		timeout    time.Duration
		transport  http.RoundTripper
		notSent    bool
	}{
		{"refused", closed.URL, 2 * time.Second, nil, true},
		{"dns", "http://bot.example", 5 * time.Second, noSuchHost, true},
		{"dial timeout", slow.URL, 50 * time.Millisecond, hangingDial, true},
		{"timeout", slow.URL, 50 * time.Millisecond, nil, false},
	} {
		c := NewHTTPClient("SECRET", "-100")
		c.BaseURL, c.HTTP = tc.base, &http.Client{Timeout: tc.timeout, Transport: tc.transport}
		_, err := c.SendTo(context.Background(), 555, "x")
		var ne *NetError
		if !errors.As(err, &ne) || ne.NotSent != tc.notSent {
			t.Fatalf("%s: %#v, want a NetError with NotSent %v", tc.name, err, tc.notSent)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("%s: the token leaked: %v", tc.name, err)
		}
	}
}

// apiServer answers every Bot API call with result and records the last method and body.
func apiServer(t *testing.T, result string) (*HTTPClient, *string, *map[string]any) {
	t.Helper()
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		body = nil
		_ = json.Unmarshal(raw, &body)
		_, _ = io.WriteString(w, `{"ok":true,"result":`+result+`}`)
	}))
	t.Cleanup(srv.Close)
	c := NewHTTPClient("TOKEN", "-100")
	c.BaseURL = srv.URL
	return c, &path, &body
}

func TestHTTPClient_SendToPostsPlainTextToTheChat(t *testing.T) {
	c, path, body := apiServer(t, `{"message_id":7}`)
	id, err := c.SendTo(context.Background(), 555, "⚠️ Outcome unknown <b>")
	if err != nil || id != 7 {
		t.Fatalf("SendTo = %d, %v", id, err)
	}
	b := *body
	if *path != "/botTOKEN/sendMessage" || b["chat_id"] != float64(555) || b["text"] != "⚠️ Outcome unknown <b>" {
		t.Fatalf("path %s body %v", *path, b)
	}
	if _, ok := b["parse_mode"]; ok {
		t.Fatalf("alerts are plain text: %v", b)
	}
	if lp, _ := b["link_preview_options"].(map[string]any); lp["is_disabled"] != true {
		t.Fatalf("link previews must be off: %v", b)
	}
}

func TestHTTPClient_GetUpdates(t *testing.T) {
	c, path, body := apiServer(t, `[{"update_id":10,"message":{"chat":{"id":555,"type":"private"},"text":"/start abc"}},{"update_id":11}]`)
	ups, err := c.GetUpdates(context.Background(), 9, 10)
	if err != nil || len(ups) != 2 {
		t.Fatalf("GetUpdates = %+v, %v", ups, err)
	}
	if *path != "/botTOKEN/getUpdates" || (*body)["offset"] != float64(9) || (*body)["timeout"] != float64(10) {
		t.Fatalf("path %s body %v", *path, *body)
	}
	if au, _ := (*body)["allowed_updates"].([]any); len(au) != 1 || au[0] != "message" {
		t.Fatalf("allowed_updates: %v", *body)
	}
	m := ups[0].Message
	if ups[0].UpdateID != 10 || m == nil || m.Chat.ID != 555 || m.Chat.Type != "private" || m.Text != "/start abc" || ups[1].Message != nil {
		t.Fatalf("updates: %+v", ups)
	}
}

func TestHTTPClient_GetMe(t *testing.T) {
	c, path, _ := apiServer(t, `{"id":1,"is_bot":true,"username":"sb_alerts_bot"}`)
	u, err := c.GetMe(context.Background())
	if err != nil || u != "sb_alerts_bot" || *path != "/botTOKEN/getMe" {
		t.Fatalf("GetMe = %q, %v (path %s)", u, err, *path)
	}
}
