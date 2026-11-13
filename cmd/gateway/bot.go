package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// botView is the GET /api/me/bot body: all of the user's online agents together.
type botView struct {
	Online bool      `json:"online"`
	Books  []botBook `json:"books"`
	DryRun bool      `json:"dryRun"`
	Busy   bool      `json:"busy"`
}

// botPresence is one agent's open bot socket (one per bot code) and its last status.
type botPresence struct {
	client      *Client
	books       []botBook
	dryRun      bool
	busy        string    // id of the order the agent is running; "" = idle
	freeSince   time.Time // when the agent last became idle (socket open or busy cleared)
	version     string
	hostName    string
	cpu, mem    *int        // percent; nil = not reported
	windowsJSON string      // the last status's windows, as a JSON array
	windows     []botWindow // the same, decoded
	boot        string      // the last status's boot id (agent process); "" = not sent
	proto       int         // the last status's protocol; 0 = none yet or a Phase 1 agent
}

// viewOf aggregates a user's online agents. No agents is the offline answer. A bookmaker is ready
// when it is ready on an agent that is not busy; dry run when any agent is in dry run; busy when
// every agent is busy.
func viewOf(agents map[int64]*botPresence) botView {
	if len(agents) == 0 {
		return botView{Books: []botBook{}, DryRun: true}
	}
	ready := map[string]bool{}
	v := botView{Online: true, Busy: true}
	for _, p := range agents {
		v.DryRun = v.DryRun || p.dryRun
		v.Busy = v.Busy && p.busy != ""
		for _, b := range p.books {
			ready[b.Bookmaker] = ready[b.Bookmaker] || (b.Ready && p.busy == "")
		}
	}
	v.Books = make([]botBook, 0, len(ready))
	for book, ok := range ready {
		v.Books = append(v.Books, botBook{Bookmaker: book, Ready: ok})
	}
	slices.SortFunc(v.Books, func(a, b botBook) int { return strings.Compare(a.Bookmaker, b.Bookmaker) })
	return v
}

// botHub holds each user's agents (bot sockets, one per bot code) and the orders sent to them, in
// memory (one gateway instance: a restart means offline until the agents reconnect, and forgets
// sent orders). It records agents in the accounts DB and tells the user's other /ws sockets when
// presence changes.
type botHub struct {
	reg    *connRegistry
	acc    *accounts.Service // agent registry; nil in unit tests that need no DB
	now    func() time.Time
	mu     sync.Mutex
	agents map[int64]map[int64]*botPresence // user id → code id → presence
	orders map[string]botOrder              // order id → order, until its final event or orderMemory
	logs   *minuteLimiter                   // POST /api/me/bot/logs lines per user per minute
	shots  *minuteLimiter                   // POST /api/me/bot/screenshots per user per minute
	// shotsDir holds screenshots as <user id>/<id>.jpg (<data dir>/bot-screenshots); "" = none.
	shotsDir string
	shotsMu  sync.Mutex // held from a user directory's size check to the write, so uploads cannot overrun the cap
}

func newBotHub(reg *connRegistry, acc *accounts.Service) *botHub {
	return &botHub{reg: reg, acc: acc, now: time.Now, agents: map[int64]map[int64]*botPresence{}, orders: map[string]botOrder{},
		logs: &minuteLimiter{limit: logsPerMinute}, shots: &minuteLimiter{limit: shotsPerMinute}}
}

func (b *botHub) view(userID int64) botView {
	b.mu.Lock()
	freed := b.pruneLocked()
	v := viewOf(b.agents[userID])
	b.mu.Unlock()
	b.notifyAll(freed)
	return v
}

// presenceLocked returns userID's agent for codeID, or nil. b.mu must be held.
func (b *botHub) presenceLocked(userID, codeID int64) *botPresence {
	return b.agents[userID][codeID]
}

