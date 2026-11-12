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

// botBook is one bookmaker the bot runs, as its last status reported it.
type botBook struct {
	Bookmaker string `json:"bookmaker"`
	Ready     bool   `json:"ready"`
}

// botView is the GET /api/me/bot body.
type botView struct {
	Online bool      `json:"online"`
	Books  []botBook `json:"books"`
	DryRun bool      `json:"dryRun"`
	Busy   bool      `json:"busy"`
}

// botPresence is one user's open bot socket and the bot's last status.
type botPresence struct {
	client *Client
	books  []botBook
	dryRun bool
	busy   string // id of the order the bot is running; "" = idle
}

// view of a presence; a nil presence is the offline answer.
func (p *botPresence) view() botView {
	if p == nil {
		return botView{Books: []botBook{}, DryRun: true}
	}
	return botView{Online: true, Books: append([]botBook{}, p.books...), DryRun: p.dryRun, Busy: p.busy != ""}
}

// botHub holds each user's bot presence and the orders sent to bots, in memory (one gateway
// instance: a restart means offline until the bot reconnects, and forgets sent orders). It tells
// the user's other /ws sockets when presence changes.
type botHub struct {
	reg    *connRegistry
	now    func() time.Time
	mu     sync.Mutex
	bots   map[int64]*botPresence // user id → presence
	orders map[string]botOrder    // order id → order, until its final event or orderMemory
}

func newBotHub(reg *connRegistry) *botHub {
	return &botHub{reg: reg, now: time.Now, bots: map[int64]*botPresence{}, orders: map[string]botOrder{}}
}

func (b *botHub) view(userID int64) botView {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bots[userID].view()
}

// attach makes c its user's bot socket: online, no books yet, dry run, idle. The user's older bot
// socket, if any, is closed, and its busy state goes with it.
func (b *botHub) attach(c *Client) {
	userID := c.principal.UserID
	b.mu.Lock()
	old := b.bots[userID]
	b.bots[userID] = &botPresence{client: c, books: []botBook{}, dryRun: true}
	b.mu.Unlock()
	if old != nil {
		// Close waits for the peer's close frame; do not hold up the new socket's handler.
		go old.client.conn.Close(websocket.StatusNormalClosure, "replaced by a newer bot socket")
	}
	b.changed(userID)
}

// detach takes the user's bot offline if c is still its bot socket (a replaced socket is not).
func (b *botHub) detach(c *Client) {
	userID := c.principal.UserID
	b.mu.Lock()
	p := b.bots[userID]
	if p == nil || p.client != c {
		b.mu.Unlock()
		return
	}
	delete(b.bots, userID)
	b.mu.Unlock()
	b.changed(userID)
}

var botChangedMsg = []byte(`{"type":"bot"}`)

// changed sends {"type":"bot"} to the user's non-bot /ws sockets; the app re-fetches GET /api/me/bot.
func (b *botHub) changed(userID int64) { b.reg.sendUser(userID, botChangedMsg) }

// principalOf is the principal RequireSession / RequireBotCode put on the request.
func principalOf(r *http.Request) accounts.Principal {
	p, _ := accounts.PrincipalFrom(r.Context())
	return p
}

// setStatus stores the bot's last status. false: codeID has no open bot socket for userID (never
// connected, closed, or replaced by another code's socket).
func (b *botHub) setStatus(userID, codeID int64, books []botBook, dryRun bool) bool {
	if books == nil {
		books = []botBook{}
	}
	b.mu.Lock()
	p := b.bots[userID]
	if p == nil || p.client.principal.CodeID != codeID {
		b.mu.Unlock()
		return false
	}
	changed := p.dryRun != dryRun || !slices.Equal(p.books, books)
	p.books, p.dryRun = books, dryRun
	b.mu.Unlock()
	if changed {
		b.changed(userID)
	}
	return true
}

