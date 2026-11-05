package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// Event is one arb-hub transition: an upsert (apply) or a removal (explicit or evicted).
type Event struct {
	Key     string // gateway oppStateKey
	Arb     Arb
	Removed bool
}

type enqueuer interface{ Enqueue(Job) }

// Options tune the notifier; zero values get defaults.
type Options struct {
	EditInterval   time.Duration         // TELEGRAM_EDIT_INTERVAL_S (default 10s)
	ReconcileEvery time.Duration         // default 30s
	StartupGrace   time.Duration         // no reconcile before this; EVICTION_TTL_S + 5s (default 125s)
	SendPause      time.Duration         // pause new sends after a permanent send failure (default 60s)
	Present        func(key string) bool // is the arb still in the gateway's hub? (hub.Has)
	Now            func() time.Time
}

// Status is GET /api/telegram/status.
type Status struct {
	Configured  bool   `json:"configured"`
	Enabled     bool   `json:"enabled"`
	PresetName  string `json:"preset_name"`
	Tracked     int64  `json:"tracked"`
	SentTotal   int64  `json:"sent_total"`
	EditsTotal  int64  `json:"edits_total"`
	LastSentAt  int64  `json:"last_sent_at"`
	LastError   string `json:"last_error"`
	LastErrorAt int64  `json:"last_error_at"`
}

type entryState int

const (
	stateSending entryState = iota // sendMessage in flight, no message id yet
	stateLive
	stateGone // delete (or GONE-edit fallback) queued
)

type entry struct {
	Tracked
	state   entryState
	removed bool   // a Remove arrived while the send was in flight
	dirty   bool   // a change waits for the edit interval
	text    string // text of the in-flight send
}

// Notifier owns the per-arb lifecycle. handle/onResult/tick/reconcile run only on the Run goroutine.
type Notifier struct {
	store  Store
	out    enqueuer
	render *Renderer
	opts   Options

	events  chan Event
	results chan Result

	cfgMu sync.RWMutex
	cfg   BotConfig

	seen       map[string]Arb // last-seen arb per key still in the hub (min-age / enable candidates)
	tracked    map[string]*entry
	startedAt  time.Time
	pauseUntil time.Time

	trackedN, sentN, editsN, errorsN, droppedN atomic.Int64

	stMu       sync.Mutex
	lastSentAt int64
	lastErr    string
	lastErrAt  int64
}

func NewNotifier(store Store, out enqueuer, render *Renderer, opts Options) *Notifier {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.EditInterval <= 0 {
		opts.EditInterval = 10 * time.Second
	}
	if opts.ReconcileEvery <= 0 {
		opts.ReconcileEvery = 30 * time.Second
	}
	if opts.StartupGrace <= 0 {
		opts.StartupGrace = 125 * time.Second
	}
	if opts.SendPause <= 0 {
		opts.SendPause = 60 * time.Second
	}
	if opts.Present == nil {
		opts.Present = func(string) bool { return true }
	}
	return &Notifier{
		store: store, out: out, render: render, opts: opts,
		events: make(chan Event, 1024), results: make(chan Result, 256),
		seen: map[string]Arb{}, tracked: map[string]*entry{}, startedAt: opts.Now(),
	}
}

// Observe hands an event to the notifier without ever blocking the hub; it drops on overflow.
func (n *Notifier) Observe(ev Event) {
	select {
	case n.events <- ev:
	default:
		n.droppedN.Add(1)
	}
}

// Deliver is the outbox result callback.
func (n *Notifier) Deliver(r Result) { n.results <- r }

// Load reads config and tracked messages. Call before Run.
func (n *Notifier) Load(ctx context.Context) error {
	cfg, err := n.store.LoadConfig(ctx)
	if err != nil {
		return fmt.Errorf("telegram config: %w", err)
	}
	cfg.Normalize()
	n.cfgMu.Lock()
	n.cfg = cfg
	n.cfgMu.Unlock()
	msgs, err := n.store.LoadMessages(ctx)
	if err != nil {
		return fmt.Errorf("telegram messages: %w", err)
	}
	now := n.opts.Now()
	for key, t := range msgs {
		if now.Sub(time.UnixMilli(t.SentAt)) > 48*time.Hour {
			_ = n.store.DeleteMessage(ctx, key)
			continue
		}
		n.tracked[key] = &entry{Tracked: t, state: stateLive}
	}
	n.trackedN.Store(int64(len(n.tracked)))
	n.startedAt = now
	return nil
}

