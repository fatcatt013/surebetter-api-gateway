package telegram

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func newTestOutbox(c Client, clk *fakeClock) *Outbox {
	o := NewOutbox(c, 60, nil)
	o.now, o.sleep = clk.now, clk.sleep
	o.bucket = newBucket(60, 3, clk.now())
	return o
}

func TestOutbox_PriorityGoneTestSendEdit(t *testing.T) {
	o := newTestOutbox(&scriptedClient{}, newFakeClock())
	o.Enqueue(Job{Kind: JobEdit, Key: "a", Text: "e"})
	o.Enqueue(Job{Kind: JobSend, Key: "b", Text: "s"})
	o.Enqueue(Job{Kind: JobTest, Text: "t"})
	o.Enqueue(Job{Kind: JobGone, Key: "c", Text: "g"})
	var order []JobKind
	for {
		j, ok := o.next()
		if !ok {
			break
		}
		order = append(order, j.Kind)
	}
	if want := []JobKind{JobGone, JobTest, JobSend, JobEdit}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestOutbox_CoalescesEditsAndGoneDropsPendingEdit(t *testing.T) {
	o := newTestOutbox(&scriptedClient{}, newFakeClock())
	o.Enqueue(Job{Kind: JobEdit, Key: "a", Text: "v1"})
	o.Enqueue(Job{Kind: JobEdit, Key: "b", Text: "b1"})
	o.Enqueue(Job{Kind: JobEdit, Key: "a", Text: "v2"})
	j, _ := o.next()
	if j.Key != "a" || j.Text != "v2" {
		t.Fatalf("first edit = %+v, want a/v2 (latest text, original position)", j)
	}
	o.Enqueue(Job{Kind: JobGone, Key: "b", Text: "gone"})
	j, _ = o.next()
	if j.Kind != JobGone {
		t.Fatalf("got %+v, want the gone job", j)
	}
	if _, ok := o.next(); ok {
		t.Fatal("b's pending edit must have been dropped by its gone job")
	}
}

func TestOutbox_429WaitsRetryAfter(t *testing.T) {
	clk := newFakeClock()
	c := &scriptedClient{errs: []error{&APIError{Code: 429, RetryAfter: 7}}}
	o := newTestOutbox(c, clk)
	r := o.do(context.Background(), Job{Kind: JobSend, Text: "x"})
	if r.Err != nil || r.MessageID != 1 || len(c.calls) != 2 {
		t.Fatalf("result %+v calls %v", r, c.calls)
	}
	if !containsDuration(clk.slept, 7*time.Second) {
		t.Fatalf("slept %v, want a 7s wait", clk.slept)
	}
}

func TestOutbox_RetriesServerErrorsThreeTimes(t *testing.T) {
	clk := newFakeClock()
	e := &APIError{Code: 502}
	c := &scriptedClient{errs: []error{e, e, e, e}}
	o := newTestOutbox(c, clk)
	r := o.do(context.Background(), Job{Kind: JobSend, Text: "x"})
	if r.Err == nil || len(c.calls) != 4 {
		t.Fatalf("want 1 try + 3 retries then failure; calls=%d err=%v", len(c.calls), r.Err)
	}
	for _, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		if !containsDuration(clk.slept, d) {
			t.Fatalf("missing backoff %v in %v", d, clk.slept)
		}
	}
}

func TestOutbox_PermanentErrorIsNotRetried(t *testing.T) {
	c := &scriptedClient{errs: []error{&APIError{Code: 400, Description: "chat not found"}}}
	r := newTestOutbox(c, newFakeClock()).do(context.Background(), Job{Kind: JobSend, Text: "x"})
	if r.Err == nil || len(c.calls) != 1 {
		t.Fatalf("calls=%d err=%v", len(c.calls), r.Err)
	}
}

func TestBucket_BurstThenRate(t *testing.T) {
	now := time.Now()
	b := newBucket(60, 3, now)
	for i := 0; i < 3; i++ {
		if d := b.take(now); d != 0 {
			t.Fatalf("burst token %d: wait %v", i, d)
		}
	}
	if d := b.take(now); d < 990*time.Millisecond || d > time.Second {
		t.Fatalf("4th call wait = %v, want ~1s at 60/min", d)
	}
}

func TestOutbox_RunDeliversReplyAndCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Result, 1)
	o := NewOutbox(&scriptedClient{}, 600, func(r Result) { got <- r })
	go o.Run(ctx)
	reply := make(chan Result, 1)
	o.Enqueue(Job{Kind: JobTest, Text: "t", Reply: reply})
	o.Enqueue(Job{Kind: JobSend, Key: "k", Text: "s"})
	select {
	case r := <-reply:
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("test job reply never arrived")
	}
	select {
	case r := <-got:
		if r.Job.Key != "k" {
			t.Fatalf("callback result %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send result never delivered")
	}
}

func containsDuration(ds []time.Duration, want time.Duration) bool {
	for _, d := range ds {
		if d == want {
			return true
		}
	}
	return false
}

func TestOutbox_GoneDeletesTheMessage(t *testing.T) {
	c := &scriptedClient{}
	r := newTestOutbox(c, newFakeClock()).do(context.Background(), Job{Kind: JobGone, MessageID: 5, Text: "gone"})
	if r.Err != nil || !reflect.DeepEqual(c.calls, []string{"delete"}) {
		t.Fatalf("result %+v calls %v, want a single delete", r, c.calls)
	}
}

func TestOutbox_GoneFallsBackToEditWhenDeleteRefused(t *testing.T) {
	c := &scriptedClient{errs: []error{&APIError{Code: 400, Description: "Bad Request: message can't be deleted"}}}
	r := newTestOutbox(c, newFakeClock()).do(context.Background(), Job{Kind: JobGone, MessageID: 5, Text: "gone"})
	if r.Err != nil || !reflect.DeepEqual(c.calls, []string{"delete", "edit:gone"}) {
		t.Fatalf("result %+v calls %v, want delete then the GONE edit", r, c.calls)
	}
}
