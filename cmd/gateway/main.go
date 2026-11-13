package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/surebetter/api-gateway/internal/accounts"
	"github.com/surebetter/api-gateway/internal/telegram"
)

var version = "dev"

// ── Config ────────────────────────────────────────────────────────────────────
type Config struct {
	NatsURL           string
	RedisURL          string
	Port              string
	MaxClients        int
	SnapshotCacheMs   int64
	BroadcastBatchMs  int64
	JWTSecret         string
	AccountsDB        string // ACCOUNTS_DB: SQLite file for users, sessions, codes and per-user state
	AuthRequired      bool   // AUTH_REQUIRED: feed, snapshots and Telegram need a login (default true)
	EvictionTTLS      int    // EVICTION_TTL_S: seconds until an unseen PREMATCH entry is evicted
	EvictionLiveTTLS  int    // EVICTION_LIVE_TTL_S: shorter TTL for "-live" arbs (fast removal)
	EvictionIntervalS int    // EVICTION_INTERVAL_S: sweep cadence in seconds
	ValueEvictionTTLS int    // VALUE_EVICTION_TTL_S: seconds until an unseen PREMATCH value bet is evicted
	// VALUE_LIVE_EVICTION_TTL_S: shorter TTL for live value bets (engine heartbeats them every ~3s)
	ValueLiveEvictionTTLS int

	// Telegram bot (internal/telegram). Empty token or chat id → the feature is off.
	TelegramToken         string
	TelegramChatID        string
	TelegramEditIntervalS int
	TelegramGoneDeleteS   int
	TelegramRatePerMin    int
	TelegramTZ            string
}

// authRequired fails closed: only the exact value "false" disables authentication.
func authRequired(v string) bool {
	return strings.ToLower(strings.TrimSpace(v)) != "false"
}

func configFromEnv() Config {
	return Config{
		NatsURL:           getenv("NATS_URL", "nats://localhost:4222"),
		RedisURL:          getenv("REDIS_URL", "redis://localhost:6379"),
		Port:              getenv("PORT", "8080"),
		MaxClients:        getenvInt("MAX_CLIENTS", 100),
		SnapshotCacheMs:   int64(getenvInt("SNAPSHOT_CACHE_MS", 500)),
		BroadcastBatchMs:  int64(getenvInt("BROADCAST_BATCH_MS", 50)),
		JWTSecret:         getenv("JWT_SECRET", ""),
		AccountsDB:        getenv("ACCOUNTS_DB", "/data/accounts.db"),
		AuthRequired:      authRequired(getenv("AUTH_REQUIRED", "true")),
		EvictionTTLS:      getenvInt("EVICTION_TTL_S", 120),
		EvictionLiveTTLS:  getenvInt("EVICTION_LIVE_TTL_S", 8),
		EvictionIntervalS: getenvInt("EVICTION_INTERVAL_S", 3),
		ValueEvictionTTLS: getenvInt("VALUE_EVICTION_TTL_S", 120),

		ValueLiveEvictionTTLS: getenvInt("VALUE_LIVE_EVICTION_TTL_S", 15),

		TelegramToken:         getenv("TELEGRAM_TOKEN", ""),
		TelegramChatID:        getenv("TELEGRAM_CHAT_ID", ""),
		TelegramEditIntervalS: getenvInt("TELEGRAM_EDIT_INTERVAL_S", 10),
		TelegramGoneDeleteS:   getenvInt("TELEGRAM_GONE_DELETE_AFTER_S", 0),
		TelegramRatePerMin:    getenvInt("TELEGRAM_RATE_PER_MIN", 18),
		TelegramTZ:            getenv("TELEGRAM_TZ", "Europe/Prague"),
	}
}

// ── Message types sent to Angular clients ────────────────────────────────────
type WSMessage struct {
	Type string          `json:"type"` // "snapshot" | "patch" | "ping" | "state"
	Data json.RawMessage `json:"data,omitempty"`
	Ops  []PatchOp       `json:"ops,omitempty"` // RFC 6902 JSON Patch
	Key  string          `json:"key,omitempty"` // "state" messages: the per-user state key that changed
}

