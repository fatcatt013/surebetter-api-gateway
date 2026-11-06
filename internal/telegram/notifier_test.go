package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var bg = context.Background()

func t0() time.Time { return time.Date(2026, 11, 4, 13, 0, 0, 0, time.UTC) }

func upsert(key string, a Arb) Event { return Event{Key: key, Arb: a} }
func remove(key string) Event        { return Event{Key: key, Removed: true} }

func onlyJob(t *testing.T, out *fakeOut, kind JobKind) Job {
	t.Helper()
	jobs := out.take()
	if len(jobs) != 1 || jobs[0].Kind != kind {
		t.Fatalf("jobs = %+v, want exactly one of kind %d", jobs, kind)
	}
	return jobs[0]
}

func noJobs(t *testing.T, out *fakeOut) {
	t.Helper()
	if jobs := out.take(); len(jobs) != 0 {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
}

// sendLive posts key and acknowledges it as message id.
func sendLive(t *testing.T, n *Notifier, out *fakeOut, key string, a Arb, now time.Time, id int64) {
	t.Helper()
	n.handle(bg, upsert(key, a), now)
	j := onlyJob(t, out, JobSend)
	n.onResult(bg, Result{Job: j, MessageID: id}, now)
}

func TestNotifier_SendsOnFirstMatchAndPersists(t *testing.T) {
	st := newMemStore()
	n, out := newTestNotifier(t, st, nil)
	now := t0()
	n.handle(bg, upsert("k", SampleArb(now)), now)
	j := onlyJob(t, out, JobSend)
	if !strings.Contains(j.Text, "Cody Benton") || j.Key != "k" {
		t.Fatalf("send job %+v", j)
	}
	n.onResult(bg, Result{Job: j, MessageID: 42}, now)
	if st.msgs["k"].MessageID != 42 || n.Status().Tracked != 1 || n.Status().SentTotal != 1 {
		t.Fatalf("not persisted/tracked: %+v %+v", st.msgs, n.Status())
	}
	n.handle(bg, upsert("k", SampleArb(now)), now.Add(time.Second))
	noJobs(t, out) // same text → nothing
}

func TestNotifier_DisabledSendsNothing(t *testing.T) {
	st := newMemStore()
	st.cfg = validConfig()
	st.cfg.Enabled = false
	n, out := newTestNotifier(t, st, nil)
	n.handle(bg, upsert("k", SampleArb(t0())), t0())
	n.tick(t0().Add(time.Second))
	noJobs(t, out)
}

func TestNotifier_TooYoungThenTickSends(t *testing.T) {
	st := newMemStore()
	st.cfg = validConfig()
	st.cfg.Filter.MinAgeSec = 60
	n, out := newTestNotifier(t, st, nil)
	now := t0()
	a := SampleArb(now) // first seen 38s ago
	n.handle(bg, upsert("k", a), now)
	noJobs(t, out)
	n.tick(now.Add(10 * time.Second)) // 48s old
	noJobs(t, out)
	n.tick(now.Add(22 * time.Second)) // 60s old
	onlyJob(t, out, JobSend)
}

func TestNotifier_EditThrottledThenSentByTick(t *testing.T) {
	n, out := newTestNotifier(t, newMemStore(), nil)
	now := t0()
	a := SampleArb(now)
	sendLive(t, n, out, "k", a, now, 1)

	n.handle(bg, upsert("k", withOdds(a, 2.15, 2.02)), now.Add(3*time.Second))
	noJobs(t, out) // within the 10s edit interval
	n.tick(now.Add(9 * time.Second))
	noJobs(t, out)
	n.tick(now.Add(10 * time.Second))
	j := onlyJob(t, out, JobEdit)
	if j.MessageID != 1 || !strings.Contains(j.Text, "2.15") {
		t.Fatalf("edit job %+v", j)
	}
	n.onResult(bg, Result{Job: j, MessageID: 1}, now.Add(10*time.Second))
	n.handle(bg, upsert("k", withOdds(a, 2.15, 2.02)), now.Add(30*time.Second))
	noJobs(t, out) // unchanged text
}

func TestNotifier_GoneIgnoresThrottleAndUntracks(t *testing.T) {
	st := newMemStore()
	n, out := newTestNotifier(t, st, nil)
	now := t0()
	sendLive(t, n, out, "k", SampleArb(now), now, 1)
	n.handle(bg, remove("k"), now.Add(time.Second))
	j := onlyJob(t, out, JobGone)
	if !strings.Contains(j.Text, "GONE") || j.MessageID != 1 {
		t.Fatalf("gone job %+v", j)
	}
	n.onResult(bg, Result{Job: j, MessageID: 1}, now.Add(2*time.Second))
	if _, ok := st.msgs["k"]; ok || n.Status().Tracked != 0 {
		t.Fatal("gone message must be untracked and deleted from the store")
	}
	if f, ok := st.msgs["gone:1"]; !ok || f.MessageID != 1 {
		t.Fatalf("the pending delete must be stored, got %+v", st.msgs)
	}
}

func TestNotifier_GoneMessageDeletedAfterDelay(t *testing.T) {
	st := newMemStore()
	n, out := newTestNotifier(t, st, nil)
	now := t0()
	sendLive(t, n, out, "k", SampleArb(now), now, 1)
	n.handle(bg, remove("k"), now.Add(time.Second))
	n.onResult(bg, Result{Job: onlyJob(t, out, JobGone), MessageID: 1}, now.Add(2*time.Second))
	n.tick(now.Add(4 * time.Minute))
	noJobs(t, out)
	n.tick(now.Add(5*time.Minute + 2*time.Second))
	d := onlyJob(t, out, JobDelete)
	if d.MessageID != 1 {
		t.Fatalf("delete job %+v", d)
	}
	n.tick(now.Add(5*time.Minute + 3*time.Second))
	noJobs(t, out) // queued once
	n.onResult(bg, Result{Job: d}, now.Add(5*time.Minute+4*time.Second))
	if len(st.msgs) != 0 {
		t.Fatalf("store must be empty after the delete, got %+v", st.msgs)
	}
}

func TestNotifier_PendingDeleteSurvivesRestart(t *testing.T) {
	now := t0()
	st := newMemStore()
	st.cfg = validConfig()
	st.msgs["gone:9"] = Tracked{MessageID: 9, SentAt: now.UnixMilli(), GoneAt: now.UnixMilli()}
	n, out := newTestNotifier(t, st, nil)
	if n.Status().Tracked != 0 {
		t.Fatal("a pending delete is not a tracked arb")
	}
	n.tick(now.Add(5 * time.Minute))
	if d := onlyJob(t, out, JobDelete); d.MessageID != 9 {
		t.Fatalf("delete job %+v", d)
	}
}

func TestNotifier_ZeroDelayDeletesAtOnce(t *testing.T) {
	st := newMemStore()
	n, out := newTestNotifier(t, st, nil)
	n.opts.GoneDeleteAfter = 0
	now := t0()
	sendLive(t, n, out, "k", SampleArb(now), now, 1)
	n.handle(bg, remove("k"), now.Add(time.Second))
	d := onlyJob(t, out, JobDelete)
	n.onResult(bg, Result{Job: d}, now.Add(2*time.Second))
	if len(st.msgs) != 0 || n.Status().Tracked != 0 {
		t.Fatalf("store %+v tracked %d", st.msgs, n.Status().Tracked)
	}
}

func TestNotifier_RemoveWhileSendingGoesGoneAfterSend(t *testing.T) {
	n, out := newTestNotifier(t, newMemStore(), nil)
	now := t0()
	n.handle(bg, upsert("k", SampleArb(now)), now)
	send := onlyJob(t, out, JobSend)
	n.handle(bg, remove("k"), now.Add(time.Second))
	noJobs(t, out)
	n.onResult(bg, Result{Job: send, MessageID: 5}, now.Add(2*time.Second))
	if j := onlyJob(t, out, JobGone); j.MessageID != 5 {
		t.Fatalf("gone job %+v", j)
	}
}

func TestNotifier_SendFailurePausesNewSends(t *testing.T) {
	n, out := newTestNotifier(t, newMemStore(), nil)
	now := t0()
	n.handle(bg, upsert("k", SampleArb(now)), now)
	send := onlyJob(t, out, JobSend)
	n.onResult(bg, Result{Job: send, Err: &APIError{Code: 403, Description: "bot was kicked"}}, now)
	if n.Status().LastError == "" || n.Status().Tracked != 0 {
		t.Fatalf("status %+v", n.Status())
	}
	n.handle(bg, upsert("k", SampleArb(now)), now.Add(10*time.Second))
	noJobs(t, out)
	n.handle(bg, upsert("k", SampleArb(now)), now.Add(61*time.Second))
	onlyJob(t, out, JobSend)
}

func TestNotifier_DisableKeepsLifecycle(t *testing.T) {
	n, out := newTestNotifier(t, newMemStore(), nil)
	now := t0()
	sendLive(t, n, out, "k", SampleArb(now), now, 1)
	off := validConfig()
	off.Enabled = false
	if _, err := n.SetConfig(bg, off); err != nil {
		t.Fatal(err)
	}
	n.handle(bg, remove("k"), now.Add(time.Second))
	onlyJob(t, out, JobGone)
}

func TestNotifier_ReconcileAfterGrace(t *testing.T) {
	present := map[string]bool{"k": true}
	n, out := newTestNotifier(t, newMemStore(), present)
	now := n.startedAt
	sendLive(t, n, out, "k", SampleArb(now), now, 1)
	present["k"] = false
	n.reconcile(now.Add(30 * time.Second)) // inside the 125s grace
	noJobs(t, out)
	n.reconcile(now.Add(126 * time.Second))
	onlyJob(t, out, JobGone)
}

func TestNotifier_RestartResumesStoredMessage(t *testing.T) {
	now := t0()
	a := SampleArb(now)
	st := newMemStore()
	st.cfg = validConfig()
	st.msgs["k"] = Tracked{MessageID: 77, SentAt: now.UnixMilli(), LastEditAt: now.UnixMilli(), FirstDetectedAt: a.firstSeen(), Arb: a, Bankroll: DefaultBankroll, PresetName: "p"}
	n, out := newTestNotifier(t, st, nil)
	n.handle(bg, upsert("k", a), now.Add(time.Second))
	noJobs(t, out) // same opportunity: no new message (text hash unknown → treated as changed but throttled)
	n.tick(now.Add(11 * time.Second))
	if j := onlyJob(t, out, JobEdit); j.MessageID != 77 {
		t.Fatalf("resumed edit %+v", j)
	}
}

func TestNotifier_RedetectionClosesOldMessage(t *testing.T) {
	n, out := newTestNotifier(t, newMemStore(), nil)
	now := t0()
	a := SampleArb(now)
	sendLive(t, n, out, "k", a, now, 1)
	b := a
	b.FirstDetectedAt = a.FirstDetectedAt + 90_000
	n.handle(bg, upsert("k", b), now.Add(2*time.Minute))
	g := onlyJob(t, out, JobGone)
	n.onResult(bg, Result{Job: g, MessageID: 1}, now.Add(2*time.Minute))
	n.handle(bg, upsert("k", b), now.Add(2*time.Minute+3*time.Second))
	onlyJob(t, out, JobSend)
}

func TestNotifier_EditOnDeletedMessageUntracks(t *testing.T) {
	st := newMemStore()
	n, out := newTestNotifier(t, st, nil)
	now := t0()
	a := SampleArb(now)
	sendLive(t, n, out, "k", a, now, 1)
	n.handle(bg, upsert("k", withOdds(a, 2.2, 2.0)), now.Add(11*time.Second))
	j := onlyJob(t, out, JobEdit)
	n.onResult(bg, Result{Job: j, Err: &APIError{Code: 400, Description: "Bad Request: message to edit not found"}}, now.Add(12*time.Second))
	if _, ok := st.msgs["k"]; ok || n.Status().Tracked != 0 {
		t.Fatal("deleted message must be untracked")
	}
}

func TestNotifier_ObserveDropsWhenFull(t *testing.T) {
	n, _ := newTestNotifier(t, newMemStore(), nil)
	for i := 0; i < cap(n.events)+5; i++ {
		n.Observe(remove("x"))
	}
	if n.droppedN.Load() != 5 {
		t.Fatalf("dropped = %d, want 5", n.droppedN.Load())
	}
}

func TestNotifier_SetConfigValidates(t *testing.T) {
	n, _ := newTestNotifier(t, newMemStore(), nil)
	bad := validConfig()
	bad.Bankroll.Currency = "USD"
	_, err := n.SetConfig(bg, bad)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
}

func TestNotifier_SendTestUsesSampleAndPrefix(t *testing.T) {
	n, _ := newTestNotifier(t, newMemStore(), nil)
	id, err := n.SendTest(bg)
	if err != nil || id != 99 {
		t.Fatalf("SendTest = %d, %v", id, err)
	}
}

// An ambiguous send (timeout) may have posted a message whose id we never got. The next
// successful send bounds the gap; every id in it is probed with a GONE edit.
func TestNotifier_AmbiguousSendProbesGapAndFadesAcceptedOrphans(t *testing.T) {
	st := newMemStore()
	n, out := newTestNotifier(t, st, nil)
	now := t0()
	sendLive(t, n, out, "a", SampleArb(now), now, 10)

	n.handle(bg, upsert("b", SampleArb(now)), now)
	send := onlyJob(t, out, JobSend)
	n.onResult(bg, Result{Job: send, Err: errors.New("context deadline exceeded")}, now)
	noJobs(t, out)

	// After the pause "b" is sent again and lands at 13: ids 11 and 12 may be orphans.
	n.tick(now.Add(61 * time.Second))
	resend := onlyJob(t, out, JobSend)
	n.onResult(bg, Result{Job: resend, MessageID: 13}, now.Add(61*time.Second))
	jobs := out.take()
	if len(jobs) != 2 || jobs[0].Kind != JobOrphan || jobs[0].MessageID != 11 || jobs[1].MessageID != 12 {
		t.Fatalf("probe jobs %+v, want orphan probes for 11 and 12", jobs)
	}
	if !strings.Contains(jobs[0].Text, "GONE") {
		t.Fatalf("probe text %q, want a GONE render", jobs[0].Text)
	}

	// 11 was ours (edit accepted), 12 someone else's (rejected).
	n.onResult(bg, Result{Job: jobs[0], MessageID: 11}, now.Add(62*time.Second))
	n.onResult(bg, Result{Job: jobs[1], Err: &APIError{Code: 400, Description: "Bad Request: message can't be edited"}}, now.Add(62*time.Second))
	if _, ok := st.msgs["gone:11"]; !ok {
		t.Fatalf("accepted orphan must fade like a GONE message, store %+v", st.msgs)
	}
	if _, ok := st.msgs["gone:12"]; ok {
		t.Fatal("a rejected probe must leave the message alone")
	}
	if n.Status().LastError != "context deadline exceeded" {
		t.Fatalf("a rejected probe is not an error: %q", n.Status().LastError)
	}
	n.tick(now.Add(62*time.Second + 5*time.Minute))
	if d := onlyJob(t, out, JobDelete); d.MessageID != 11 {
		t.Fatalf("delete %+v, want the orphan 11", d)
	}

	// The gap is closed: the next send probes nothing.
	n.handle(bg, upsert("c", SampleArb(now)), now.Add(7*time.Minute))
	n.onResult(bg, Result{Job: onlyJob(t, out, JobSend), MessageID: 14}, now.Add(7*time.Minute))
	noJobs(t, out)
}

func TestNotifier_PermanentSendFailureProbesNothing(t *testing.T) {
	n, out := newTestNotifier(t, newMemStore(), nil)
	now := t0()
	sendLive(t, n, out, "a", SampleArb(now), now, 10)
	n.handle(bg, upsert("b", SampleArb(now)), now)
	n.onResult(bg, Result{Job: onlyJob(t, out, JobSend), Err: &APIError{Code: 400, Description: "Bad Request: can't parse entities"}}, now)
	n.tick(now.Add(61 * time.Second))
	n.onResult(bg, Result{Job: onlyJob(t, out, JobSend), MessageID: 12}, now.Add(61*time.Second))
	noJobs(t, out)
}

func TestNotifier_OrphanProbeIsCapped(t *testing.T) {
	n, out := newTestNotifier(t, newMemStore(), nil)
	now := t0()
	sendLive(t, n, out, "a", SampleArb(now), now, 10)
	n.handle(bg, upsert("b", SampleArb(now)), now)
	n.onResult(bg, Result{Job: onlyJob(t, out, JobSend), Err: &APIError{Code: 502}}, now)
	n.tick(now.Add(61 * time.Second))
	n.onResult(bg, Result{Job: onlyJob(t, out, JobSend), MessageID: 500}, now.Add(61*time.Second))
	jobs := out.take()
	if len(jobs) != orphanProbeMax || jobs[0].MessageID != 11 || jobs[len(jobs)-1].MessageID != 10+orphanProbeMax {
		t.Fatalf("got %d probes (%+v), want ids 11..%d", len(jobs), jobs, 10+orphanProbeMax)
	}
}

// After a restart the highest stored id bounds the gap.
func TestNotifier_MaxMessageIDLoadedFromStore(t *testing.T) {
	now := t0()
	st := newMemStore()
	st.cfg = validConfig()
	st.msgs["gone:40"] = Tracked{MessageID: 40, SentAt: now.UnixMilli(), GoneAt: now.UnixMilli()}
	n, _ := newTestNotifier(t, st, nil)
	if n.maxMsgID != 40 {
		t.Fatalf("maxMsgID = %d, want 40", n.maxMsgID)
	}
}
