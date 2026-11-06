// Package queue implements Aether's bounded, rate-limited outbound delivery
// queue.
//
// Jobs are handed to a worker pool which calls transport.Messenger.Send. A
// hand-rolled per-provider token bucket decides when a worker may deliver: it
// never sleeps on the wall clock, it parks on the injected Clock's AfterFunc.
// Tests therefore advance a fake clock instead of waiting.
package queue

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/charlesrobsampson/aether/internal/metrics"
	"github.com/charlesrobsampson/aether/internal/transport"
)

// ErrClosed is returned by Enqueue after Shutdown has begun.
var ErrClosed = errors.New("queue: closed")

// ErrNoMessenger is returned by New when no transport.Messenger is supplied.
var ErrNoMessenger = errors.New("queue: messenger is required")

// Defaults mirror the limits documented in plan §3.14 and open-question #8.
const (
	defaultDepth       = 256
	defaultRate        = 1.0
	defaultBurst       = 5
	defaultWorkers     = 1
	defaultSendTimeout = 10 * time.Second

	// signalBuffer bounds the unexported test-observation channels. Sends are
	// non-blocking, so a test that falls behind misses a notification rather
	// than stalling the worker.
	signalBuffer = 256
)

// Job is one outbound delivery.
type Job struct {
	// Recipient is a phone number or provider-specific conversation ID.
	Recipient string
	// Text is the message body.
	Text string
	// Provider selects the rate-limit bucket. Empty means the default bucket.
	Provider string
}

// Clock is the injectable time source used by the rate limiter.
type Clock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) func()
}

// Options configures a Queue. Zero values select the documented defaults.
type Options struct {
	// Depth is the bounded channel capacity (limits.queue_depth).
	Depth int
	// RatePerSecond and Burst configure the per-provider token bucket.
	RatePerSecond float64
	Burst         int
	// Workers is the number of delivery goroutines. Defaults to 1.
	Workers int
	// SendTimeout bounds one Messenger.Send attempt. Defaults to 10s; a
	// non-positive value selects the default.
	SendTimeout time.Duration
	// Messenger performs the actual delivery.
	Messenger transport.Messenger
	// Clock defaults to the system clock.
	Clock Clock
	// Muted reports whether a recipient should be dropped at enqueue time.
	Muted func(recipient string) bool
	// Metrics receives outbound/dropped counters. Defaults to metrics.Default.
	Metrics *metrics.Registry
	// OnError observes a failed delivery. Delivery failures are not retried.
	OnError func(Job, error)
}

// Queue is a bounded outbound delivery queue. It is safe for concurrent use.
type Queue struct {
	jobs        chan Job
	messenger   transport.Messenger
	clock       Clock
	muted       func(string) bool
	metrics     *metrics.Registry
	onError     func(Job, error)
	limiter     *limiter
	sendTimeout time.Duration

	// sendCtx is the parent of every per-attempt deadline. Forced shutdown
	// cancels it so an in-flight provider call cannot outlive Shutdown.
	sendCtx    context.Context
	sendCancel context.CancelFunc

	// mu guards closed and, through RLock, the send on jobs. Holding RLock
	// while sending lets Shutdown wait for in-flight Enqueue calls instead of
	// racing a closed channel.
	mu        sync.RWMutex
	closed    bool
	closing   chan struct{}
	forceStop chan struct{}
	forceOnce sync.Once

	wg sync.WaitGroup

	// Unexported hooks used by package tests to synchronise without sleeping.
	delivered chan struct{}
	waiting   chan struct{}
}

