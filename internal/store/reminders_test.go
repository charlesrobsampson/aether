package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestReminders(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	due := time.Date(2026, 11, 17, 13, 0, 0, 0, time.UTC)
	created := time.Date(2026, 11, 16, 17, 41, 0, 0, time.UTC)
	id, err := st.InsertReminder(ctx, ReminderRecord{
		Sender:    "+15555550102",
		GroupID:   "group.example",
		ThreadID:  "errand",
		Satellite: "remind",
		Text:      "wash your ring",
		DueAt:     due,
		CreatedAt: created,
	})
	if err != nil {
		t.Fatalf("InsertReminder: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id = %d, want a positive row id", id)
	}

	pending, err := st.PendingReminders(ctx)
	if err != nil {
		t.Fatalf("PendingReminders: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	got := pending[0]
	if got.ID != id || got.Sender != "+15555550102" || got.GroupID != "group.example" ||
		got.ThreadID != "errand" || got.Satellite != "remind" || got.Text != "wash your ring" {
		t.Fatalf("pending = %+v", got)
	}
	if !got.DueAt.Equal(due) || !got.CreatedAt.Equal(created) {
		t.Fatalf("times = due %v created %v, want %v / %v", got.DueAt, got.CreatedAt, due, created)
	}

	firedAt := time.Date(2026, 11, 17, 13, 0, 3, 0, time.UTC)
	if err := st.MarkReminderFired(ctx, id, firedAt); err != nil {
		t.Fatalf("MarkReminderFired: %v", err)
	}
	if pending, err = st.PendingReminders(ctx); err != nil {
		t.Fatalf("PendingReminders after fire: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after fire = %d, want 0", len(pending))
	}

	// A second mark must not overwrite the first send time.
	if err := st.MarkReminderFired(ctx, id, firedAt.Add(time.Hour)); err != nil {
		t.Fatalf("MarkReminderFired second: %v", err)
	}
	rec, err := st.ReminderByID(ctx, id)
	if err != nil {
		t.Fatalf("ReminderByID: %v", err)
	}
	if !rec.FiredAt.Equal(firedAt) {
		t.Fatalf("fired_at = %v, want %v", rec.FiredAt, firedAt)
	}
}

func TestPendingRemindersForPrincipal(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	rows := []ReminderRecord{
		{Sender: "+15555550102", Text: "mine later", DueAt: base.Add(2 * time.Hour)},
		{Sender: "+15555550102", Text: "mine sooner", DueAt: base.Add(time.Hour)},
		{Sender: "+15555550102", GroupID: "group.one", Text: "group one", DueAt: base},
		{Sender: "+15555550103", Text: "someone else", DueAt: base},
	}
	for _, rec := range rows {
		if _, err := st.InsertReminder(ctx, rec); err != nil {
			t.Fatalf("InsertReminder(%q): %v", rec.Text, err)
		}
	}

	got, err := st.PendingRemindersFor(ctx, "+15555550102")
	if err != nil {
		t.Fatalf("PendingRemindersFor(direct): %v", err)
	}
	if len(got) != 2 || got[0].Text != "mine sooner" || got[1].Text != "mine later" {
		t.Fatalf("direct pending = %+v, want [mine sooner, mine later]", got)
	}

	got, err = st.PendingRemindersFor(ctx, "group.one")
	if err != nil {
		t.Fatalf("PendingRemindersFor(group): %v", err)
	}
	if len(got) != 1 || got[0].Text != "group one" {
		t.Fatalf("group pending = %+v, want [group one]", got)
	}

	if got, err = st.PendingRemindersFor(ctx, ""); err != nil || len(got) != 0 {
		t.Fatalf("PendingRemindersFor(empty) = %+v, %v; want empty", got, err)
	}
}

func TestInsertReminderValidation(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	due := time.Date(2026, 11, 17, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		rec  ReminderRecord
	}{
		{"missing sender", ReminderRecord{Text: "x", DueAt: due}},
		{"missing text", ReminderRecord{Sender: "+15555550102", DueAt: due}},
		{"missing due", ReminderRecord{Sender: "+15555550102", Text: "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.InsertReminder(ctx, tc.rec); err == nil {
				t.Fatal("InsertReminder = nil, want error")
			}
		})
	}
}

func TestReminderByIDMissing(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.ReminderByID(ctx, 999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ReminderByID(999) = %v, want sql.ErrNoRows", err)
	}
}

func TestInsertScheduleRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	due := time.Date(2026, 11, 18, 13, 30, 0, 0, time.UTC)
	created := time.Date(2026, 11, 17, 17, 41, 0, 0, time.UTC)
	endsAt := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	id, err := st.InsertSchedule(ctx, ReminderRecord{
		Sender:    "+15555550102",
		Text:      "walk the dog",
		DueAt:     due,
		CreatedAt: created,
		Kind:      ReminderWeekdays,
		AtMinutes: 7*60 + 30,
		Remaining: 10,
		EndsAt:    endsAt,
	})
	if err != nil {
		t.Fatalf("InsertSchedule: %v", err)
	}

	rec, err := st.ReminderByID(ctx, id)
	if err != nil {
		t.Fatalf("ReminderByID: %v", err)
	}
	if rec.Kind != ReminderWeekdays || rec.AtMinutes != 7*60+30 || rec.Remaining != 10 {
		t.Fatalf("schedule = %+v", rec)
	}
	if !rec.Recurring() {
		t.Error("Recurring() = false, want true")
	}
	if !rec.DueAt.Equal(due) || !rec.EndsAt.Equal(endsAt) || !rec.CreatedAt.Equal(created) {
		t.Fatalf("times = due %v ends %v created %v", rec.DueAt, rec.EndsAt, rec.CreatedAt)
	}
	if !rec.FiredAt.IsZero() || !rec.LastFiredAt.IsZero() {
		t.Fatalf("fired = %v last = %v, want zero", rec.FiredAt, rec.LastFiredAt)
	}

	pending, err := st.PendingReminders(ctx)
	if err != nil {
		t.Fatalf("PendingReminders: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("pending = %+v, want the schedule", pending)
	}
}

func TestInsertScheduleValidation(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	due := time.Date(2026, 11, 17, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		rec  ReminderRecord
	}{
		{"missing sender", ReminderRecord{Text: "x", DueAt: due, Kind: ReminderDaily}},
		{"missing text", ReminderRecord{Sender: "+15555550102", DueAt: due, Kind: ReminderDaily}},
		{"missing due", ReminderRecord{Sender: "+15555550102", Text: "x", Kind: ReminderDaily}},
		{"unknown kind", ReminderRecord{Sender: "+15555550102", Text: "x", DueAt: due, Kind: "hourly"}},
		{"daily without clock", ReminderRecord{Sender: "+15555550102", Text: "x", DueAt: due, Kind: ReminderDaily, AtMinutes: -1}},
		{"weekly without weekday", ReminderRecord{Sender: "+15555550102", Text: "x", DueAt: due, Kind: ReminderWeekly, AtMinutes: 540}},
		{"interval without interval", ReminderRecord{Sender: "+15555550102", Text: "x", DueAt: due, Kind: ReminderInterval}},
		{"interval with calendar fields", ReminderRecord{Sender: "+15555550102", Text: "x", DueAt: due, Kind: ReminderInterval, IntervalSeconds: 60, AtMinutes: 540}},
		{"negative remaining", ReminderRecord{Sender: "+15555550102", Text: "x", DueAt: due, Kind: ReminderDaily, AtMinutes: 540, Remaining: -2}},
		{"end before first", ReminderRecord{Sender: "+15555550102", Text: "x", DueAt: due, Kind: ReminderDaily, AtMinutes: 540, EndsAt: due}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.InsertSchedule(ctx, tc.rec); err == nil {
				t.Fatal("InsertSchedule = nil, want error")
			}
		})
	}
}

