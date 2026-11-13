package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Handler serves /api/telegram/*. A nil notifier (TELEGRAM_TOKEN or TELEGRAM_CHAT_ID unset)
// answers status with configured:false and everything else with 503. userOf names the signed-in
// user of a request (0 or a nil func = unknown); a saved config records it as updated_by.
func Handler(n *Notifier, userOf func(*http.Request) int64) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/telegram/status", func(w http.ResponseWriter, _ *http.Request) {
		if n == nil {
			writeJSON(w, http.StatusOK, Status{Configured: false})
			return
		}
		writeJSON(w, http.StatusOK, n.Status())
	})
	mux.HandleFunc("GET /api/telegram/config", func(w http.ResponseWriter, _ *http.Request) {
		if n == nil {
			unconfigured(w)
			return
		}
		writeJSON(w, http.StatusOK, n.Config())
	})
	mux.HandleFunc("PUT /api/telegram/config", func(w http.ResponseWriter, r *http.Request) {
		if n == nil {
			unconfigured(w)
			return
		}
		var c BotConfig
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&c); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		c.UpdatedBy = 0
		if userOf != nil {
			c.UpdatedBy = userOf(r)
		}
		saved, err := n.SetConfig(r.Context(), c)
		var ve *ValidationError
		switch {
		case errors.As(err, &ve):
			writeErr(w, http.StatusBadRequest, ve.Error())
		case err != nil:
			writeErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSON(w, http.StatusOK, saved)
		}
	})
	mux.HandleFunc("POST /api/telegram/test", func(w http.ResponseWriter, r *http.Request) {
		if n == nil {
			unconfigured(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		id, err := n.SendTest(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"message_id": id})
	})
	return mux
}

func unconfigured(w http.ResponseWriter) {
	writeErr(w, http.StatusServiceUnavailable, "telegram not configured: set TELEGRAM_TOKEN and TELEGRAM_CHAT_ID")
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
