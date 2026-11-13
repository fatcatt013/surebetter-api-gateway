package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler_Unconfigured(t *testing.T) {
	h := Handler(nil, nil)
	if rec := do(t, h, "GET", "/api/telegram/status", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	for _, c := range [][2]string{{"GET", "/api/telegram/config"}, {"PUT", "/api/telegram/config"}, {"POST", "/api/telegram/test"}} {
		if rec := do(t, h, c[0], c[1], "{}"); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", c[0], c[1], rec.Code)
		}
	}
}

func TestHandler_ConfigRoundTripAndValidation(t *testing.T) {
	n, _ := newTestNotifier(t, newMemStore(), nil)
	h := Handler(n, nil)
	cfg := validConfig()
	cfg.Preset.Name = "New"
	raw, _ := json.Marshal(cfg)
	if rec := do(t, h, "PUT", "/api/telegram/config", string(raw)); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"New"`) {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/telegram/config", ""); !strings.Contains(rec.Body.String(), `"name":"New"`) {
		t.Fatalf("GET: %s", rec.Body)
	}
	if rec := do(t, h, "PUT", "/api/telegram/config", `{"enabled":true}`); rec.Code != 400 {
		t.Fatalf("invalid PUT = %d, want 400", rec.Code)
	}
	if rec := do(t, h, "PUT", "/api/telegram/config", `{nope`); rec.Code != 400 {
		t.Fatalf("bad JSON = %d, want 400", rec.Code)
	}
}

func TestHandler_TestAndStatus(t *testing.T) {
	n, _ := newTestNotifier(t, newMemStore(), nil)
	h := Handler(n, nil)
	if rec := do(t, h, "POST", "/api/telegram/test", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"message_id":99`) {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/telegram/status", ""); !strings.Contains(rec.Body.String(), `"configured":true`) {
		t.Fatalf("status: %s", rec.Body)
	}
}

func TestHandler_PutConfigRecordsWhoSavedIt(t *testing.T) {
	n, _ := newTestNotifier(t, newMemStore(), nil)
	h := Handler(n, func(*http.Request) int64 { return 7 })
	cfg := validConfig()
	cfg.UpdatedBy = 99 // a client cannot claim someone else
	raw, _ := json.Marshal(cfg)
	if rec := do(t, h, "PUT", "/api/telegram/config", string(raw)); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"updated_by":7`) {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if n.Config().UpdatedBy != 7 {
		t.Fatalf("updated_by = %d, want 7", n.Config().UpdatedBy)
	}
	anon := Handler(n, nil) // AUTH_REQUIRED=false: nobody known
	if rec := do(t, anon, "PUT", "/api/telegram/config", string(raw)); rec.Code != 200 || n.Config().UpdatedBy != 0 {
		t.Fatalf("anonymous PUT: %d %s", rec.Code, rec.Body)
	}
}
