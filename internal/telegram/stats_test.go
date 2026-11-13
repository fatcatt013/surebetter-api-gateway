package telegram

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRecorder keeps every stats call in order.
type fakeRecorder struct {
	sent   []SentArb
	ends   []SentEnd
	sweeps [][]int64
	calls  []string
}

func (f *fakeRecorder) RecordSent(s SentArb) {
	f.sent = append(f.sent, s)
	f.calls = append(f.calls, "sent")
}
func (f *fakeRecorder) RecordEnd(e SentEnd) {
	f.ends = append(f.ends, e)
	f.calls = append(f.calls, "end")
}
func (f *fakeRecorder) RecordSweep(keep []int64) {
	f.sweeps = append(f.sweeps, keep)
	f.calls = append(f.calls, "sweep")
}

// newStatsNotifier is newTestNotifier with a stats recorder and a clock fixed at t0 for Load.
func newStatsNotifier(t *testing.T, st *memStore, present map[string]bool) (*Notifier, *fakeOut, *fakeRecorder) {
	t.Helper()
	if st.cfg.Preset == nil {
		st.cfg = validConfig()
	}
	out, rec := &fakeOut{}, &fakeRecorder{}
	opts := Options{EditInterval: 10 * time.Second, StartupGrace: 125 * time.Second, SendPause: 60 * time.Second,
		GoneDeleteAfter: 5 * time.Minute, Stats: rec, Now: t0}
	if present != nil {
		opts.Present = func(k string) bool { return present[k] }
	}
	n := NewNotifier(st, out, newTestRenderer(t), opts)
	if err := n.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return n, out, rec
}

func TestStats_OneRowPerSentMessageWithTheMessagesMoney(t *testing.T) {
	st := newMemStore()
	st.cfg = validConfig()
	st.cfg.UpdatedBy = 3
	n, out, rec := newStatsNotifier(t, st, nil)
	now := t0()
	a := SampleArb(now)
	n.handle(bg, upsert("k", a), now)
	j := onlyJob(t, out, JobSend)
	if len(rec.sent) != 0 {
		t.Fatalf("recorded before the send succeeded: %+v", rec.sent)
	}
	n.onResult(bg, Result{Job: j, MessageID: 42}, now.Add(time.Second))
	want := SentArb{
		MessageID: 42, SentAt: now.Add(time.Second).UnixMilli(), UserID: 3, ArbKey: "k", FirstDetectedAt: a.FirstDetectedAt,
		Sport: "tennis", IsLive: true, MarketType: "GAME_HANDICAP", Line: 3.5, ProfitPct: 2.96, ShownProfitPct: 2.96,
		Currency: "CZK", TotalStake: 10000, Payout: 10296, Profit: 296,
		Legs: []SentLeg{
			{Bookmaker: "betano", OutcomeID: "cody benton:+3.5", Odds: 2.10, Stake: 4903},
			{Bookmaker: "tipsport", OutcomeID: "cody benton:-3.5", Odds: 2.02, Stake: 5097},
		},
	}
	if len(rec.sent) != 1 || !reflect.DeepEqual(rec.sent[0], want) {
		t.Fatalf("sent\n got %+v\nwant %+v", rec.sent, want)
	}
	// The message text says the same money.
	if !strings.Contains(j.Text, "10 000 CZK → 10 296 CZK") {
		t.Fatalf("message money differs from the row: %s", j.Text)
	}

	// Edits add no row.
	b := withOdds(a, 2.20, 2.02)
	n.handle(bg, upsert("k", b), now.Add(20*time.Second))
	e := onlyJob(t, out, JobEdit)
	n.onResult(bg, Result{Job: e, MessageID: 42}, now.Add(21*time.Second))
	if len(rec.sent) != 1 || len(rec.ends) != 0 {
		t.Fatalf("edit recorded something: %+v %+v", rec.sent, rec.ends)
	}
}

