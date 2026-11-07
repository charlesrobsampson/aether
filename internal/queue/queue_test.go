package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/metrics"
	"github.com/charlesrobsampson/aether/internal/testutil"
	"github.com/charlesrobsampson/aether/internal/transport"
)

// TestQueueDrain verifies that every accepted job reaches the messenger and
// that the outbound counter tracks exactly one increment per delivered job.
func TestQueueDrain(t *testing.T) {
	messenger := transport.NewFake()
	registry := metrics.New()
	q := newTestQueue(t, Options{
		Depth:         16,
		RatePerSecond: 1000,
		Burst:         100,
		Messenger:     messenger,
		Clock:         testutil.NewFakeClock(testStart()),
		Metrics:       registry,
	})

	const jobs = 5
	for i := 0; i < jobs; i++ {
		recipient := fmt.Sprintf("+15555550%03d", i)
		if err := q.Enqueue(Job{Recipient: recipient, Text: "hello", Provider: "fake"}); err != nil {
			t.Fatalf("Enqueue(%s): %v", recipient, err)
		}
	}
	for i := 0; i < jobs; i++ {
		waitSignal(t, q.delivered, "delivery")
	}

	calls := messenger.Calls()
	if len(calls) != jobs {
		t.Fatalf("messenger calls = %d, want %d: %+v", len(calls), jobs, calls)
	}
	for i, call := range calls {
		wantRecipient := fmt.Sprintf("+15555550%03d", i)
		if call.Recipient != wantRecipient {
			t.Errorf("call %d recipient = %q, want %q", i, call.Recipient, wantRecipient)
		}
		if call.Text != "hello" {
			t.Errorf("call %d text = %q, want %q", i, call.Text, "hello")
		}
	}

	snapshot := registry.Snapshot()
	if snapshot.OutboundJobs != jobs {
		t.Errorf("outbound jobs = %d, want %d", snapshot.OutboundJobs, jobs)
	}
	if snapshot.Dropped != 0 {
		t.Errorf("dropped = %d, want 0", snapshot.Dropped)
	}
}

// TestRateLimit drives the token bucket with a fake clock: the first job is
// allowed inside the burst, the second and third wait for one token each and
// are never dropped.
func TestRateLimit(t *testing.T) {
	clock := testutil.NewFakeClock(testStart())
	messenger := &channelMessenger{sent: make(chan sentMessage, 8)}
	q := newTestQueue(t, Options{
		Depth:         8,
		RatePerSecond: 1,
		Burst:         1,
		Messenger:     messenger,
		Clock:         clock,
		Metrics:       metrics.New(),
	})

	for i := 1; i <= 3; i++ {
		if err := q.Enqueue(Job{
			Recipient: fmt.Sprintf("+1555555000%d", i),
			Text:      fmt.Sprintf("job-%d", i),
			Provider:  "fake",
		}); err != nil {
			t.Fatalf("Enqueue job %d: %v", i, err)
		}
	}

	if got := recvSent(t, messenger.sent); got.Text != "job-1" {
		t.Fatalf("first send = %q, want job-1", got.Text)
	}

	// The worker has consumed the burst token and must now park on the clock.
	waitSignal(t, q.waiting, "rate-limit wait")
	clock.Advance(500 * time.Millisecond)
	assertNoSend(t, messenger.sent, "job-2 before a full token refilled")

	clock.Advance(500 * time.Millisecond)
	if got := recvSent(t, messenger.sent); got.Text != "job-2" {
		t.Fatalf("second send = %q, want job-2", got.Text)
	}

	waitSignal(t, q.waiting, "rate-limit wait")
	clock.Advance(time.Second)
	if got := recvSent(t, messenger.sent); got.Text != "job-3" {
		t.Fatalf("third send = %q, want job-3", got.Text)
	}
}

