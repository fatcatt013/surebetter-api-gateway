package telegram

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// JobKind is a job's kind. The numbers say nothing about priority: next() sets the order, gone queue (JobGone,
// JobDelete, JobOrphan in arrival order) > JobTest > JobAlert > JobSend > JobEdit > JobReply.
type JobKind int

const (
	JobGone   JobKind = iota // GONE edit of a vanished arb
	JobDelete                // delete a gone arb's message (after GoneDeleteAfter)
	JobTest                  // POST /api/telegram/test
	JobSend                  // first message for an arb
	JobEdit                  // live update (coalesced per key)
	// JobOrphan edits a message id that an ambiguous send may have posted to GONE. Telegram lets a
	// bot edit only its own messages, so the edit doubles as an ownership probe. Runs in the gone queue.
	JobOrphan
	// JobAlert sends plain text to one private chat (Job.ChatID): a bot alert or the linker's answer. It runs after
	// the test jobs and before the arb sends, sharing the rate limit. One surely not sent (a 429, or a NetError with
	// NotSent) is tried again by Run after alertRetryDelays (a 429 after its retry_after), then given up; a timeout or
	// a 5xx is never retried (it may have been posted). Its final result goes to Job.Done only, never to the arb notifier.
	JobAlert
	// JobReply sends the linker's one-line answer to a stranger's private message (Job.ChatID): plain text, tried once,
	// result dropped. It runs last, after every arb job (gone > delete > test > alert > send > edit > reply), and at most
	// maxQueuedReplies wait: anyone can message the bot, so its answers must never delay arb posts or alerts.
	JobReply
)

const (
	maxQueuedReplies = 20  // more JobReply jobs than this waiting: the new one is dropped
	maxQueuedAlerts  = 200 // more JobAlert jobs than this waiting: the oldest is dropped (its Done gets ErrQueueFull)
)

// ErrQueueFull is the result of an alert dropped from a full queue.
var ErrQueueFull = errors.New("telegram: alert queue full")

// alertRetryDelays: the waits before an alert's 2nd, 3rd and 4th try.
var alertRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

// AlertNoRetry as a JobAlert's Attempt makes its first try the last (the app's Send test waits for the answer).
const AlertNoRetry = 3

// Job is one Bot API call. Reply, when set, receives the Result instead of the outbox callback.
type Job struct {
	Kind      JobKind
	Key       string
	MessageID int64
	Text      string
	Reply     chan<- Result
	ChatID    int64        // JobAlert: the private chat
	Done      func(Result) // JobAlert: receives the final result (nil: nobody)
	Attempt   int          // JobAlert: retries already made (0 to AlertNoRetry)
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

	after func(time.Duration, func()) // runs f after d (time.AfterFunc; a test runs it at once)

	mu        sync.Mutex
	bucket    *bucket
	gone      []Job
	tests     []Job
	alerts    []Job
	replies   []Job
	sends     []Job
	edits     map[string]Job
	editOrder []string
	wake      chan struct{}
}

// NewOutbox: ratePerMin is TELEGRAM_RATE_PER_MIN; deliver receives every non-Reply result except alerts' (nil: none
// yet, see SetDeliver).
func NewOutbox(c Client, ratePerMin int, deliver func(Result)) *Outbox {
	o := &Outbox{client: c, deliver: deliver, now: time.Now, sleep: sleepCtx, edits: map[string]Job{}, wake: make(chan struct{}, 1),
		after: func(d time.Duration, f func()) { time.AfterFunc(d, f) }}
	o.bucket = newBucket(ratePerMin, 3, o.now())
	return o
}

// SetDeliver sets where the results of jobs without Reply go (alerts excepted): the arb notifier of a shared outbox.
func (o *Outbox) SetDeliver(f func(Result)) {
	o.mu.Lock()
	o.deliver = f
	o.mu.Unlock()
}