func TestStats_RowUsesTheArbAsRenderedNotALaterUpdate(t *testing.T) {
	n, out, rec := newStatsNotifier(t, newMemStore(), nil)
	now := t0()
	a := SampleArb(now)
	n.handle(bg, upsert("k", a), now)
	j := onlyJob(t, out, JobSend)
	n.handle(bg, upsert("k", withOdds(a, 2.50, 2.02)), now.Add(time.Second)) // arrives while the send is in flight
	n.onResult(bg, Result{Job: j, MessageID: 5}, now.Add(2*time.Second))
	if len(rec.sent) != 1 || rec.sent[0].Legs[0].Odds != 2.10 || rec.sent[0].Payout != 10296 {
		t.Fatalf("row should be the sent text's arb: %+v", rec.sent)
	}
}

func TestStats_FailedSendRecordsNothingAndTheResendOneRow(t *testing.T) {
	n, out, rec := newStatsNotifier(t, newMemStore(), nil)
	now := t0()
	a := SampleArb(now)
	n.handle(bg, upsert("k", a), now)
	j := onlyJob(t, out, JobSend)
	n.onResult(bg, Result{Job: j, Err: &APIError{Code: 400, Description: "Bad Request: chat not found"}}, now)
	if len(rec.sent) != 0 || len(rec.ends) != 0 {
		t.Fatalf("failed send recorded: %+v %+v", rec.sent, rec.ends)
	}
	later := now.Add(2 * time.Minute) // past the send pause
	n.tick(later)
	j = onlyJob(t, out, JobSend)
	n.onResult(bg, Result{Job: j, MessageID: 8}, later)
	if len(rec.sent) != 1 || rec.sent[0].MessageID != 8 {
		t.Fatalf("resend: %+v", rec.sent)
	}
}

func TestStats_EndReasons(t *testing.T) {
	now := t0()
	at := now.Add(3 * time.Minute)
	cases := []struct {
		name string
		end  func(n *Notifier, a Arb)
		want SentEnd
	}{
		{"removed", func(n *Notifier, _ Arb) { n.handle(bg, remove("k"), at) }, SentEnd{MessageID: 1, EndedAt: at.UnixMilli(), Reason: EndRemoved}},
		{"evicted", func(n *Notifier, _ Arb) { n.handle(bg, Event{Key: "k", Removed: true, Evicted: true}, at) },
			SentEnd{MessageID: 1, EndedAt: at.UnixMilli(), Reason: EndEvicted}},
		{"redetected", func(n *Notifier, a Arb) {
			b := a
			b.FirstDetectedAt += 90_000
			n.handle(bg, upsert("k", b), at)
		}, SentEnd{MessageID: 1, EndedAt: at.UnixMilli(), Reason: EndRedetected}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, out, rec := newStatsNotifier(t, newMemStore(), nil)
			a := SampleArb(now)
			sendLive(t, n, out, "k", a, now, 1)
			c.end(n, a)
			if len(rec.ends) != 1 || rec.ends[0] != c.want {
				t.Fatalf("ends %+v, want %+v", rec.ends, c.want)
			}
			// The GONE edit and the later delete add no second end.
			g := onlyJob(t, out, JobGone)
			n.onResult(bg, Result{Job: g, MessageID: 1}, at)
			n.tick(at.Add(6 * time.Minute)) // the redetected arb is also posted anew here
			for _, d := range out.take() {
				if d.Kind == JobDelete {
					n.onResult(bg, Result{Job: d, MessageID: 1}, at.Add(6*time.Minute))
				}
			}
			if len(rec.ends) != 1 {
				t.Fatalf("ended twice: %+v", rec.ends)
			}
		})
	}
}

func TestStats_ReconcileAbsentEnds(t *testing.T) {
	present := map[string]bool{"k": true}
	n, out, rec := newStatsNotifier(t, newMemStore(), present)
	now := t0()
	sendLive(t, n, out, "k", SampleArb(now), now, 1)
	present["k"] = false
	at := now.Add(3 * time.Minute)
	n.reconcile(at)
	if len(rec.ends) != 1 || rec.ends[0] != (SentEnd{MessageID: 1, EndedAt: at.UnixMilli(), Reason: EndAbsent}) {
		t.Fatalf("ends %+v", rec.ends)
	}
}

