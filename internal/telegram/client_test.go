package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPClient_SendPostsHTMLAndReturnsID(t *testing.T) {
	var gotPath string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	}))
	defer srv.Close()
	c := NewHTTPClient("TOKEN", "-5306930735")
	c.BaseURL = srv.URL
	id, err := c.Send(context.Background(), "<b>hi</b>")
	if err != nil || id != 42 {
		t.Fatalf("Send = %d, %v", id, err)
	}
	if gotPath != "/botTOKEN/sendMessage" {
		t.Fatalf("path = %s", gotPath)
	}
	if body["chat_id"] != "-5306930735" || body["parse_mode"] != "HTML" || body["text"] != "<b>hi</b>" {
		t.Fatalf("body = %v", body)
	}
	if lp, _ := body["link_preview_options"].(map[string]any); lp["is_disabled"] != true {
		t.Fatalf("link previews must be disabled: %v", body)
	}
}

func TestHTTPClient_ParsesAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`)
	}))
	defer srv.Close()
	c := NewHTTPClient("T", "1")
	c.BaseURL = srv.URL
	_, err := c.Send(context.Background(), "x")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 429 || ae.RetryAfter != 7 {
		t.Fatalf("err = %#v", err)
	}
}

func TestHTTPClient_EditNotModifiedIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/editMessageText") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same"}`)
	}))
	defer srv.Close()
	c := NewHTTPClient("T", "1")
	c.BaseURL = srv.URL
	if err := c.Edit(context.Background(), 5, "x"); err != nil {
		t.Fatalf("not modified must be nil, got %v", err)
	}
}

func TestHTTPClient_NetworkErrorHidesToken(t *testing.T) {
	c := NewHTTPClient("SECRET123", "1")
	c.BaseURL = "http://127.0.0.1:1" // nothing listens
	_, err := c.Send(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "SECRET123") {
		t.Fatalf("err must exist and not leak the token: %v", err)
	}
}

func TestErrorClassifiers(t *testing.T) {
	if !retryable(errors.New("dial tcp: refused")) || !retryable(&APIError{Code: 502}) || retryable(&APIError{Code: 400}) || retryable(&APIError{Code: 429}) {
		t.Fatal("retryable")
	}
	if !isMessageGone(&APIError{Code: 400, Description: "Bad Request: message to edit not found"}) {
		t.Fatal("isMessageGone")
	}
	migrated := &APIError{Code: 400, Description: "Bad Request: group chat was upgraded to a supergroup chat", MigrateToChatID: -1001234}
	if !strings.Contains(migrated.Error(), "-1001234") {
		t.Fatal("migration hint must name the new chat id")
	}
}
