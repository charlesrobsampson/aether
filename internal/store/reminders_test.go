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
