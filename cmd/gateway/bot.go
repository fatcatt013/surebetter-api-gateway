package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"nhooyr.io/websocket"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// botBook is one bookmaker an agent runs, as its last status reported it.
type botBook struct {
	Bookmaker string `json:"bookmaker"`
	Ready     bool   `json:"ready"`
}

// botBookView is one bookmaker in GET /api/me/bot: ready = a protocol-2 online agent has an idle
// window of it that no order has reserved; usable = one has an idle or busy window of it (an order
// can go or wait for it).
type botBookView struct {
	Bookmaker string `json:"bookmaker"`
	Ready     bool   `json:"ready"`
	Usable    bool   `json:"usable"`
}

// botToday counts the user's orders of the current Prague day (dry runs included).
type botToday struct {
	Orders int `json:"orders"`
	Placed int `json:"placed"`
}

// botView is the GET /api/me/bot body: all of the user's online agents together, plus the settings
// and the scheduler's counts.
type botView struct {
	Online       bool          `json:"online"`
	Books        []botBookView `json:"books"`
	DryRun       bool          `json:"dryRun"`
	Busy         bool          `json:"busy"`
	Update       bool          `json:"update"` // online, and every online agent is older than protocol 2
	Paused       bool          `json:"paused"`
	PausedReason string        `json:"pausedReason"`
	Running      int           `json:"running"`
	Waiting      int           `json:"waiting"`
	OrdersAtOnce int           `json:"ordersAtOnce"`
	Today        botToday      `json:"today"`
}

// botPresence is one agent's open bot socket (one per bot code) and its last status.
type botPresence struct {
	client      *Client
	books       []botBook
	dryRun      bool // the agent's own switch (its local page); orders carry the user's setting instead
	version     string
	hostName    string
	cpu, mem    *int                 // percent; nil = not reported
	windowsJSON string               // the last status's windows, as a JSON array
	windows     []botWindow          // the same, decoded
	boot        string               // the last status's boot id (agent process); "" = not sent
	proto       int                  // the last status's protocol; 0 = none yet or a Phase 1 agent
	idleSince   map[string]time.Time // window id → when the gateway first saw it idle and not reserved
	reservedAt  time.Time            // when its number of reserved windows last changed (or the socket opened)
}

// botHub holds each user's agents (bot sockets, one per bot code), their orders (waiting and
// running) and settings, in memory (one gateway instance), and the order history, settings and
// agent registry in the accounts DB. It tells the user's other /ws sockets what changed.
type botHub struct {
	reg    *connRegistry
	acc    *accounts.Service // agent registry, settings, orders; nil in unit tests that need no DB
	now    func() time.Time
	mu     sync.Mutex
	agents map[int64]map[int64]*botPresence // user id → code id → presence
	orders map[string]*botOrder             // order id → waiting or running order
	// settings caches each user's bot settings (loaded by loadSettings before b.mu is taken).
	settings map[int64]accounts.BotSettings
	commands map[string]*pendingCommand // window command id → the app request waiting for the agent's answer
	// dbMu orders the database writes of the hub's changes: release takes it before it lets go of b.mu.
	dbMu  sync.Mutex
	logs  *minuteLimiter // POST /api/me/bot/logs lines per user per minute
	shots *minuteLimiter // POST /api/me/bot/screenshots per user per minute
	// shotsDir holds screenshots as <user id>/<id>.jpg (<data dir>/bot-screenshots); "" = none.
	shotsDir string
	shotsMu  sync.Mutex // held from a user directory's size check to the write, so uploads cannot overrun the cap
}

func newBotHub(reg *connRegistry, acc *accounts.Service) *botHub {
	return &botHub{reg: reg, acc: acc, now: time.Now, agents: map[int64]map[int64]*botPresence{}, orders: map[string]*botOrder{},
		settings: map[int64]accounts.BotSettings{}, commands: map[string]*pendingCommand{},
		logs: &minuteLimiter{limit: logsPerMinute}, shots: &minuteLimiter{limit: shotsPerMinute}}
}

// view is userID's GET /api/me/bot body.
func (b *botHub) view(userID int64) botView {
	b.loadSettings(userID)
	b.mu.Lock()
	fx := &effects{}
	b.expireLocked(fx)
	v := b.viewLocked(userID)
	b.release(fx)
	if b.acc != nil {
		from, to := pragueDayOf(b.now())
		if day, err := b.acc.BotOrderDay(userID, from.UnixMilli(), to.UnixMilli()); err != nil {
			log.Error().Err(err).Msg("bot day stats failed")
		} else {
			v.Today = botToday{Orders: day.Orders, Placed: day.Placed}
		}
	}
	return v
}

