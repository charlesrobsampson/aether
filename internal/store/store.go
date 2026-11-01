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
// as UTC RFC3339; a zero CreatedAt means "now".
type RouteLog struct {
	Direction string
	Sender    string
	Recipient string
	Satellite string
	Thread    string
	Text      string
	Status    string
	Error     string
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
			(direction, sender, recipient, satellite, thread, text, status, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Direction, rec.Sender, rec.Recipient, rec.Satellite, rec.Thread,
		rec.Text, rec.Status, rec.Error, createdAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("store: log route: %w", err)
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migrations: %w", err)
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
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_route_log_created_at ON route_log (created_at)`,

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
		resolved_by  TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_approvals_phone_status ON approvals (phone, status)`,
}