// Enqueue never blocks. A newer edit for a key replaces the queued one; a gone job drops it.
func (o *Outbox) Enqueue(j Job) {
	var dropped *Job
	o.mu.Lock()
	switch j.Kind {
	case JobGone, JobDelete, JobOrphan:
		o.dropEdit(j.Key)
		o.gone = append(o.gone, j)
	case JobTest:
		o.tests = append(o.tests, j)
	case JobAlert:
		if len(o.alerts) >= maxQueuedAlerts {
			d := o.alerts[0]
			o.alerts = o.alerts[1:]
			dropped = &d
		}
		o.alerts = append(o.alerts, j)
	case JobReply:
		if len(o.replies) >= maxQueuedReplies {
			o.mu.Unlock()
			log.Debug().Int64("chat", j.ChatID).Msg("telegram: reply queue full, answer dropped")
			return
		}
		o.replies = append(o.replies, j)
	case JobSend:
		o.sends = append(o.sends, j)
	case JobEdit:
		if _, ok := o.edits[j.Key]; !ok {
			o.editOrder = append(o.editOrder, j.Key)
		}
		o.edits[j.Key] = j
	}
	o.mu.Unlock()
	if dropped != nil {
		log.Warn().Int64("chat", dropped.ChatID).Msg("telegram: alert queue full, the oldest alert dropped")
		if dropped.Done != nil {
			dropped.Done(Result{Job: *dropped, Err: ErrQueueFull})
		}
	}
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
	case len(o.alerts) > 0:
		return pop(&o.alerts), true
	case len(o.sends) > 0:
		return pop(&o.sends), true
	case len(o.editOrder) > 0:
		k := o.editOrder[0]
		o.editOrder = o.editOrder[1:]
		j := o.edits[k]
		delete(o.edits, k)
		return j, true
	case len(o.replies) > 0:
		return pop(&o.replies), true
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
		switch {
		case j.Kind == JobAlert:
			o.finishAlert(r)
		case j.Kind == JobReply:
			if r.Err != nil {
				log.Debug().Err(r.Err).Int64("chat", j.ChatID).Msg("telegram: reply not sent")
			}
		case j.Reply != nil:
			j.Reply <- r
		default:
			o.mu.Lock()
			deliver := o.deliver
			o.mu.Unlock()
			if deliver != nil {
				deliver(r)
			}
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
		if j.Kind == JobAlert || j.Kind == JobReply {
			// An alert's next try is scheduled by Run (finishAlert), a reply has none: the worker never sleeps on them.
			return Result{Job: j, Err: err}
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
		// A send that timed out or got a 5xx may still have been posted; retrying it posts a
		// duplicate whose id we never learn. Sends retry only on 429 (not processed); the notifier
		// re-sends on the next tick and probes for the orphan (see Notifier.probeOrphans).
		if retryable(err) && retries < maxRetries && !isSend(j.Kind) {
			if o.sleep(ctx, time.Duration(1<<retries)*time.Second) != nil {
				return Result{Job: j, Err: err}
			}
			retries++
			continue
		}
		return Result{Job: j, Err: err}
	}
}

func isSend(k JobKind) bool { return k == JobSend || k == JobTest }

func (o *Outbox) call(ctx context.Context, j Job) (int64, error) {
	switch j.Kind {
	case JobSend, JobTest:
		return o.client.Send(ctx, j.Text)
	case JobDelete:
		return j.MessageID, o.client.Delete(ctx, j.MessageID)
	case JobAlert, JobReply:
		return o.client.SendTo(ctx, j.ChatID, j.Text)
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

// finishAlert hands an alert's result to its Done, or queues another try when the alert was surely not sent (a 429, a
// NetError with NotSent), after alertRetryDelays (a 429 after its retry_after), until AlertNoRetry retries were made.
func (o *Outbox) finishAlert(r Result) {
	j := r.Job
	if r.Err != nil && j.Attempt < AlertNoRetry {
		if wait, ok := alertRetryWait(r.Err, j.Attempt); ok {
			j.Attempt++
			o.after(wait, func() { o.Enqueue(j) })
			return
		}
	}
	if j.Done != nil {
		j.Done(r)
	}
}

// alertRetryWait is how long before an alert refused with err is tried again; false: never — it may have been posted
// (a timeout, a 5xx, a connection broken after the request went out) or was refused for good (another 4xx).
func alertRetryWait(err error, attempt int) (time.Duration, bool) {
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == 429 {
		if ae.RetryAfter > 0 {
			return time.Duration(ae.RetryAfter) * time.Second, true
		}
		return alertRetryDelays[attempt], true
	}
	var ne *NetError
	if errors.As(err, &ne) && ne.NotSent {
		return alertRetryDelays[attempt], true
	}
	return 0, false
}
