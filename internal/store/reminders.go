package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Reminder kinds. An empty Kind is a one-shot reminder; the other kinds repeat.
const (
	ReminderDaily    = "daily"
	ReminderWeekdays = "weekdays"
	ReminderWeekly   = "weekly"
	ReminderInterval = "interval"
)

// UnlimitedReminders marks a recurring reminder that never ends on its own.
const UnlimitedReminders = -1

// reminderColumns is the projection every reminder query shares.
const reminderColumns = `id, sender, group_id, thread_id, satellite, text, due_at, created_at,
	fired_at, kind, weekdays, at_minutes, interval_seconds, remaining, ends_at, last_fired_at`

// ReminderRecord is one durable reminder. For a one-shot reminder FiredAt is
// zero while it is pending and stamped after the first successful send. For a
// recurring reminder DueAt is the next occurrence, LastFiredAt is the last
// successful send, and FiredAt is stamped only when the series ends.
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
	// Kind is "", ReminderDaily, ReminderWeekdays, ReminderWeekly, or
	// ReminderInterval.
	Kind string
	// Weekdays is a bitmask (1 << time.Weekday) for ReminderWeekly.
	Weekdays int
	// AtMinutes is the local wall-clock time for calendar kinds, in minutes
	// after midnight.
	AtMinutes int
	// IntervalSeconds is the elapsed interval for ReminderInterval.
	IntervalSeconds int
	// Remaining is the number of sends left, or UnlimitedReminders.
	Remaining int
	// EndsAt stops a recurring reminder before this instant. Zero means no
	// date limit.
	EndsAt time.Time
	// LastFiredAt is the last successful send of a recurring reminder.
	LastFiredAt time.Time
}

// Recurring reports whether the record repeats.
func (r ReminderRecord) Recurring() bool { return r.Kind != "" }

// InsertReminder appends a pending one-shot reminder and returns its row ID. A
// zero CreatedAt means now. Use InsertSchedule for recurring reminders.
func (s *Store) InsertReminder(ctx context.Context, rec ReminderRecord) (int64, error) {
	rec.Kind = ""
	rec.Weekdays = 0
	rec.AtMinutes = 0
	rec.IntervalSeconds = 0
	rec.Remaining = UnlimitedReminders
	rec.EndsAt = time.Time{}
	rec.LastFiredAt = time.Time{}
	return s.insertReminder(ctx, rec)
}

// InsertSchedule appends a pending recurring reminder and returns its row ID.
func (s *Store) InsertSchedule(ctx context.Context, rec ReminderRecord) (int64, error) {
	if strings.TrimSpace(rec.Sender) == "" {
		return 0, errors.New("store: reminder sender is empty")
	}
	if strings.TrimSpace(rec.Text) == "" {
		return 0, errors.New("store: reminder text is empty")
	}
	if rec.DueAt.IsZero() {
		return 0, errors.New("store: reminder due time is empty")
	}
	// A zero count is the struct's zero value, which reads as "no limit".
	if rec.Remaining == 0 {
		rec.Remaining = UnlimitedReminders
	}
	switch rec.Kind {
	case ReminderDaily, ReminderWeekdays, ReminderWeekly:
		if rec.AtMinutes < 0 || rec.AtMinutes >= 24*60 {
			return 0, fmt.Errorf("store: reminder at_minutes %d is out of range", rec.AtMinutes)
		}
		if rec.Kind == ReminderWeekly && rec.Weekdays == 0 {
			return 0, errors.New("store: weekly reminder has no weekday")
		}
		if rec.IntervalSeconds != 0 {
			return 0, errors.New("store: calendar reminder has an interval")
		}
	case ReminderInterval:
		if rec.IntervalSeconds <= 0 {
			return 0, errors.New("store: interval reminder has no interval")
		}
		if rec.AtMinutes != 0 || rec.Weekdays != 0 {
			return 0, errors.New("store: interval reminder has calendar fields")
		}
	default:
		return 0, fmt.Errorf("store: unknown reminder kind %q", rec.Kind)
	}
	if rec.Remaining != UnlimitedReminders && rec.Remaining <= 0 {
		return 0, fmt.Errorf("store: reminder remaining %d is not positive", rec.Remaining)
	}
	if !rec.EndsAt.IsZero() && !rec.DueAt.Before(rec.EndsAt) {
		return 0, errors.New("store: reminder has no occurrence before its end")
	}
	return s.insertReminder(ctx, rec)
}

