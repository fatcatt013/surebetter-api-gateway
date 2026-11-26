package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
	"github.com/surebetter/api-gateway/internal/arbfilter"
)

// Automatic mode (contract plans/2026-11-25-bot-auto-mode-contract.md): the user's settings, the API (§4.1) and the
// auto part of GET /api/me/bot. The picking is in bot_auto_pick.go, the day's orders and the auto pause in
// bot_auto_state.go, the skip log in bot_auto_log.go.

// The automatic modes.
const (
	autoOff  = "off"
	autoLog  = "log"  // Log only: decisions are logged, nothing is sent
	autoDry  = "dry"  // Dry run: auto orders go with dry_run true
	autoLive = "live" // Live: auto orders go as the controller's mode says
)

// autoModesOf is the modes a deployment of phase accepts (BOT_AUTO_PHASE): A Log only, B + Dry run, C + Live.
func autoModesOf(phase string) []string {
	switch phase {
	case "C":
		return []string{autoOff, autoLog, autoDry, autoLive}
	case "B":
		return []string{autoOff, autoLog, autoDry}
	}
	return []string{autoOff, autoLog}
}

// autoPreset names the saved preset the filter was copied from.
type autoPreset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// autoBookLimits is one bookmaker's auto limits (whole Kč; 0 = none).
type autoBookLimits struct {
	MinStake    int64 `json:"minStake"`
	MaxStake    int64 `json:"maxStake"`
	StakePerDay int64 `json:"stakePerDay"`
}

// autoLimits is the auto mode's own limits (0 = none, except atOnce 1–20).
type autoLimits struct {
	AtOnce int `json:"atOnce"`
	PerDay int `json:"perDay"`
	// PlacedPerDay caps today's placed auto orders of the class (0 = none).
	PlacedPerDay int                       `json:"placedPerDay"`
	StakePerDay  int64                     `json:"stakePerDay"`
	GapSec       int                       `json:"gapSec"`
	Books        []string                  `json:"books"` // [] = every bot book
	PerBook      map[string]autoBookLimits `json:"perBook"`
}

func defaultAutoLimits() autoLimits {
	return autoLimits{AtOnce: 1, PerDay: 20, GapSec: 10, Books: []string{}, PerBook: map[string]autoBookLimits{}}
}

// autoSettings is bot_settings.auto_json: the mode, the preset copy (preset + filter), the auto bankroll and limits.
type autoSettings struct {
	Mode      string            `json:"mode"`
	Preset    *autoPreset       `json:"preset"`
	Filter    *arbfilter.Filter `json:"filter"`
	Bankroll  *botBankroll      `json:"bankroll"`
	Limits    autoLimits        `json:"limits"`
	UpdatedAt int64             `json:"updatedAt"`
}

func defaultAutoSettings() autoSettings {
	return autoSettings{Mode: autoOff, Limits: defaultAutoLimits()}
}

// autoSettingsOf reads a stored auto_json: "" or unreadable = the defaults (mode off).
func autoSettingsOf(js string) autoSettings {
	if js == "" {
		return defaultAutoSettings()
	}
	cfg := defaultAutoSettings()
	if json.Unmarshal([]byte(js), &cfg) != nil || !slices.Contains(autoModesOf("C"), cfg.Mode) {
		return defaultAutoSettings()
	}
	cfg.Limits = cfg.Limits.normalized()
	return cfg
}

// normalized is l with a nil books list and per-book map made empty (the API never shows null).
func (l autoLimits) normalized() autoLimits {
	if l.Books == nil {
		l.Books = []string{}
	}
	if l.PerBook == nil {
		l.PerBook = map[string]autoBookLimits{}
	}
	return l
}

// autoCfgCache is a user's parsed auto settings, kept while auto_json is unchanged.
type autoCfgCache struct {
	raw string
	cfg autoSettings
}

// autoSettingsLocked is userID's stored auto settings (cached by auto_json). b.mu must be held.
func (b *botHub) autoSettingsLocked(userID int64) autoSettings {
	raw := b.settingsLocked(userID).AutoJSON
	if c, ok := b.auto.cfgs[userID]; ok && c.raw == raw {
		return c.cfg
	}
	cfg := autoSettingsOf(raw)
	b.auto.cfgs[userID] = autoCfgCache{raw: raw, cfg: cfg}
	return cfg
}

