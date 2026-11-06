package dispatch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/model"
)

func inbound(text string) model.Inbound {
	return model.Inbound{Sender: "+15555550102", Text: text}
}

// TestDispatchQueueBounded proves Submit is non-blocking: with a single worker
// busy and a depth of one, the third job is rejected instead of waiting, and
// the accepted jobs still run in order.
func TestDispatchQueueBounded(t *testing.T) {
	started := make(chan string, 4)
	release := make(chan struct{})

	q, err := New(Options{
		Depth: 1,
		Handler: func(_ context.Context, in model.Inbound) {
			started <- in.Text
			<-release
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// The first job occupies the worker, so the second fills the buffer.
	if err := q.Submit(context.Background(), inbound("first")); err != nil {
		t.Fatalf("Submit(first) error = %v", err)
	}
	select {
	case got := <-started:
		if got != "first" {
			t.Fatalf("first dispatched = %q, want first", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start the first job")
	}

	if err := q.Submit(context.Background(), inbound("second")); err != nil {
		t.Fatalf("Submit(second) error = %v", err)
	}

	// The buffer is full and the worker is blocked, so this must fail fast.
	before := time.Now()
	err = q.Submit(context.Background(), inbound("third"))
	if !errors.Is(err, ErrFull) {
		t.Fatalf("Submit(third) error = %v, want ErrFull", err)
	}
	if elapsed := time.Since(before); elapsed > 100*time.Millisecond {
		t.Fatalf("Submit(third) took %s, want an immediate rejection", elapsed)
	}

	close(release)
	select {
	case got := <-started:
		if got != "second" {
			t.Fatalf("second dispatched = %q, want second", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not drain the accepted second job")
	}

	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

// TestDispatchShutdownDrains proves a graceful shutdown runs every accepted job,
// while an expired shutdown deadline cancels the worker and drops the remainder
// without hanging.
func TestDispatchShutdownDrains(t *testing.T) {
	t.Run("graceful shutdown drains accepted jobs", func(t *testing.T) {
		var mu sync.Mutex
		var ran []string
		q, err := New(Options{
			Depth: 4,
			Handler: func(_ context.Context, in model.Inbound) {
				mu.Lock()
				ran = append(ran, in.Text)
				mu.Unlock()
			},
		})
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}

		for _, text := range []string{"a", "b", "c"} {
			if err := q.Submit(context.Background(), inbound(text)); err != nil {
				t.Fatalf("Submit(%s) error = %v", text, err)
			}
		}
		if err := q.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(ran) != 3 || ran[0] != "a" || ran[1] != "b" || ran[2] != "c" {
			t.Fatalf("dispatched jobs = %v, want [a b c]", ran)
		}
	})

	t.Run("expired shutdown drops the remainder", func(t *testing.T) {
		started := make(chan struct{}, 1)
		var ran atomic.Int32
		q, err := New(Options{
			Depth: 2,
			Handler: func(ctx context.Context, _ model.Inbound) {
				ran.Add(1)
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
			},
		})
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}

		if err := q.Submit(context.Background(), inbound("blocking")); err != nil {
			t.Fatalf("Submit(blocking) error = %v", err)
		}
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not start the blocking job")
		}
		if err := q.Submit(context.Background(), inbound("buffered")); err != nil {
			t.Fatalf("Submit(buffered) error = %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		err = q.Shutdown(ctx)
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown() error = %v, want DeadlineExceeded", err)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("Shutdown() took %s, want a prompt return", elapsed)
		}
		if got := ran.Load(); got != 1 {
			t.Fatalf("dispatched jobs = %d, want only the in-flight job", got)
		}
	})
}