type PatchOp struct {
	Op    string      `json:"op"` // "replace" | "add" | "remove"
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// ── ArbOpportunity (mirrors Rust struct from arb-engine) ─────────────────────
type ArbOpportunity struct {
	EventID    string            `json:"event_id"`
	Sport      string            `json:"sport"`
	MarketType string            `json:"market_type"`
	Line       float64           `json:"line"`
	ProfitPct  float64           `json:"profit_pct"`
	Stakes     []StakeAllocation `json:"stakes"`
	DetectedAt int64             `json:"detected_at"`
	// FirstDetectedAt is the ms epoch of this arb's FIRST detection, preserved across
	// re-emits (the frontend's live "age"). LastChangedAt is the last time its profit%/
	// odds changed while it stayed an arb (== FirstDetectedAt until the first change).
	// Both are engine-owned; the gateway relays them verbatim.
	FirstDetectedAt int64  `json:"first_detected_at"`
	LastChangedAt   int64  `json:"last_changed_at"`
	NameHome        string `json:"name_home"`
	NameAway        string `json:"name_away"`
	League          string `json:"league"`
	MarketName      string `json:"market_name"`
	StartTime       int64  `json:"start_time"`
	// PlayerName is the normalized player surname for player-scoped markets
	// (GAME_HANDICAP, PLAYER_*); empty for all others. It is part of the arb's
	// identity — the arb-engine keys its composite storage on it — so it MUST be in
	// oppStateKey too, or two distinct player arbs on the same event/market/line
	// collide in client state and only one is ever shown.
	PlayerName string `json:"player_name"`
	// Removed=true is an explicit removal signal from the arb-engine (the arb no
	// longer holds). The gateway drops it from client state immediately instead of
	// waiting out the eviction TTL. Omitted (false) for normal opportunities.
	Removed bool `json:"removed"`
}

// oppStateKey returns the composite state key, mirroring the arb-engine's storage
// key exactly: {event_id}:{market_type}:{player_name}:{line:.2f} for player-scoped
// markets, {event_id}:{market_type}:{line:.2f} otherwise. Including player_name is
// required so two distinct player arbs on the same event/market/line don't collide.
func oppStateKey(opp ArbOpportunity) string {
	if opp.PlayerName != "" {
		return fmt.Sprintf("%s:%s:%s:%.2f", opp.EventID, opp.MarketType, opp.PlayerName, opp.Line)
	}
	return fmt.Sprintf("%s:%s:%.2f", opp.EventID, opp.MarketType, opp.Line)
}

type StakeAllocation struct {
	Bookmaker   string  `json:"bookmaker"`
	OutcomeID   string  `json:"outcome_id"`
	Label       string  `json:"label"`
	DecimalOdds float64 `json:"decimal_odds"`
	StakePct    float64 `json:"stake_pct"`
	MatchURL    string  `json:"match_url"`
	// MarketName is the winning bookmaker's own raw market label for this leg. Relayed
	// verbatim; without it the field would be dropped on unmarshal→re-marshal to clients.
	MarketName string `json:"market_name"`
	// OddsUpdatedAt is the ms epoch when this leg's odds were last refreshed (advances on
	// every update, incl. the ~3s heartbeat, even when the value is unchanged). Relayed
	// verbatim so the frontend can show a per-odd "last updated" stamp.
	OddsUpdatedAt int64 `json:"odds_updated_at"`
	// SourceID is the bookmaker's own id for this selection (Betano: the odds button's
	// data-selnid). Relayed verbatim for the browser extension; omitted when unavailable.
	SourceID string `json:"source_id,omitempty"`
	// SourceMarketRef is the bookmaker's own id for this leg's market container on its
	// page (Fortuna: the card's market-type-id). Relayed verbatim for the browser extension.
	SourceMarketRef string `json:"source_market_ref,omitempty"`
	// FairOdds is the consensus fair odds for this leg's outcome at this leg's bookmaker
	// (the value-bet consensus, leave-one-out); nil = no consensus. Relayed verbatim —
	// always present, null when unknown — for bots rule 2 ("outlier first").
	FairOdds *float64 `json:"fair_odds"`
}

// ── ValueBet (mirrors the arb-engine's value.rs ValueBet) ──────────────────
// The gateway only needs the routing fields of a value bet (id = state key,
// is_live / event_id = eviction class, removed = explicit removal), so it decodes
// just those and relays the engine's original JSON verbatim. Unlike ArbOpportunity there is
// no struct mirror to keep in sync: a field the engine adds reaches the frontend
// without a gateway change.
type ValueBet struct {
	ID      string `json:"id"`
	EventID string `json:"event_id"`
	Sport   string `json:"sport"` // bare sport — never carries "-live" (use isLive)
	IsLive  bool   `json:"is_live"`
	Removed bool   `json:"removed"`
	raw     json.RawMessage
}

// isLive reports whether v is a live value bet: the engine's is_live flag, or —
// for a payload without it — an event_id whose sport segment ends in "-live"
// (`football-live:abc123`). `sport` itself is bare for value bets, so it cannot
// carry the live class the way ArbOpportunity.Sport does.
func (v ValueBet) isLive() bool {
	if v.IsLive {
		return true
	}
	sport, _, _ := strings.Cut(v.EventID, ":")
	return strings.HasSuffix(sport, "-live")
}

// UnmarshalJSON decodes the routing fields and keeps the full original payload.
func (v *ValueBet) UnmarshalJSON(b []byte) error {
	type routing ValueBet // no methods → no recursion
	var r routing
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*v = ValueBet(r)
	v.raw = append(json.RawMessage(nil), b...)
	return nil
}

// MarshalJSON relays the engine's payload verbatim (falls back to the routing
// fields for a ValueBet built in code rather than decoded).
func (v ValueBet) MarshalJSON() ([]byte, error) {
	if len(v.raw) > 0 {
		return v.raw, nil
	}
	type routing ValueBet
	return json.Marshal(routing(v))
}

// ── Hub — manages all WebSocket client connections ───────────────────────────

// hubSpec describes one independent channel served by a StateHub: how an item is
// keyed, whether it is live (eviction TTL class), whether it is an explicit
// removal, and the JSON Patch path prefix its ops use. The arb channel (/ws) and
// the value-bet channel (/ws/value) are two StateHubs with different specs.
type hubSpec[T any] struct {
	name    string // "arb" | "value" — used in log lines
	prefix  string // patch path prefix incl. slashes, e.g. "/arb/"
	key     func(T) string
	isLive  func(T) bool
	removed func(T) bool
}

var arbSpec = hubSpec[ArbOpportunity]{
	name:    "arb",
	prefix:  "/arb/",
	key:     oppStateKey,
	isLive:  func(o ArbOpportunity) bool { return strings.HasSuffix(o.Sport, "-live") },
	removed: func(o ArbOpportunity) bool { return o.Removed },
}

// valueSpec keys value bets on the engine-assigned id verbatim. The id may
// contain ':' '|' '.' — none need JSON-Pointer escaping, and the engine never
// emits '/' (contract), so the id is used as-is like the arb composite key.
var valueSpec = hubSpec[ValueBet]{
	name:    "value",
	prefix:  "/value/",
	key:     func(v ValueBet) string { return v.ID },
	isLive:  ValueBet.isLive,
	removed: func(v ValueBet) bool { return v.Removed },
}

// hubEntry wraps an item with a lastSeen timestamp for eviction tracking.
type hubEntry[T any] struct {
	opp      T
	lastSeen time.Time
}

// arbEntry wraps ArbOpportunity with a lastSeen timestamp for eviction tracking.
type arbEntry = hubEntry[ArbOpportunity]

// StateHub holds the client set, item state and snapshot cache of one channel.
type StateHub[T any] struct {
	spec hubSpec[T]

	mu      sync.RWMutex
	clients map[*Client]struct{}
	state   map[string]hubEntry[T] // current state
	stateMu sync.RWMutex

	snapshotCache    []byte
	snapshotCachedAt int64

	cfg       Config
	connCount atomic.Int64
	msgSent   atomic.Int64

	// onChange, when set (arb hub with Telegram on), observes every state transition: an apply
	// (removed=false), an explicit removal (removed=true) or an eviction (removed and evicted).
	// Called outside stateMu; it must not block (the Telegram notifier's Observe is a non-blocking
	// channel send).
	onChange func(key string, item T, removed, evicted bool)
}

// Hub is the arbitrage channel (/ws, arb.opportunities).
type Hub = StateHub[ArbOpportunity]

// ValueHub is the value-bet channel (/ws/value, value.opportunities).
type ValueHub = StateHub[ValueBet]

func newStateHub[T any](cfg Config, spec hubSpec[T]) *StateHub[T] {
	return &StateHub[T]{
		spec:    spec,
		clients: make(map[*Client]struct{}),
		state:   make(map[string]hubEntry[T]),
		cfg:     cfg,
	}
}

func NewHub(cfg Config) *Hub { return newStateHub(cfg, arbSpec) }

func NewValueHub(cfg Config) *ValueHub { return newStateHub(cfg, valueSpec) }

func (h *StateHub[T]) Register(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.connCount.Add(1)
}

func (h *StateHub[T]) Unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	h.connCount.Add(-1)
}

