package queue

import (
	"context"
	"errors"
	"fmt"
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
