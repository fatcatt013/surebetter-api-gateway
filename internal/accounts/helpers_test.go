package accounts

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

const testPassword = "correct horse battery"

func newTestService(t *testing.T) (*Service, *fakeClock, *recConns) {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clk := newClock()
	s := NewService(db, clk.now)
	rec := &recConns{}
	s.SetConnections(rec)
	return s, clk, rec
}

// mustUser registers email with role through a fresh invite and returns its principal and session token.
func mustUser(t *testing.T, s *Service, email, role string) (Principal, string) {
	t.Helper()
	inv, err := s.CreateInvite(0, role)
	if err != nil {
		t.Fatal(err)
	}
	p, token, err := s.Register("10.0.0.1", inv.Code, email, testPassword)
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	return p, token
}

// recConns records what the service asked the WebSocket layer to do.
type recConns struct {
	mu                     sync.Mutex
	users, sessions, codes []int64
	states                 []string // "user:key:client"
}

func (r *recConns) CloseUser(id int64) { r.mu.Lock(); r.users = append(r.users, id); r.mu.Unlock() }
func (r *recConns) CloseSession(id int64) {
	r.mu.Lock()
	r.sessions = append(r.sessions, id)
	r.mu.Unlock()
}
func (r *recConns) CloseCode(id int64) { r.mu.Lock(); r.codes = append(r.codes, id); r.mu.Unlock() }
func (r *recConns) NotifyState(user int64, key string, except string) {
	r.mu.Lock()
	r.states = append(r.states, fmt.Sprintf("%d:%s:%s", user, key, except))
	r.mu.Unlock()
}