// autoModeLocked is userID's auto settings and the mode that runs: the stored mode when this deployment's phase has
// it, else off. b.mu must be held.
func (b *botHub) autoModeLocked(userID int64) (autoSettings, string) {
	cfg := b.autoSettingsLocked(userID)
	if !slices.Contains(autoModesOf(b.autoPhase), cfg.Mode) {
		return cfg, autoOff
	}
	return cfg, cfg.Mode
}

// autoDryOf is effectiveDryRun: the controller's dry run, or mode Dry run. It is also the class of the auto orders
// (contract §1): the class the checks that count by class count.
func autoDryOf(mode string, st accounts.BotSettings) bool { return st.DryRun || mode == autoDry }

// pausedOrdersOf reads the stored ids of the orders that paused auto (never nil).
func pausedOrdersOf(js string) []string {
	var ids []string
	if json.Unmarshal([]byte(js), &ids) != nil || ids == nil {
		return []string{}
	}
	return ids
}

// maxPausedOrders is the most order ids an auto pause keeps (the oldest is dropped).
const maxPausedOrders = 20

// ── views ─────────────────────────────────────────────────────────────────────

// autoToday counts the auto orders of the auto mode's class created this Prague day.
type autoToday struct {
	Orders int   `json:"orders"`
	Placed int   `json:"placed"`
	Stake  int64 `json:"stake"` // Kč
}

// autoView is the GET /api/me/bot/auto body.
type autoView struct {
	Settings        autoSettings   `json:"settings"`
	EffectiveDryRun bool           `json:"effectiveDryRun"`
	Paused          bool           `json:"paused"`
	PausedReason    string         `json:"pausedReason"`
	PausedOrders    []string       `json:"pausedOrders"`
	Today           autoToday      `json:"today"`
	Counts          map[string]int `json:"counts"`
	BotBooks        []string       `json:"botBooks"`
}

// autoSummary is the auto part of GET /api/me/bot.
type autoSummary struct {
	Mode            string   `json:"mode"`
	EffectiveDryRun bool     `json:"effectiveDryRun"`
	Paused          bool     `json:"paused"`
	PausedReason    string   `json:"pausedReason"`
	Today           botToday `json:"today"`
}

// autoTodayLocked counts userID's auto orders of class dry created this Prague day (zero when the day cannot be
// read). b.mu must be held.
func (b *botHub) autoTodayLocked(userID int64, dry bool, fx *effects) autoToday {
	var t autoToday
	day, err := b.autoDayLocked(userID, fx)
	if err != nil {
		return t
	}
	for _, r := range day.recs {
		if r.source != sourceAuto || r.dry != dry {
			continue
		}
		t.Orders++
		t.Stake += r.stake
		if r.state == "placed" {
			t.Placed++
		}
	}
	return t
}

// autoViewLocked is userID's GET /api/me/bot/auto body without the counts (autoCounts, under its own lock). The
// settings show the mode that runs (autoModeLocked). b.mu must be held.
func (b *botHub) autoViewLocked(userID int64, fx *effects) autoView {
	st := b.settingsLocked(userID)
	cfg, mode := b.autoModeLocked(userID)
	cfg.Mode = mode
	dry := autoDryOf(mode, st)
	return autoView{Settings: cfg, EffectiveDryRun: dry, Paused: st.AutoPausedReason != "", PausedReason: st.AutoPausedReason,
		PausedOrders: pausedOrdersOf(st.AutoPausedOrders), Today: b.autoTodayLocked(userID, dry, fx), BotBooks: b.botBooksLocked(userID)}
}

// autoSummaryLocked is the auto part of userID's GET /api/me/bot. b.mu must be held.
func (b *botHub) autoSummaryLocked(userID int64, fx *effects) autoSummary {
	st := b.settingsLocked(userID)
	_, mode := b.autoModeLocked(userID)
	dry := autoDryOf(mode, st)
	t := b.autoTodayLocked(userID, dry, fx)
	return autoSummary{Mode: mode, EffectiveDryRun: dry, Paused: st.AutoPausedReason != "", PausedReason: st.AutoPausedReason,
		Today: botToday{Orders: t.Orders, Placed: t.Placed}}
}

