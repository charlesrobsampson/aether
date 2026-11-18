package testsat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/scheduler"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/internal/testutil"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

// recurringFixture wires a Manager to a fake clock, a recording sender, and the
// in-memory reminder store. timezone and limit are read on every call so a test
// can change them mid-series.
type recurringFixture struct {
	manager  *Manager
	clock    *testutil.FakeClock
	sender   *recordingSender
	store    *fakeReminderStore
	timezone string
	limit    string
}

func newRecurringFixture(t *testing.T, start time.Time) *recurringFixture {
	t.Helper()

	clock := testutil.NewFakeClock(start)
	sender := &recordingSender{}
	f := &recurringFixture{
		clock:    clock,
		sender:   sender,
		store:    newFakeReminderStore(),
		timezone: "America/Chicago",
	}
	f.manager = New(Options{
		Client:         sender,
		Scheduler:      scheduler.New(clock),
		Store:          f.store,
		Clock:          clock,
		Timezone:       func(string) string { return f.timezone },
		RecurringLimit: func(string) string { return f.limit },
	})
	return f
}

// remind sends one command through HandleRemind.
func (f *recurringFixture) remind(t *testing.T, text string) {
	t.Helper()
	if err := f.manager.HandleRemind(sdk.Transmission{
		Trigger:   "remind",
		Text:      text,
		Sender:    testSender,
		Satellite: testSatellite,
	}); err != nil {
		t.Fatalf("HandleRemind(%q): %v", text, err)
	}
}

// lastText returns the most recent outbound text.
func (f *recurringFixture) lastText(t *testing.T) string {
	t.Helper()
	responses := f.sender.Responses()
	if len(responses) == 0 {
		t.Fatal("no responses")
	}
	return responses[len(responses)-1].Text
}

// TestRecurringDailyFiresAtWallClock covers the conversational daily form and
// checks that the acknowledgement names the rule and the first occurrence.
func TestRecurringDailyFiresAtWallClock(t *testing.T) {
	// Monday 2026-01-05 06:00 America/Chicago.
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := newRecurringFixture(t, start)

	f.remind(t, "remind me walk the dog every day at 07:30")
	ack := requireResponses(t, f.sender, 1)[0].Text
	for _, want := range []string{
		"Recurring reminder set: walk the dog",
		"every day at 07:30",
		"First: Mon 2026-01-05 07:30 CST",
	} {
		if !strings.Contains(ack, want) {
			t.Errorf("ack = %q, want it to contain %q", ack, want)
		}
	}

	rec := f.store.only(t)
	if rec.Kind != store.ReminderDaily || rec.AtMinutes != 7*60+30 {
		t.Fatalf("stored schedule = %+v", rec)
	}
	if rec.Remaining != store.UnlimitedReminders {
		t.Fatalf("remaining = %d, want unlimited", rec.Remaining)
	}

	// 07:30 CST is 13:30 UTC, 1h30m after the start.
	f.clock.Advance(time.Hour + 29*time.Minute)
	requireResponses(t, f.sender, 1)

	f.clock.Advance(time.Minute)
	got := requireResponses(t, f.sender, 2)
	if got[1].Text != "walk the dog" {
		t.Errorf("text = %q, want %q", got[1].Text, "walk the dog")
	}

	// The series keeps going the next day.
	f.clock.Advance(24 * time.Hour)
	requireResponses(t, f.sender, 3)

	if rec := f.store.only(t); !rec.FiredAt.IsZero() {
		t.Fatalf("fired_at = %v, want the series still pending", rec.FiredAt)
	}
}

// TestRecurringForms checks every supported repeat rule reaches the store with
// the right kind and fields.
func TestRecurringForms(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		text      string
		kind      string
		weekdays  int
		atMinutes int
		interval  time.Duration
		rule      string
	}{
		{
			name:      "daily",
			text:      "remind me water plants every day at 09:00",
			kind:      store.ReminderDaily,
			atMinutes: 9 * 60,
			rule:      "every day at 09:00",
		},
		{
			name:      "weekdays",
			text:      "remind me stretch every weekday at 7:30",
			kind:      store.ReminderWeekdays,
			atMinutes: 7*60 + 30,
			rule:      "every weekday at 07:30",
		},
		{
			name:      "weekly full name",
			text:      "remind me call mom every monday at 18:00",
			kind:      store.ReminderWeekly,
			weekdays:  1 << time.Monday,
			atMinutes: 18 * 60,
			rule:      "every Monday at 18:00",
		},
		{
			name:      "weekly short name and pm clock",
			text:      "remind me review notes every tue at 6:15pm",
			kind:      store.ReminderWeekly,
			weekdays:  1 << time.Tuesday,
			atMinutes: 18*60 + 15,
			rule:      "every Tuesday at 18:15",
		},
		{
			name:     "interval hours",
			text:     "remind me stand up every 2 hours",
			kind:     store.ReminderInterval,
			interval: 2 * time.Hour,
			rule:     "every 2 hours",
		},
		{
			name:     "interval minutes",
			text:     "remind me breathe every 90 minutes",
			kind:     store.ReminderInterval,
			interval: 90 * time.Minute,
			rule:     "every 90 minutes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecurringFixture(t, start)
			f.remind(t, tc.text)

			rec := f.store.only(t)
			if rec.Kind != tc.kind || rec.Weekdays != tc.weekdays ||
				rec.AtMinutes != tc.atMinutes || rec.IntervalSeconds != int(tc.interval/time.Second) {
				t.Fatalf("stored schedule = %+v", rec)
			}
			if ack := f.lastText(t); !strings.Contains(ack, tc.rule) {
				t.Errorf("ack = %q, want rule %q", ack, tc.rule)
			}
		})
	}
}