// Run is the notifier's event loop.
func (n *Notifier) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	rec := time.NewTicker(n.opts.ReconcileEvery)
	defer rec.Stop()
	for {
		select {
		case ev := <-n.events:
			n.handle(ctx, ev, n.opts.Now())
		case r := <-n.results:
			n.onResult(ctx, r, n.opts.Now())
		case <-tick.C:
			n.tick(n.opts.Now())
		case <-rec.C:
			n.reconcile(n.opts.Now())
		case <-ctx.Done():
			return
		}
	}
}

func (n *Notifier) handle(_ context.Context, ev Event, now time.Time) {
	if ev.Removed {
		delete(n.seen, ev.Key)
		if e := n.tracked[ev.Key]; e != nil {
			n.goGone(ev.Key, e, now)
		}
		return
	}
	n.seen[ev.Key] = ev.Arb
	e := n.tracked[ev.Key]
	if e == nil {
		n.maybeSend(ev.Key, ev.Arb, now)
		return
	}
	if e.state == stateGone {
		return
	}
	if e.FirstDetectedAt != 0 && ev.Arb.firstSeen() != 0 && ev.Arb.firstSeen() != e.FirstDetectedAt {
		// The engine re-detected it: the old opportunity ended without a removal reaching us.
		n.goGone(ev.Key, e, now)
		return
	}
	e.Arb = ev.Arb
	if ev.Arb.ProfitPct > e.PeakProfit {
		e.PeakProfit = ev.Arb.ProfitPct
	}
	if e.state == stateLive {
		n.maybeEdit(ev.Key, e, now)
	}
}

func (n *Notifier) view(e *entry, now time.Time, gone bool) View {
	return View{Arb: e.Arb, Bankroll: e.Bankroll, PresetName: e.PresetName, Now: now, Gone: gone, GoneAt: now, PeakProfit: e.PeakProfit}
}

func (n *Notifier) maybeSend(key string, a Arb, now time.Time) {
	if now.Before(n.pauseUntil) {
		return
	}
	cfg := n.Config()
	if !cfg.Enabled || cfg.Preset == nil || !Matches(a, cfg.Filter, cfg.Bankroll, now.UnixMilli()) {
		return
	}
	e := &entry{
		Tracked: Tracked{FirstDetectedAt: a.firstSeen(), PeakProfit: a.ProfitPct, Arb: a, Bankroll: cfg.Bankroll, PresetName: cfg.Preset.Name},
		state:   stateSending,
	}
	e.text = n.render.Render(n.view(e, now, false))
	n.tracked[key] = e
	n.trackedN.Add(1)
	n.out.Enqueue(Job{Kind: JobSend, Key: key, Text: e.text})
}

func (n *Notifier) maybeEdit(key string, e *entry, now time.Time) {
	text := n.render.Render(n.view(e, now, false))
	sum := digest(text)
	if sum == e.LastTextSHA {
		e.dirty = false
		return
	}
	if now.Sub(time.UnixMilli(e.LastEditAt)) < n.opts.EditInterval {
		e.dirty = true
		return
	}
	e.dirty = false
	e.LastEditAt = now.UnixMilli()
	e.LastTextSHA = sum // optimistic; reset on failure
	n.out.Enqueue(Job{Kind: JobEdit, Key: key, MessageID: e.MessageID, Text: text})
}

func (n *Notifier) goGone(key string, e *entry, now time.Time) {
	switch e.state {
	case stateSending:
		e.removed = true
	case stateLive:
		e.state = stateGone
		n.out.Enqueue(Job{Kind: JobGone, Key: key, MessageID: e.MessageID, Text: n.render.Render(n.view(e, now, true))})
	}
}

func (n *Notifier) onResult(ctx context.Context, r Result, now time.Time) {
	key := r.Job.Key
	e := n.tracked[key]
	if e == nil {
		return
	}
	switch r.Job.Kind {
	case JobSend:
		if r.Err != nil {
			n.recordErr(r.Err, now)
			n.pauseUntil = now.Add(n.opts.SendPause)
			n.untrack(ctx, key, false)
			return
		}
		e.MessageID, e.SentAt = r.MessageID, now.UnixMilli()
		e.LastEditAt, e.LastTextSHA, e.text = e.SentAt, digest(e.text), ""
		e.state = stateLive
		n.sentN.Add(1)
		n.stMu.Lock()
		n.lastSentAt = e.SentAt
		n.stMu.Unlock()
		n.persist(ctx, key, e)
		if e.removed {
			n.goGone(key, e, now)
		}
	case JobEdit:
		if e.state != stateLive {
			return
		}
		if r.Err != nil {
			n.recordErr(r.Err, now)
			if isMessageGone(r.Err) {
				n.untrack(ctx, key, true)
				return
			}
			e.LastTextSHA, e.dirty = "", true
			return
		}
		n.editsN.Add(1)
		n.persist(ctx, key, e)
	case JobGone:
		if r.Err != nil {
			n.recordErr(r.Err, now)
		} else {
			n.editsN.Add(1)
		}
		n.untrack(ctx, key, true)
	}
}

