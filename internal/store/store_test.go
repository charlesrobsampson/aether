package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
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
			"requested_at", "resolved_at", "resolved_by",
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
			"thread", "text", "status", "error", "created_at",
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