func (h *StateHub[T]) ClientCount() int64 { return h.connCount.Load() }

// Has reports whether key is in the hub's current state (the Telegram reconcile sweep uses it).
func (h *StateHub[T]) Has(key string) bool {
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	_, ok := h.state[key]
	return ok
}

// ApplyOpportunity stores opp under its composite key and returns the patch op to broadcast.
func (h *StateHub[T]) ApplyOpportunity(opp T) PatchOp {
	key := h.spec.key(opp)
	h.stateMu.Lock()
	h.state[key] = hubEntry[T]{opp: opp, lastSeen: time.Now()}
	h.snapshotCache = nil
	h.stateMu.Unlock()
	if h.onChange != nil {
		h.onChange(key, opp, false, false)
	}
	return PatchOp{
		Op:    "replace",
		Path:  h.spec.prefix + key,
		Value: opp,
	}
}

// RemoveOpportunity deletes opp's key from state (if present) and returns the
// "remove" PatchOp to broadcast. Driven by an explicit removal from the arb-engine
// (opp.Removed=true) so a vanished/no-longer-valid arb disappears immediately rather
// than after the eviction TTL. Returns ok=false (and a zero PatchOp) if the key was
// not in state, so the bridge can skip broadcasting a no-op remove.
func (h *StateHub[T]) RemoveOpportunity(opp T) (PatchOp, bool) {
	key := h.spec.key(opp)
	h.stateMu.Lock()
	_, present := h.state[key]
	if present {
		delete(h.state, key)
		h.snapshotCache = nil
	}
	h.stateMu.Unlock()
	if !present {
		return PatchOp{}, false
	}
	if h.onChange != nil {
		h.onChange(key, opp, true, false)
	}
	return PatchOp{Op: "remove", Path: h.spec.prefix + key}, true
}

