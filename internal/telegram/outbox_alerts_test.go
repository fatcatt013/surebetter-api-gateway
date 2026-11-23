package telegram

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestOutbox_AlertRunsAfterTestsBeforeSends(t *testing.T) {
	o := newTestOutbox(&scriptedClient{}, newFakeClock())
	o.Enqueue(Job{Kind: JobEdit, Key: "a", Text: "e"})
	o.Enqueue(Job{Kind: JobSend, Key: "b", Text: "s"})
	o.Enqueue(Job{Kind: JobAlert, ChatID: 5, Text: "al"})
	o.Enqueue(Job{Kind: JobTest, Text: "t"})
	o.Enqueue(Job{Kind: JobDelete, Key: "d", MessageID: 3})
	o.Enqueue(Job{Kind: JobGone, Key: "c", Text: "g"})
	var order []JobKind
	for {
		j, ok := o.next()
		if !ok {
			break
		}
		order = append(order, j.Kind)
	}
	if want := []JobKind{JobDelete, JobGone, JobTest, JobAlert, JobSend, JobEdit}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v (gone queue in arrival order, then test, alert, send, edit)", order, want)
	}
}

func TestOutbox_AlertGoesToItsChatAndDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &scriptedClient{}
	o := newTestOutbox(c, newFakeClock())
	delivered := make(chan Result, 1)
	o.SetDeliver(func(r Result) { delivered <- r })
	go o.Run(ctx)
	done := make(chan Result, 1)
	o.Enqueue(Job{Kind: JobAlert, ChatID: 42, Text: "hi", Done: func(r Result) { done <- r }})
	select {
	case r := <-done:
		if r.Err != nil || r.MessageID != 1 {
			t.Fatalf("result %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alert result never arrived")
	}
	select {
	case r := <-delivered:
		t.Fatalf("an alert result reached the arb notifier: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	if !reflect.DeepEqual(c.calls, []string{"sendTo:42:hi"}) {
		t.Fatalf("calls %v", c.calls)
	}
}

// An alert is tried once per run of the worker (no sleeping in it): Run schedules the next try.
func TestOutbox_AlertDoTriesOnce(t *testing.T) {
	clk := newFakeClock()
	c := &scriptedClient{errs: []error{&APIError{Code: 429, RetryAfter: 7}}}
	r := newTestOutbox(c, clk).do(context.Background(), Job{Kind: JobAlert, ChatID: 1, Text: "x"})
	if r.Err == nil || len(c.calls) != 1 || len(clk.slept) != 0 {
		t.Fatalf("calls=%d slept=%v err=%v", len(c.calls), clk.slept, r.Err)
	}
}

// Retried only when surely not sent: a 429 (after its retry_after, else the schedule) and a NetError with NotSent.
func TestOutbox_AlertRetriesWhenSurelyNotSentThenGivesUp(t *testing.T) {
	o := newTestOutbox(&scriptedClient{}, newFakeClock())
	var waits []time.Duration
	o.after = func(d time.Duration, f func()) { waits = append(waits, d); f() }
	var done []Result
	cur := Job{Kind: JobAlert, ChatID: 42, Text: "a", Done: func(r Result) { done = append(done, r) }}
	refused := &NetError{Msg: "dial tcp: connection refused", NotSent: true}
	errs := []error{&APIError{Code: 429, RetryAfter: 7}, refused, &APIError{Code: 429}, refused}
	for i, e := range errs {
		o.finishAlert(Result{Job: cur, Err: e})
		if i < len(errs)-1 {
			next, ok := o.next()
			if !ok || next.Kind != JobAlert || next.Attempt != i+1 || next.ChatID != 42 {
				t.Fatalf("try %d: %+v %v", i+2, next, ok)
			}
			cur = next
		}
	}
	if want := []time.Duration{7 * time.Second, 5 * time.Second, 15 * time.Second}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("waits %v, want %v (a 429 waits its retry_after, else 2/5/15 s by try)", waits, want)
	}
	if len(done) != 1 || done[0].Err != errs[3] {
		t.Fatalf("done %+v, want the last error once", done)
	}
	if _, ok := o.next(); ok {
		t.Fatal("a fourth retry was queued")
	}
}

