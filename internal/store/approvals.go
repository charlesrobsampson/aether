package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Approval statuses mirror plan §3.5: a new-number request starts pending and
// is resolved to approved, denied, or expired. Expired rows are kept as
// history; a later message from the same phone creates a new pending row.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
)

// ApprovalRecord is one row from the approvals table (plan §3.8). ResolvedAt
// and ResolvedBy are zero/empty while the request is pending.
type ApprovalRecord struct {
	ID          int64
	Phone       string
	Maintainer  string
	Status      string
	RequestedAt time.Time
	ResolvedAt  time.Time
	ResolvedBy  string
}

// InsertApproval appends one request and returns its row ID. An empty Status
// means pending; a zero RequestedAt means now. The caller owns rotation and
// notification; this method only persists the row.
func (s *Store) InsertApproval(ctx context.Context, rec ApprovalRecord) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store: closed")
	}
	if strings.TrimSpace(rec.Phone) == "" {
		return 0, errors.New("store: approval phone is empty")
	}

	status := rec.Status
	if status == "" {
		status = ApprovalPending
	}
	if !validApprovalStatus(status) {
		return 0, fmt.Errorf("store: invalid approval status %q", status)
	}

	requestedAt := rec.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}

	var resolvedAt any
	if !rec.ResolvedAt.IsZero() {
		resolvedAt = rec.ResolvedAt.UTC().Format(time.RFC3339)
	}
	var resolvedBy any
	if rec.ResolvedBy != "" {
		resolvedBy = rec.ResolvedBy
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals
			(phone, maintainer, status, requested_at, resolved_at, resolved_by)
		VALUES (?, ?, ?, ?, ?, ?)`,
		rec.Phone, rec.Maintainer, status,
		requestedAt.UTC().Format(time.RFC3339), resolvedAt, resolvedBy,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert approval for %q: %w", rec.Phone, err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: approval id: %w", err)
	}
	return id, nil
}

// PendingApproval returns the newest pending request for phone, or nil when
// there is none.
func (s *Store) PendingApproval(ctx context.Context, phone string) (*ApprovalRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: closed")
	}
	if strings.TrimSpace(phone) == "" {
		return nil, errors.New("store: approval phone is empty")
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT id, phone, maintainer, status, requested_at,
		       COALESCE(resolved_at, ''), COALESCE(resolved_by, '')
		FROM approvals
		WHERE phone = ? AND status = ?
		ORDER BY id DESC
		LIMIT 1`, phone, ApprovalPending)

	rec, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: pending approval for %q: %w", phone, err)
	}
	return &rec, nil
}

// PendingApprovals returns every pending request, oldest first.
func (s *Store) PendingApprovals(ctx context.Context) ([]ApprovalRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: closed")
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, phone, maintainer, status, requested_at,
		       COALESCE(resolved_at, ''), COALESCE(resolved_by, '')
		FROM approvals
		WHERE status = ?
		ORDER BY requested_at, id`, ApprovalPending)
	if err != nil {
		return nil, fmt.Errorf("store: list pending approvals: %w", err)
	}
	defer rows.Close()

	var out []ApprovalRecord
	for rows.Next() {
		rec, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan pending approval: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pending approvals: %w", err)
	}
	return out, nil
}

// ResolveApprovals marks every pending request for phone with status. It is
// idempotent: when nothing is pending the update affects zero rows and returns
// nil. A zero resolvedAt means now.
func (s *Store) ResolveApprovals(ctx context.Context, phone, status, resolvedBy string, resolvedAt time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store: closed")
	}
	if strings.TrimSpace(phone) == "" {
		return 0, errors.New("store: approval phone is empty")
	}
	if status == ApprovalPending || !validApprovalStatus(status) {
		return 0, fmt.Errorf("store: invalid resolved approval status %q", status)
	}

	if resolvedAt.IsZero() {
		resolvedAt = time.Now()
	}
	var resolvedByValue any
	if resolvedBy != "" {
		resolvedByValue = resolvedBy
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE approvals
		SET status = ?, resolved_at = ?, resolved_by = ?
		WHERE phone = ? AND status = ?`,
		status, resolvedAt.UTC().Format(time.RFC3339), resolvedByValue,
		phone, ApprovalPending,
	)
	if err != nil {
		return 0, fmt.Errorf("store: resolve approvals for %q: %w", phone, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: resolve approvals rows: %w", err)
	}
	return affected, nil
}

// ExpireApprovals marks pending requests older than before as expired. The rows
// stay in the table as history. It returns how many rows changed.
func (s *Store) ExpireApprovals(ctx context.Context, before time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store: closed")
	}
	if before.IsZero() {
		return 0, errors.New("store: expiry cutoff is zero")
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE approvals
		SET status = ?, resolved_at = ?, resolved_by = NULL
		WHERE status = ? AND requested_at < ?`,
		ApprovalExpired, time.Now().UTC().Format(time.RFC3339),
		ApprovalPending, before.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("store: expire approvals: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: expire approvals rows: %w", err)
	}
	return affected, nil
}

func validApprovalStatus(status string) bool {
	switch status {
	case ApprovalPending, ApprovalApproved, ApprovalDenied, ApprovalExpired:
		return true
	default:
		return false
	}
}

type approvalScanner interface {
	Scan(dest ...any) error
}

func scanApproval(row approvalScanner) (ApprovalRecord, error) {
	var rec ApprovalRecord
	var requestedAt, resolvedAt string
	if err := row.Scan(
		&rec.ID, &rec.Phone, &rec.Maintainer, &rec.Status,
		&requestedAt, &resolvedAt, &rec.ResolvedBy,
	); err != nil {
		return ApprovalRecord{}, err
	}

	parsed, err := time.Parse(time.RFC3339, requestedAt)
	if err != nil {
		return ApprovalRecord{}, fmt.Errorf("parse requested_at %q: %w", requestedAt, err)
	}
	rec.RequestedAt = parsed

	if resolvedAt != "" {
		parsed, err = time.Parse(time.RFC3339, resolvedAt)
		if err != nil {
			return ApprovalRecord{}, fmt.Errorf("parse resolved_at %q: %w", resolvedAt, err)
		}
		rec.ResolvedAt = parsed
	}
	return rec, nil
}
