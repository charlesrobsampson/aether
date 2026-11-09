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
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/charlesrobsampson/aether/internal/metrics"
	"github.com/charlesrobsampson/aether/internal/transport"
)

// ErrClosed is returned by Enqueue after Shutdown has begun.
var ErrClosed = errors.New("queue: closed")

// ErrFull is returned when the bounded buffer cannot accept a job (or a whole
// batch) right now. Callers should push back rather than block (plan §3.3).
var ErrFull = errors.New("queue: full")

// ErrNoMessenger is returned by New when no transport.Messenger is supplied.
var ErrNoMessenger = errors.New("queue: messenger is required")

// Defaults mirror the limits documented in plan §3.14 and open-question #8.
const (
	defaultDepth       = 256
	defaultRate        = 1.0
	defaultBurst       = 5
	defaultWorkers     = 1
	defaultSendTimeout = 10 * time.Second
	defaultAttempts    = 3

	defaultRetryBackoff = 500 * time.Millisecond
	maxRetryBackoff     = 30 * time.Second

	// signalBuffer bounds the unexported test-observation channels. Sends are
	// non-blocking, so a test that falls behind misses a notification rather
	// than stalling the worker.
	signalBuffer = 256
)

// Job is one outbound delivery.
type Job struct {
	// Recipient is a phone number or provider-specific conversation ID.
	Recipient string
	// Text is the message body. It is ignored when Reaction is set.
	Text string
	// Reaction, when non-nil, attaches a reaction to an existing message
	// instead of sending text.
	Reaction *transport.Reaction
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
	// Attempts bounds delivery attempts per job, including the first. A
	// non-positive value selects the default (3).
	Attempts int
	// RetryBackoff is the base for deterministic exponential backoff between
	// attempts. A non-positive value selects the default (500ms); the delay is
	// capped at 30s.
	RetryBackoff time.Duration
	// Messenger performs the actual delivery.
	Messenger transport.Messenger
	// Clock defaults to the system clock.
	Clock Clock
	// Muted reports whether a recipient should be dropped at enqueue time.
	Muted func(recipient string) bool
	// Metrics receives outbound/dropped counters. Defaults to metrics.Default.
	Metrics *metrics.Registry
	// OnError observes every failed delivery attempt.
	OnError func(Job, error)
	// OnDeadLetter observes a job that exhausted its attempts or failed
	// terminally. It is called once per job.
	OnDeadLetter func(Job, error)
}

// Queue is a bounded outbound delivery queue. It is safe for concurrent use.
type Queue struct {
	jobs         chan Job
	messenger    transport.Messenger
	clock        Clock
	muted        func(string) bool
	metrics      *metrics.Registry
	onError      func(Job, error)
	onDeadLetter func(Job, error)
	limiter      *limiter
	sendTimeout  time.Duration
	attempts     int
	retryBackoff time.Duration

	// sendCtx is the parent of every per-attempt deadline. Forced shutdown
	// cancels it so an in-flight provider call cannot outlive Shutdown.
	sendCtx    context.Context
	sendCancel context.CancelFunc

	// mu serialises enqueue with shutdown. It also guards closed, so an
	// Enqueue that wins the race can finish before Shutdown closes the
	// channel.
	mu        sync.Mutex
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
	attempts := opts.Attempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	retryBackoff := opts.RetryBackoff
	if retryBackoff <= 0 {
		retryBackoff = defaultRetryBackoff
	}
	if retryBackoff > maxRetryBackoff {
		retryBackoff = maxRetryBackoff
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
		jobs:         make(chan Job, depth),
		messenger:    opts.Messenger,
		clock:        clock,
		muted:        opts.Muted,
		metrics:      registry,
		onError:      opts.OnError,
		onDeadLetter: opts.OnDeadLetter,
		limiter:      newLimiter(rate, float64(burst)),
		sendTimeout:  sendTimeout,
		attempts:     attempts,
		retryBackoff: retryBackoff,
		sendCtx:      sendCtx,
		sendCancel:   sendCancel,
		closing:      make(chan struct{}),
		forceStop:    make(chan struct{}),
		delivered:    make(chan struct{}, signalBuffer),
		waiting:      make(chan struct{}, signalBuffer),
	}

	q.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go q.worker()
	}
	return q, nil
}

// Enqueue accepts one job or reports why it was not accepted. It is shorthand
// for EnqueueBatch with a single job.
//
// Muted recipients are dropped silently: the job is counted in
// aether_dropped_total and Enqueue returns nil. When the buffer is full,
// Enqueue returns ErrFull immediately instead of blocking. After Shutdown it
// returns ErrClosed.
func (q *Queue) Enqueue(job Job) error {
	return q.EnqueueBatch([]Job{job})
}

// EnqueueBatch accepts every job or none. It checks free capacity once, under
// the same mutex that serialises shutdown, so a rejected batch never leaves a
// partial fan-out behind (plan §3.3). Muted recipients are counted as dropped
// and do not consume capacity. Sends into the buffer are non-blocking after the
// capacity check, so the critical section is bounded.
func (q *Queue) EnqueueBatch(jobs []Job) error {
	if len(jobs) == 0 {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return ErrClosed
	}

	accepted := make([]Job, 0, len(jobs))
	muted := 0
	for _, job := range jobs {
		if q.muted != nil && q.muted(job.Recipient) {
			muted++
			continue
		}
		accepted = append(accepted, job)
	}

	if free := cap(q.jobs) - len(q.jobs); len(accepted) > free {
		q.metrics.IncQueueFull(metrics.QueueOutbound)
		return ErrFull
	}

	q.metrics.AddDropped(uint64(muted))
	q.metrics.AddOutboundJobs(uint64(len(accepted)))
	for _, job := range accepted {
		q.jobs <- job
	}
	q.metrics.SetOutboundQueueDepth(len(q.jobs))
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
			q.metrics.SetOutboundQueueDepth(len(q.jobs))
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
			q.metrics.SetOutboundQueueDepth(len(q.jobs))
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
			q.metrics.SetOutboundQueueDepth(len(q.jobs))
			return
		}
	}
}

