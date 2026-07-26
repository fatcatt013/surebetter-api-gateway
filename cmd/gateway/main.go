package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
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
	EvictionTTLS      int // EVICTION_TTL_S: seconds until an unseen PREMATCH entry is evicted
	EvictionLiveTTLS  int // EVICTION_LIVE_TTL_S: shorter TTL for "-live" arbs (fast removal)
	EvictionIntervalS int // EVICTION_INTERVAL_S: sweep cadence in seconds
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
		EvictionTTLS:      getenvInt("EVICTION_TTL_S", 120),
		EvictionLiveTTLS:  getenvInt("EVICTION_LIVE_TTL_S", 8),
		EvictionIntervalS: getenvInt("EVICTION_INTERVAL_S", 3),
	}
}

// ── Message types sent to Angular clients ────────────────────────────────────
type WSMessage struct {
	Type string          `json:"type"` // "snapshot" | "patch" | "ping"
	Data json.RawMessage `json:"data,omitempty"`
	Ops  []PatchOp       `json:"ops,omitempty"` // RFC 6902 JSON Patch
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

// arbEntry wraps ArbOpportunity with a lastSeen timestamp for eviction tracking.
type arbEntry struct {
	opp      ArbOpportunity
	lastSeen time.Time
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
}

// ── Hub — manages all WebSocket client connections ───────────────────────────
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
	state   map[string]arbEntry // current arb state
	stateMu sync.RWMutex

	snapshotCache    []byte
	snapshotCachedAt int64

	cfg       Config
	connCount atomic.Int64
	msgSent   atomic.Int64
}

func NewHub(cfg Config) *Hub {
	return &Hub{
		clients: make(map[*Client]struct{}),
		state:   make(map[string]arbEntry),
		cfg:     cfg,
	}
}

func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.connCount.Add(1)
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	h.connCount.Add(-1)
}

func (h *Hub) ClientCount() int64 { return h.connCount.Load() }

// ApplyOpportunity stores opp under its composite key and returns the patch op to broadcast.
func (h *Hub) ApplyOpportunity(opp ArbOpportunity) PatchOp {
	key := oppStateKey(opp)
	h.stateMu.Lock()
	h.state[key] = arbEntry{opp: opp, lastSeen: time.Now()}
	h.snapshotCache = nil
	h.stateMu.Unlock()
	return PatchOp{
		Op:    "replace",
		Path:  fmt.Sprintf("/arb/%s", key),
		Value: opp,
	}
}

// RemoveOpportunity deletes opp's key from state (if present) and returns the
// "remove" PatchOp to broadcast. Driven by an explicit removal from the arb-engine
// (opp.Removed=true) so a vanished/no-longer-valid arb disappears immediately rather
// than after the eviction TTL. Returns ok=false (and a zero PatchOp) if the key was
// not in state, so the bridge can skip broadcasting a no-op remove.
func (h *Hub) RemoveOpportunity(opp ArbOpportunity) (PatchOp, bool) {
	key := oppStateKey(opp)
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
	return PatchOp{Op: "remove", Path: fmt.Sprintf("/arb/%s", key)}, true
}

// evictStale removes Hub state entries not updated within their TTL and returns
// one "remove" PatchOp per evicted key. Live arbs ("-live" sport) use the shorter
// liveTTL so a vanished live arb disappears fast; prematch uses prematchTTL. The
// arb-engine re-publishes a still-valid live arb on the fetcher heartbeat cadence
// (~3s), so liveTTL only fires once an arb genuinely stops being detected.
// Invalidates snapshotCache if any entries were removed. Safe to call concurrently.
func (h *Hub) evictStale(prematchTTL, liveTTL time.Duration) []PatchOp {
	var ops []PatchOp
	h.stateMu.Lock()
	for key, entry := range h.state {
		ttl := prematchTTL
		if strings.HasSuffix(entry.opp.Sport, "-live") {
			ttl = liveTTL
		}
		if time.Since(entry.lastSeen) > ttl {
			delete(h.state, key)
			ops = append(ops, PatchOp{Op: "remove", Path: fmt.Sprintf("/arb/%s", key)})
		}
	}
	if len(ops) > 0 {
		h.snapshotCache = nil
	}
	h.stateMu.Unlock()
	return ops
}

