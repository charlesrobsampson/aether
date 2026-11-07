package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestFakeMessenger(t *testing.T) {
	var _ Messenger = (*Fake)(nil)

	fake := NewFake()
	if got := fake.Name(); got != "fake" {
		t.Fatalf("Name() = %q, want %q", got, "fake")
	}
	if got := fake.Calls(); len(got) != 0 {
		t.Fatalf("fresh Fake has %d calls, want 0", len(got))
	}

	// A successful send is recorded verbatim.
	if err := fake.Send(context.Background(), "+15555550100", "hello"); err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}
	want := []Call{{Recipient: "+15555550100", Text: "hello"}}
	if got := fake.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Calls() = %#v, want %#v", got, want)
	}

	// FailWith makes Send return exactly the injected error, but the attempt is
	// still recorded.
	sentinel := errors.New("transport unavailable")
	fake.FailWith(sentinel)
	err := fake.Send(context.Background(), "+15555550101", "second")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Send() error = %v, want %v", err, sentinel)
	}
	want = append(want, Call{Recipient: "+15555550101", Text: "second"})
	if got := fake.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after failed Send, Calls() = %#v, want %#v", got, want)
	}

	// Clearing the error restores successful sends.
	fake.FailWith(nil)
	if err := fake.Send(context.Background(), "+15555550102", "third"); err != nil {
		t.Fatalf("Send() after FailWith(nil) error = %v, want nil", err)
	}

	// Calls returns a copy; callers cannot mutate the Fake's history.
	got := fake.Calls()
	got[0].Text = "mutated"
	if fake.Calls()[0].Text != "hello" {
		t.Fatal("Calls() exposed the Fake's internal slice")
	}

	// The Fake is safe for concurrent use.
	var wg sync.WaitGroup
	const senders = 20
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fake.Send(context.Background(), "+15555550103", "concurrent"); err != nil {
				t.Errorf("concurrent Send() error = %v", err)
			}
		}()
	}
	wg.Wait()
	if got, want := len(fake.Calls()), 3+senders; got != want {
		t.Fatalf("Calls() length = %d, want %d", got, want)
	}
}

// TestRetryable pins the retry classification: only network/context errors and
// provider 429/5xx statuses are retryable, and Retry-After is preserved.
func TestRetryable(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantRetry bool
		wantAfter time.Duration
	}{
		{name: "nil", err: nil, wantRetry: false},
		{name: "context deadline", err: context.DeadlineExceeded, wantRetry: true},
		{name: "context canceled", err: context.Canceled, wantRetry: true},
		{name: "network error", err: &net.DNSError{Err: "timeout", Name: "example"}, wantRetry: true},
		{name: "429", err: &SendError{StatusCode: 429, RetryAfter: 2 * time.Second}, wantRetry: true, wantAfter: 2 * time.Second},
		{name: "500", err: &SendError{StatusCode: 500}, wantRetry: true},
		{name: "503", err: &SendError{StatusCode: 503}, wantRetry: true},
		{name: "400 is terminal", err: &SendError{StatusCode: 400}, wantRetry: false},
		{name: "401 is terminal", err: &SendError{StatusCode: 401}, wantRetry: false},
		{name: "403 is terminal", err: &SendError{StatusCode: 403}, wantRetry: false},
		{name: "404 is terminal", err: &SendError{StatusCode: 404}, wantRetry: false},
		{name: "409 is terminal", err: &SendError{StatusCode: 409}, wantRetry: false},
		{name: "unknown error is terminal", err: errors.New("boom"), wantRetry: false},
		{name: "wrapped network error", err: fmt.Errorf("send: %w", &net.DNSError{Err: "timeout"}), wantRetry: true},
		{name: "wrapped terminal status", err: fmt.Errorf("send: %w", &SendError{StatusCode: 400}), wantRetry: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			retry, after := Retryable(tc.err)
			if retry != tc.wantRetry {
				t.Fatalf("Retryable(%v) = %v, want %v", tc.err, retry, tc.wantRetry)
			}
			if after != tc.wantAfter {
				t.Fatalf("Retryable(%v) Retry-After = %s, want %s", tc.err, after, tc.wantAfter)
			}
		})
	}
}