func TestStats_RemovedWhileSendingEndsAtTheRemoval(t *testing.T) {
	n, out, rec := newStatsNotifier(t, newMemStore(), nil)
	now := t0()
	n.handle(bg, upsert("k", SampleArb(now)), now)
	j := onlyJob(t, out, JobSend)
	n.handle(bg, remove("k"), now.Add(time.Second))
	n.onResult(bg, Result{Job: j, MessageID: 9}, now.Add(3*time.Second))
	if !reflect.DeepEqual(rec.calls, []string{"sweep", "sent", "end"}) {
		t.Fatalf("calls %v", rec.calls)
	}
	if rec.ends[0] != (SentEnd{MessageID: 9, EndedAt: now.Add(time.Second).UnixMilli(), Reason: EndRemoved}) {
		t.Fatalf("end %+v", rec.ends[0])
	}
}

func TestStats_MessageDeletedByHandEndsUnknown(t *testing.T) {
	n, out, rec := newStatsNotifier(t, newMemStore(), nil)
	now := t0()
	a := SampleArb(now)
	sendLive(t, n, out, "k", a, now, 1)
	n.handle(bg, upsert("k", withOdds(a, 2.20, 2.02)), now.Add(20*time.Second))
	e := onlyJob(t, out, JobEdit)
	n.onResult(bg, Result{Job: e, Err: &APIError{Code: 400, Description: "Bad Request: message to edit not found"}}, now.Add(21*time.Second))
	if len(rec.ends) != 1 || rec.ends[0] != (SentEnd{MessageID: 1, Reason: EndUnknown}) {
		t.Fatalf("ends %+v", rec.ends)
	}
}

func TestStats_LoadSweepsAllButTrackedMessages(t *testing.T) {
	now := t0()
	a := SampleArb(now)
	st := newMemStore()
	st.cfg = validConfig()
	st.msgs["k"] = Tracked{MessageID: 77, SentAt: now.UnixMilli(), FirstDetectedAt: a.firstSeen(), Arb: a, Bankroll: DefaultBankroll}
	st.msgs["gone:70"] = Tracked{MessageID: 70, SentAt: now.UnixMilli(), GoneAt: now.UnixMilli()}
	_, _, rec := newStatsNotifier(t, st, nil)
	if !reflect.DeepEqual(rec.sweeps, [][]int64{{77}}) {
		t.Fatalf("sweeps %v, want [[77]]", rec.sweeps)
	}
}

func TestStats_RestoredMessageEndsUnknownWhenFoundAbsent(t *testing.T) {
	now := t0()
	a := SampleArb(now)
	st := newMemStore()
	st.cfg = validConfig()
	st.msgs["k"] = Tracked{MessageID: 77, SentAt: now.UnixMilli(), FirstDetectedAt: a.firstSeen(), Arb: a, Bankroll: DefaultBankroll}
	present := map[string]bool{}
	n, _, rec := newStatsNotifier(t, st, present)
	// After a restart, absence says only that the arb ended at some point during the downtime.
	n.reconcile(now.Add(3 * time.Minute))
	if len(rec.ends) != 1 || rec.ends[0] != (SentEnd{MessageID: 77, Reason: EndUnknown}) {
		t.Fatalf("ends %+v", rec.ends)
	}
}

func TestStats_RestoredMessageRemovedHasAKnownEnd(t *testing.T) {
	now := t0()
	a := SampleArb(now)
	st := newMemStore()
	st.cfg = validConfig()
	st.msgs["k"] = Tracked{MessageID: 77, SentAt: now.UnixMilli(), FirstDetectedAt: a.firstSeen(), Arb: a, Bankroll: DefaultBankroll}
	n, _, rec := newStatsNotifier(t, st, nil)
	at := now.Add(time.Minute)
	n.handle(bg, upsert("k", a), now.Add(time.Second)) // the arb is still alive after the restart
	n.handle(bg, remove("k"), at)
	if len(rec.ends) != 1 || rec.ends[0] != (SentEnd{MessageID: 77, EndedAt: at.UnixMilli(), Reason: EndRemoved}) {
		t.Fatalf("ends %+v", rec.ends)
	}
}

