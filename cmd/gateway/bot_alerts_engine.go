package main

import (
	"slices"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

const (
	windowAlertGrace   = 60 * time.Second // a window problem alerts once it lasts this long
	agentAlertGrace    = 2 * time.Minute  // an agent offline alerts once it lasts this long
	alertStartQuiet    = 3 * time.Minute  // no agent offline alert this soon after the gateway started
	windowRealertPause = 10 * time.Minute // a window does not alert again this soon after its alert resolved
)

// alertScope names what a window or agent alert is about: at most one is open per scope.
type alertScope struct {
	kind     string // alertWindow or alertAgent
	userID   int64
	codeID   int64
	windowID string // window alerts only
}

// pendingAlert is a grace timer: the alert fires at due unless the state changes back first.
type pendingAlert struct {
	due    time.Time
	state  string // window: the problem state (logged_out, blocked, down)
	reason string // window: the agent's reason, if any
	label  string // window: "Tipsport 1"
}

// openAlert is a window or agent alert that fired and is not resolved.
type openAlert struct {
	alert  botAlert
	stored bool // it is in the app (row and messages): its resolution is too
}

// alertEngine is the alerts' state, in memory, guarded by botHub.mu.
type alertEngine struct {
	started  time.Time                    // the gateway start (the agent quiet period)
	settings map[int64]alertSettings      // user id → switches (loadAlertSettings)
	pending  map[alertScope]*pendingAlert // grace timers, checked by tick
	open     map[alertScope]*openAlert
	pauseTil map[alertScope]time.Time // window scope → no new alert before then
	names    map[int64]string         // code id → agent name (from its bot socket)
	revoked  map[int64]bool           // code ids revoked since the start: never alerted about again
	// settingsErr: the user's switches could not be read (and are not cached): alerts go to the app only.
	settingsErr map[int64]bool
	// startChecked: the start check (agents seen in the last 24 h and not back) ran (checkAgentsAfterStart).
	startChecked bool
}

func newAlertEngine(started time.Time) *alertEngine {
	return &alertEngine{started: started, settings: map[int64]alertSettings{}, pending: map[alertScope]*pendingAlert{},
		open: map[alertScope]*openAlert{}, pauseTil: map[alertScope]time.Time{}, names: map[int64]string{},
		revoked: map[int64]bool{}, settingsErr: map[int64]bool{}}
}

// loadAlertSettings caches userID's alert switches (call without b.mu; loadSettings calls it). A read error leaves
// them uncached: until a later read works, alerts go to the app only (alertSettingsLocked).
func (b *botHub) loadAlertSettings(userID int64) {
	b.mu.Lock()
	if b.alerts == nil {
		b.mu.Unlock()
		return
	}
	_, ok := b.alerts.settings[userID]
	b.mu.Unlock()
	if ok {
		return
	}
	st := defaultAlertSettings()
	if b.acc != nil {
		js, found, err := b.acc.BotAlertSettings(userID)
		if err != nil {
			log.Error().Err(err).Int64("user", userID).Msg("bot alert settings read failed; alerts go to the app only")
			b.mu.Lock()
			b.alerts.settingsErr[userID] = true
			b.mu.Unlock()
			return
		}
		if found {
			st = alertSettingsOf(js)
		}
	}
	b.mu.Lock()
	if _, ok := b.alerts.settings[userID]; !ok {
		b.alerts.settings[userID] = st
	}
	delete(b.alerts.settingsErr, userID)
	b.mu.Unlock()
}

// alertSettingsLocked is userID's switches: the defaults when not loaded, every kind to the app only (never Telegram
// on a guess) when the last read failed. b.mu must be held; b.alerts not nil.
func (b *botHub) alertSettingsLocked(userID int64) alertSettings {
	if st, ok := b.alerts.settings[userID]; ok {
		return st
	}
	if b.alerts.settingsErr[userID] {
		app := alertChannels{App: true}
		return alertSettings{Attention: app, Window: app, Agent: app, Order: app}
	}
	return defaultAlertSettings()
}

// alertsCodeRevoked forgets a revoked agent (connRegistry.CloseCode, before its socket closes): its graces end, its
// open window and agent alerts resolve, its pauses and name go, and it never alerts again. Call it without b.mu.
func (b *botHub) alertsCodeRevoked(codeID int64) {
	b.mu.Lock()
	if b.alerts == nil {
		b.mu.Unlock()
		return
	}
	fx := &effects{}
	b.alerts.revoked[codeID] = true
	for sc := range b.alerts.pending {
		if sc.codeID == codeID {
			delete(b.alerts.pending, sc)
		}
	}
	var open []alertScope
	for sc := range b.alerts.open {
		if sc.codeID == codeID {
			open = append(open, sc)
		}
	}
	slices.SortFunc(open, compareScopes)
	for _, sc := range open {
		b.resolveLocked(sc, fx)
	}
	for sc := range b.alerts.pauseTil {
		if sc.codeID == codeID {
			delete(b.alerts.pauseTil, sc)
		}
	}
	delete(b.alerts.names, codeID)
	b.release(fx)
}

// agentNameLocked is the agent's name as its bot socket gave it, else "Agent <code id>". b.mu must be held.
func (b *botHub) agentNameLocked(codeID int64) string {
	if b.alerts != nil {
		if n := b.alerts.names[codeID]; n != "" {
			return n
		}
	}
	return "Agent " + strconv.FormatInt(codeID, 10)
}

// raiseLocked fires a new alert for userID (its ID and time are set here): with its kind's app switch on it is stored
// and sent to the app ({"type":"alert"}), with the Telegram switch on and Telegram configured it goes to the user's
// linked chat after the locks. scope (window and agent alerts) records it as that scope's open alert. b.mu must be held.
func (b *botHub) raiseLocked(userID int64, a botAlert, scope *alertScope, fx *effects) {
	ch := b.alertSettingsLocked(userID).of(a.Kind)
	a.ID, a.CreatedAt = newOrderID(), b.now().UnixMilli()
	if scope != nil {
		b.alerts.open[*scope] = &openAlert{alert: a, stored: ch.App}
	}
	if ch.App {
		if b.acc != nil {
			row := a.row(userID)
			fx.write(func() error { return b.acc.InsertBotAlert(row) })
		}
		fx.events = append(fx.events, userMsg{userID: userID, payload: alertMessage(a)})
	}
	if ch.Telegram && b.tg != nil {
		tg := b.tg
		fx.later = append(fx.later, func() { tg.send(userID, a) })
	}
	log.Info().Int64("user", userID).Str("kind", a.Kind).Str("title", a.Title).Str("detail", a.Detail).Bool("app", ch.App).
		Bool("telegram", ch.Telegram).Msg("bot alert")
}

// resolveLocked resolves scope's open alert, if any: its row and the app get resolvedAt (an alert that was never in
// the app, or that the user dismissed: the row only). No Telegram message. b.mu must be held; b.alerts not nil.
func (b *botHub) resolveLocked(scope alertScope, fx *effects) {
	oa := b.alerts.open[scope]
	if oa == nil {
		return
	}
	delete(b.alerts.open, scope)
	if !oa.stored {
		return
	}
	a := oa.alert
	a.ResolvedAt = b.now().UnixMilli()
	if b.acc != nil {
		id, at := a.ID, a.ResolvedAt
		fx.write(func() error { return b.acc.ResolveBotAlert(id, at) })
	}
	if a.DismissedAt != 0 {
		return // dismissed in the app: it must not come back there
	}
	fx.events = append(fx.events, userMsg{userID: scope.userID, payload: alertMessage(a)})
}

// alertsDismissed copies the dismissal of rows (just dismissed in the database) onto the engine's open window and
// agent alerts, so their later resolution does not bring them back to the app. They stay open in the engine (one per
// scope, and the re-alert pause after it resolves). Call it without b.mu, before telling the app.
func (b *botHub) alertsDismissed(userID int64, rows []accounts.BotAlertRow) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.alerts == nil {
		return
	}
	at := map[string]int64{}
	for _, r := range rows {
		at[r.ID] = r.DismissedAt
	}
	for sc, oa := range b.alerts.open {
		if t, ok := at[oa.alert.ID]; ok && sc.userID == userID && oa.alert.DismissedAt == 0 {
			oa.alert.DismissedAt = t
		}
	}
}