// deliver waits for a token, then sends the job with a bounded per-attempt
// deadline. Retryable failures are retried with exponential backoff (or the
// provider's Retry-After, whichever is longer) up to the configured attempt
// budget; exhaustion or a terminal failure is dead-lettered. The test hook fires
// once per completed job.
func (q *Queue) deliver(job Job) {
	provider := providerLabel(job.Provider)
	var lastErr error
	for attempt := 1; attempt <= q.attempts; attempt++ {
		if !q.waitForToken(job) {
			q.metrics.IncDropped()
			return
		}

		ctx, cancel := context.WithTimeout(q.sendCtx, q.sendTimeout)
		start := time.Now()
		var err error
		if job.Reaction != nil {
			err = q.messenger.React(ctx, *job.Reaction)
		} else {
			err = q.messenger.Send(ctx, job.Recipient, job.Text)
		}
		elapsed := time.Since(start)
		cancel()

		if err == nil {
			q.metrics.ObserveOutboundSendDuration(provider, metrics.SendResultOK, elapsed)
			q.signalDelivered()
			return
		}

		lastErr = err
		reason := sendFailureReason(err)
		q.metrics.IncOutboundSendFailure(provider, reason)

		retry, retryAfter := transport.Retryable(err)
		result := metrics.SendResultTerminal
		if retry {
			result = metrics.SendResultRetryable
		}
		q.metrics.ObserveOutboundSendDuration(provider, result, elapsed)

		if q.onError != nil {
			q.onError(job, err)
		}

		if !retry || attempt == q.attempts {
			break
		}
		// A forced shutdown has cancelled sendCtx; retrying would only delay
		// the drain and cannot succeed.
		if q.sendCtx.Err() != nil {
			q.metrics.IncDropped()
			return
		}
		q.metrics.IncOutboundRetry(provider, reason)
		if !q.waitRetry(attempt, retryAfter) {
			q.metrics.IncDropped()
			return
		}
	}

	q.metrics.IncDeadLetter(provider)
	q.metrics.SetOutboundQueueDepth(len(q.jobs))
	if q.onDeadLetter != nil {
		q.onDeadLetter(job, lastErr)
	}
	q.signalDelivered()
}

// providerLabel keeps the metric label set bounded and non-empty. An unset job
// provider is the default bucket.
func providerLabel(provider string) string {
	if provider == "" {
		return "default"
	}
	return provider
}

// sendFailureReason maps a send error onto the bounded reason labels documented
// for aether_outbound_send_failures_total and aether_outbound_retries_total.
func sendFailureReason(err error) string {
	if err == nil {
		return metrics.SendFailureOther
	}

	var sendErr *transport.SendError
	if errors.As(err, &sendErr) && sendErr.StatusCode > 0 {
		switch {
		case sendErr.StatusCode == http.StatusTooManyRequests:
			return metrics.SendFailure429
		case sendErr.StatusCode >= 500 && sendErr.StatusCode <= 599:
			return metrics.SendFailure5xx
		case sendErr.StatusCode >= 400 && sendErr.StatusCode <= 499:
			return metrics.SendFailure4xx
		default:
			return metrics.SendFailureOther
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return metrics.SendFailureTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return metrics.SendFailureTimeout
		}
		return metrics.SendFailureNetwork
	}
	if errors.Is(err, context.Canceled) {
		return metrics.SendFailureNetwork
	}
	return metrics.SendFailureOther
}

// waitForToken parks on the fake-friendly clock until the job's provider bucket
// has a token. It reports false when a forced shutdown interrupts the wait.
func (q *Queue) waitForToken(job Job) bool {
	for {
		if q.sendCtx.Err() != nil {
			return false
		}

		now := q.clock.Now()
		if q.limiter.allow(job.Provider, now) {
			return true
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
			return false
		}
	}
}

// waitRetry waits for the larger of the exponential backoff and the provider's
// Retry-After. It reports false when a forced shutdown interrupts the wait.
func (q *Queue) waitRetry(attempt int, retryAfter time.Duration) bool {
	delay := q.backoff(attempt)
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay <= 0 {
		return true
	}

	fired := make(chan struct{})
	cancel := q.clock.AfterFunc(delay, func() {
		close(fired)
	})
	q.signalWaiting()

	select {
	case <-fired:
		return true
	case <-q.forceStop:
		cancel()
		return false
	}
}

// backoff returns base * 2^(attempt-1), capped at maxRetryBackoff.
func (q *Queue) backoff(attempt int) time.Duration {
	delay := q.retryBackoff
	if delay <= 0 {
		return 0
	}
	if delay > maxRetryBackoff {
		delay = maxRetryBackoff
	}
	for i := 1; i < attempt; i++ {
		if delay >= maxRetryBackoff/2 {
			return maxRetryBackoff
		}
		delay *= 2
	}
	return delay
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