// tick sends throttled edits whose interval has passed, and posts untracked arbs that now match
// (they reached min age, or the bot was just enabled).
func (n *Notifier) tick(now time.Time) {
	for key, e := range n.tracked {
		if e.state == stateLive && e.dirty {
			n.maybeEdit(key, e, now)
		}
	}
	for key, a := range n.seen {
		if _, ok := n.tracked[key]; !ok {
			n.maybeSend(key, a, now)
		}
	}
}

// reconcile closes messages whose arb the hub no longer holds (a dropped Remove, or a restart).
func (n *Notifier) reconcile(now time.Time) {
	if now.Sub(n.startedAt) < n.opts.StartupGrace {
		return
	}
	for key := range n.seen {
		if !n.opts.Present(key) {
			delete(n.seen, key)
		}
	}
	for key, e := range n.tracked {
		if e.state == stateLive && !n.opts.Present(key) {
			n.goGone(key, e, now)
		}
	}
}

func (n *Notifier) untrack(ctx context.Context, key string, stored bool) {
	delete(n.tracked, key)
	n.trackedN.Add(-1)
	if stored {
		if err := n.store.DeleteMessage(ctx, key); err != nil {
			log.Warn().Err(err).Str("key", key).Msg("telegram: deleting tracked message failed")
		}
	}
}

func (n *Notifier) persist(ctx context.Context, key string, e *entry) {
	if err := n.store.SaveMessage(ctx, key, e.Tracked); err != nil {
		log.Warn().Err(err).Str("key", key).Msg("telegram: saving tracked message failed")
	}
}

func (n *Notifier) recordErr(err error, now time.Time) {
	n.errorsN.Add(1)
	n.stMu.Lock()
	n.lastErr, n.lastErrAt = err.Error(), now.UnixMilli()
	n.stMu.Unlock()
	log.Warn().Err(err).Msg("telegram call failed")
}

// Config returns the current bot config.
func (n *Notifier) Config() BotConfig {
	n.cfgMu.RLock()
	defer n.cfgMu.RUnlock()
	return n.cfg
}

// SetConfig validates, stores and applies a config (future sends only).
func (n *Notifier) SetConfig(ctx context.Context, c BotConfig) (BotConfig, error) {
	c.Normalize()
	if err := c.Validate(); err != nil {
		return BotConfig{}, &ValidationError{Err: err}
	}
	c.UpdatedAt = n.opts.Now().UnixMilli()
	if err := n.store.SaveConfig(ctx, c); err != nil {
		return BotConfig{}, err
	}
	n.cfgMu.Lock()
	n.cfg = c
	n.cfgMu.Unlock()
	return c, nil
}

// Status reports counters for the popover.
func (n *Notifier) Status() Status {
	c := n.Config()
	n.stMu.Lock()
	defer n.stMu.Unlock()
	s := Status{
		Configured: true, Enabled: c.Enabled, Tracked: n.trackedN.Load(),
		SentTotal: n.sentN.Load(), EditsTotal: n.editsN.Load(),
		LastSentAt: n.lastSentAt, LastError: n.lastErr, LastErrorAt: n.lastErrAt,
	}
	if c.Preset != nil {
		s.PresetName = c.Preset.Name
	}
	return s
}

// SendTest posts the sample arb with a TEST prefix, bypassing the tracker.
func (n *Notifier) SendTest(ctx context.Context) (int64, error) {
	c := n.Config()
	now := n.opts.Now()
	bk := c.Bankroll
	if bk.Mode == "" {
		bk = DefaultBankroll
	}
	name := ""
	if c.Preset != nil {
		name = c.Preset.Name
	}
	text := n.render.Render(View{Arb: SampleArb(now), Bankroll: bk, PresetName: name, Now: now, Test: true})
	reply := make(chan Result, 1)
	n.out.Enqueue(Job{Kind: JobTest, Text: text, Reply: reply})
	select {
	case r := <-reply:
		if r.Err != nil {
			n.recordErr(r.Err, now)
		}
		return r.MessageID, r.Err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// WriteMetrics appends the bot's Prometheus counters to the gateway's /metrics.
func (n *Notifier) WriteMetrics(w io.Writer) {
	fmt.Fprintf(w, "gateway_telegram_sent_total %d\ngateway_telegram_edits_total %d\ngateway_telegram_errors_total %d\ngateway_telegram_events_dropped_total %d\ngateway_telegram_tracked %d\n",
		n.sentN.Load(), n.editsN.Load(), n.errorsN.Load(), n.droppedN.Load(), n.trackedN.Load())
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
