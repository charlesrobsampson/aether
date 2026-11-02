package testsat

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/scheduler"
	"github.com/charlesrobsampson/aether/internal/testutil"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

const (
	testSender    = "+15555550102"
	testSatellite = "testsat"
)

// recordingSender captures every /message envelope the satellite emits. It is
// the SDK client seam, so no HTTP server or real Aether instance is required.
type recordingSender struct {
	mu        sync.Mutex
	responses []sdk.Response
	err       error
}

func (s *recordingSender) Send(_ context.Context, resp sdk.Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, resp)
	return s.err
}

func (s *recordingSender) Responses() []sdk.Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sdk.Response, len(s.responses))
	copy(out, s.responses)
	return out
}

func newFixture(t *testing.T, start time.Time) (*Manager, *testutil.FakeClock, *recordingSender) {
	t.Helper()

	clock := testutil.NewFakeClock(start)
	sender := &recordingSender{}
	manager := New(Options{
		Client:    sender,
		Scheduler: scheduler.New(clock),
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
	})
	return manager, clock, sender
}

func requireResponses(t *testing.T, sender *recordingSender, want int) []sdk.Response {
	t.Helper()

	got := sender.Responses()
	if len(got) != want {
		t.Fatalf("responses = %d, want %d (%+v)", len(got), want, got)
	}
	return got
}

// TestReminderFlow covers the §3.16 contract: a relative reminder fires exactly
// once after its delay, an absolute reminder fires once at the converted instant,
// and /test echoes "test received" back through /message.
func TestReminderFlow(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC) // 06:00 in America/Chicago

	t.Run("relative reminder fires once after 30 seconds", func(t *testing.T) {
		manager, clock, sender := newFixture(t, start)

		err := manager.HandleRemind(sdk.Transmission{
			Trigger:   "remind",
			Text:      "remind me call bob in 30 seconds",
			Sender:    testSender,
			Satellite: testSatellite,
			ThreadID:  "errand",
		})
		if err != nil {
			t.Fatalf("HandleRemind: %v", err)
		}

		requireResponses(t, sender, 0)

		clock.Advance(29 * time.Second)
		requireResponses(t, sender, 0)

		clock.Advance(time.Second)
		got := requireResponses(t, sender, 1)
		if got[0].Text != "call bob" {
			t.Errorf("text = %q, want %q", got[0].Text, "call bob")
		}
		if len(got[0].Recipients) != 1 || got[0].Recipients[0] != testSender {
			t.Errorf("recipients = %v, want [%s]", got[0].Recipients, testSender)
		}
		if got[0].Satellite != testSatellite {
			t.Errorf("satellite = %q, want %q", got[0].Satellite, testSatellite)
		}
		if got[0].ThreadID != "errand" {
			t.Errorf("thread = %q, want %q", got[0].ThreadID, "errand")
		}

		// A reminder is one-shot: moving the clock again must not re-fire it.
		clock.Advance(time.Hour)
		requireResponses(t, sender, 1)
	})

	t.Run("short relative form defaults the message", func(t *testing.T) {
		manager, clock, sender := newFixture(t, start)

		if err := manager.HandleRemind(sdk.Transmission{
			Trigger:   "remind",
			Text:      "in 30 seconds",
			Sender:    testSender,
			Satellite: testSatellite,
		}); err != nil {
			t.Fatalf("HandleRemind: %v", err)
		}

		clock.Advance(30 * time.Second)
		got := requireResponses(t, sender, 1)
		if got[0].Text != defaultReminderText {
			t.Errorf("text = %q, want %q", got[0].Text, defaultReminderText)
		}
	})

	t.Run("absolute reminder fires once at the converted instant", func(t *testing.T) {
		manager, clock, sender := newFixture(t, start)

		// 12:30 on 2026-01-05 in America/Chicago is 18:30 UTC, 6h30m away.
		err := manager.HandleRemind(sdk.Transmission{
			Trigger:   "remind",
			Text:      "remind me stretch at 2026-01-05 12:30",
			Sender:    testSender,
			Satellite: testSatellite,
		})
		if err != nil {
			t.Fatalf("HandleRemind: %v", err)
		}

		clock.Advance(6*time.Hour + 29*time.Minute)
		requireResponses(t, sender, 0)

		clock.Advance(time.Minute)
		got := requireResponses(t, sender, 1)
		if got[0].Text != "stretch" {
			t.Errorf("text = %q, want %q", got[0].Text, "stretch")
		}
		if len(got[0].Recipients) != 1 || got[0].Recipients[0] != testSender {
			t.Errorf("recipients = %v, want [%s]", got[0].Recipients, testSender)
		}

		clock.Advance(24 * time.Hour)
		requireResponses(t, sender, 1)
	})

	t.Run("test echoes test received", func(t *testing.T) {
		manager, _, sender := newFixture(t, start)

		if err := manager.HandleTest(sdk.Transmission{
			Trigger:   "test",
			Text:      "test",
			Sender:    testSender,
			Satellite: testSatellite,
		}); err != nil {
			t.Fatalf("HandleTest: %v", err)
		}

		got := requireResponses(t, sender, 1)
		if got[0].Text != "test received" {
			t.Errorf("text = %q, want %q", got[0].Text, "test received")
		}
		if len(got[0].Recipients) != 1 || got[0].Recipients[0] != testSender {
			t.Errorf("recipients = %v, want [%s]", got[0].Recipients, testSender)
		}
	})

	t.Run("rejects malformed reminders", func(t *testing.T) {
		manager, _, sender := newFixture(t, start)

		for _, text := range []string{
			"remind me",
			"remind me call bob in soon",
			"remind me call bob at not-a-time",
		} {
			if err := manager.HandleRemind(sdk.Transmission{
				Trigger: "remind",
				Text:    text,
				Sender:  testSender,
			}); err == nil {
				t.Errorf("HandleRemind(%q) = nil, want error", text)
			}
		}
		requireResponses(t, sender, 0)
	})

	t.Run("missing sender is rejected", func(t *testing.T) {
		manager, _, _ := newFixture(t, start)

		if err := manager.HandleTest(sdk.Transmission{Text: "test"}); err == nil {
			t.Error("HandleTest without sender = nil, want error")
		}
	})
}
