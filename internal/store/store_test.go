package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// expectedSchema is the durable shape of the four tables in plan §3.8. Later
// tasks query these columns by name, so the test asserts both the table set and
// the column order.
var expectedSchema = []struct {
	table   string
	columns []string
}{
	{
		table: "approvals",
		columns: []string{
			"id", "phone", "maintainer", "status",
			"requested_at", "resolved_at", "resolved_by", "display_name",
		},
	},
	{
		table: "inbound_dedupe",
		columns: []string{
			"sender", "message_id", "seen_at",
		},
	},
	{
		table: "patches",
		columns: []string{
			"id", "phone", "patch", "applied_by", "applied_at",
		},
	},
	{
		table: "route_log",
		columns: []string{
			"id", "direction", "sender", "recipient", "satellite",
			"thread", "text", "status", "error", "created_at", "message_id",
		},
	},
	{
		table: "satellites",
		columns: []string{
			"name", "trigger", "endpoint", "config",
			"registered_by", "created_at", "updated_at",
		},
	},
}

func TestMigrations(t *testing.T) {
	t.Run("memory database", func(t *testing.T) {
		st, err := Open(":memory:")
		if err != nil {
			t.Fatalf("Open(:memory:): %v", err)
		}
		t.Cleanup(func() {
			if err := st.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			t.Fatalf("Ping: %v", err)
		}
		assertSchema(t, st)
	})

	t.Run("idempotent across reopen", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "aether.db")

		for attempt := 1; attempt <= 2; attempt++ {
			st, err := Open(path)
			if err != nil {
				t.Fatalf("Open attempt %d: %v", attempt, err)
			}
			assertSchema(t, st)

			if err := st.Close(); err != nil {
				t.Fatalf("Close attempt %d: %v", attempt, err)
			}
		}
	})
}

func TestWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aether.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	var journalMode string
	if err := st.DB().QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := st.DB().QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}

	var foreignKeys int
	if err := st.DB().QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}

	// A single writer connection is what keeps SQLite contention predictable.
	if got := st.DB().Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", got)
	}
}

func assertSchema(t *testing.T, st *Store) {
	t.Helper()

	rows, err := st.DB().Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}

	var wantTables []string
	for _, want := range expectedSchema {
		wantTables = append(wantTables, want.table)
	}
	if !reflect.DeepEqual(tables, wantTables) {
		t.Fatalf("tables = %v, want %v", tables, wantTables)
	}

	for _, want := range expectedSchema {
		columns := tableColumns(t, st, want.table)
		if !reflect.DeepEqual(columns, want.columns) {
			t.Errorf("%s columns = %v, want %v", want.table, columns, want.columns)
		}
	}
}

func tableColumns(t *testing.T, st *Store, table string) []string {
	t.Helper()

	rows, err := st.DB().Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()

	var columns []string
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
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info(%s): %v", table, err)
	}
	return columns
}