// BroadcastPatch sends a JSON Patch delta to all connected clients
func (h *Hub) BroadcastPatch(ops []PatchOp) {
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
			log.Warn().Msg("Client send buffer full, skipping delta")
		}
	}
}

// Snapshot returns all current arb opportunities as JSON (cached for SnapshotCacheMs)
func (h *Hub) Snapshot() []byte {
	// Write lock required: cache miss path writes snapshotCache and snapshotCachedAt.
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	now := time.Now().UnixMilli()
	if h.snapshotCache != nil && now-h.snapshotCachedAt < h.cfg.SnapshotCacheMs {
		return h.snapshotCache
	}
	plain := make(map[string]ArbOpportunity, len(h.state))
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
	conn *websocket.Conn
	send chan []byte
	hub  *Hub
}

func (c *Client) writePump(ctx context.Context) {
	pingTicker := time.NewTicker(30 * time.Second)
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
func runNATSBridge(ctx context.Context, nc *nats.Conn, hub *Hub) {
	// Batch deltas within BroadcastBatchMs window
	pending := make([]PatchOp, 0, 32)
	ticker := time.NewTicker(time.Duration(hub.cfg.BroadcastBatchMs) * time.Millisecond)
	defer ticker.Stop()

	msgCh := make(chan *nats.Msg, 256)
	sub, _ := nc.ChanSubscribe("arb.opportunities", msgCh)
	defer sub.Unsubscribe()

	for {
		select {
		case msg := <-msgCh:
			var opp ArbOpportunity
			if err := json.Unmarshal(msg.Data, &opp); err != nil {
				log.Error().Err(err).Msg("Failed to parse arb opportunity")
				continue
			}

			if opp.Removed {
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
	prematchTTL := time.Duration(hub.cfg.EvictionTTLS) * time.Second
	liveTTL := time.Duration(hub.cfg.EvictionLiveTTLS) * time.Second
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
				log.Info().Int("evicted", len(ops)).Msg("Stale arb entries evicted")
			}
		case <-ctx.Done():
			return
		}
	}
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────
func wsHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if hub.ClientCount() >= int64(hub.cfg.MaxClients) {
			http.Error(w, "max clients reached", http.StatusServiceUnavailable)
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // TODO: add JWT auth check here
		})
		if err != nil {
			log.Error().Err(err).Msg("WS accept failed")
			return
		}

		client := &Client{conn: conn, send: make(chan []byte, 64), hub: hub}
		hub.Register(client)
		defer hub.Unregister(client)

		ctx := r.Context()

		// Send initial full snapshot
		snapshot := hub.Snapshot()
		msg := WSMessage{Type: "snapshot", Data: snapshot}
		if err := wsjson.Write(ctx, conn, msg); err != nil {
			log.Error().Err(err).Msg("Failed to send snapshot")
			return
		}

		client.writePump(ctx)
	}
}

func snapshotHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(hub.Snapshot())
	}
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","service":"api-gateway","version":"%s"}`, version)
}

func metricsHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w,
			"gateway_ws_clients_connected %d\ngateway_messages_sent_total %d\n",
			hub.ClientCount(), hub.msgSent.Load(),
		)
	}
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
	hub := NewHub(cfg)

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

	// Start NATS→hub bridge
	go runNATSBridge(ctx, nc, hub)

	// Start stale eviction sweep
	go runEvictionSweep(ctx, hub)

	// HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler(hub))
	mux.HandleFunc("/api/arb/snapshot", snapshotHandler(hub))
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/metrics", metricsHandler(hub))

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%s", cfg.Port),
		Handler:      mux,
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
