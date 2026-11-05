package telegram

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

// JobKind is also the priority order: lower runs first.
type JobKind int

const (
	JobGone JobKind = iota // delete the message of a vanished arb (Text = GONE fallback edit)
	JobTest                // POST /api/telegram/test
	JobSend                // first message for an arb
	JobEdit                // live update (coalesced per key)
)

// Job is one Bot API call. Reply, when set, receives the Result instead of the outbox callback.
type Job struct {
	Kind      JobKind
	Key       string
	MessageID int64
	Text      string
	Reply     chan<- Result
}

// Result is the outcome of a Job. MessageID is the new id for sends.
type Result struct {
	Job       Job
	MessageID int64
	Err       error
}

const (
	maxRetries  = 3 // 5xx / network: 1s, 2s, 4s
	max429Waits = 5
)

// Outbox serialises all Bot API calls through one worker, a token bucket and a priority queue.
type Outbox struct {
	client  Client
	deliver func(Result)
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error

	mu        sync.Mutex
	bucket    *bucket
	gone      []Job
	tests     []Job
	sends     []Job
	edits     map[string]Job
	editOrder []string
	wake      chan struct{}
}

// NewOutbox: ratePerMin is TELEGRAM_RATE_PER_MIN; deliver receives every non-Reply result.
func NewOutbox(c Client, ratePerMin int, deliver func(Result)) *Outbox {
	o := &Outbox{client: c, deliver: deliver, now: time.Now, sleep: sleepCtx, edits: map[string]Job{}, wake: make(chan struct{}, 1)}
	o.bucket = newBucket(ratePerMin, 3, o.now())
	return o
}

// Enqueue never blocks. A newer edit for a key replaces the queued one; a gone job drops it.
func (o *Outbox) Enqueue(j Job) {
	o.mu.Lock()
	switch j.Kind {
	case JobGone:
		o.dropEdit(j.Key)
		o.gone = append(o.gone, j)
	case JobTest:
		o.tests = append(o.tests, j)
	case JobSend:
		o.sends = append(o.sends, j)
	case JobEdit:
		if _, ok := o.edits[j.Key]; !ok {
			o.editOrder = append(o.editOrder, j.Key)
		}
		o.edits[j.Key] = j
	}
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *Outbox) dropEdit(key string) {
	if _, ok := o.edits[key]; !ok {
		return
	}
	delete(o.edits, key)
	for i, k := range o.editOrder {
		if k == key {
			o.editOrder = append(o.editOrder[:i], o.editOrder[i+1:]...)
			return
		}
	}
}

func (o *Outbox) next() (Job, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	pop := func(q *[]Job) Job { j := (*q)[0]; *q = (*q)[1:]; return j }
	switch {
	case len(o.gone) > 0:
		return pop(&o.gone), true
	case len(o.tests) > 0:
		return pop(&o.tests), true
	case len(o.sends) > 0:
		return pop(&o.sends), true
	case len(o.editOrder) > 0:
		k := o.editOrder[0]
		o.editOrder = o.editOrder[1:]
		j := o.edits[k]
		delete(o.edits, k)
		return j, true
	}
	return Job{}, false
}

// Run processes jobs until ctx ends.
func (o *Outbox) Run(ctx context.Context) {
	for {
		j, ok := o.next()
		if !ok {
			select {
			case <-o.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		r := o.do(ctx, j)
		if ctx.Err() != nil {
			return
		}
		if j.Reply != nil {
			j.Reply <- r
		} else if o.deliver != nil {
			o.deliver(r)
		}
	}
}

func (o *Outbox) do(ctx context.Context, j Job) Result {
	retries, waits429 := 0, 0
	for {
		if err := o.waitToken(ctx); err != nil {
			return Result{Job: j, Err: err}
		}
		id, err := o.call(ctx, j)
		if err == nil {
			return Result{Job: j, MessageID: id}
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == 429 && waits429 < max429Waits {
			waits429++
			wait := time.Duration(ae.RetryAfter) * time.Second
			if wait <= 0 {
				wait = time.Second
			}
			if o.sleep(ctx, wait) != nil {
				return Result{Job: j, Err: err}
			}
			continue
		}
		if retryable(err) && retries < maxRetries {
			if o.sleep(ctx, time.Duration(1<<retries)*time.Second) != nil {
				return Result{Job: j, Err: err}
			}
			retries++
			continue
		}
		return Result{Job: j, Err: err}
	}
}

func (o *Outbox) call(ctx context.Context, j Job) (int64, error) {
	switch j.Kind {
	case JobSend, JobTest:
		return o.client.Send(ctx, j.Text)
	case JobGone:
		err := o.client.Delete(ctx, j.MessageID)
		if cannotDelete(err) {
			return j.MessageID, o.client.Edit(ctx, j.MessageID, j.Text)
		}
		return j.MessageID, err
	default:
		return j.MessageID, o.client.Edit(ctx, j.MessageID, j.Text)
	}
}

func (o *Outbox) waitToken(ctx context.Context) error {
	for {
		o.mu.Lock()
		d := o.bucket.take(o.now())
		o.mu.Unlock()
		if d == 0 {
			return nil
		}
		if err := o.sleep(ctx, d); err != nil {
			return err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// bucket is a token bucket: `capacity` burst, refilled at perMin per minute.
type bucket struct {
	tokens, capacity, perSec float64
	last                     time.Time
}

func newBucket(perMin, burst int, now time.Time) *bucket {
	if perMin <= 0 {
		perMin = 18
	}
	return &bucket{tokens: float64(burst), capacity: float64(burst), perSec: float64(perMin) / 60, last: now}
}

// take consumes a token and returns 0, or returns how long until one is available.
func (b *bucket) take(now time.Time) time.Duration {
	b.tokens = math.Min(b.capacity, b.tokens+now.Sub(b.last).Seconds()*b.perSec)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	return time.Duration((1 - b.tokens) / b.perSec * float64(time.Second))
}