// TestClaimInbound covers the dedupe primitive behind /receive: the first
// claim is fresh, repeats are duplicates, an absent message_id is always fresh,
// expired rows are reclaimed, and concurrent claims admit exactly one message.
func TestClaimInbound(t *testing.T) {
	const (
		sender = "+15555550102"
		ttl    = time.Minute
	)

	open := func(t *testing.T) *Store {
		t.Helper()
		st, err := Open(":memory:")
		if err != nil {
			t.Fatalf("Open(:memory:): %v", err)
		}
		t.Cleanup(func() {
			if err := st.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return st
	}

	t.Run("first claim is fresh and the second is a duplicate", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()

		fresh, err := st.ClaimInbound(ctx, sender, "msg-1", ttl)
		if err != nil {
			t.Fatalf("ClaimInbound(first): %v", err)
		}
		if !fresh {
			t.Fatal("ClaimInbound(first) = duplicate, want fresh")
		}

		fresh, err = st.ClaimInbound(ctx, sender, "msg-1", ttl)
		if err != nil {
			t.Fatalf("ClaimInbound(second): %v", err)
		}
		if fresh {
			t.Fatal("ClaimInbound(second) = fresh, want duplicate")
		}
	})

	t.Run("empty message id is always fresh and writes nothing", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()

		for attempt := 1; attempt <= 2; attempt++ {
			fresh, err := st.ClaimInbound(ctx, sender, "", ttl)
			if err != nil {
				t.Fatalf("ClaimInbound(empty attempt %d): %v", attempt, err)
			}
			if !fresh {
				t.Fatalf("ClaimInbound(empty attempt %d) = duplicate, want fresh", attempt)
			}
		}

		var count int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM inbound_dedupe`).Scan(&count); err != nil {
			t.Fatalf("count inbound_dedupe: %v", err)
		}
		if count != 0 {
			t.Fatalf("inbound_dedupe rows = %d, want 0 for an absent message_id", count)
		}
	})

	t.Run("expired claim is reclaimed", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()

		if fresh, err := st.ClaimInbound(ctx, sender, "msg-2", ttl); err != nil || !fresh {
			t.Fatalf("ClaimInbound(first) = %v, %v; want fresh", fresh, err)
		}

		old := time.Now().Add(-2 * ttl).UTC().Format(time.RFC3339)
		if _, err := st.DB().Exec(
			`UPDATE inbound_dedupe SET seen_at = ? WHERE sender = ? AND message_id = ?`,
			old, sender, "msg-2",
		); err != nil {
			t.Fatalf("backdate claim: %v", err)
		}

		fresh, err := st.ClaimInbound(ctx, sender, "msg-2", ttl)
		if err != nil {
			t.Fatalf("ClaimInbound(reclaimed): %v", err)
		}
		if !fresh {
			t.Fatal("ClaimInbound(reclaimed) = duplicate, want the expired row to be reclaimed")
		}
	})

	t.Run("claim survives a reopen", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dedupe.db")
		ctx := context.Background()

		first, err := Open(path)
		if err != nil {
			t.Fatalf("Open(first): %v", err)
		}
		fresh, err := first.ClaimInbound(ctx, sender, "msg-reopen", ttl)
		if err != nil || !fresh {
			t.Fatalf("ClaimInbound(first) = %v, %v; want fresh", fresh, err)
		}
		if err := first.Close(); err != nil {
			t.Fatalf("Close(first): %v", err)
		}

		second, err := Open(path)
		if err != nil {
			t.Fatalf("Open(second): %v", err)
		}
		t.Cleanup(func() {
			if err := second.Close(); err != nil {
				t.Errorf("Close(second): %v", err)
			}
		})

		fresh, err = second.ClaimInbound(ctx, sender, "msg-reopen", ttl)
		if err != nil {
			t.Fatalf("ClaimInbound(second): %v", err)
		}
		if fresh {
			t.Fatal("ClaimInbound(after reopen) = fresh, want the stored key to remain a duplicate")
		}
	})

	t.Run("concurrent claims admit exactly one", func(t *testing.T) {
		st := open(t)
		const claims = 16
		var wg sync.WaitGroup
		results := make(chan bool, claims)
		errs := make(chan error, claims)
		for i := 0; i < claims; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fresh, err := st.ClaimInbound(context.Background(), sender, "msg-3", ttl)
				if err != nil {
					errs <- err
					return
				}
				results <- fresh
			}()
		}
		wg.Wait()
		close(results)
		close(errs)

		for err := range errs {
			t.Fatalf("concurrent ClaimInbound: %v", err)
		}

		freshCount := 0
		for fresh := range results {
			if fresh {
				freshCount++
			}
		}
		if freshCount != 1 {
			t.Fatalf("fresh claims = %d, want exactly 1", freshCount)
		}
	})
}

// TestMigrationAddsMessageIDToExistingRouteLog proves A04 upgrades a database
// created before the column existed without losing its rows. SQLite lacks
// ADD COLUMN IF NOT EXISTS, so Open inspects the table first.
func TestMigrationAddsMessageIDToExistingRouteLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE route_log (
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
	)`); err != nil {
		t.Fatalf("create old route_log: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO route_log (direction, status, created_at) VALUES ('inbound', 'dispatched', '2026-01-05T12:00:00Z')`); err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old database: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open(existing): %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	var (
		createdAt string
		messageID string
	)
	if err := st.DB().QueryRow(`SELECT created_at, message_id FROM route_log WHERE id = 1`).
		Scan(&createdAt, &messageID); err != nil {
		t.Fatalf("read upgraded route_log: %v", err)
	}
	if createdAt != "2026-01-05T12:00:00Z" {
		t.Errorf("created_at = %q, want the original row preserved", createdAt)
	}
	if messageID != "" {
		t.Errorf("message_id = %q, want the default empty string", messageID)
	}
}