func TestAdvanceAndCancelReminder(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	base := time.Date(2026, 11, 17, 13, 0, 0, 0, time.UTC)
	id, err := st.InsertSchedule(ctx, ReminderRecord{
		Sender:          "+15555550102",
		Text:            "stand up",
		DueAt:           base,
		Kind:            ReminderInterval,
		IntervalSeconds: 3600,
		Remaining:       2,
	})
	if err != nil {
		t.Fatalf("InsertSchedule: %v", err)
	}

	// A successful send moves the row and decrements the count.
	fired := base.Add(time.Minute)
	next := base.Add(time.Hour)
	found, err := st.AdvanceReminder(ctx, id, next, 1, fired, false)
	if err != nil || !found {
		t.Fatalf("AdvanceReminder = %v, %v; want true, nil", found, err)
	}
	rec, err := st.ReminderByID(ctx, id)
	if err != nil {
		t.Fatalf("ReminderByID: %v", err)
	}
	if !rec.DueAt.Equal(next) || rec.Remaining != 1 || !rec.LastFiredAt.Equal(fired) {
		t.Fatalf("advanced = %+v", rec)
	}
	if !rec.FiredAt.IsZero() {
		t.Fatalf("fired_at = %v, want zero while pending", rec.FiredAt)
	}

	// The final send completes the series.
	fired2 := next.Add(time.Minute)
	found, err = st.AdvanceReminder(ctx, id, time.Time{}, 0, fired2, true)
	if err != nil || !found {
		t.Fatalf("AdvanceReminder(done) = %v, %v; want true, nil", found, err)
	}
	rec, err = st.ReminderByID(ctx, id)
	if err != nil {
		t.Fatalf("ReminderByID after done: %v", err)
	}
	if rec.FiredAt.IsZero() || !rec.FiredAt.Equal(fired2) {
		t.Fatalf("fired_at = %v, want %v", rec.FiredAt, fired2)
	}
	pending, err := st.PendingReminders(ctx)
	if err != nil {
		t.Fatalf("PendingReminders: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want empty after the series ends", pending)
	}

	// A second advance cannot revive a completed series.
	found, err = st.AdvanceReminder(ctx, id, next, 1, fired2, false)
	if err != nil || found {
		t.Fatalf("AdvanceReminder(after done) = %v, %v; want false, nil", found, err)
	}
}

