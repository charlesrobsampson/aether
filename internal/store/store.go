// Package store owns Aether's SQLite persistence.
//
// Open uses the pure-Go modernc.org/sqlite driver, enables WAL mode and a
// 5-second busy timeout, and runs the schema migrations from plan §3.8. The
// pool is capped at one connection so SQLite has a single writer, which is what
// keeps contention and SQLITE_BUSY errors predictable.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Store wraps the SQLite handle. Close it when the process exits.
type Store struct {
	db *sql.DB
}

// Open opens the SQLite database at dsn, configures it, and migrates the
// schema. Use ":memory:" for a throwaway database in tests.
//
// Open is idempotent: opening an existing database a second time re-runs the
// CREATE ... IF NOT EXISTS migrations without changing existing data.
func Open(dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("store: empty DSN")
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", dsn, err)
	}

	// A single connection is both the single-writer guarantee and the reason
	// the connection-scoped pragmas below stay in effect for the Store's life.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	st := &Store{db: db}
	if err := st.configure(); err != nil {
		db.Close()
		return nil, err
	}
	if err := st.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

// DB exposes the underlying handle for queries. Callers must leave the pool
// settings alone; use Store.Close to release it.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// SatelliteRecord is the durable mirror of one satellites/<name>.json document
// in the SQLite registry (plan §3.8).
type SatelliteRecord struct {
	Name         string
	Trigger      string
	Endpoint     string
	Config       []byte
	RegisteredBy string
}

