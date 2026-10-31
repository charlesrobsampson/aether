package scheduler

import (
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/testutil"
)

func TestScheduler(t *testing.T) {
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

	t.Run("After fires exactly once at the deadline", func(t *testing.T) {
		clock := testutil.NewFakeClock(start)
		s := New(clock)

		fired := 0
		var firedAt time.Time
		s.After(30*time.Second, func() {
			fired++
			firedAt = clock.Now()
		})

		clock.Advance(29 * time.Second)
		if fired != 0 {
			t.Fatalf("After(30s) fired after 29s: %d calls", fired)
		}

		clock.Advance(time.Second)
		if fired != 1 {
			t.Fatalf("After(30s) fired %d times at +30s, want 1", fired)
		}
		if want := start.Add(30 * time.Second); !firedAt.Equal(want) {
			t.Fatalf("After(30s) fired at %v, want %v", firedAt, want)
		}

		clock.Advance(time.Hour)
		if fired != 1 {
			t.Fatalf("After(30s) fired %d times after advancing past the deadline, want 1", fired)
		}
	})

	t.Run("At fires exactly once at the absolute instant", func(t *testing.T) {
		clock := testutil.NewFakeClock(start)
		s := New(clock)

		at := start.Add(2 * time.Hour)
		fired := 0
		var firedAt time.Time
		s.At(at, func() {
			fired++
			firedAt = clock.Now()
		})

		clock.Advance(2*time.Hour - time.Nanosecond)
		if fired != 0 {
			t.Fatalf("At fired %d times before the instant", fired)
		}

		clock.Advance(time.Nanosecond)
		if fired != 1 {
			t.Fatalf("At fired %d times at the instant, want 1", fired)
		}
		if !firedAt.Equal(at) {
			t.Fatalf("At fired at %v, want %v", firedAt, at)
		}

		clock.Advance(24 * time.Hour)
		if fired != 1 {
			t.Fatalf("At fired %d times after advancing past the instant, want 1", fired)
		}
	})

	t.Run("Cancel prevents firing", func(t *testing.T) {
		clock := testutil.NewFakeClock(start)
		s := New(clock)

		fired := false
		id := s.After(time.Minute, func() { fired = true })

		if !s.Cancel(id) {
			t.Fatal("Cancel returned false for a live timer")
		}
		if s.Cancel(id) {
			t.Fatal("Cancel returned true for an already-cancelled timer")
		}

		clock.Advance(time.Hour)
		if fired {
			t.Fatal("cancelled timer fired")
		}
	})

	t.Run("Every fires through the clock and reschedules", func(t *testing.T) {
		clock := testutil.NewFakeClock(start)
		s := New(clock)

		var fired []time.Time
		id, err := s.Every("*/5 * * * *", func() { fired = append(fired, clock.Now()) })
		if err != nil {
			t.Fatalf("Every(*/5 * * * *): %v", err)
		}

		// 03:04:05 -> the first activation is 03:05:00.
		clock.Advance(55 * time.Second)
		if len(fired) != 1 || !fired[0].Equal(start.Add(55*time.Second)) {
			t.Fatalf("after first activation fired %v, want [%v]", fired, start.Add(55*time.Second))
		}

		clock.Advance(5 * time.Minute)
		if len(fired) != 2 || !fired[1].Equal(start.Add(5*time.Minute+55*time.Second)) {
			t.Fatalf("after second activation fired %v, want two activations", fired)
		}

		if !s.Cancel(id) {
			t.Fatal("Cancel returned false for a recurring timer")
		}
		clock.Advance(time.Hour)
		if len(fired) != 2 {
			t.Fatalf("cancelled recurring timer fired %d times, want 2", len(fired))
		}
	})
}

func TestCronNext(t *testing.T) {
	friday := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

	next, err := Next("0 7 * * 1-5", friday)
	if err != nil {
		t.Fatalf("Next(0 7 * * 1-5): %v", err)
	}
	want := time.Date(2026, time.January, 2, 7, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("Next(0 7 * * 1-5) = %v, want %v", next, want)
	}

	// Saturday must roll to Monday 07:00.
	saturday := time.Date(2026, time.January, 3, 12, 0, 0, 0, time.UTC)
	next, err = Next("0 7 * * 1-5", saturday)
	if err != nil {
		t.Fatalf("Next on Saturday: %v", err)
	}
	want = time.Date(2026, time.January, 5, 7, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("Next on Saturday = %v, want %v", next, want)
	}

	if _, err := Next("0 0 7 * * *", friday); err == nil {
		t.Fatal("Next accepted a 6-field spec")
	}
	if _, err := Next("not a cron spec", friday); err == nil {
		t.Fatal("Next accepted an invalid spec")
	}
	if _, err := Next("", friday); err == nil {
		t.Fatal("Next accepted an empty spec")
	}
}