// viewLocked aggregates userID's online agents and orders. No agents is the offline answer. A
// bookmaker (from any online agent's books or windows) is ready when a protocol-2 agent has an idle
// window of it that no order has reserved, usable when one has an idle or busy window of it; busy:
// online and no ready window at all; update: online and no protocol-2 agent. dryRun is the setting.
// b.mu must be held.
func (b *botHub) viewLocked(userID int64) botView {
	st := b.settingsLocked(userID)
	v := botView{Books: []botBookView{}, DryRun: st.DryRun, Paused: st.Paused, PausedReason: st.PausedReason, OrdersAtOnce: st.OrdersAtOnce}
	for _, o := range b.orders {
		if o.userID != userID {
			continue
		}
		if o.state == orderRunning {
			v.Running++
		} else {
			v.Waiting++
		}
	}
	agents := b.agents[userID]
	if len(agents) == 0 {
		return v
	}
	v.Online, v.Update = true, true
	reserved := b.reservedLocked(userID)
	books := map[string]*botBookView{}
	book := func(name string) *botBookView {
		if books[name] == nil {
			books[name] = &botBookView{Bookmaker: name}
		}
		return books[name]
	}
	free := false
	for codeID, p := range agents {
		v.Update = v.Update && p.proto < botProto
		for _, bk := range p.books {
			book(bk.Bookmaker) // reported by an agent, but only a window makes it ready or usable
		}
		for _, w := range p.windows {
			bv := book(w.Bookmaker)
			if p.proto < botProto {
				continue
			}
			ok := w.State == "idle" && reserved[codeID][w.ID] == ""
			bv.Ready = bv.Ready || ok
			bv.Usable = bv.Usable || w.State == "idle" || w.State == "busy"
			free = free || ok
		}
	}
	v.Busy = !free
	for _, bv := range books {
		v.Books = append(v.Books, *bv)
	}
	slices.SortFunc(v.Books, func(a, b botBookView) int { return strings.Compare(a.Bookmaker, b.Bookmaker) })
	return v
}

// presenceLocked returns userID's agent for codeID, or nil. b.mu must be held.
func (b *botHub) presenceLocked(userID, codeID int64) *botPresence {
	return b.agents[userID][codeID]
}

// attach makes c the bot socket of its code's agent: online, no books or windows yet, dry run. That
// code's older socket, if any, is closed; other codes' agents stay. The orders this code is running
// keep their windows reserved (an agent may reconnect mid-order, with money on a slip). Waiting
// orders are retried. The agent's registry row is upserted before the app is told.
func (b *botHub) attach(c *Client) {
	userID, codeID := c.principal.UserID, c.principal.CodeID
	b.loadSettings(userID)
	b.mu.Lock()
	fx := &effects{}
	b.expireLocked(fx)
	if b.agents[userID] == nil {
		b.agents[userID] = map[int64]*botPresence{}
	}
	old := b.agents[userID][codeID]
	b.agents[userID][codeID] = &botPresence{client: c, books: []botBook{}, dryRun: true, windowsJSON: "[]",
		idleSince: map[string]time.Time{}, reservedAt: b.now()}
	b.dispatchLocked(userID, fx)
	b.release(fx)
	if old != nil {
		// Close waits for the peer's close frame; do not hold up the new socket's handler.
		go old.client.conn.Close(websocket.StatusNormalClosure, "replaced by a newer bot socket")
	}
	b.touch(userID, codeID)
	b.changed(userID)
}

// detach takes the agent offline if c is still its code's bot socket (a replaced socket is not).
func (b *botHub) detach(c *Client) {
	userID, codeID := c.principal.UserID, c.principal.CodeID
	b.mu.Lock()
	p := b.agents[userID][codeID]
	if p == nil || p.client != c {
		b.mu.Unlock()
		return
	}
	delete(b.agents[userID], codeID)
	if len(b.agents[userID]) == 0 {
		delete(b.agents, userID)
	}
	b.mu.Unlock()
	b.touch(userID, codeID)
	b.changed(userID)
}

// touch moves the agent's last seen in the registry (inserting its row on first connect).
func (b *botHub) touch(userID, codeID int64) {
	if b.acc == nil {
		return
	}
	if err := b.acc.TouchBotAgent(userID, codeID); err != nil {
		log.Error().Err(err).Int64("code", codeID).Msg("bot agent touch failed")
	}
}

