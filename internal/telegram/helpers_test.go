package telegram

import (
	"context"
	"sync"
	"testing"
	"time"
)

// openFilter lets every arb through (the frontend's default FilterState).
func openFilter() Filter {
	return Filter{LiveState: "all", BlockedMarkets: map[string][]string{}}
}

// validConfig is an enabled config that passes Validate.
func validConfig() BotConfig {
	return BotConfig{
		Enabled:  true,
		Preset:   &PresetRef{ID: "p1", Name: "Live tennis >2%"},
		Filter:   openFilter(),
		Bankroll: DefaultBankroll,
	}
}

// fakeClock: now() is fixed until sleep() advances it, so rate limits and backoff are instant and observable.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	c.slept = append(c.slept, d)
	return nil
}

// scriptedClient returns the queued errors in order (nil = success) and records calls.
type scriptedClient struct {
	mu    sync.Mutex
	errs  []error
	calls []string
	next  int64
}

func (s *scriptedClient) pop() error {
	if len(s.errs) == 0 {
		return nil
	}
	e := s.errs[0]
	s.errs = s.errs[1:]
	return e
}

func (s *scriptedClient) Send(_ context.Context, text string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "send:"+text)
	if err := s.pop(); err != nil {
		return 0, err
	}
	s.next++
	return s.next, nil
}

func (s *scriptedClient) Edit(_ context.Context, id int64, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "edit:"+text)
	return s.pop()
}

func (s *scriptedClient) Delete(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "delete")
	return s.pop()
}

type memStore struct {
	cfg  BotConfig
	msgs map[string]Tracked
}

func newMemStore() *memStore { return &memStore{msgs: map[string]Tracked{}} }

func (m *memStore) LoadConfig(context.Context) (BotConfig, error)   { return m.cfg, nil }
func (m *memStore) SaveConfig(_ context.Context, c BotConfig) error { m.cfg = c; return nil }
func (m *memStore) LoadMessages(context.Context) (map[string]Tracked, error) {
	out := map[string]Tracked{}
	for k, v := range m.msgs {
		out[k] = v
	}
	return out, nil
}
func (m *memStore) SaveMessage(_ context.Context, k string, t Tracked) error {
	m.msgs[k] = t
	return nil
}
func (m *memStore) DeleteMessage(_ context.Context, k string) error { delete(m.msgs, k); return nil }

// fakeOut records jobs; a Reply job is answered immediately with message id 99.
type fakeOut struct{ jobs []Job }

func (f *fakeOut) Enqueue(j Job) {
	if j.Reply != nil {
		j.Reply <- Result{Job: j, MessageID: 99}
		return
	}
	f.jobs = append(f.jobs, j)
}

func (f *fakeOut) take() []Job { j := f.jobs; f.jobs = nil; return j }

// withOdds copies the arb with new leg odds (the stakes slice is copied, not shared).
func withOdds(a Arb, odds ...float64) Arb {
	a.Stakes = append([]Stake(nil), a.Stakes...)
	for i, o := range odds {
		a.Stakes[i].DecimalOdds = o
	}
	return a
}

// newTestNotifier: enabled open config, fake outbox, everything "present" unless overridden.
func newTestNotifier(t *testing.T, store *memStore, present map[string]bool) (*Notifier, *fakeOut) {
	t.Helper()
	if store.cfg.Preset == nil {
		store.cfg = validConfig()
	}
	out := &fakeOut{}
	opts := Options{EditInterval: 10 * time.Second, StartupGrace: 125 * time.Second, SendPause: 60 * time.Second, GoneDeleteAfter: 5 * time.Minute}
	if present != nil {
		opts.Present = func(k string) bool { return present[k] }
	}
	n := NewNotifier(store, out, newTestRenderer(t), opts)
	if err := n.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return n, out
}