func TestCancelReminderScopesToPrincipal(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	base := time.Date(2026, 11, 17, 13, 0, 0, 0, time.UTC)
	direct, err := st.InsertReminder(ctx, ReminderRecord{Sender: "+15555550102", Text: "direct", DueAt: base})
	if err != nil {
		t.Fatalf("InsertReminder(direct): %v", err)
	}
	group, err := st.InsertReminder(ctx, ReminderRecord{Sender: "+15555550102", GroupID: "group.one", Text: "group", DueAt: base})
	if err != nil {
		t.Fatalf("InsertReminder(group): %v", err)
	}
	schedule, err := st.InsertSchedule(ctx, ReminderRecord{
		Sender: "+15555550102", Text: "recurring", DueAt: base,
		Kind: ReminderDaily, AtMinutes: 540,
	})
	if err != nil {
		t.Fatalf("InsertSchedule: %v", err)
	}

	// Another principal cannot cancel these rows.
	for _, id := range []int64{direct, group, schedule} {
		if ok, err := st.CancelReminder(ctx, id, "+15555550199"); err != nil || ok {
			t.Fatalf("CancelReminder(%d, other) = %v, %v; want false, nil", id, ok, err)
		}
	}

	// A direct reminder belongs to its sender, a group reminder to the group.
	if ok, err := st.CancelReminder(ctx, direct, "+15555550102"); err != nil || !ok {
		t.Fatalf("CancelReminder(direct) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := st.CancelReminder(ctx, group, "group.one"); err != nil || !ok {
		t.Fatalf("CancelReminder(group) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := st.CancelReminder(ctx, schedule, "+15555550102"); err != nil || !ok {
		t.Fatalf("CancelReminder(schedule) = %v, %v; want true, nil", ok, err)
	}
	pending, err := st.PendingReminders(ctx)
	if err != nil {
		t.Fatalf("PendingReminders: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want empty", pending)
	}

	// A second cancel reports the row is gone.
	if ok, err := st.CancelReminder(ctx, direct, "+15555550102"); err != nil || ok {
		t.Fatalf("CancelReminder(twice) = %v, %v; want false, nil", ok, err)
	}
}