// attach makes c the bot socket of its code's agent: online, no books yet, dry run. That code's
// older socket, if any, is closed; other codes' agents stay. The agent starts busy with the order
// it was sent and that is still running (no final event, not forgotten): it may have reconnected
// mid-order, with money on a slip, and must not be given a second order. Otherwise it starts idle.
// The agent's registry row is upserted before the app is told.
func (b *botHub) attach(c *Client) {
	userID, codeID := c.principal.UserID, c.principal.CodeID
	b.mu.Lock()
	freed := b.pruneLocked()
	if b.agents[userID] == nil {
		b.agents[userID] = map[int64]*botPresence{}
	}
	old := b.agents[userID][codeID]
	b.agents[userID][codeID] = &botPresence{client: c, books: []botBook{}, dryRun: true, busy: b.runningLocked(userID, codeID),
		freeSince: b.now(), windowsJSON: "[]"}
	b.mu.Unlock()
	b.notifyAll(freed)
	if old != nil {
		// Close waits for the peer's close frame; do not hold up the new socket's handler.
		go old.client.conn.Close(websocket.StatusNormalClosure, "replaced by a newer bot socket")
	}
	b.touch(userID, codeID)
	b.changed(userID)
}

// runningLocked is the id of the order sent to codeID's agent that is still remembered (the one
// sent last if, unexpectedly, there are several), or "". b.mu must be held.
func (b *botHub) runningLocked(userID, codeID int64) string {
	id, latest := "", time.Time{}
	for oid, o := range b.orders {
		if o.userID == userID && o.codeID == codeID && (id == "" || o.expires.After(latest)) {
			id, latest = oid, o.expires
		}
	}
	return id
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
	mux.Handle("POST /api/me/bot/status", acc.RequireBotCode(http.HandlerFunc(b.handleStatus)))
	mux.Handle("POST /api/me/bot/events", acc.RequireBotCode(http.HandlerFunc(b.handleEvent)))
	mux.Handle("GET /api/me/bot/agents", acc.RequireSession(http.HandlerFunc(b.handleAgents)))
	mux.Handle("POST /api/me/bot/logs", acc.RequireBotCode(http.HandlerFunc(b.handlePostLogs)))
	mux.Handle("GET /api/me/bot/logs", acc.RequireSession(http.HandlerFunc(b.handleGetLogs)))
	mux.Handle("GET /api/me/bot/logs.txt", acc.RequireSession(http.HandlerFunc(b.handleLogsText)))
	mux.Handle("POST /api/me/bot/screenshots", acc.RequireBotCode(http.HandlerFunc(b.handlePostShot)))
	mux.Handle("GET /api/me/bot/screenshots/{id}", acc.RequireSession(http.HandlerFunc(b.handleGetShot)))
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

// orderMemory is how long a sent order is remembered for its events when no final event comes.
const orderMemory = 10 * time.Minute

// botLeg is one leg of an order, as the app sends it and the agent receives it.
type botLeg struct {
	Bookmaker       string  `json:"bookmaker"`
	MatchURL        string  `json:"match_url"`
	SourceID        string  `json:"source_id"`
	SourceMarketRef string  `json:"source_market_ref"`
	MarketName      string  `json:"market_name"`
	Label           string  `json:"label"`
	Odds            float64 `json:"odds"`
	Stake           float64 `json:"stake"`
}

// botOrderMsg is an order: POST /api/me/bot/orders sends arb_key and legs (an order_id in the body is
// ignored); the agent's bot socket receives it with the gateway's order_id.
type botOrderMsg struct {
	OrderID string   `json:"order_id"`
	ArbKey  string   `json:"arb_key"`
	Legs    []botLeg `json:"legs"`
}

// botOrder is what the gateway remembers of a sent order, to relay its events.
type botOrder struct {
	userID  int64
	codeID  int64 // the agent the order was sent to; only it may post the order's events
	arbKey  string
	boot    string // the agent's boot id when the order was sent; "" = none
	expires time.Time
}

// endedOrder is an order the gateway ended itself, to relay after b.mu is released.
type endedOrder struct{ id, arbKey string }

var (
	errBotOffline   = errors.New("bot_offline")
	errBotBusy      = errors.New("bot_busy")
	errBookNotReady = errors.New("book_not_ready")
)

// validOrder checks the shape: an arb key (at most 256 bytes), exactly 2 legs at different bookmakers, each with a
// selection id, odds above 1 and a positive stake.
func validOrder(o botOrderMsg) bool {
	if strings.TrimSpace(o.ArbKey) == "" || len(o.ArbKey) > 256 || len(o.Legs) != 2 || o.Legs[0].Bookmaker == o.Legs[1].Bookmaker {
		return false
	}
	for _, l := range o.Legs {
		if strings.TrimSpace(l.Bookmaker) == "" || l.Bookmaker != strings.TrimSpace(l.Bookmaker) || strings.TrimSpace(l.SourceID) == "" || !(l.Odds > 1) || !(l.Stake > 0) {
			return false
		}
	}
	return true
}

// newOrderID is 8 random bytes as 16 hex characters.
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

// ready reports whether the agent's last status lists book as ready.
func (p *botPresence) ready(book string) bool {
	for _, x := range p.books {
		if x.Bookmaker == book && x.Ready {
			return true
		}
	}
	return false
}

// freeLocked clears the agent's busy flag if it is busy with orderID; true if it was. b.mu must be held.
func (b *botHub) freeLocked(userID, codeID int64, orderID string) bool {
	p := b.presenceLocked(userID, codeID)
	if p == nil || p.busy != orderID {
		return false
	}
	p.busy = ""
	p.freeSince = b.now()
	return true
}

// pruneLocked forgets orders older than orderMemory. An agent that was busy with a forgotten order
// is idle again; the users whose busy flag changed are returned, to be passed to notifyAll after
// b.mu is released. b.mu must be held.
func (b *botHub) pruneLocked() []int64 {
	now := b.now()
	var freed []int64
	for id, o := range b.orders {
		if now.Before(o.expires) {
			continue
		}
		delete(b.orders, id)
		if b.freeLocked(o.userID, o.codeID, id) {
			freed = append(freed, o.userID)
		}
	}
	return freed
}

// notifyAll sends {"type":"bot"} for each user in ids (call without b.mu).
func (b *botHub) notifyAll(ids []int64) {
	for _, id := range ids {
		b.changed(id)
	}
}

// order sends a valid order to one of userID's agents and marks that agent busy.
func (b *botHub) order(userID int64, o botOrderMsg) (string, error) {
	o.OrderID = newOrderID()
	payload, err := json.Marshal(struct {
		Type  string      `json:"type"`
		Order botOrderMsg `json:"order"`
	}{"order", o})
	if err != nil {
		return "", err
	}
	freed, err := b.startOrder(userID, o, payload)
	b.notifyAll(freed)
	if err != nil {
		return "", err
	}
	b.changed(userID) // busy
	return o.OrderID, nil
}

// pickLocked chooses the agent for an order: online, idle, with both legs' bookmakers ready; of
// those the one idle longest (lowest code id on a tie). Errors: no agent online (errBotOffline);
// agents with both books ready exist but all are busy (errBotBusy); no agent has both books ready
// (errBookNotReady). Agents in skip are left out. b.mu must be held.
func (b *botHub) pickLocked(userID int64, o botOrderMsg, skip map[int64]bool) (int64, *botPresence, error) {
	agents := b.agents[userID]
	if len(agents) == 0 {
		return 0, nil, errBotOffline
	}
	var bestID int64
	var best *botPresence
	capable := false
	for codeID, p := range agents {
		if skip[codeID] || !p.ready(o.Legs[0].Bookmaker) || !p.ready(o.Legs[1].Bookmaker) {
			continue
		}
		capable = true
		if p.busy != "" {
			continue
		}
		if best == nil || p.freeSince.Before(best.freeSince) || (p.freeSince.Equal(best.freeSince) && codeID < bestID) {
			bestID, best = codeID, p
		}
	}
	switch {
	case best != nil:
		return bestID, best, nil
	case capable:
		return 0, nil, errBotBusy
	default:
		return 0, nil, errBookNotReady
	}
}

// startOrder prunes forgotten orders first (a forgotten order must not keep an agent busy), then
// picks an agent and sends. An agent whose socket does not take the order (its send buffer is
// full: it is not draining) is left out and the pick repeated; when no other agent can take it,
// the answer is errBotOffline. freed lists the users whose busy flag the prune cleared.
func (b *botHub) startOrder(userID int64, o botOrderMsg, payload []byte) (freed []int64, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	freed = b.pruneLocked()
	var skip map[int64]bool
	var codeID int64
	var p *botPresence
	for {
		codeID, p, err = b.pickLocked(userID, o, skip)
		if err != nil {
			if skip != nil {
				err = errBotOffline // the agents that could take it are not draining their sockets
			}
			return freed, err
		}
		if p.client.trySend(payload) {
			break
		}
		if skip == nil {
			skip = map[int64]bool{}
		}
		skip[codeID] = true
	}
	p.busy = o.OrderID
	b.orders[o.OrderID] = botOrder{userID: userID, codeID: codeID, arbKey: o.ArbKey, boot: p.boot, expires: b.now().Add(orderMemory)}
	return freed, nil
}

func (b *botHub) handleOrder(w http.ResponseWriter, r *http.Request) {
	var in botOrderMsg
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || !validOrder(in) {
		accounts.WriteError(w, http.StatusBadRequest, "bad_order")
		return
	}
	userID := principalOf(r).UserID
	id, err := b.order(userID, in)
	switch {
	case err == nil:
		log.Info().Int64("user", userID).Str("order_id", id).Str("arb_key", in.ArbKey).Msg("bot order sent")
		accounts.WriteJSON(w, http.StatusAccepted, map[string]string{"order_id": id})
	case errors.Is(err, errBotOffline), errors.Is(err, errBotBusy), errors.Is(err, errBookNotReady):
		accounts.WriteError(w, http.StatusConflict, err.Error())
	default:
		log.Error().Err(err).Msg("bot order failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
	}
}

// botEvents lists the events an agent may post; true = final (the order is over).
var botEvents = map[string]bool{
	"accepted":        false,
	"leg_ready":       false,
	"placed":          true,
	"stopped":         true,
	"outcome_unknown": true,
}

var errUnknownOrder = errors.New("unknown_order")

// botEventIn is the POST /api/me/bot/events body.
type botEventIn struct {
	OrderID   string `json:"order_id"`
	Event     string `json:"event"`
	Bookmaker string `json:"bookmaker"`
	Reason    string `json:"reason"`
}

// botEventMsg is the {"type":"botEvent"} message the user's non-bot /ws sockets receive.
type botEventMsg struct {
	Type      string `json:"type"`
	OrderID   string `json:"order_id"`
	ArbKey    string `json:"arb_key"`
	Event     string `json:"event"`
	Bookmaker string `json:"bookmaker,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// event relays one order event, posted by codeID's agent, to the user's non-bot /ws sockets. A
// final event forgets the order and, if that agent is still busy with it, clears busy.
// errUnknownOrder: the order is unknown, finished, older than orderMemory, another user's, or was
// sent to another agent.
func (b *botHub) event(userID, codeID int64, in botEventIn) error {
	final := botEvents[in.Event]
	b.mu.Lock()
	freed := b.pruneLocked()
	o, ok := b.orders[in.OrderID]
	if !ok || o.userID != userID || o.codeID != codeID {
		b.mu.Unlock()
		b.notifyAll(freed)
		return errUnknownOrder
	}
	cleared := false
	if final {
		delete(b.orders, in.OrderID)
		cleared = b.freeLocked(userID, codeID, in.OrderID)
	}
	b.mu.Unlock()
	b.notifyAll(freed)
	if err := b.relayEvent(userID, o.arbKey, in); err != nil {
		return err
	}
	if cleared {
		b.changed(userID) // after the botEvent: same goroutine, same send channels, so in order
	}
	return nil
}

// relayEvent sends an order event as {"type":"botEvent"} to the user's non-bot /ws sockets (call
// without b.mu).
func (b *botHub) relayEvent(userID int64, arbKey string, in botEventIn) error {
	payload, err := json.Marshal(botEventMsg{Type: "botEvent", OrderID: in.OrderID, ArbKey: arbKey,
		Event: in.Event, Bookmaker: in.Bookmaker, Reason: in.Reason})
	if err != nil {
		return err
	}
	b.reg.sendUser(userID, payload)
	return nil
}

func (b *botHub) handleEvent(w http.ResponseWriter, r *http.Request) {
	var in botEventIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		accounts.WriteError(w, http.StatusBadRequest, "bad_event")
		return
	}
	if _, ok := botEvents[in.Event]; !ok {
		accounts.WriteError(w, http.StatusBadRequest, "bad_event")
		return
	}
	p := principalOf(r)
	switch err := b.event(p.UserID, p.CodeID, in); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errUnknownOrder):
		accounts.WriteError(w, http.StatusNotFound, "unknown_order")
	default:
		log.Error().Err(err).Msg("bot event failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
	}
}