// evictStale removes Hub state entries not updated within their TTL and returns
// one "remove" PatchOp per evicted key. Live items (spec.isLive) use the shorter
// liveTTL so a vanished live arb disappears fast; prematch uses prematchTTL. The
// arb-engine re-publishes a still-valid live arb on the fetcher heartbeat cadence
// (~3s), so liveTTL only fires once an arb genuinely stops being detected.
// Invalidates snapshotCache if any entries were removed. Safe to call concurrently.
func (h *StateHub[T]) evictStale(prematchTTL, liveTTL time.Duration) []PatchOp {
	var ops []PatchOp
	var evicted []hubEntry[T]
	var evictedKeys []string
	h.stateMu.Lock()
	for key, entry := range h.state {
		ttl := prematchTTL
		if h.spec.isLive(entry.opp) {
			ttl = liveTTL
		}
		if time.Since(entry.lastSeen) > ttl {
			delete(h.state, key)
			ops = append(ops, PatchOp{Op: "remove", Path: h.spec.prefix + key})
			evicted = append(evicted, entry)
			evictedKeys = append(evictedKeys, key)
		}
	}
	if len(ops) > 0 {
		h.snapshotCache = nil
	}
	h.stateMu.Unlock()
	if h.onChange != nil {
		for i, e := range evicted {
			h.onChange(evictedKeys[i], e.opp, true, true)
		}
	}
	return ops
}