// TestRecurringIntervalFires checks an interval rule repeats on its interval.
func TestRecurringIntervalFires(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := newRecurringFixture(t, start)
	f.remind(t, "remind me stand up every 2 hours")
	requireResponses(t, f.sender, 1)

	f.clock.Advance(2 * time.Hour)
	requireResponses(t, f.sender, 2)
	f.clock.Advance(2 * time.Hour)
	requireResponses(t, f.sender, 3)
}

// TestRecurringCountStops checks a trailing "N times" completes the series.
func TestRecurringCountStops(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := newRecurringFixture(t, start)

	f.remind(t, "remind me stretch every day at 07:30 2 times")
	ack := requireResponses(t, f.sender, 1)[0].Text
	if !strings.Contains(ack, "(2 times)") {
		t.Errorf("ack = %q, want the repeat count", ack)
	}
	if rec := f.store.only(t); rec.Remaining != 2 {
		t.Fatalf("remaining = %d, want 2", rec.Remaining)
	}

	f.clock.Advance(time.Hour + 30*time.Minute)
	requireResponses(t, f.sender, 2)
	if rec := f.store.only(t); rec.Remaining != 1 || !rec.FiredAt.IsZero() {
		t.Fatalf("after first send = %+v, want one left and pending", rec)
	}

	f.clock.Advance(24 * time.Hour)
	requireResponses(t, f.sender, 3)
	rec := f.store.only(t)
	if rec.Remaining != 0 || rec.FiredAt.IsZero() {
		t.Fatalf("after final send = %+v, want a completed series", rec)
	}

	f.clock.Advance(24 * time.Hour)
	requireResponses(t, f.sender, 3)
}

// TestRecurringUntilStops checks "until <date>" ends the series before that
// day starts.
func TestRecurringUntilStops(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := newRecurringFixture(t, start)

	f.remind(t, "remind me journal every day at 07:30 until 2026-01-08")
	ack := requireResponses(t, f.sender, 1)[0].Text
	if !strings.Contains(ack, "until 2026-01-08") {
		t.Errorf("ack = %q, want the end date", ack)
	}
	rec := f.store.only(t)
	wantEnd := time.Date(2026, 1, 8, 6, 0, 0, 0, time.UTC) // midnight CST
	if !rec.EndsAt.Equal(wantEnd) {
		t.Fatalf("ends_at = %v, want %v", rec.EndsAt, wantEnd)
	}

	// Jan 5, 6, and 7 at 07:30 CST fire; Jan 8 is past the end.
	f.clock.Advance(time.Hour + 30*time.Minute)
	requireResponses(t, f.sender, 2)
	f.clock.Advance(24 * time.Hour)
	requireResponses(t, f.sender, 3)
	f.clock.Advance(24 * time.Hour)
	requireResponses(t, f.sender, 4)
	f.clock.Advance(24 * time.Hour)
	requireResponses(t, f.sender, 4)

	if rec := f.store.only(t); rec.FiredAt.IsZero() {
		t.Fatalf("fired_at = zero, want a completed series")
	}
}

