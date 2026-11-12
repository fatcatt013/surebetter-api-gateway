package main

import (
	"sync"

	"nhooyr.io/websocket"
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

// botHub holds each user's bot presence in memory (one gateway instance: a restart means offline
// until the bot reconnects) and tells the user's other /ws sockets when it changes.
type botHub struct {
	reg  *connRegistry
	mu   sync.Mutex
	bots map[int64]*botPresence // user id → presence
}

func newBotHub(reg *connRegistry) *botHub {
	return &botHub{reg: reg, bots: map[int64]*botPresence{}}
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