func TestStats_NoRecorderIsFine(t *testing.T) {
	n, out := newTestNotifier(t, newMemStore(), nil)
	now := t0()
	sendLive(t, n, out, "k", SampleArb(now), now, 1)
	n.handle(bg, remove("k"), now.Add(time.Minute))
	onlyJob(t, out, JobGone)
}

// ── StatsWriter ──────────────────────────────────────────────────────────────

type fakeSink struct {
	mu    sync.Mutex
	calls []string
	fail  bool
	block chan struct{} // when set, every call waits for it
}

func (s *fakeSink) note(c string) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	if s.fail {
		return errors.New("disk full")
	}
	return nil
}
func (s *fakeSink) InsertSent(a SentArb) error { return s.note("insert") }
func (s *fakeSink) EndSent(e SentEnd) error    { return s.note("end:" + e.Reason) }
func (s *fakeSink) SweepSent(keep []int64) error {
	return s.note("sweep")
}
func (s *fakeSink) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStatsWriter_WritesInOrderAndSurvivesErrors(t *testing.T) {
	sink := &fakeSink{fail: true}
	w := NewStatsWriter(sink, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	w.RecordSweep([]int64{1})
	w.RecordSent(SentArb{MessageID: 1})
	w.RecordEnd(SentEnd{MessageID: 1, Reason: EndRemoved})
	want := []string{"sweep", "insert", "end:removed"}
	waitFor(t, func() bool { return len(sink.got()) == 3 })
	if !reflect.DeepEqual(sink.got(), want) {
		t.Fatalf("calls %v, want %v", sink.got(), want)
	}
	if w.Errors() != 3 {
		t.Fatalf("errors %d, want 3", w.Errors())
	}
}

func TestStatsWriter_NeverBlocksWhenFull(t *testing.T) {
	sink := &fakeSink{block: make(chan struct{})}
	w := NewStatsWriter(sink, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			w.RecordSent(SentArb{MessageID: int64(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RecordSent blocked on a stuck sink")
	}
	if w.Dropped() == 0 {
		t.Fatal("expected drops with a stuck sink and a buffer of 2")
	}
	close(sink.block)
}

func TestStats_RestoredMessageRedetectedEndsUnknown(t *testing.T) {
	now := t0()
	a := SampleArb(now)
	st := newMemStore()
	st.cfg = validConfig()
	st.msgs["k"] = Tracked{MessageID: 77, SentAt: now.UnixMilli(), FirstDetectedAt: a.firstSeen(), Arb: a, Bankroll: DefaultBankroll}
	n, _, rec := newStatsNotifier(t, st, nil)
	b := a
	b.FirstDetectedAt += 90_000 // the old one ended during the downtime, when exactly is unknown
	n.handle(bg, upsert("k", b), now.Add(time.Second))
	if len(rec.ends) != 1 || rec.ends[0] != (SentEnd{MessageID: 77, Reason: EndUnknown}) {
		t.Fatalf("ends %+v", rec.ends)
	}
}

func TestStatsWriter_MetricsOnTheNotifiersMetrics(t *testing.T) {
	w := NewStatsWriter(&fakeSink{}, 1)
	w.RecordSent(SentArb{})
	w.RecordSent(SentArb{}) // no Run: the second is dropped
	n := NewNotifier(newMemStore(), &fakeOut{}, newTestRenderer(t), Options{Stats: w})
	var b strings.Builder
	n.WriteMetrics(&b)
	if !strings.Contains(b.String(), "gateway_telegram_stats_dropped_total 1\n") || !strings.Contains(b.String(), "gateway_telegram_stats_errors_total 0\n") {
		t.Fatalf("metrics:\n%s", b.String())
	}
}
