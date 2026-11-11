package main

import (
	"encoding/json"
	"sync"

	"nhooyr.io/websocket"
)

// connRegistry tracks every open WebSocket (both hubs) with its principal, so account changes
// (logout, disabled user, revoked extension code) close them and state writes notify the
// user's other feed sockets. It implements accounts.Connections.
type connRegistry struct {
	mu      sync.Mutex
	clients map[*Client]struct{}
}

func newConnRegistry() *connRegistry { return &connRegistry{clients: map[*Client]struct{}{}} }

func (r *connRegistry) add(c *Client) {
	r.mu.Lock()
	r.clients[c] = struct{}{}
	r.mu.Unlock()
}

func (r *connRegistry) remove(c *Client) {
	r.mu.Lock()
	delete(r.clients, c)
	r.mu.Unlock()
}

// closeWhere closes matching sockets with 1008. Close waits for the peer's close frame, so each
// runs in its own goroutine and the caller (an HTTP handler) is not held up.
func (r *connRegistry) closeWhere(match func(*Client) bool) {
	r.mu.Lock()
	var hit []*Client
	for c := range r.clients {
		if match(c) {
			hit = append(hit, c)
		}
	}
	r.mu.Unlock()
	for _, c := range hit {
		go c.conn.Close(websocket.StatusPolicyViolation, "signed out")
	}
}

func (r *connRegistry) CloseUser(id int64) {
	r.closeWhere(func(c *Client) bool { return id != 0 && c.principal.UserID == id })
}

func (r *connRegistry) CloseSession(id int64) {
	r.closeWhere(func(c *Client) bool { return id != 0 && c.principal.SessionID == id })
}

func (r *connRegistry) CloseCode(id int64) {
	r.closeWhere(func(c *Client) bool { return id != 0 && c.principal.CodeID == id })
}

// NotifyState sends {"type":"state","key":…} to the user's /ws sockets (not /ws/value), except
// the writer's own socket (client id exceptClient; empty notifies all). A full send buffer drops the message; the next change resends.
func (r *connRegistry) NotifyState(userID int64, key string, exceptClient string) {
	if userID == 0 {
		return
	}
	payload, _ := json.Marshal(WSMessage{Type: "state", Key: key})
	r.mu.Lock()
	defer r.mu.Unlock()
	for c := range r.clients {
		if !c.notify || c.principal.UserID != userID {
			continue
		}
		if c.cid != "" && c.cid == exceptClient {
			continue
		}
		select {
		case c.send <- payload:
		default:
		}
	}
}
