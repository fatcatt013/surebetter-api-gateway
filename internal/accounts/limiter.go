package accounts

import (
	"sync"
	"time"
)

// limiter counts failures per key (an IP or an email). The max-th failure within window locks
// the key for lock. State is in memory: one gateway instance, and a restart forgetting locks is fine.
type limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	lock    time.Duration
	now     func() time.Time
	entries map[string]*limitEntry
}

type limitEntry struct {
	fails       []time.Time
	lockedUntil time.Time
}

func newLimiter(max int, window, lock time.Duration, now func() time.Time) *limiter {
	return &limiter{max: max, window: window, lock: lock, now: now, entries: map[string]*limitEntry{}}
}

// check reports whether key may try now and, when locked, how long until it may.
func (l *limiter) check(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return true, 0
	}
	if now := l.now(); now.Before(e.lockedUntil) {
		return false, e.lockedUntil.Sub(now)
	}
	return true, 0
}

// fail records a failure for key.
func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil {
		if len(l.entries) >= 10000 {
			l.prune(now)
		}
		e = &limitEntry{}
		l.entries[key] = e
	}
	kept := e.fails[:0]
	for _, t := range e.fails {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	e.fails = append(kept, now)
	if len(e.fails) >= l.max {
		e.lockedUntil = now.Add(l.lock)
		e.fails = nil
	}
}

// reset forgets key's failures (after a successful login).
func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// prune drops entries that are neither locked nor holding a failure inside the window.
func (l *limiter) prune(now time.Time) {
	for k, e := range l.entries {
		recent := len(e.fails) > 0 && now.Sub(e.fails[len(e.fails)-1]) < l.window
		if !recent && !now.Before(e.lockedUntil) {
			delete(l.entries, k)
		}
	}
}