// newBotAPI serves /api/me/bot and /api/me/bot/*, behind CSRF like the accounts API.
func newBotAPI(acc *accounts.Service, b *botHub) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/me/bot", acc.RequireSession(http.HandlerFunc(b.handleGet)))
	mux.Handle("POST /api/me/bot/orders", acc.RequireSession(http.HandlerFunc(b.handleOrder)))
	mux.Handle("POST /api/me/bot/status", acc.RequireBotCode(http.HandlerFunc(b.handleStatus)))
	return accounts.CSRF(mux)
}

func (b *botHub) handleGet(w http.ResponseWriter, r *http.Request) {
	accounts.WriteJSON(w, http.StatusOK, b.view(principalOf(r).UserID))
}

func (b *botHub) handleStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Books  []botBook `json:"books"`
		DryRun *bool     `json:"dryRun"`
	}
	if !accounts.DecodeJSON(w, r, &in, 16<<10) {
		return
	}
	dryRun := in.DryRun == nil || *in.DryRun // never Live by omission
	p := principalOf(r)
	if !b.setStatus(p.UserID, p.CodeID, in.Books, dryRun) {
		accounts.WriteError(w, http.StatusConflict, "bot_offline")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// orderMemory is how long a sent order is remembered for its events when no final event comes.
const orderMemory = 10 * time.Minute

// botLeg is one leg of an order, as the app sends it and the bot receives it.
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
// ignored); the bot socket receives it with the gateway's order_id.
type botOrderMsg struct {
	OrderID string   `json:"order_id"`
	ArbKey  string   `json:"arb_key"`
	Legs    []botLeg `json:"legs"`
}

// botOrder is what the gateway remembers of a sent order, to relay its events.
type botOrder struct {
	userID  int64
	arbKey  string
	expires time.Time
}

var (
	errBotOffline   = errors.New("bot_offline")
	errBotBusy      = errors.New("bot_busy")
	errBookNotReady = errors.New("book_not_ready")
)

// validOrder checks the shape: an arb key, exactly 2 legs at different bookmakers, each with a
// selection id, odds above 1 and a positive stake.
func validOrder(o botOrderMsg) bool {
	if strings.TrimSpace(o.ArbKey) == "" || len(o.Legs) != 2 || o.Legs[0].Bookmaker == o.Legs[1].Bookmaker {
		return false
	}
	for _, l := range o.Legs {
		if strings.TrimSpace(l.Bookmaker) == "" || strings.TrimSpace(l.SourceID) == "" || !(l.Odds > 1) || !(l.Stake > 0) {
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

// ready reports whether the bot's last status lists book as ready.
func (p *botPresence) ready(book string) bool {
	for _, x := range p.books {
		if x.Bookmaker == book && x.Ready {
			return true
		}
	}
	return false
}

// pruneLocked forgets orders older than orderMemory. b.mu must be held.
func (b *botHub) pruneLocked() {
	now := b.now()
	for id, o := range b.orders {
		if !now.Before(o.expires) {
			delete(b.orders, id)
		}
	}
}

// order sends a valid order to userID's bot socket and marks the bot busy. Checks, in this order:
// a bot is online (errBotOffline), it is idle (errBotBusy), every leg's bookmaker is ready
// (errBookNotReady).
func (b *botHub) order(userID int64, o botOrderMsg) (string, error) {
	o.OrderID = newOrderID()
	payload, err := json.Marshal(struct {
		Type  string      `json:"type"`
		Order botOrderMsg `json:"order"`
	}{"order", o})
	if err != nil {
		return "", err
	}
	if err := b.startOrder(userID, o, payload); err != nil {
		return "", err
	}
	b.changed(userID) // busy
	return o.OrderID, nil
}

func (b *botHub) startOrder(userID int64, o botOrderMsg, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.bots[userID]
	if p == nil {
		return errBotOffline
	}
	if p.busy != "" {
		return errBotBusy
	}
	for _, l := range o.Legs {
		if !p.ready(l.Bookmaker) {
			return errBookNotReady
		}
	}
	if !p.client.trySend(payload) {
		return errBotOffline // the socket is not draining its buffer
	}
	p.busy = o.OrderID
	b.pruneLocked()
	b.orders[o.OrderID] = botOrder{userID: userID, arbKey: o.ArbKey, expires: b.now().Add(orderMemory)}
	return nil
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