// Maybe sent (a timeout, a 5xx, another network error) or refused for good (403): never tried again.
func TestOutbox_AlertMaybeSentOrForbiddenIsFinal(t *testing.T) {
	o := newTestOutbox(&scriptedClient{}, newFakeClock())
	o.after = func(time.Duration, func()) { t.Fatal("retried") }
	var done []Result
	for _, e := range []error{
		&NetError{Msg: "context deadline exceeded (Client.Timeout exceeded)", NotSent: false},
		&APIError{Code: 502},
		&APIError{Code: 500},
		errors.New("connection reset by peer"),
		&APIError{Code: 403, Description: "Forbidden: bot was blocked by the user"},
	} {
		o.finishAlert(Result{Job: Job{Kind: JobAlert, Done: func(r Result) { done = append(done, r) }}, Err: e})
	}
	o.finishAlert(Result{Job: Job{Kind: JobAlert, Attempt: AlertNoRetry, Done: func(r Result) { done = append(done, r) }},
		Err: &NetError{Msg: "refused", NotSent: true}})
	if len(done) != 6 {
		t.Fatalf("done %+v", done)
	}
	if AlertNoRetry != len(alertRetryDelays) {
		t.Fatalf("AlertNoRetry = %d, want %d", AlertNoRetry, len(alertRetryDelays))
	}
}

// The linker's answers to strangers run last, after every arb job, and at most maxQueuedReplies wait.
func TestOutbox_RepliesRunLastAndAreCapped(t *testing.T) {
	o := newTestOutbox(&scriptedClient{}, newFakeClock())
	for i := 0; i < 3*maxQueuedReplies; i++ {
		o.Enqueue(Job{Kind: JobReply, ChatID: int64(i), Text: "r"})
	}
	o.Enqueue(Job{Kind: JobEdit, Key: "a", Text: "e"})
	o.Enqueue(Job{Kind: JobSend, Key: "b", Text: "s"})
	o.Enqueue(Job{Kind: JobAlert, ChatID: 5, Text: "al"})
	var order []JobKind
	for {
		j, ok := o.next()
		if !ok {
			break
		}
		order = append(order, j.Kind)
	}
	if len(order) != 3+maxQueuedReplies || order[0] != JobAlert || order[1] != JobSend || order[2] != JobEdit {
		t.Fatalf("order = %v, want alert, send, edit, then %d replies", order, maxQueuedReplies)
	}
	for _, k := range order[3:] {
		if k != JobReply {
			t.Fatalf("order = %v", order)
		}
	}
}

// A reply is tried once (a 429 is not waited for); its result never reaches the arb notifier.
func TestOutbox_ReplyTriedOnceNeverDelivered(t *testing.T) {
	clk := newFakeClock()
	c := &scriptedClient{errs: []error{&APIError{Code: 429, RetryAfter: 7}}}
	if r := newTestOutbox(c, clk).do(context.Background(), Job{Kind: JobReply, ChatID: 1, Text: "x"}); r.Err == nil ||
		len(c.calls) != 1 || len(clk.slept) != 0 {
		t.Fatalf("calls=%v slept=%v err=%v", c.calls, clk.slept, r.Err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c2 := &scriptedClient{}
	o := newTestOutbox(c2, newFakeClock())
	delivered := make(chan Result, 1)
	o.SetDeliver(func(r Result) { delivered <- r })
	go o.Run(ctx)
	o.Enqueue(Job{Kind: JobReply, ChatID: 42, Text: "hi"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		c2.mu.Lock()
		n := len(c2.calls)
		c2.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reply was never sent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case r := <-delivered:
		t.Fatalf("a reply result reached the arb notifier: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
}

// Beyond maxQueuedAlerts the oldest queued alert is dropped and its Done gets ErrQueueFull.
func TestOutbox_AlertQueueDropsTheOldest(t *testing.T) {
	o := newTestOutbox(&scriptedClient{}, newFakeClock())
	var dropped []Result
	for i := 0; i < maxQueuedAlerts+2; i++ {
		o.Enqueue(Job{Kind: JobAlert, ChatID: int64(i), Text: "a", Done: func(r Result) { dropped = append(dropped, r) }})
	}
	if len(dropped) != 2 || dropped[0].Job.ChatID != 0 || dropped[1].Job.ChatID != 1 || !errors.Is(dropped[0].Err, ErrQueueFull) {
		t.Fatalf("dropped %+v", dropped)
	}
	j, _ := o.next()
	if j.ChatID != 2 {
		t.Fatalf("first queued alert: %+v", j)
	}
}