// BroadcastPatch sends a JSON Patch delta to all connected clients
func (h *StateHub[T]) BroadcastPatch(ops []PatchOp) {
	if len(ops) == 0 {
		return
	}
	msg := WSMessage{Type: "patch", Ops: ops}
	payload, err := json.Marshal(msg)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal patch")
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for client := range h.clients {
		select {
		case client.send <- payload:
			h.msgSent.Add(1)
		default:
			// Client send buffer full — they'll get the next update
			log.Warn().Str("hub", h.spec.name).Msg("Client send buffer full, skipping delta")
		}
	}
}

// Snapshot returns all current items as a flat {key: item} JSON object (cached for SnapshotCacheMs)
func (h *StateHub[T]) Snapshot() []byte {
	// Write lock required: cache miss path writes snapshotCache and snapshotCachedAt.
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	now := time.Now().UnixMilli()
	if h.snapshotCache != nil && now-h.snapshotCachedAt < h.cfg.SnapshotCacheMs {
		return h.snapshotCache
	}
	plain := make(map[string]T, len(h.state))
	for k, e := range h.state {
		plain[k] = e.opp
	}
	data, _ := json.Marshal(plain)
	h.snapshotCache = data
	h.snapshotCachedAt = now
	return data
}

// ── Client — a single WebSocket connection ───────────────────────────────────
type Client struct {
	conn      *websocket.Conn
	send      chan []byte
	principal accounts.Principal // zero when AUTH_REQUIRED is off and nobody signed in
	cid       string             // client id from ?cid=; a state write skips the socket that made it
	notify    bool               // receives {"type":"state"} messages (the /ws feed only)
	bot       bool               // the user's bot socket (/ws?bot=1): gets orders, never bot/botEvent messages
}

