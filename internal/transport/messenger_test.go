package transport

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
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
