package main

import (
	"net/http"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// wsGate decides who may open a feed socket and protects the feed and Telegram routes.
// A nil gate (tests without accounts) leaves everything open, like before accounts existed.
type wsGate struct {
	acc      *accounts.Service
	reg      *connRegistry
	required bool // AUTH_REQUIRED
}

// admit returns the connecting principal: a ?ticket= (extension), else the session cookie of a
// same-site page. With AUTH_REQUIRED off, anyone else is admitted with a zero principal.
func (g *wsGate) admit(r *http.Request) (accounts.Principal, bool) {
	if g == nil || g.acc == nil {
		return accounts.Principal{}, true
	}
	if t := r.URL.Query().Get("ticket"); t != "" {
		if p, ok := g.acc.RedeemTicket(t); ok {
			return p, true
		}
		return accounts.Principal{}, !g.required
	}
	// A browser always sends Origin on a WebSocket; a cookie from another site's page must not
	// open the feed. An absent Origin is a non-browser client.
	if o := r.Header.Get("Origin"); o == "" || accounts.SameHost(o, r) {
		p, ok, err := g.acc.Authenticate(r)
		if err != nil {
			log.Error().Err(err).Msg("ws auth failed")
		}
		if ok {
			return p, true
		}
	}
	return accounts.Principal{}, !g.required
}

func (g *wsGate) track(c *Client) {
	if g != nil && g.reg != nil {
		g.reg.add(c)
	}
}

// stillValid re-checks a principal after track(), closing the window in which a logout could
// have run between admit and registration. Zero principals and nil gates are always valid.
func (g *wsGate) stillValid(p accounts.Principal) bool {
	if g == nil || g.acc == nil || p.UserID == 0 {
		return true
	}
	return g.acc.StillValid(p)
}

func (g *wsGate) untrack(c *Client) {
	if g != nil && g.reg != nil {
		g.reg.remove(c)
	}
}

// protect requires a session or extension code when AUTH_REQUIRED is on.
func (g *wsGate) protect(h http.Handler) http.Handler {
	if g == nil || g.acc == nil || !g.required {
		return h
	}
	return g.acc.RequireAuth(h)
}

// adminOnly requires an admin session (and same-site writes) when AUTH_REQUIRED is on.
func (g *wsGate) adminOnly(h http.Handler) http.Handler {
	if g == nil || g.acc == nil || !g.required {
		return h
	}
	return g.acc.RequireAdmin(accounts.CSRF(h))
}