func (c *Client) writePump(ctx context.Context) {
	pingTicker := time.NewTicker(20 * time.Second) // Chrome stops an extension service worker after 30 s without events; a /ws?feed=0 socket's only traffic is these pings, so they must come more often than every 30 s.

	defer pingTicker.Stop()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			if err := c.conn.Write(ctx, websocket.MessageText, msg); err != nil {
				log.Debug().Err(err).Msg("WS write error")
				return
			}
		case <-pingTicker.C:
			ping, _ := json.Marshal(WSMessage{Type: "ping"})
			if err := c.conn.Write(ctx, websocket.MessageText, ping); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// ── NATS → Hub bridge ─────────────────────────────────────────────────────────
// runNATSBridge subscribes the arb hub to arb.opportunities.
func runNATSBridge(ctx context.Context, nc *nats.Conn, hub *Hub) {
	runHubBridge(ctx, nc, hub, "arb.opportunities")
}

// runValueBridge subscribes the value hub to value.opportunities.
func runValueBridge(ctx context.Context, nc *nats.Conn, hub *ValueHub) {
	runHubBridge(ctx, nc, hub, "value.opportunities")
}

func runHubBridge[T any](ctx context.Context, nc *nats.Conn, hub *StateHub[T], subject string) {
	// Batch deltas within BroadcastBatchMs window
	pending := make([]PatchOp, 0, 32)
	ticker := time.NewTicker(time.Duration(hub.cfg.BroadcastBatchMs) * time.Millisecond)
	defer ticker.Stop()

	msgCh := make(chan *nats.Msg, 256)
	sub, _ := nc.ChanSubscribe(subject, msgCh)
	defer sub.Unsubscribe()

	for {
		select {
		case msg := <-msgCh:
			var opp T
			if err := json.Unmarshal(msg.Data, &opp); err != nil {
				log.Error().Err(err).Msgf("Failed to parse %s opportunity", hub.spec.name)
				continue
			}
			if hub.spec.key(opp) == "" {
				// Malformed message (no identity) — a patch path of just the prefix
				// would address the whole collection on the client.
				log.Error().Str("subject", subject).Msg("Dropping message with empty state key")
				continue
			}

			if hub.spec.removed(opp) {
				// Explicit removal from the arb-engine — drop it now (skip the no-op
				// case where we never had this key).
				if op, ok := hub.RemoveOpportunity(opp); ok {
					pending = append(pending, op)
				}
			} else {
				pending = append(pending, hub.ApplyOpportunity(opp))
			}

		case <-ticker.C:
			if len(pending) > 0 {
				hub.BroadcastPatch(pending)
				pending = pending[:0]
			}

		case <-ctx.Done():
			return
		}
	}
}

// ── Stale eviction sweep ──────────────────────────────────────────────────────
func runEvictionSweep(ctx context.Context, hub *Hub) {
	runHubEvictionSweep(ctx, hub,
		time.Duration(hub.cfg.EvictionTTLS)*time.Second,
		time.Duration(hub.cfg.EvictionLiveTTLS)*time.Second)
}

// runValueEvictionSweep evicts prematch value bets unseen for VALUE_EVICTION_TTL_S
// and live ones (is_live) unseen for VALUE_LIVE_EVICTION_TTL_S. The engine
// heartbeats active live value bets every VALUE_LIVE_HEARTBEAT_MS (~3s) and removes
// them explicitly, so the live TTL is only the backstop for a missed removal.
func runValueEvictionSweep(ctx context.Context, hub *ValueHub) {
	ttl := time.Duration(hub.cfg.ValueEvictionTTLS) * time.Second
	if ttl <= 0 {
		ttl = 120 * time.Second
	}
	liveTTL := time.Duration(hub.cfg.ValueLiveEvictionTTLS) * time.Second
	if liveTTL <= 0 {
		liveTTL = 15 * time.Second
	}
	runHubEvictionSweep(ctx, hub, ttl, liveTTL)
}

func runHubEvictionSweep[T any](ctx context.Context, hub *StateHub[T], prematchTTL, liveTTL time.Duration) {
	interval := time.Duration(hub.cfg.EvictionIntervalS) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	if prematchTTL <= 0 {
		prematchTTL = 120 * time.Second
	}
	if liveTTL <= 0 {
		liveTTL = 8 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ops := hub.evictStale(prematchTTL, liveTTL)
			if len(ops) > 0 {
				hub.BroadcastPatch(ops)
				log.Info().Int("evicted", len(ops)).Msgf("Stale %s entries evicted", hub.spec.name)
			}
		case <-ctx.Done():
			return
		}
	}
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────
func wsHandler[T any](hub *StateHub[T], gate *wsGate, notify bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if hub.ClientCount() >= int64(hub.cfg.MaxClients) {
			http.Error(w, "max clients reached", http.StatusServiceUnavailable)
			return
		}
		principal, ok := gate.admit(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		q := r.URL.Query()
		// /ws?bot=1 is the user's betting bot: only a bot code may open it, and it never gets the feed.
		botSocket := notify && q.Get("bot") == "1"
		if botSocket && !gate.botAllowed(principal) {
			http.Error(w, "bot code required", http.StatusForbidden)
			return
		}
		feedOff := notify && (q.Get("feed") == "0" || botSocket)
		if feedOff && principal.UserID == 0 {
			// Feed-off sockets only carry per-user messages; an anonymous one can never receive anything.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// Origin is checked by gate.admit for cookie sessions; tickets come from the
			// extension's own origin.
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Error().Err(err).Msg("WS accept failed")
			return
		}

		defer conn.CloseNow()
		// The gateway never reads client messages; CloseRead handles control frames and cancels
		// ctx as soon as the peer closes or the connection breaks.
		ctx := conn.CloseRead(r.Context())

		client := &Client{conn: conn, send: make(chan []byte, 64), principal: principal, notify: notify,
			cid: accounts.ValidClientID(q.Get("cid")), bot: botSocket}
		if !feedOff {
			hub.Register(client)
			defer hub.Unregister(client)
		}
		gate.track(client)
		defer gate.untrack(client)
		if !gate.stillValid(principal) {
			conn.Close(websocket.StatusPolicyViolation, "signed out")
			return
		}
		if botSocket {
			gate.bots.attach(client)
			defer gate.bots.detach(client)
		}

		if !feedOff {
			// Send initial full snapshot
			snapshot := hub.Snapshot()
			msg := WSMessage{Type: "snapshot", Data: snapshot}
			if err := wsjson.Write(ctx, conn, msg); err != nil {
				log.Error().Err(err).Msg("Failed to send snapshot")
				return
			}
		}

		client.writePump(ctx)
	}
}

func snapshotHandler[T any](hub *StateHub[T]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(hub.Snapshot())
	}
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","service":"api-gateway","version":"%s"}`, version)
}