// botBooksLocked is the bookmakers some online protocol-2 agent of userID has a window of (any state), sorted.
// b.mu must be held.
func (b *botHub) botBooksLocked(userID int64) []string {
	out := []string{}
	for _, p := range b.agents[userID] {
		if p.proto < botProto {
			continue
		}
		for _, w := range p.windows {
			if !slices.Contains(out, w.Bookmaker) {
				out = append(out, w.Bookmaker)
			}
		}
	}
	slices.Sort(out)
	return out
}

func (b *botHub) handleGetAuto(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	if err := b.loadSettings(userID); err != nil {
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	b.mu.Lock()
	fx := &effects{}
	v := b.autoViewLocked(userID, fx)
	b.release(fx)
	v.Counts = b.auto.countsOf(userID, b.autoDayFrom())
	accounts.WriteJSON(w, http.StatusOK, v)
}

// ── PUT /api/me/bot/auto ──────────────────────────────────────────────────────

// autoPutIn is a PUT body: any subset of the fields (absent or null = unchanged).
type autoPutIn struct {
	Mode     *string         `json:"mode"`
	Preset   json.RawMessage `json:"preset"`
	Filter   json.RawMessage `json:"filter"`
	Bankroll json.RawMessage `json:"bankroll"`
	Limits   json.RawMessage `json:"limits"`
}

var errBadAuto = errors.New("bad_auto")

// present: a field the body sets (not absent, not null).
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// apply returns cfg with in's fields after the shape and range checks (contract §4.1 1); errBadAuto otherwise.
func (in autoPutIn) apply(cfg autoSettings) (autoSettings, error) {
	if in.Mode != nil {
		if !slices.Contains(autoModesOf("C"), *in.Mode) {
			return cfg, errBadAuto
		}
		cfg.Mode = *in.Mode
	}
	if present(in.Preset) != present(in.Filter) {
		return cfg, errBadAuto // preset and filter only together
	}
	if present(in.Preset) {
		var p autoPreset
		var f arbfilter.Filter
		if json.Unmarshal(in.Preset, &p) != nil || json.Unmarshal(in.Filter, &f) != nil || !validAutoPreset(p) || !validAutoFilter(f) {
			return cfg, errBadAuto
		}
		cfg.Preset, cfg.Filter = &p, &f
	}
	if present(in.Bankroll) {
		var br botBankroll
		if json.Unmarshal(in.Bankroll, &br) != nil || !validAutoBankroll(br) {
			return cfg, errBadAuto
		}
		cfg.Bankroll = &br
	}
	if present(in.Limits) {
		l := defaultAutoLimits()
		l.Books, l.PerBook = nil, nil
		if json.Unmarshal(in.Limits, &l) != nil {
			return cfg, errBadAuto
		}
		l = l.normalized()
		if !validAutoLimits(l) {
			return cfg, errBadAuto
		}
		cfg.Limits = l
	}
	return cfg, nil
}

func validAutoPreset(p autoPreset) bool {
	return runeLen(p.ID) >= 1 && runeLen(p.ID) <= 64 && runeLen(p.Name) >= 1 && runeLen(p.Name) <= 100
}

// validAutoFilter: bookmakers 1–20 ids, sports null or ≤ 40 of ≤ 40 characters, live_state, profit bounds null or
// 0–100, min_age_sec 0–3600, three_leg, blocked_markets ≤ 40 sports × ≤ 200 types of ≤ 64 characters.
func validAutoFilter(f arbfilter.Filter) bool {
	if len(f.Bookmakers) < 1 || len(f.Bookmakers) > 20 || len(f.Sports) > 40 || len(f.BlockedMarkets) > 40 ||
		f.MinAgeSec < 0 || f.MinAgeSec > 3600 || (f.ThreeLeg != "include" && f.ThreeLeg != "exclude") ||
		(f.LiveState != "all" && f.LiveState != "live" && f.LiveState != "prematch") {
		return false
	}
	for _, id := range f.Bookmakers {
		if !bookmakerPattern.MatchString(id) {
			return false
		}
	}
	for _, s := range f.Sports {
		if runeLen(s) > 40 {
			return false
		}
	}
	for _, p := range []*float64{f.ProfitMin, f.ProfitMax} {
		if p != nil && !(*p >= 0 && *p <= 100) {
			return false
		}
	}
	for sport, types := range f.BlockedMarkets {
		if runeLen(sport) > 40 || len(types) > 200 {
			return false
		}
		for _, t := range types {
			if runeLen(t) > 64 {
				return false
			}
		}
	}
	return true
}

// validAutoBankroll: an order's bankroll (validOddsDropFields) in whole crowns; split needs total > 0, maxleg max_leg > 0.
func validAutoBankroll(br botBankroll) bool {
	whole := func(v float64) bool { return v == math.Trunc(v) }
	return validOddsDropFields(botOrderIn{Bankroll: &br}) && whole(br.Total) && whole(br.MaxLeg) &&
		((br.Mode == "split" && br.Total > 0) || (br.Mode == "maxleg" && br.MaxLeg > 0))
}

// validAutoLimits: atOnce 1–20, perDay 0–1000, placedPerDay 0–1000, stakePerDay 0–maxKc, gapSec 0–3600, books ≤ 20 ids, perBook ≤ 20
// entries keyed by id with stakes 0–maxKc and maxStake 0 or ≥ minStake.
func validAutoLimits(l autoLimits) bool {
	if l.AtOnce < 1 || l.AtOnce > 20 || l.PerDay < 0 || l.PerDay > 1000 || l.PlacedPerDay < 0 || l.PlacedPerDay > 1000 || l.StakePerDay < 0 || l.StakePerDay > maxKc ||
		l.GapSec < 0 || l.GapSec > 3600 || len(l.Books) > 20 || len(l.PerBook) > 20 {
		return false
	}
	for _, id := range l.Books {
		if !bookmakerPattern.MatchString(id) {
			return false
		}
	}
	for id, pb := range l.PerBook {
		if !bookmakerPattern.MatchString(id) {
			return false
		}
		for _, v := range []int64{pb.MinStake, pb.MaxStake, pb.StakePerDay} {
			if v < 0 || v > maxKc {
				return false
			}
		}
		if pb.MaxStake != 0 && pb.MaxStake < pb.MinStake {
			return false
		}
	}
	return true
}

// handlePutAuto changes the user's auto settings (contract §4.1): the checks in order (400 bad_auto, 409
// mode_unavailable, auto_incomplete, controller_dry_run), then stored with updatedAt, the picker's copy replaced at
// once and {"type":"bot"}. A mode change never clears an auto pause.
func (b *botHub) handlePutAuto(w http.ResponseWriter, r *http.Request) {
	var in autoPutIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		accounts.WriteError(w, http.StatusBadRequest, "bad_auto")
		return
	}
	userID := principalOf(r).UserID
	if err := b.loadSettings(userID); err != nil { // never save over a row that could not be read
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	b.mu.Lock()
	st := b.settingsLocked(userID)
	cfg, err := in.apply(b.autoSettingsLocked(userID))
	code, status := "", 0
	switch {
	case err != nil:
		code, status = "bad_auto", http.StatusBadRequest
	case in.Mode != nil && !slices.Contains(autoModesOf(b.autoPhase), *in.Mode):
		code, status = "mode_unavailable", http.StatusConflict
	case cfg.Mode != autoOff && (cfg.Preset == nil || cfg.Filter == nil || cfg.Bankroll == nil):
		code, status = "auto_incomplete", http.StatusConflict
	case in.Mode != nil && *in.Mode == autoLive && st.DryRun:
		code, status = "controller_dry_run", http.StatusConflict
	}
	if code != "" {
		b.mu.Unlock()
		accounts.WriteError(w, status, code)
		return
	}
	cfg.UpdatedAt = b.now().UnixMilli()
	st.AutoJSON = mustJSON(cfg)
	fx := &effects{}
	b.saveSettingsLocked(userID, st, fx)
	b.autoActivateLocked(userID)
	fx.notify(userID)
	v := b.autoViewLocked(userID, fx)
	b.release(fx)
	v.Counts = b.auto.countsOf(userID, b.autoDayFrom())
	log.Info().Int64("user", userID).Str("mode", cfg.Mode).Msg("bot auto settings saved")
	accounts.WriteJSON(w, http.StatusOK, v)
}

// ── Resume ────────────────────────────────────────────────────────────────────

const titleAutoPaused = "Auto paused"

// handleResumeAuto ends the auto pause (contract §4.1): 409 not_paused; 409 needs_check while an order that paused it
// needs a check (checkable and not checked, or a real order still running: it may end needing one). On success the
// reason and ids are cleared, the open "Auto paused" alerts resolved and {"type":"bot"} sent.
func (b *botHub) handleResumeAuto(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	if err := b.loadSettings(userID); err != nil {
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	for attempt := 0; ; attempt++ {
		b.mu.Lock()
		st := b.settingsLocked(userID)
		ids := pausedOrdersOf(st.AutoPausedOrders)
		paused := st.AutoPausedReason != ""
		open := false
		for _, id := range ids {
			if o := b.orders[id]; o != nil && !o.dryRun {
				open = true
			}
		}
		b.mu.Unlock()
		if !paused {
			accounts.WriteError(w, http.StatusConflict, "not_paused")
			return
		}
		if open {
			accounts.WriteError(w, http.StatusConflict, "needs_check")
			return
		}
		// Under dbMu: every change released before has written its rows (an order that just ended reads as ended).
		b.dbMu.Lock()
		code := ""
		for _, id := range ids {
			row, found, err := b.acc.BotOrderByID(userID, id)
			if err != nil {
				log.Error().Err(err).Msg("bot auto resume: order read failed")
				code = "internal"
				break
			}
			if found && ((checkableOf(row.State, row.DryRun, row.FlowJSON) && row.CheckedAt == 0) || (!row.DryRun && !botEvents[row.State])) {
				code = "needs_check" // unchecked, or a real order not over yet
				break
			}
		}
		b.dbMu.Unlock()
		if code == "internal" {
			accounts.WriteError(w, http.StatusInternalServerError, code)
			return
		}
		if code != "" {
			accounts.WriteError(w, http.StatusConflict, code)
			return
		}
		b.mu.Lock()
		st = b.settingsLocked(userID)
		if st.AutoPausedReason == "" || !slices.Equal(pausedOrdersOf(st.AutoPausedOrders), ids) {
			b.mu.Unlock() // changed while the orders were read: look again
			if attempt < 3 {
				continue
			}
			accounts.WriteError(w, http.StatusConflict, "needs_check")
			return
		}
		st.AutoPausedReason, st.AutoPausedOrders = "", "[]"
		fx := &effects{}
		b.saveSettingsLocked(userID, st, fx)
		// Resolved in the same ordered writes: an alert of a pause raised after this Resume stays open.
		var rows []accounts.BotAlertRow
		at := b.now().UnixMilli()
		fx.write(func() error {
			var err error
			rows, err = b.acc.ResolveBotAlertsTitled(userID, titleAutoPaused, at)
			return err
		})
		fx.notify(userID)
		b.release(fx)
		for _, row := range rows {
			b.reg.sendUser(userID, alertMessage(alertOfRow(row)))
		}
		break
	}
	log.Info().Int64("user", userID).Msg("bot auto resumed")
	w.WriteHeader(http.StatusNoContent)
}

// ── GET /api/me/bot/auto/log ──────────────────────────────────────────────────

// autoLogKinds are the kinds of skip-log line.
var autoLogKinds = []string{"skip", "would_send", "sent"}

func (b *botHub) handleAutoLog(w http.ResponseWriter, r *http.Request) {
	userID := principalOf(r).UserID
	q := r.URL.Query()
	limit := 200
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxAutoLog {
			accounts.WriteError(w, http.StatusBadRequest, "bad_query")
			return
		}
		limit = n
	}
	reason, kind := q.Get("reason"), q.Get("kind")
	if (reason != "" && !slices.Contains(autoReasonCodes, reason)) || (kind != "" && !slices.Contains(autoLogKinds, kind)) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_query")
		return
	}
	lines, counts := b.auto.read(userID, b.autoDayFrom(), reason, kind, limit)
	accounts.WriteJSON(w, http.StatusOK, struct {
		Lines  []autoLogLine  `json:"lines"`
		Counts map[string]int `json:"counts"`
	}{lines, counts})
}
