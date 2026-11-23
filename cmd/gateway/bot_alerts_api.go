package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

const (
	alertsDefaultLimit = 50
	alertsMaxLimit     = 200
)

// handleGetAlerts is GET /api/me/bot/alerts?limit=: the alerts not dismissed (open first, newest first) and the count
// of open attention and problem alerts.
func (b *botHub) handleGetAlerts(w http.ResponseWriter, r *http.Request) {
	limit := alertsDefaultLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > alertsMaxLimit {
			accounts.WriteError(w, http.StatusBadRequest, "bad_query")
			return
		}
		limit = n
	}
	userID := principalOf(r).UserID
	rows, err := b.acc.ListBotAlerts(userID, limit)
	var open int
	if err == nil {
		open, err = b.acc.CountOpenBotAlerts(userID)
	}
	if err != nil {
		log.Error().Err(err).Msg("bot alerts list failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]botAlert, 0, len(rows))
	for _, row := range rows {
		out = append(out, alertOfRow(row))
	}
	accounts.WriteJSON(w, http.StatusOK, struct {
		Alerts []botAlert `json:"alerts"`
		Open   int        `json:"open"`
	}{out, open})
}

// handleDismissAlert is POST /api/me/bot/alerts/{id}/dismiss; the user's app sockets get the dismissed alert.
func (b *botHub) handleDismissAlert(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	row, found, err := b.acc.DismissBotAlert(userID, r.PathValue("id"), b.now().UnixMilli())
	switch {
	case err != nil:
		log.Error().Err(err).Msg("bot alert dismiss failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	case !found:
		accounts.WriteError(w, http.StatusNotFound, "unknown_alert")
		return
	}
	a := alertOfRow(row)
	b.reg.sendUser(userID, alertMessage(a))
	accounts.WriteJSON(w, http.StatusOK, a)
}

// handleDismissAllAlerts is POST /api/me/bot/alerts/dismiss-all; each dismissed alert goes to the app sockets.
func (b *botHub) handleDismissAllAlerts(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	rows, err := b.acc.DismissAllBotAlerts(userID, b.now().UnixMilli())
	if err != nil {
		log.Error().Err(err).Msg("bot alerts dismiss all failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	for _, row := range rows {
		b.reg.sendUser(userID, alertMessage(alertOfRow(row)))
	}
	accounts.WriteJSON(w, http.StatusOK, map[string]int{"dismissed": len(rows)})
}

// handleGetAlertSettings is GET /api/me/bot/alert-settings (the defaults without a row).
func (b *botHub) handleGetAlertSettings(w http.ResponseWriter, r *http.Request) {
	js, found, err := b.acc.BotAlertSettings(principalOf(r).UserID)
	if err != nil {
		log.Error().Err(err).Msg("bot alert settings read failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	st := defaultAlertSettings()
	if found {
		st = alertSettingsOf(js)
	}
	accounts.WriteJSON(w, http.StatusOK, st)
}

// alertSettingsIn is a PUT body: every kind and switch is required.
type alertSettingsIn struct {
	Attention *alertChannelsIn `json:"attention"`
	Window    *alertChannelsIn `json:"window"`
	Agent     *alertChannelsIn `json:"agent"`
	Order     *alertChannelsIn `json:"order"`
}

type alertChannelsIn struct {
	App      *bool `json:"app"`
	Telegram *bool `json:"telegram"`
}

// settings is in as alertSettings; false when a kind or a switch is missing.
func (in alertSettingsIn) settings() (alertSettings, bool) {
	var st alertSettings
	for _, k := range []struct {
		in  *alertChannelsIn
		out *alertChannels
	}{{in.Attention, &st.Attention}, {in.Window, &st.Window}, {in.Agent, &st.Agent}, {in.Order, &st.Order}} {
		if k.in == nil || k.in.App == nil || k.in.Telegram == nil {
			return st, false
		}
		*k.out = alertChannels{App: *k.in.App, Telegram: *k.in.Telegram}
	}
	return st, true
}

// handlePutAlertSettings is PUT /api/me/bot/alert-settings: stored, and the engine uses them at once.
func (b *botHub) handlePutAlertSettings(w http.ResponseWriter, r *http.Request) {
	var in alertSettingsIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		accounts.WriteError(w, http.StatusBadRequest, "bad_alert_settings")
		return
	}
	st, ok := in.settings()
	if !ok {
		accounts.WriteError(w, http.StatusBadRequest, "bad_alert_settings")
		return
	}
	userID := principalOf(r).UserID
	if err := b.acc.SaveBotAlertSettings(userID, mustJSON(st)); err != nil {
		log.Error().Err(err).Msg("bot alert settings save failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	b.mu.Lock()
	if b.alerts != nil {
		b.alerts.settings[userID] = st
	}
	b.mu.Unlock()
	accounts.WriteJSON(w, http.StatusOK, st)
}