var botChangedMsg = []byte(`{"type":"bot"}`)

// changed sends {"type":"bot"} to the user's non-bot /ws sockets; the app re-fetches GET /api/me/bot.
func (b *botHub) changed(userID int64) { b.reg.sendUser(userID, botChangedMsg) }

// principalOf is the principal RequireSession / RequireBotCode put on the request.
func principalOf(r *http.Request) accounts.Principal {
	p, _ := accounts.PrincipalFrom(r.Context())
	return p
}

// newBotAPI serves /api/me/bot and /api/me/bot/*, behind CSRF like the accounts API.
func newBotAPI(acc *accounts.Service, b *botHub) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/me/bot", acc.RequireSession(http.HandlerFunc(b.handleGet)))
	mux.Handle("POST /api/me/bot/orders", acc.RequireSession(http.HandlerFunc(b.handleOrder)))
	mux.Handle("GET /api/me/bot/orders", acc.RequireSession(http.HandlerFunc(b.handleGetOrders)))
	mux.Handle("DELETE /api/me/bot/orders/{id}", acc.RequireSession(http.HandlerFunc(b.handleCancelOrder)))
	mux.Handle("POST /api/me/bot/status", acc.RequireBotCode(http.HandlerFunc(b.handleStatus)))
	mux.Handle("POST /api/me/bot/events", acc.RequireBotCode(http.HandlerFunc(b.handleEvent)))
	mux.Handle("GET /api/me/bot/agents", acc.RequireSession(http.HandlerFunc(b.handleAgents)))
	mux.Handle("POST /api/me/bot/logs", acc.RequireBotCode(http.HandlerFunc(b.handlePostLogs)))
	mux.Handle("GET /api/me/bot/logs", acc.RequireSession(http.HandlerFunc(b.handleGetLogs)))
	mux.Handle("GET /api/me/bot/logs.txt", acc.RequireSession(http.HandlerFunc(b.handleLogsText)))
	mux.Handle("POST /api/me/bot/screenshots", acc.RequireBotCode(http.HandlerFunc(b.handlePostShot)))
	mux.Handle("GET /api/me/bot/screenshots/{id}", acc.RequireSession(http.HandlerFunc(b.handleGetShot)))
	mux.Handle("GET /api/me/bot/settings", acc.RequireSession(http.HandlerFunc(b.handleGetSettings)))
	mux.Handle("PUT /api/me/bot/settings", acc.RequireSession(http.HandlerFunc(b.handlePutSettings)))
	mux.Handle("POST /api/me/bot/agents/{agentId}/windows", acc.RequireSession(http.HandlerFunc(b.handleAddWindow)))
	mux.Handle("POST /api/me/bot/agents/{agentId}/windows/{windowId}/{action}", acc.RequireSession(http.HandlerFunc(b.handleWindowAction)))
	mux.Handle("POST /api/me/bot/commands", acc.RequireBotCode(http.HandlerFunc(b.handleCommandAck)))
	mux.Handle("PUT /api/me/bot/flows", acc.RequireBotCode(http.HandlerFunc(b.handlePutFlows)))
	mux.Handle("GET /api/me/bot/flows", acc.RequireSession(http.HandlerFunc(b.handleGetFlows)))
	mux.Handle("PUT /api/me/bot/agents/{agentId}/recordings/{kind}", acc.RequireSession(http.HandlerFunc(b.handlePutRecording)))
	mux.Handle("GET /api/me/bot/agents/{agentId}/recordings", acc.RequireSession(http.HandlerFunc(b.handleAgentRecordings)))
	mux.Handle("GET /api/me/bot/recordings", acc.RequireBotCode(http.HandlerFunc(b.handleOwnRecordings)))
	mux.Handle("GET /api/me/bot/recordings/{kind}", acc.RequireBotCode(http.HandlerFunc(b.handleOwnRecording)))
	return accounts.CSRF(mux)
}

func (b *botHub) handleGet(w http.ResponseWriter, r *http.Request) {
	accounts.WriteJSON(w, http.StatusOK, b.view(principalOf(r).UserID))
}

// newOrderID is 8 random bytes as 16 hex characters (order, command and screenshot ids).
func newOrderID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// trySend queues payload on c without blocking; false when its send buffer is full.
func (c *Client) trySend(payload []byte) bool {
	select {
	case c.send <- payload:
		return true
	default:
		return false
	}
}