// insertReminder writes one reminder row.
func (s *Store) insertReminder(ctx context.Context, rec ReminderRecord) (int64, error) {
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
		INSERT INTO reminders (
			sender, group_id, thread_id, satellite, text, due_at, created_at,
			kind, weekdays, at_minutes, interval_seconds, remaining, ends_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Sender,
		rec.GroupID,
		rec.ThreadID,
		rec.Satellite,
		rec.Text,
		rec.DueAt.UTC().Format(time.RFC3339),
		createdAt.UTC().Format(time.RFC3339),
		rec.Kind,
		rec.Weekdays,
		rec.AtMinutes,
		rec.IntervalSeconds,
		rec.Remaining,
		formatOptionalTime(rec.EndsAt),
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
// can deliver it late instead of dropping it. Recurring rows stay pending and
// carry their next occurrence in DueAt.
func (s *Store) PendingReminders(ctx context.Context) ([]ReminderRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: closed")
	}

	rows, err := s.db.QueryContext(ctx, `SELECT `+reminderColumns+`
		FROM reminders
		WHERE fired_at = ''
		ORDER BY due_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list pending reminders: %w", err)
	}
	defer rows.Close()

	return scanReminders(rows)
}

// PendingRemindersFor returns the pending reminders owned by one principal. A
// group principal owns the reminders set in that group; a direct principal owns
// reminders with no group. Results are oldest due first.
func (s *Store) PendingRemindersFor(ctx context.Context, principal string) ([]ReminderRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: closed")
	}
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `SELECT `+reminderColumns+`
		FROM reminders
		WHERE fired_at = ''
		  AND ((group_id <> '' AND group_id = ?) OR (group_id = '' AND sender = ?))
		ORDER BY due_at, id`, principal, principal)
	if err != nil {
		return nil, fmt.Errorf("store: list reminders for %q: %w", principal, err)
	}
	defer rows.Close()

	return scanReminders(rows)
}

// scanReminders reads the common reminder projection. The caller closes rows.
func scanReminders(rows *sql.Rows) ([]ReminderRecord, error) {
	var out []ReminderRecord
	for rows.Next() {
		rec, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate reminders: %w", err)
	}
	return out, nil
}

// scanReminder reads one reminder row in reminderColumns order.
func scanReminder(scanner interface{ Scan(...any) error }) (ReminderRecord, error) {
	var (
		rec                               ReminderRecord
		dueAt, createdAt, firedAt, endsAt string
		lastFiredAt                       string
	)
	if err := scanner.Scan(
		&rec.ID, &rec.Sender, &rec.GroupID, &rec.ThreadID, &rec.Satellite,
		&rec.Text, &dueAt, &createdAt, &firedAt, &rec.Kind, &rec.Weekdays,
		&rec.AtMinutes, &rec.IntervalSeconds, &rec.Remaining, &endsAt, &lastFiredAt,
	); err != nil {
		return ReminderRecord{}, fmt.Errorf("store: scan reminder: %w", err)
	}

	var err error
	if rec.DueAt, err = time.Parse(time.RFC3339, dueAt); err != nil {
		return ReminderRecord{}, fmt.Errorf("store: parse reminder %d due_at: %w", rec.ID, err)
	}
	if rec.CreatedAt, err = time.Parse(time.RFC3339, createdAt); err != nil {
		return ReminderRecord{}, fmt.Errorf("store: parse reminder %d created_at: %w", rec.ID, err)
	}
	if rec.FiredAt, err = parseOptionalTime(firedAt); err != nil {
		return ReminderRecord{}, fmt.Errorf("store: parse reminder %d fired_at: %w", rec.ID, err)
	}
	if rec.EndsAt, err = parseOptionalTime(endsAt); err != nil {
		return ReminderRecord{}, fmt.Errorf("store: parse reminder %d ends_at: %w", rec.ID, err)
	}
	if rec.LastFiredAt, err = parseOptionalTime(lastFiredAt); err != nil {
		return ReminderRecord{}, fmt.Errorf("store: parse reminder %d last_fired_at: %w", rec.ID, err)
	}
	return rec, nil
}

// MarkReminderFired stamps the first successful send of a one-shot reminder.
// It is idempotent: a second call leaves the original fired_at in place, so a
// duplicate callback cannot rewrite history. A zero at means now.
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

// AdvanceReminder records a successful recurring send and moves the row to its
// next occurrence. done stamps fired_at so the series stops appearing as
// pending. It reports false when the row is gone, which happens when the user
// cancelled the reminder while the send was in flight.
func (s *Store) AdvanceReminder(ctx context.Context, id int64, nextDue time.Time, remaining int, lastFired time.Time, done bool) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("store: closed")
	}
	if id <= 0 {
		return false, errors.New("store: reminder id is empty")
	}
	if lastFired.IsZero() {
		lastFired = time.Now()
	}
	if !done && nextDue.IsZero() {
		return false, errors.New("store: next occurrence is empty")
	}
	if done && nextDue.IsZero() {
		nextDue = lastFired
	}
	if remaining != UnlimitedReminders && remaining < 0 {
		return false, fmt.Errorf("store: reminder remaining %d is not positive", remaining)
	}

	var (
		result sql.Result
		err    error
	)
	if done {
		result, err = s.db.ExecContext(ctx, `
			UPDATE reminders
			SET due_at = ?, remaining = ?, last_fired_at = ?, fired_at = ?
			WHERE id = ? AND fired_at = ''`,
			nextDue.UTC().Format(time.RFC3339),
			remaining,
			lastFired.UTC().Format(time.RFC3339),
			lastFired.UTC().Format(time.RFC3339),
			id,
		)
	} else {
		result, err = s.db.ExecContext(ctx, `
			UPDATE reminders
			SET due_at = ?, remaining = ?, last_fired_at = ?
			WHERE id = ? AND fired_at = ''`,
			nextDue.UTC().Format(time.RFC3339),
			remaining,
			lastFired.UTC().Format(time.RFC3339),
			id,
		)
	}
	if err != nil {
		return false, fmt.Errorf("store: advance reminder %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: advance reminder %d: %w", id, err)
	}
	return affected > 0, nil
}

// CancelReminder deletes one pending reminder owned by principal. It reports
// false when the id is unknown, already fired, or owned by someone else.
func (s *Store) CancelReminder(ctx context.Context, id int64, principal string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("store: closed")
	}
	if id <= 0 {
		return false, errors.New("store: reminder id is empty")
	}
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return false, nil
	}

	result, err := s.db.ExecContext(ctx, `
		DELETE FROM reminders
		WHERE id = ? AND fired_at = ''
		  AND ((group_id <> '' AND group_id = ?) OR (group_id = '' AND sender = ?))`,
		id, principal, principal)
	if err != nil {
		return false, fmt.Errorf("store: cancel reminder %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: cancel reminder %d: %w", id, err)
	}
	return affected > 0, nil
}

// ReminderByID returns one reminder, including a fired row, for diagnostics
// and tests. It returns sql.ErrNoRows when the ID is unknown.
func (s *Store) ReminderByID(ctx context.Context, id int64) (*ReminderRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: closed")
	}

	row := s.db.QueryRowContext(ctx, `SELECT `+reminderColumns+`
		FROM reminders
		WHERE id = ?`, id)
	rec, err := scanReminder(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("store: get reminder %d: %w", id, err)
	}
	return &rec, nil
}

// formatOptionalTime renders a time column, using an empty string for zero.
func formatOptionalTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// parseOptionalTime reads a time column, treating an empty string as zero.
func parseOptionalTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}