// New starts the worker pool and returns a ready queue.
func New(opts Options) (*Queue, error) {
	if opts.Messenger == nil {
		return nil, ErrNoMessenger
	}

	depth := opts.Depth
	if depth <= 0 {
		depth = defaultDepth
	}
	rate := opts.RatePerSecond
	if rate <= 0 {
		rate = defaultRate
	}
	burst := opts.Burst
	if burst <= 0 {
		burst = defaultBurst
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	sendTimeout := opts.SendTimeout
	if sendTimeout <= 0 {
		sendTimeout = defaultSendTimeout
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	registry := opts.Metrics
	if registry == nil {
		registry = metrics.Default
	}
	sendCtx, sendCancel := context.WithCancel(context.Background())

	q := &Queue{
		jobs:        make(chan Job, depth),
		messenger:   opts.Messenger,
		clock:       clock,
		muted:       opts.Muted,
		metrics:     registry,
		onError:     opts.OnError,
		limiter:     newLimiter(rate, float64(burst)),
		sendTimeout: sendTimeout,
		sendCtx:     sendCtx,
		sendCancel:  sendCancel,
		closing:     make(chan struct{}),
		forceStop:   make(chan struct{}),
		delivered:   make(chan struct{}, signalBuffer),
		waiting:     make(chan struct{}, signalBuffer),
	}

	q.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go q.worker()
	}
	return q, nil
}

// Enqueue accepts a job or reports why it was not accepted.
//
// Muted recipients are dropped silently: the job is counted in
// aether_dropped_total and Enqueue returns nil. When the buffer is full,
// Enqueue blocks until a slot frees or Shutdown begins. After Shutdown it
// returns ErrClosed.
func (q *Queue) Enqueue(job Job) error {
	q.mu.RLock()
	defer q.mu.RUnlock()

	if q.closed {
		return ErrClosed
	}
	if q.muted != nil && q.muted(job.Recipient) {
		q.metrics.IncDropped()
		return nil
	}

	q.jobs <- job
	q.metrics.IncOutboundJobs()
	return nil
}

// Shutdown stops accepting jobs, drains the queue, and waits for the workers
// to exit. If ctx expires first, the workers abandon the remaining jobs (each
// counted as dropped) and Shutdown returns ctx.Err(). Shutdown is idempotent.
func (q *Queue) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.closing)
	}
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		q.sendCancel()
		return nil
	case <-ctx.Done():
		q.forceOnce.Do(func() { close(q.forceStop) })
		q.sendCancel()
		<-done
		return ctx.Err()
	}
}

// worker consumes jobs until shutdown, then drains what is buffered.
func (q *Queue) worker() {
	defer q.wg.Done()

	for {
		select {
		case job := <-q.jobs:
			q.deliver(job)
		case <-q.closing:
			q.drain()
			return
		}
	}
}

// drain delivers buffered jobs until the channel is empty. If shutdown is
// forced, the remainder is counted as dropped.
func (q *Queue) drain() {
	for {
		select {
		case job := <-q.jobs:
			q.deliver(job)
		case <-q.forceStop:
			q.dropRemaining()
			return
		default:
			return
		}
	}
}

// dropRemaining discards buffered jobs after a forced shutdown.
func (q *Queue) dropRemaining() {
	for {
		select {
		case <-q.jobs:
			q.metrics.IncDropped()
		default:
			return
		}
	}
}

// deliver waits for a token, sends the job with a bounded per-attempt deadline,
// and signals the test hook.
func (q *Queue) deliver(job Job) {
	for {
		now := q.clock.Now()
		if q.limiter.allow(job.Provider, now) {
			ctx, cancel := context.WithTimeout(q.sendCtx, q.sendTimeout)
			err := q.messenger.Send(ctx, job.Recipient, job.Text)
			cancel()
			if err != nil && q.onError != nil {
				q.onError(job, err)
			}
			q.signalDelivered()
			return
		}

		fired := make(chan struct{})
		cancel := q.clock.AfterFunc(q.limiter.delay(job.Provider, q.clock.Now()), func() {
			close(fired)
		})
		q.signalWaiting()

		select {
		case <-fired:
		case <-q.forceStop:
			cancel()
			q.metrics.IncDropped()
			return
		}
	}
}

func (q *Queue) signalDelivered() {
	select {
	case q.delivered <- struct{}{}:
	default:
	}
}

func (q *Queue) signalWaiting() {
	select {
	case q.waiting <- struct{}{}:
	default:
	}
}

// systemClock is the production Clock backed by the time package.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) func() {
	timer := time.AfterFunc(d, f)
	return func() { timer.Stop() }
}

// bucket is one provider's token-bucket state.
type bucket struct {
	tokens float64
	last   time.Time
}

// limiter is a hand-rolled token bucket keyed by provider.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
	}
}

// allow refills the bucket to now and consumes one token when available.
func (l *limiter) allow(provider string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.bucketFor(provider, now)
	l.refill(b, now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// delay returns how long until the provider's bucket holds one token.
func (l *limiter) delay(provider string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.bucketFor(provider, now)
	l.refill(b, now)
	if b.tokens >= 1 {
		return 0
	}

	seconds := (1 - b.tokens) / l.rate
	d := time.Duration(seconds * float64(time.Second))
	if d < time.Nanosecond {
		d = time.Nanosecond
	}
	return d
}

// bucketFor returns the provider's bucket, creating a full one on first use.
// The caller must hold l.mu.
func (l *limiter) bucketFor(provider string, now time.Time) *bucket {
	b := l.buckets[provider]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[provider] = b
	}
	return b
}

// refill adds the tokens earned since the last update, capped at the burst.
// The caller must hold l.mu.
func (l *limiter) refill(b *bucket, now time.Time) {
	if now.After(b.last) {
		b.tokens += now.Sub(b.last).Seconds() * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	} else if now.Before(b.last) {
		// A clock that moves backwards must not mint tokens.
		b.last = now
	}
}