// TestRecurringDefaultLimit checks the configured default applies only when the
// command has no explicit limit.
func TestRecurringDefaultLimit(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	t.Run("default applies", func(t *testing.T) {
		f := newRecurringFixture(t, start)
		f.limit = "3 times"
		f.remind(t, "remind me water every day at 09:00")
		if rec := f.store.only(t); rec.Remaining != 3 {
			t.Fatalf("remaining = %d, want 3", rec.Remaining)
		}
	})

	t.Run("explicit count wins", func(t *testing.T) {
		f := newRecurringFixture(t, start)
		f.limit = "3 times"
		f.remind(t, "remind me water every day at 09:00 5 times")
		if rec := f.store.only(t); rec.Remaining != 5 {
			t.Fatalf("remaining = %d, want 5", rec.Remaining)
		}
	})

	t.Run("forever leaves it unlimited", func(t *testing.T) {
		f := newRecurringFixture(t, start)
		f.limit = "forever"
		f.remind(t, "remind me water every day at 09:00")
		if rec := f.store.only(t); rec.Remaining != store.UnlimitedReminders {
			t.Fatalf("remaining = %d, want unlimited", rec.Remaining)
		}
	})
}

// TestRecurringLiveTimezone checks a moved user keeps the wall-clock time from
// the next occurrence onward.
func TestRecurringLiveTimezone(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := newRecurringFixture(t, start)
	f.remind(t, "remind me walk the dog every day at 07:30")

	// First occurrence: 07:30 CST.
	f.clock.Advance(time.Hour + 30*time.Minute)
	requireResponses(t, f.sender, 2)

	// Move to Eastern before the next occurrence is computed.
	f.timezone = "America/New_York"

	// Jan 6 07:30 CST was already armed; it still fires at 13:30 UTC, and the
	// following occurrence is then computed as 07:30 EST (12:30 UTC).
	f.clock.Advance(24 * time.Hour)
	requireResponses(t, f.sender, 3)

	f.clock.Advance(23*time.Hour - time.Minute)
	requireResponses(t, f.sender, 3)
	f.clock.Advance(time.Minute)
	requireResponses(t, f.sender, 4)
}

// TestNextOccurrenceUsesLocation checks the pure rule keeps the wall clock.
func TestNextOccurrenceUsesLocation(t *testing.T) {
	rec := recurrence{kind: store.ReminderDaily, atMinutes: 7*60 + 30, remaining: store.UnlimitedReminders}
	after := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation(Chicago): %v", err)
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation(New_York): %v", err)
	}

	first, ok := nextOccurrence(rec, chicago, after)
	if !ok || !first.Equal(time.Date(2026, 1, 5, 13, 30, 0, 0, time.UTC)) {
		t.Fatalf("Chicago next = %v, %v; want 13:30 UTC", first, ok)
	}
	second, ok := nextOccurrence(rec, newYork, after)
	if !ok || !second.Equal(time.Date(2026, 1, 5, 12, 30, 0, 0, time.UTC)) {
		t.Fatalf("New York next = %v, %v; want 12:30 UTC", second, ok)
	}
}

// TestRecurringMissedOccurrenceFiresOnce checks a restart fires one late
// occurrence and then resumes at the next future one.
func TestRecurringMissedOccurrenceFiresOnce(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := newRecurringFixture(t, start)

	if _, err := f.store.InsertSchedule(context.Background(), store.ReminderRecord{
		Sender:    testSender,
		Satellite: testSatellite,
		Text:      "missed walk",
		DueAt:     start.Add(-72 * time.Hour),
		Kind:      store.ReminderDaily,
		AtMinutes: 7*60 + 30,
		Remaining: store.UnlimitedReminders,
	}); err != nil {
		t.Fatalf("InsertSchedule: %v", err)
	}
	if err := f.manager.RestoreReminders(context.Background()); err != nil {
		t.Fatalf("RestoreReminders: %v", err)
	}

	// The overdue row fires once at the next tick.
	f.clock.Advance(time.Second)
	got := requireResponses(t, f.sender, 1)
	if got[0].Text != "missed walk" {
		t.Fatalf("text = %q, want %q", got[0].Text, "missed walk")
	}

	// It does not replay the other missed days; the next fire is the next
	// future occurrence, today at 07:30 CST (13:30 UTC).
	f.clock.Advance(time.Hour)
	requireResponses(t, f.sender, 1)
	f.clock.Advance(30 * time.Minute)
	requireResponses(t, f.sender, 2)

	// The series then continues the following day.
	f.clock.Advance(24 * time.Hour)
	requireResponses(t, f.sender, 3)
}