func metricsHandler(hub *Hub, valueHub *ValueHub, tg *telegram.Notifier) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w,
			"gateway_ws_clients_connected %d\ngateway_messages_sent_total %d\n",
			hub.ClientCount(), hub.msgSent.Load(),
		)
		fmt.Fprintf(w,
			"gateway_value_ws_clients_connected %d\ngateway_value_messages_sent_total %d\n",
			valueHub.ClientCount(), valueHub.msgSent.Load(),
		)
		if tg != nil {
			tg.WriteMetrics(w)
		}
	}
}

// newMux wires the HTTP routes. Patterns without a trailing slash match exactly, so "/ws" does
// not swallow "/ws/value". tg may be nil (Telegram off); gate may be nil (no accounts: all open).
func newMux(hub *Hub, valueHub *ValueHub, tg *telegram.Notifier, gate *wsGate) *http.ServeMux {
	mux := http.NewServeMux()
	if gate != nil && gate.acc != nil && gate.bots == nil {
		gate.bots = newBotHub(gate.reg, gate.acc) // tests build wsGate literals without one
	}
	mux.HandleFunc("/ws", wsHandler(hub, gate, true))
	mux.HandleFunc("/ws/value", wsHandler(valueHub, gate, false))
	mux.Handle("/api/arb/snapshot", gate.protect(snapshotHandler(hub)))
	mux.Handle("/api/value/snapshot", gate.protect(snapshotHandler(valueHub)))
	mux.Handle("/api/telegram/", gate.adminOnly(telegram.Handler(tg, requestUserID)))
	if gate != nil && gate.acc != nil {
		accountsAPI := gate.acc.Handler()
		mux.Handle("/api/auth/", accountsAPI)
		mux.Handle("/api/me/", accountsAPI)
		mux.Handle("/api/admin/", accountsAPI)
		// More specific than /api/me/, so these win over the accounts API.
		botAPI := newBotAPI(gate.acc, gate.bots)
		mux.Handle("/api/me/bot", botAPI)
		mux.Handle("/api/me/bot/", botAPI)
	}
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/metrics", metricsHandler(hub, valueHub, tg))
	return mux
}