// TestMutedDropped verifies that the mute predicate drops jobs at enqueue time:
// the job never reaches the messenger and is counted as dropped.
func TestMutedDropped(t *testing.T) {
	const mutedRecipient = "+15555550199"
	const allowedRecipient = "+15555550102"

	messenger := transport.NewFake()
	registry := metrics.New()
	q := newTestQueue(t, Options{
		Depth:         8,
		RatePerSecond: 1000,
		Burst:         10,
		Messenger:     messenger,
		Clock:         testutil.NewFakeClock(testStart()),
		Metrics:       registry,
		Muted:         func(recipient string) bool { return recipient == mutedRecipient },
	})

	if err := q.Enqueue(Job{Recipient: mutedRecipient, Text: "muted", Provider: "fake"}); err != nil {
		t.Fatalf("Enqueue muted: %v", err)
	}
	if err := q.Enqueue(Job{Recipient: allowedRecipient, Text: "allowed", Provider: "fake"}); err != nil {
		t.Fatalf("Enqueue allowed: %v", err)
	}
	waitSignal(t, q.delivered, "allowed delivery")

	calls := messenger.Calls()
	if len(calls) != 1 {
		t.Fatalf("messenger calls = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].Recipient != allowedRecipient || calls[0].Text != "allowed" {
		t.Errorf("send = %+v, want only the allowed recipient", calls[0])
	}

	snapshot := registry.Snapshot()
	if snapshot.Dropped != 1 {
		t.Errorf("dropped = %d, want 1", snapshot.Dropped)
	}
	if snapshot.OutboundJobs != 1 {
		t.Errorf("outbound jobs = %d, want 1", snapshot.OutboundJobs)
	}
}

// TestShutdown verifies that Shutdown drains queued jobs before returning and
// that Enqueue rejects new work afterwards.
func TestShutdown(t *testing.T) {
	messenger := transport.NewFake()
	q := newTestQueue(t, Options{
		Depth:         16,
		RatePerSecond: 1000,
		Burst:         100,
		Messenger:     messenger,
		Clock:         testutil.NewFakeClock(testStart()),
		Metrics:       metrics.New(),
	})

	const jobs = 5
	for i := 0; i < jobs; i++ {
		if err := q.Enqueue(Job{
			Recipient: fmt.Sprintf("+15555550%03d", i),
			Text:      "drain",
			Provider:  "fake",
		}); err != nil {
			t.Fatalf("Enqueue job %d: %v", i, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- q.Shutdown(ctx) }()

	for i := 0; i < jobs; i++ {
		waitSignal(t, q.delivered, "drained delivery")
	}
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := len(messenger.Calls()); got != jobs {
		t.Fatalf("messenger calls after drain = %d, want %d", got, jobs)
	}
	if err := q.Enqueue(Job{Recipient: "+15555550102", Text: "late", Provider: "fake"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after Shutdown = %v, want ErrClosed", err)
	}
}

// blockingMessenger parks inside Send until its context ends, which lets tests
// prove both the per-attempt deadline and forced-shutdown cancellation.
type blockingMessenger struct {
	started chan struct{}
	ctxErr  chan error
}

func (m *blockingMessenger) Send(ctx context.Context, _, _ string) error {
	select {
	case m.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	select {
	case m.ctxErr <- ctx.Err():
	default:
	}
	return ctx.Err()
}

func (m *blockingMessenger) Name() string { return "blocking" }

// TestSendTimeout verifies that one send attempt is bounded by SendTimeout.
func TestSendTimeout(t *testing.T) {
	messenger := &blockingMessenger{
		started: make(chan struct{}, 1),
		ctxErr:  make(chan error, 1),
	}
	q := newTestQueue(t, Options{
		Depth:         8,
		RatePerSecond: 1000,
		Burst:         100,
		Messenger:     messenger,
		Clock:         testutil.NewFakeClock(testStart()),
		Metrics:       metrics.New(),
		SendTimeout:   20 * time.Millisecond,
		// This test is about one attempt's deadline, not the retry policy.
		Attempts: 1,
	})

	if err := q.Enqueue(Job{Recipient: "+15555550102", Text: "hang", Provider: "fake"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitSignal(t, messenger.started, "send start")
	waitSignal(t, q.delivered, "timed-out delivery")

	select {
	case err := <-messenger.ctxErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("send context error = %v, want DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send context was never cancelled")
	}
}

// TestShutdownCancelsInflightSend verifies a forced shutdown does not wait for
// the full send timeout of an in-flight provider call.
func TestShutdownCancelsInflightSend(t *testing.T) {
	messenger := &blockingMessenger{
		started: make(chan struct{}, 1),
		ctxErr:  make(chan error, 1),
	}
	q := newTestQueue(t, Options{
		Depth:         8,
		RatePerSecond: 1000,
		Burst:         100,
		Messenger:     messenger,
		Clock:         testutil.NewFakeClock(testStart()),
		Metrics:       metrics.New(),
		SendTimeout:   10 * time.Second,
	})

	if err := q.Enqueue(Job{Recipient: "+15555550102", Text: "hang", Provider: "fake"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitSignal(t, messenger.started, "send start")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := q.Shutdown(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %s, want a prompt cancellation", elapsed)
	}

	select {
	case got := <-messenger.ctxErr:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("send context error = %v, want Canceled", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight send was not cancelled")
	}
}

// sentMessage is one observed delivery from channelMessenger.
type sentMessage struct {
	Recipient string
	Text      string
}

// channelMessenger reports each Send on a channel so tests can synchronise
// with the worker without sleeping.
type channelMessenger struct {
	sent chan sentMessage
}

func (m *channelMessenger) Send(_ context.Context, recipient, text string) error {
	m.sent <- sentMessage{Recipient: recipient, Text: text}
	return nil
}

func (m *channelMessenger) Name() string { return "channel" }

func newTestQueue(t *testing.T, opts Options) *Queue {
	t.Helper()

	q, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = q.Shutdown(ctx)
	})
	return q
}

func recvSent(t *testing.T, ch <-chan sentMessage) sentMessage {
	t.Helper()

	select {
	case msg := <-ch:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for messenger send")
		return sentMessage{}
	}
}

func assertNoSend(t *testing.T, ch <-chan sentMessage, what string) {
	t.Helper()

	select {
	case msg := <-ch:
		t.Fatalf("unexpected send while waiting for %s: %+v", what, msg)
	default:
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func testStart() time.Time {
	return time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
}

// scriptedMessenger returns a scripted sequence of errors and reports every
// attempt so retry tests can advance the fake clock deterministically. When
// blockFirst is set, the first attempt parks until release is closed.
type scriptedMessenger struct {
	mu         sync.Mutex
	errs       []error
	calls      int
	blockFirst bool
	started    chan struct{}
	release    chan struct{}
	attempts   chan struct{}
}

func newScriptedMessenger(errs ...error) *scriptedMessenger {
	return &scriptedMessenger{
		errs:     errs,
		attempts: make(chan struct{}, 16),
	}
}

func (m *scriptedMessenger) Send(ctx context.Context, _, _ string) error {
	m.mu.Lock()
	attempt := m.calls
	m.calls++
	block := m.blockFirst && attempt == 0
	err := error(nil)
	if attempt < len(m.errs) {
		err = m.errs[attempt]
	}
	m.mu.Unlock()

	select {
	case m.attempts <- struct{}{}:
	default:
	}

	if block {
		select {
		case m.started <- struct{}{}:
		default:
		}
		select {
		case <-m.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (m *scriptedMessenger) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *scriptedMessenger) Name() string { return "scripted" }

// deadLetterRecord is one OnDeadLetter observation.
type deadLetterRecord struct {
	job Job
	err error
}

// TestEnqueueFailFast proves a saturated queue pushes back instead of blocking
// the caller (plan §3.3, §3.9).
func TestEnqueueFailFast(t *testing.T) {
	clock := testutil.NewFakeClock(testStart())
	messenger := newScriptedMessenger()
	messenger.blockFirst = true
	messenger.started = make(chan struct{}, 1)
	messenger.release = make(chan struct{})

	q := newTestQueue(t, Options{
		Depth:         1,
		RatePerSecond: 1000,
		Burst:         100,
		Messenger:     messenger,
		Clock:         clock,
		Metrics:       metrics.New(),
	})

	if err := q.Enqueue(Job{Recipient: "+15555550101", Text: "in-flight", Provider: "fake"}); err != nil {
		t.Fatalf("Enqueue(in-flight): %v", err)
	}
	waitSignal(t, messenger.started, "in-flight send")

	if err := q.Enqueue(Job{Recipient: "+15555550102", Text: "buffered", Provider: "fake"}); err != nil {
		t.Fatalf("Enqueue(buffered): %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- q.Enqueue(Job{Recipient: "+15555550103", Text: "rejected", Provider: "fake"})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrFull) {
			t.Fatalf("Enqueue(full) = %v, want ErrFull", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked on a full queue, want a fail-fast ErrFull")
	}

	close(messenger.release)
	waitSignal(t, q.delivered, "in-flight delivery")
	waitSignal(t, q.delivered, "buffered delivery")
}

// TestEnqueueBatchAtomic proves a batch either fits entirely or enqueues
// nothing, and that muted recipients never consume capacity.
func TestEnqueueBatchAtomic(t *testing.T) {
	t.Run("oversized batch enqueues nothing", func(t *testing.T) {
		clock := testutil.NewFakeClock(testStart())
		messenger := newScriptedMessenger()
		messenger.blockFirst = true
		messenger.started = make(chan struct{}, 1)
		messenger.release = make(chan struct{})
		registry := metrics.New()

		q := newTestQueue(t, Options{
			Depth:         1,
			RatePerSecond: 1000,
			Burst:         100,
			Messenger:     messenger,
			Clock:         clock,
			Metrics:       registry,
		})

		if err := q.Enqueue(Job{Recipient: "+15555550101", Text: "in-flight", Provider: "fake"}); err != nil {
			t.Fatalf("Enqueue(in-flight): %v", err)
		}
		waitSignal(t, messenger.started, "in-flight send")
		if err := q.Enqueue(Job{Recipient: "+15555550102", Text: "buffered", Provider: "fake"}); err != nil {
			t.Fatalf("Enqueue(buffered): %v", err)
		}

		before := registry.Snapshot()
		err := q.EnqueueBatch([]Job{
			{Recipient: "+15555550201", Text: "batch-1", Provider: "fake"},
			{Recipient: "+15555550202", Text: "batch-2", Provider: "fake"},
		})
		if !errors.Is(err, ErrFull) {
			t.Fatalf("EnqueueBatch(full) = %v, want ErrFull", err)
		}
		if after := registry.Snapshot(); after.OutboundJobs != before.OutboundJobs {
			t.Fatalf("outbound jobs after rejected batch = %d, want %d",
				after.OutboundJobs, before.OutboundJobs)
		}

		close(messenger.release)
		waitSignal(t, q.delivered, "in-flight delivery")
		waitSignal(t, q.delivered, "buffered delivery")

		if got := messenger.Calls(); got != 2 {
			t.Fatalf("messenger calls = %d, want the 2 accepted jobs", got)
		}
	})

	t.Run("fitting batch enqueues every job", func(t *testing.T) {
		messenger := newScriptedMessenger()
		q := newTestQueue(t, Options{
			Depth:         4,
			RatePerSecond: 1000,
			Burst:         100,
			Messenger:     messenger,
			Clock:         testutil.NewFakeClock(testStart()),
			Metrics:       metrics.New(),
		})

		jobs := []Job{
			{Recipient: "+15555550301", Text: "batch-1", Provider: "fake"},
			{Recipient: "+15555550302", Text: "batch-2", Provider: "fake"},
			{Recipient: "+15555550303", Text: "batch-3", Provider: "fake"},
		}
		if err := q.EnqueueBatch(jobs); err != nil {
			t.Fatalf("EnqueueBatch(fitting): %v", err)
		}
		for range jobs {
			waitSignal(t, q.delivered, "batch delivery")
		}
		if got := messenger.Calls(); got != len(jobs) {
			t.Fatalf("messenger calls = %d, want %d", got, len(jobs))
		}
	})

	t.Run("muted jobs do not consume capacity", func(t *testing.T) {
		const (
			mutedRecipient = "+15555550499"
			allowed        = "+15555550401"
		)
		messenger := newScriptedMessenger()
		registry := metrics.New()
		q := newTestQueue(t, Options{
			Depth:         1,
			RatePerSecond: 1000,
			Burst:         100,
			Messenger:     messenger,
			Clock:         testutil.NewFakeClock(testStart()),
			Metrics:       registry,
			Muted:         func(recipient string) bool { return recipient == mutedRecipient },
		})

		err := q.EnqueueBatch([]Job{
			{Recipient: mutedRecipient, Text: "muted", Provider: "fake"},
			{Recipient: allowed, Text: "allowed", Provider: "fake"},
		})
		if err != nil {
			t.Fatalf("EnqueueBatch with a muted job = %v, want nil", err)
		}
		waitSignal(t, q.delivered, "allowed delivery")

		if got := messenger.Calls(); got != 1 {
			t.Fatalf("messenger calls = %d, want 1", got)
		}
		snapshot := registry.Snapshot()
		if snapshot.Dropped != 1 {
			t.Errorf("dropped = %d, want 1", snapshot.Dropped)
		}
		if snapshot.OutboundJobs != 1 {
			t.Errorf("outbound jobs = %d, want 1", snapshot.OutboundJobs)
		}
	})
}

// TestSendRetriesTransient proves a retryable failure is retried with
// deterministic exponential backoff and that success stops the sequence.
func TestSendRetriesTransient(t *testing.T) {
	start := testStart()
	clock := testutil.NewFakeClock(start)
	messenger := newScriptedMessenger(
		&transport.SendError{StatusCode: 503},
		&transport.SendError{StatusCode: 503},
	)
	var (
		failuresMu sync.Mutex
		failures   int
	)
	q := newTestQueue(t, Options{
		Depth:         8,
		RatePerSecond: 1000,
		Burst:         100,
		Messenger:     messenger,
		Clock:         clock,
		Metrics:       metrics.New(),
		Attempts:      3,
		RetryBackoff:  500 * time.Millisecond,
		OnError: func(Job, error) {
			failuresMu.Lock()
			failures++
			failuresMu.Unlock()
		},
	})

	if err := q.Enqueue(Job{Recipient: "+15555550501", Text: "retry", Provider: "fake"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitSignal(t, messenger.attempts, "attempt 1")
	waitSignal(t, q.waiting, "backoff after attempt 1")
	clock.Advance(500 * time.Millisecond)

	waitSignal(t, messenger.attempts, "attempt 2")
	waitSignal(t, q.waiting, "backoff after attempt 2")
	clock.Advance(time.Second)

	waitSignal(t, q.delivered, "delivery after a successful retry")

	if got := messenger.Calls(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	failuresMu.Lock()
	gotFailures := failures
	failuresMu.Unlock()
	if gotFailures != 2 {
		t.Fatalf("OnError calls = %d, want 2", gotFailures)
	}
	if want := start.Add(1500 * time.Millisecond); !clock.Now().Equal(want) {
		t.Fatalf("clock = %s, want %s after 500ms then 1s of backoff", clock.Now(), want)
	}
}

// TestSendStopsOnTerminalError proves a non-retryable provider status is
// attempted once and then dead-lettered.
func TestSendStopsOnTerminalError(t *testing.T) {
	messenger := newScriptedMessenger(&transport.SendError{StatusCode: 400})
	dead := make(chan deadLetterRecord, 1)
	q := newTestQueue(t, Options{
		Depth:         8,
		RatePerSecond: 1000,
		Burst:         100,
		Messenger:     messenger,
		Clock:         testutil.NewFakeClock(testStart()),
		Metrics:       metrics.New(),
		Attempts:      3,
		RetryBackoff:  500 * time.Millisecond,
		OnDeadLetter: func(job Job, err error) {
			dead <- deadLetterRecord{job: job, err: err}
		},
	})

	if err := q.Enqueue(Job{Recipient: "+15555550601", Text: "terminal", Provider: "fake"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitSignal(t, q.delivered, "terminal delivery")

	if got := messenger.Calls(); got != 1 {
		t.Fatalf("attempts = %d, want 1 for a terminal status", got)
	}
	select {
	case record := <-dead:
		var sendErr *transport.SendError
		if !errors.As(record.err, &sendErr) || sendErr.StatusCode != 400 {
			t.Fatalf("dead-letter error = %v, want SendError 400", record.err)
		}
	case <-time.After(time.Second):
		t.Fatal("OnDeadLetter was not called for a terminal error")
	}
}

// TestSendHonoursRetryAfter proves a 429's Retry-After raises the backoff above
// the exponential default.
func TestSendHonoursRetryAfter(t *testing.T) {
	start := testStart()
	clock := testutil.NewFakeClock(start)
	messenger := newScriptedMessenger(&transport.SendError{
		StatusCode: 429,
		RetryAfter: 2 * time.Second,
	})
	q := newTestQueue(t, Options{
		Depth:         8,
		RatePerSecond: 1000,
		Burst:         100,
		Messenger:     messenger,
		Clock:         clock,
		Metrics:       metrics.New(),
		Attempts:      2,
		RetryBackoff:  500 * time.Millisecond,
	})

	if err := q.Enqueue(Job{Recipient: "+15555550701", Text: "throttled", Provider: "fake"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitSignal(t, messenger.attempts, "attempt 1")
	waitSignal(t, q.waiting, "retry-after wait")

	clock.Advance(1500 * time.Millisecond)
	select {
	case <-messenger.attempts:
		t.Fatal("retry happened before Retry-After elapsed")
	default:
	}

	clock.Advance(500 * time.Millisecond)
	waitSignal(t, messenger.attempts, "attempt 2")
	waitSignal(t, q.delivered, "delivery after Retry-After")

	if got := messenger.Calls(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if want := start.Add(2 * time.Second); !clock.Now().Equal(want) {
		t.Fatalf("clock = %s, want %s after honouring Retry-After", clock.Now(), want)
	}
}

// TestDeadLetterAfterAttempts proves the retry budget is bounded and exhaustion
// calls OnDeadLetter exactly once.
func TestDeadLetterAfterAttempts(t *testing.T) {
	messenger := newScriptedMessenger(
		&transport.SendError{StatusCode: 500},
		&transport.SendError{StatusCode: 500},
	)
	dead := make(chan deadLetterRecord, 2)
	clock := testutil.NewFakeClock(testStart())
	q := newTestQueue(t, Options{
		Depth:         8,
		RatePerSecond: 1000,
		Burst:         100,
		Messenger:     messenger,
		Clock:         clock,
		Metrics:       metrics.New(),
		Attempts:      2,
		RetryBackoff:  100 * time.Millisecond,
		OnDeadLetter: func(job Job, err error) {
			dead <- deadLetterRecord{job: job, err: err}
		},
	})

	if err := q.Enqueue(Job{Recipient: "+15555550801", Text: "exhausted", Provider: "fake"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitSignal(t, messenger.attempts, "attempt 1")
	waitSignal(t, q.waiting, "backoff after attempt 1")
	clock.Advance(100 * time.Millisecond)
	waitSignal(t, messenger.attempts, "attempt 2")
	waitSignal(t, q.delivered, "exhausted delivery")

	if got := messenger.Calls(); got != 2 {
		t.Fatalf("attempts = %d, want the configured 2", got)
	}
	select {
	case record := <-dead:
		var sendErr *transport.SendError
		if !errors.As(record.err, &sendErr) || sendErr.StatusCode != 500 {
			t.Fatalf("dead-letter error = %v, want SendError 500", record.err)
		}
	case <-time.After(time.Second):
		t.Fatal("OnDeadLetter was not called after exhausting attempts")
	}
	select {
	case extra := <-dead:
		t.Fatalf("unexpected second dead letter: %+v", extra)
	default:
	}
}
