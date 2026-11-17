package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReminderRecord is one durable reminder. FiredAt is zero while the reminder
// is still pending; the first successful send stamps it so a restart cannot
// deliver the same reminder twice.
type ReminderRecord struct {
	ID        int64
	Sender    string
	GroupID   string
	ThreadID  string
	Satellite string
	Text      string
	DueAt     time.Time
	CreatedAt time.Time
	FiredAt   time.Time
}

// InsertReminder appends a pending reminder and returns its row ID. A zero
// CreatedAt means now.
func (s *Store) InsertReminder(ctx context.Context, rec ReminderRecord) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store: closed")
	}
	if strings.TrimSpace(rec.Sender) == "" {
		return 0, errors.New("store: reminder sender is empty")
	}
	if strings.TrimSpace(rec.Text) == "" {
		return 0, errors.New("store: reminder text is empty")
	}
	if rec.DueAt.IsZero() {
		return 0, errors.New("store: reminder due time is empty")
	}

	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO reminders (sender, group_id, thread_id, satellite, text, due_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.Sender,
		rec.GroupID,
		rec.ThreadID,
		rec.Satellite,
		rec.Text,
		rec.DueAt.UTC().Format(time.RFC3339),
		createdAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert reminder: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: reminder id: %w", err)
	}
	return id, nil
}

// PendingReminders returns the reminders that have not fired, oldest due
// first. A reminder that was due while Aether was down is included so startup
// can deliver it late instead of dropping it.
func (s *Store) PendingReminders(ctx context.Context) ([]ReminderRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: closed")
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, sender, group_id, thread_id, satellite, text, due_at, created_at
		FROM reminders
		WHERE fired_at = ''
		ORDER BY due_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list pending reminders: %w", err)
	}
	defer rows.Close()

	var out []ReminderRecord
	for rows.Next() {
		var (
			rec              ReminderRecord
			dueAt, createdAt string
		)
		if err := rows.Scan(
			&rec.ID, &rec.Sender, &rec.GroupID, &rec.ThreadID, &rec.Satellite,
			&rec.Text, &dueAt, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan reminder: %w", err)
		}
		if rec.DueAt, err = time.Parse(time.RFC3339, dueAt); err != nil {
			return nil, fmt.Errorf("store: parse reminder %d due_at: %w", rec.ID, err)
		}
		if rec.CreatedAt, err = time.Parse(time.RFC3339, createdAt); err != nil {
			return nil, fmt.Errorf("store: parse reminder %d created_at: %w", rec.ID, err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate reminders: %w", err)
	}
	return out, nil
}

// MarkReminderFired stamps the first successful send. It is idempotent: a
// second call leaves the original fired_at in place, so a duplicate callback
// cannot rewrite history. A zero at means now.
func (s *Store) MarkReminderFired(ctx context.Context, id int64, at time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("store: closed")
	}
	if id <= 0 {
		return errors.New("store: reminder id is empty")
	}
	if at.IsZero() {
		at = time.Now()
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE reminders
		SET fired_at = ?
		WHERE id = ? AND fired_at = ''`,
		at.UTC().Format(time.RFC3339), id,
	); err != nil {
		return fmt.Errorf("store: mark reminder %d fired: %w", id, err)
	}
	return nil
}

// ReminderByID returns one reminder, including a fired row, for diagnostics
// and tests. It returns sql.ErrNoRows when the ID is unknown.
func (s *Store) ReminderByID(ctx context.Context, id int64) (*ReminderRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: closed")
	}

	var (
		rec                       ReminderRecord
		dueAt, createdAt, firedAt string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, sender, group_id, thread_id, satellite, text, due_at, created_at, fired_at
		FROM reminders
		WHERE id = ?`, id,
	).Scan(
		&rec.ID, &rec.Sender, &rec.GroupID, &rec.ThreadID, &rec.Satellite,
		&rec.Text, &dueAt, &createdAt, &firedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("store: get reminder %d: %w", id, err)
	}
	if rec.DueAt, err = time.Parse(time.RFC3339, dueAt); err != nil {
		return nil, fmt.Errorf("store: parse reminder %d due_at: %w", id, err)
	}
	if rec.CreatedAt, err = time.Parse(time.RFC3339, createdAt); err != nil {
		return nil, fmt.Errorf("store: parse reminder %d created_at: %w", id, err)
	}
	if firedAt != "" {
		if rec.FiredAt, err = time.Parse(time.RFC3339, firedAt); err != nil {
			return nil, fmt.Errorf("store: parse reminder %d fired_at: %w", id, err)
		}
	}
	return &rec, nil
}