// ── main ──────────────────────────────────────────────────────────────────────
func main() {
	healthcheck := flag.Bool("healthcheck", false, "Run health check and exit")
	flag.Parse()

	if *healthcheck {
		resp, err := http.Get("http://localhost:8080/health")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}

	logLevel, _ := zerolog.ParseLevel(getenv("LOG_LEVEL", "info"))
	zerolog.SetGlobalLevel(logLevel)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	log.Info().Str("version", version).Msg("surebetter-api-gateway starting")

	cfg := configFromEnv()

	// Accounts: users, sessions, extension codes and per-user state.
	accountsDB, err := accounts.OpenDB(cfg.AccountsDB)
	if err != nil {
		log.Fatal().Err(err).Str("path", cfg.AccountsDB).Msg("Failed to open accounts database")
	}
	defer accountsDB.Close()
	acc := accounts.NewService(accountsDB, nil)
	reg := newConnRegistry()
	acc.SetConnections(reg)
	if inv, created, err := acc.EnsureBootstrapInvite(); err != nil {
		log.Fatal().Err(err).Msg("Failed to create the bootstrap admin invite")
	} else if created {
		log.Warn().Str("invite_path", inv.Path).
			Msg("No accounts yet: open <your server>" + inv.Path + " to create the admin account (a new link is made on every start until then)")
	}
	if n, err := acc.PurgeExpiredSessions(); err != nil {
		log.Error().Err(err).Msg("Failed to purge expired sessions")
	} else if n > 0 {
		log.Info().Int64("purged", n).Msg("Expired sessions purged")
	}
	if !cfg.AuthRequired {
		log.Warn().Msg("AUTH_REQUIRED=false: the feed, the snapshots and the Telegram settings are open to anyone")
	}
	bots := newBotHub(reg, acc)
	bots.shotsDir = filepath.Join(filepath.Dir(cfg.AccountsDB), "bot-screenshots") // same volume as the DB
	// Orders the previous process left waiting or running end now (it lost them from memory).
	bots.recoverOrders()
	gate := &wsGate{acc: acc, reg: reg, required: cfg.AuthRequired, bots: bots}

	hub := NewHub(cfg)
	valueHub := NewValueHub(cfg)

	// Connect NATS
	nc, err := nats.Connect(cfg.NatsURL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to NATS")
	}
	defer nc.Drain()
	log.Info().Str("url", cfg.NatsURL).Msg("Connected to NATS")

	// Connect Redis (for snapshot)
	redisOpts, _ := redis.ParseURL(cfg.RedisURL)
	rdb := redis.NewClient(redisOpts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to Redis")
	}
	log.Info().Str("url", cfg.RedisURL).Msg("Connected to Redis")

	// Bot log lines and screenshots: 7 days, pruned at start and hourly
	go bots.runPrune(ctx)

	// Telegram bot: hook the arb hub before the bridge starts feeding it
	tg := setupTelegram(ctx, cfg, hub, rdb, acc)

	// Start NATS→hub bridge
	go runNATSBridge(ctx, nc, hub)

	// Start stale eviction sweep
	go runEvictionSweep(ctx, hub)

	// Value-bet channel: independent hub, subject, WS endpoint and snapshot
	go runValueBridge(ctx, nc, valueHub)
	go runValueEvictionSweep(ctx, valueHub)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%s", cfg.Port),
		Handler:      newMux(hub, valueHub, tg, gate),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // no timeout for WS connections
	}

	go func() {
		log.Info().Str("port", cfg.Port).Msg("HTTP server listening")
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("Server error")
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	log.Info().Msg("Shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	rdb.Close()
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var i int
	if n, _ := fmt.Sscanf(v, "%d", &i); n != 1 {
		log.Warn().Str("key", key).Str("value", v).Int("fallback", fallback).
			Msg("env var is not a valid integer, using fallback")
		return fallback
	}
	return i
}
