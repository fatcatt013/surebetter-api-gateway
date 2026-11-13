package main

import (
	"encoding/json"
	"net/http"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// botLimits is the limits part of the settings body (Kč; 0 = no limit).
type botLimits struct {
	MaxOrder       int64 `json:"maxOrder"`
	MaxLeg         int64 `json:"maxLeg"`
	PerDay         int64 `json:"perDay"`
	OrdersAtOnce   int   `json:"ordersAtOnce"`
	PauseOnUnknown bool  `json:"pauseOnUnknown"`
}

// botSettingsView is the GET/PUT /api/me/bot/settings body.
type botSettingsView struct {
	DryRun       bool      `json:"dryRun"`
	Paused       bool      `json:"paused"`
	PausedReason string    `json:"pausedReason"`
	Limits       botLimits `json:"limits"`
}

func settingsView(st accounts.BotSettings) botSettingsView {
	return botSettingsView{DryRun: st.DryRun, Paused: st.Paused, PausedReason: st.PausedReason, Limits: botLimits{MaxOrder: st.MaxOrder,
		MaxLeg: st.MaxLeg, PerDay: st.PerDay, OrdersAtOnce: st.OrdersAtOnce, PauseOnUnknown: st.PauseOnUnknown}}
}

// botSettingsIn is a PUT body: any subset of the fields (absent or null = unchanged).
type botSettingsIn struct {
	DryRun *bool `json:"dryRun"`
	Paused *bool `json:"paused"`
	Limits *struct {
		MaxOrder       *int64 `json:"maxOrder"`
		MaxLeg         *int64 `json:"maxLeg"`
		PerDay         *int64 `json:"perDay"`
		OrdersAtOnce   *int   `json:"ordersAtOnce"`
		PauseOnUnknown *bool  `json:"pauseOnUnknown"`
	} `json:"limits"`
}

// maxKc is the largest Kč limit.
const maxKc = 10_000_000

// apply returns st with in's fields, false when one is out of range (Kč 0–10 000 000, ordersAtOnce 1–20).
// paused is left to the caller.
func (in botSettingsIn) apply(st accounts.BotSettings) (accounts.BotSettings, bool) {
	if in.DryRun != nil {
		st.DryRun = *in.DryRun
	}
	if l := in.Limits; l != nil {
		for _, kc := range []struct {
			in  *int64
			out *int64
		}{{l.MaxOrder, &st.MaxOrder}, {l.MaxLeg, &st.MaxLeg}, {l.PerDay, &st.PerDay}} {
			if kc.in == nil {
				continue
			}
			if *kc.in < 0 || *kc.in > maxKc {
				return st, false
			}
			*kc.out = *kc.in
		}
		if l.OrdersAtOnce != nil {
			if *l.OrdersAtOnce < 1 || *l.OrdersAtOnce > 20 {
				return st, false
			}
			st.OrdersAtOnce = *l.OrdersAtOnce
		}
		if l.PauseOnUnknown != nil {
			st.PauseOnUnknown = *l.PauseOnUnknown
		}
	}
	return st, true
}

func (b *botHub) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	if err := b.loadSettings(userID); err != nil {
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	b.mu.Lock()
	st := b.settingsLocked(userID)
	b.mu.Unlock()
	accounts.WriteJSON(w, http.StatusOK, settingsView(st))
}

// handlePutSettings changes the user's settings. paused:true (when not paused) pauses with reason
// "Paused" (waiting orders end, agents get {"type":"pause"}); paused:false clears the reason. A
// change is saved, tells the app ({"type":"bot"}) and retries the waiting orders.
func (b *botHub) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in botSettingsIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		accounts.WriteError(w, http.StatusBadRequest, "bad_settings")
		return
	}
	userID := principalOf(r).UserID
	if err := b.loadSettings(userID); err != nil { // never save over a row that could not be read
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	b.mu.Lock()
	old := b.settingsLocked(userID)
	st, ok := in.apply(old)
	if !ok {
		b.mu.Unlock()
		accounts.WriteError(w, http.StatusBadRequest, "bad_settings")
		return
	}
	pausing := in.Paused != nil && *in.Paused && !old.Paused
	if in.Paused != nil && !*in.Paused {
		st.Paused, st.PausedReason = false, ""
	}
	fx := &effects{}
	if st != old || pausing {
		b.saveSettingsLocked(userID, st, fx)
		if pausing {
			b.pauseLocked(userID, "Paused", fx)
		}
		fx.notify(userID)
		b.dispatchLocked(userID, fx)
	}
	out := settingsView(b.settingsLocked(userID))
	b.release(fx)
	accounts.WriteJSON(w, http.StatusOK, out)
}