// UpsertSatellite inserts or updates the registry row for rec. The file on disk
// remains the source of truth; this mirror exists for queries and audit.
func (s *Store) UpsertSatellite(ctx context.Context, rec SatelliteRecord) error {
	if s == nil || s.db == nil {
		return errors.New("store: closed")
	}
	if rec.Name == "" {
		return errors.New("store: satellite name is empty")
	}
	if rec.Trigger == "" {
		return errors.New("store: satellite trigger is empty")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO satellites (name, "trigger", endpoint, config, registered_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			"trigger"     = excluded."trigger",
			endpoint      = excluded.endpoint,
			config        = excluded.config,
			registered_by = excluded.registered_by,
			updated_at    = excluded.updated_at`,
		rec.Name, rec.Trigger, rec.Endpoint, string(rec.Config), rec.RegisteredBy, now, now,
	)
	if err != nil {
		return fmt.Errorf("store: upsert satellite %q: %w", rec.Name, err)
	}
	return nil
}

// RouteLog is one inbound or outbound audit event (plan §3.8). Times are stored
// as UTC RFC3339; a zero CreatedAt means "now". MessageID is the inbound
// transport's upstream id, when it supplied one.
type RouteLog struct {
	Direction string
	Sender    string
	Recipient string
	Satellite string
	Thread    string
	Text      string
	Status    string
	Error     string
	MessageID string
	CreatedAt time.Time
}

// LogRoute appends one audit row to route_log. Direction and Status are
// required; the remaining fields may be empty.
func (s *Store) LogRoute(ctx context.Context, rec RouteLog) error {
	if s == nil || s.db == nil {
		return errors.New("store: closed")
	}
	if rec.Direction == "" {
		return errors.New("store: route log direction is empty")
	}
	if rec.Status == "" {
		return errors.New("store: route log status is empty")
	}

	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO route_log
			(direction, sender, recipient, satellite, thread, text, status, error, message_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Direction, rec.Sender, rec.Recipient, rec.Satellite, rec.Thread,
		rec.Text, rec.Status, rec.Error, rec.MessageID,
		createdAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("store: log route: %w", err)
	}
	return nil
}

// DefaultDedupeTTL is the fallback inbound dedupe window when the caller does
// not supply one (plan §3.2, §3.14).
const DefaultDedupeTTL = 15 * time.Minute

// ClaimInbound records an inbound (sender, message_id) pair and reports whether
// it is fresh. It returns true when the message should be dispatched and false
// when the same pair was seen within ttl. An empty messageID is always fresh
// and writes nothing (best-effort dedupe). Expired rows are deleted
// opportunistically, so a key can be reclaimed after the TTL.
func (s *Store) ClaimInbound(ctx context.Context, sender, messageID string, ttl time.Duration) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("store: closed")
	}
	if messageID == "" {
		return true, nil
	}
	if ttl <= 0 {
		ttl = DefaultDedupeTTL
	}

	now := time.Now().UTC()
	cutoff := now.Add(-ttl).Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: claim inbound: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM inbound_dedupe WHERE seen_at < ?`, cutoff,
	); err != nil {
		return false, fmt.Errorf("store: claim inbound: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO inbound_dedupe (sender, message_id, seen_at)
		VALUES (?, ?, ?)
		ON CONFLICT(sender, message_id) DO NOTHING`,
		sender, messageID, now.Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("store: claim inbound: %w", err)
	}

	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim inbound: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: claim inbound: %w", err)
	}
	return inserted == 1, nil
}

// ReleaseInbound removes a claim so a retry can be accepted after the message
// was not actually queued (for example /receive returned 429). An empty
// messageID is a no-op.
func (s *Store) ReleaseInbound(ctx context.Context, sender, messageID string) error {
	if s == nil || s.db == nil {
		return errors.New("store: closed")
	}
	if messageID == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM inbound_dedupe WHERE sender = ? AND message_id = ?`,
		sender, messageID,
	); err != nil {
		return fmt.Errorf("store: release inbound: %w", err)
	}
	return nil
}

// PatchRecord is one applied user-configuration patch (plan §3.8). Patch holds
// the RFC6902 operations that changed the file, as JSON.
type PatchRecord struct {
	Phone     string
	Patch     []byte
	AppliedBy string
	AppliedAt time.Time
}

// LogPatch appends one audit row to patches. Phone and a non-empty Patch are
// required; a zero AppliedAt means now. The row records who applied the patch,
// not who it was applied to, which is why AppliedBy is separate from Phone.
func (s *Store) LogPatch(ctx context.Context, rec PatchRecord) error {
	if s == nil || s.db == nil {
		return errors.New("store: closed")
	}
	if rec.Phone == "" {
		return errors.New("store: patch phone is empty")
	}
	if len(rec.Patch) == 0 {
		return errors.New("store: patch is empty")
	}

	appliedAt := rec.AppliedAt
	if appliedAt.IsZero() {
		appliedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO patches (phone, patch, applied_by, applied_at)
		VALUES (?, ?, ?, ?)`,
		rec.Phone, string(rec.Patch), rec.AppliedBy,
		appliedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("store: log patch: %w", err)
	}
	return nil
}

func (s *Store) configure() error {
	// busy_timeout must be set before any statement that can wait on a lock.
	for _, stmt := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("store: configure %q: %w", stmt, err)
		}
	}

	// journal_mode returns the resulting mode as a row, so query it rather than
	// using Exec. ":memory:" databases report "memory"; file databases report
	// "wal".
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
		return fmt.Errorf("store: enable WAL: %w", err)
	}
	return nil
}

func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin migrations: %w", err)
	}

	for _, stmt := range migrations {
		if _, err := tx.Exec(stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	// SQLite has no ADD COLUMN IF NOT EXISTS, so older databases need the
	// route_log.message_id column added explicitly. Fresh databases already
	// have it from the CREATE TABLE above.
	if err := addColumnIfMissing(tx, "route_log", "message_id",
		`ALTER TABLE route_log ADD COLUMN message_id TEXT NOT NULL DEFAULT ''`); err != nil {
		tx.Rollback()
		return fmt.Errorf("store: migrate: %w", err)
	}
	// Approvals gained a display name so maintainer notifications and pending
	// listings can show who is asking, not just an opaque UUID.
	if err := addColumnIfMissing(tx, "approvals", "display_name",
		`ALTER TABLE approvals ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`); err != nil {
		tx.Rollback()
		return fmt.Errorf("store: migrate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migrations: %w", err)
	}
	return nil
}

// addColumnIfMissing inspects table's columns and runs alter only when the
// column is absent. table, column, and the DDL are compile-time constants.
func addColumnIfMissing(tx *sql.Tx, table, column, alter string) error {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			columnTyp string
			notNull   int
			defaultV  any
			primary   int
		)
		if err := rows.Scan(&cid, &name, &columnTyp, &notNull, &defaultV, &primary); err != nil {
			return fmt.Errorf("inspect %s: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	// Release the read cursor before altering the table.
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	if _, err := tx.Exec(alter); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// migrations is ordered and additive. Every statement uses IF NOT EXISTS so a
// second Open is a no-op.
//
// Timestamps are stored as UTC RFC3339 text (plan §1.1). "trigger" is a SQLite
// keyword and therefore quoted.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS route_log (
		id         INTEGER PRIMARY KEY,
		direction  TEXT NOT NULL,
		sender     TEXT NOT NULL DEFAULT '',
		recipient  TEXT NOT NULL DEFAULT '',
		satellite  TEXT NOT NULL DEFAULT '',
		thread     TEXT NOT NULL DEFAULT '',
		text       TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL,
		error      TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		message_id TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_route_log_created_at ON route_log (created_at)`,

	`CREATE TABLE IF NOT EXISTS inbound_dedupe (
		sender     TEXT NOT NULL,
		message_id TEXT NOT NULL,
		seen_at    TEXT NOT NULL,
		PRIMARY KEY (sender, message_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_inbound_dedupe_seen_at ON inbound_dedupe (seen_at)`,

	`CREATE TABLE IF NOT EXISTS satellites (
		name          TEXT PRIMARY KEY,
		"trigger"     TEXT NOT NULL UNIQUE,
		endpoint      TEXT NOT NULL,
		config        TEXT NOT NULL,
		registered_by TEXT NOT NULL DEFAULT '',
		created_at    TEXT NOT NULL,
		updated_at    TEXT NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS patches (
		id         INTEGER PRIMARY KEY,
		phone      TEXT NOT NULL,
		patch      TEXT NOT NULL,
		applied_by TEXT NOT NULL DEFAULT '',
		applied_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_patches_phone ON patches (phone)`,

	`CREATE TABLE IF NOT EXISTS approvals (
		id           INTEGER PRIMARY KEY,
		phone        TEXT NOT NULL,
		maintainer   TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL,
		requested_at TEXT NOT NULL,
		resolved_at  TEXT,
		resolved_by  TEXT,
		display_name TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_approvals_phone_status ON approvals (phone, status)`,
}
