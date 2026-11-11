package accounts

import (
	"sync"
	"time"
)

// tickets are short-lived, single-use WebSocket credentials. A browser extension cannot set
// headers on a WebSocket, so it trades its extension code for a ticket and connects with ?ticket=.
type tickets struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]ticket
}

type ticket struct {
	p       Principal
	expires time.Time
}

func newTickets(ttl time.Duration, now func() time.Time) *tickets {
	return &tickets{ttl: ttl, now: now, m: map[string]ticket{}}
}

func (t *tickets) issue(p Principal) string {
	tok := newSessionToken()
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for k, v := range t.m {
		if !now.Before(v.expires) {
			delete(t.m, k)
		}
	}
	t.m[tok] = ticket{p: p, expires: now.Add(t.ttl)}
	return tok
}

func (t *tickets) redeem(tok string) (Principal, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[tok]
	delete(t.m, tok)
	if !ok || !t.now().Before(v.expires) {
		return Principal{}, false
	}
	return v.p, true
}