// TestRecurringCancel checks both cancel spellings stop a recurring series.
func TestRecurringCancel(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	t.Run("remind cancel", func(t *testing.T) {
		f := newRecurringFixture(t, start)
		f.remind(t, "remind me walk the dog every day at 07:30")
		rec := f.store.only(t)

		f.remind(t, "remind cancel "+itoa(rec.ID))
		if pending, err := f.store.PendingReminders(context.Background()); err != nil || len(pending) != 0 {
			t.Fatalf("pending = %+v, %v; want empty", pending, err)
		}

		f.clock.Advance(48 * time.Hour)
		requireResponses(t, f.sender, 1)
	})

	t.Run("reminders cancel", func(t *testing.T) {
		f := newRecurringFixture(t, start)
		f.remind(t, "remind me walk the dog every day at 07:30")
		rec := f.store.only(t)

		if err := f.manager.HandleReminders(sdk.Transmission{
			Trigger:   "reminders",
			Text:      "reminders cancel " + itoa(rec.ID),
			Sender:    testSender,
			Satellite: testSatellite,
		}); err != nil {
			t.Fatalf("HandleReminders(cancel): %v", err)
		}
		if pending, err := f.store.PendingReminders(context.Background()); err != nil || len(pending) != 0 {
			t.Fatalf("pending = %+v, %v; want empty", pending, err)
		}
	})

	t.Run("another principal cannot cancel", func(t *testing.T) {
		f := newRecurringFixture(t, start)
		f.remind(t, "remind me walk the dog every day at 07:30")
		rec := f.store.only(t)

		err := f.manager.HandleRemind(sdk.Transmission{
			Trigger:   "remind",
			Text:      "remind cancel " + itoa(rec.ID),
			Sender:    "+15555550199",
			Satellite: testSatellite,
		})
		message, ok := sdk.AsUserMessage(err)
		if !ok || !strings.Contains(message, "No pending reminder") {
			t.Fatalf("cancel error = %v, want a user-facing miss", err)
		}
		if pending, _ := f.store.PendingReminders(context.Background()); len(pending) != 1 {
			t.Fatalf("pending = %+v, want the reminder kept", pending)
		}
	})

	t.Run("unknown id is user safe", func(t *testing.T) {
		f := newRecurringFixture(t, start)
		err := f.manager.HandleRemind(sdk.Transmission{
			Trigger:   "remind",
			Text:      "remind cancel 99",
			Sender:    testSender,
			Satellite: testSatellite,
		})
		message, ok := sdk.AsUserMessage(err)
		if !ok || !strings.Contains(message, "No pending reminder #99") {
			t.Fatalf("cancel error = %v, want a user-facing miss", err)
		}
	})
}

// TestRecurringListRendering checks the list shows the rule and its limit.
func TestRecurringListRendering(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := newRecurringFixture(t, start)
	ctx := context.Background()

	if _, err := f.store.InsertSchedule(ctx, store.ReminderRecord{
		Sender: testSender, Text: "walk the dog",
		DueAt: start.Add(time.Hour), Kind: store.ReminderDaily,
		AtMinutes: 7*60 + 30, Remaining: 3,
	}); err != nil {
		t.Fatalf("InsertSchedule(daily): %v", err)
	}
	if _, err := f.store.InsertSchedule(ctx, store.ReminderRecord{
		Sender: testSender, Text: "call mom",
		DueAt: start.Add(2 * time.Hour), Kind: store.ReminderWeekly,
		Weekdays: 1 << time.Monday, AtMinutes: 18 * 60,
		Remaining: store.UnlimitedReminders,
		EndsAt:    time.Date(2026, 6, 1, 5, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSchedule(weekly): %v", err)
	}

	if err := f.manager.HandleReminders(sdk.Transmission{
		Trigger:   "reminders",
		Text:      "reminders",
		Sender:    testSender,
		Satellite: testSatellite,
	}); err != nil {
		t.Fatalf("HandleReminders: %v", err)
	}

	list := f.lastText(t)
	for _, want := range []string{
		"#1 ",
		"walk the dog [every day at 07:30, 3 left]",
		"#2 ",
		"call mom [every Monday at 18:00, until 2026-06-01]",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("list = %q, want it to contain %q", list, want)
		}
	}
}

// TestRecurringSendRetries checks a failed send retries a bounded number of
// times and leaves the row pending for the next startup.
func TestRecurringSendRetries(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	f := newRecurringFixture(t, start)
	f.remind(t, "remind me walk the dog every day at 07:30")
	f.sender.err = errors.New("send failed")

	// First attempt at 07:30, then retries one minute apart.
	f.clock.Advance(time.Hour + 30*time.Minute)
	f.clock.Advance(time.Minute)
	f.clock.Advance(time.Minute)

	// The ack plus three attempts.
	requireResponses(t, f.sender, 4)

	// No further retries, and the row is still pending for the next startup.
	f.clock.Advance(24 * time.Hour)
	requireResponses(t, f.sender, 4)
	if rec := f.store.only(t); !rec.FiredAt.IsZero() {
		t.Fatalf("fired_at = %v, want the reminder left pending", rec.FiredAt)
	}
}

// itoa renders a reminder id without importing strconv in every test.
func itoa(id int64) string {
	return fmt.Sprintf("%d", id)
}
