package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestTempConfig(t *testing.T) {
	homeBefore, homeSetBefore := os.LookupEnv("HOME")
	envBefore, envSetBefore := os.LookupEnv("AETHER_CONFIG_DIR")

	t.Run("copies and restores", func(t *testing.T) {
		srcRoot := repoExamplesConfig(t)
		root := TempConfig(t)

		if root == "" {
			t.Fatal("TempConfig returned an empty root")
		}
		if filepath.Clean(root) == filepath.Clean(srcRoot) {
			t.Fatalf("TempConfig returned the source tree %q instead of a copy", root)
		}
		if got := os.Getenv("AETHER_CONFIG_DIR"); got != root {
			t.Fatalf("AETHER_CONFIG_DIR = %q, want %q", got, root)
		}

		// Every file in the example tree must be copied byte-for-byte.
		err := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(srcRoot, path)
			if err != nil {
				return err
			}
			want, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			got, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				return fmt.Errorf("missing copied file %s: %w", rel, err)
			}
			if !bytes.Equal(got, want) {
				return fmt.Errorf("copied file %s differs from the source", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("copied config tree is incomplete: %v", err)
		}

		// WriteJSON must create parent directories and valid JSON.
		written := filepath.Join(root, "users", "written.json")
		WriteJSON(t, written, map[string]any{"timezone": "UTC", "muted": true})
		raw, err := os.ReadFile(written)
		if err != nil {
			t.Fatalf("read WriteJSON output: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("WriteJSON output is not valid JSON: %v\n%s", err, raw)
		}
		if decoded["timezone"] != "UTC" || decoded["muted"] != true {
			t.Fatalf("WriteJSON round trip = %#v", decoded)
		}
		if _, err := os.Stat(filepath.Join(srcRoot, "users", "written.json")); !os.IsNotExist(err) {
			t.Fatalf("WriteJSON touched the source tree: stat error = %v", err)
		}

		homeAfter, homeSetAfter := os.LookupEnv("HOME")
		if homeSetAfter != homeSetBefore || homeAfter != homeBefore {
			t.Fatalf("TempConfig changed HOME: before=(%q,%v) after=(%q,%v)",
				homeBefore, homeSetBefore, homeAfter, homeSetAfter)
		}
	})

	// t.Setenv's cleanup runs when the subtest ends; the outer test can now
	// observe that the environment was restored.
	envAfter, envSetAfter := os.LookupEnv("AETHER_CONFIG_DIR")
	if envSetAfter != envSetBefore || envAfter != envBefore {
		t.Fatalf("AETHER_CONFIG_DIR was not restored: before=(%q,%v) after=(%q,%v)",
			envBefore, envSetBefore, envAfter, envSetAfter)
	}
	homeAfter, homeSetAfter := os.LookupEnv("HOME")
	if homeSetAfter != homeSetBefore || homeAfter != homeBefore {
		t.Fatalf("HOME was not restored: before=(%q,%v) after=(%q,%v)",
			homeBefore, homeSetBefore, homeAfter, homeSetAfter)
	}
}

func TestFakeClock(t *testing.T) {
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	clock := NewFakeClock(start)

	if got := clock.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}

	var mu sync.Mutex
	var fired []string
	record := func(name string) func() {
		return func() {
			mu.Lock()
			fired = append(fired, name)
			mu.Unlock()
		}
	}
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), fired...)
	}

	// Register out of deadline order to prove Advance sorts by deadline and
	// breaks ties by registration order.
	clock.AfterFunc(2*time.Second, record("2s"))
	clock.AfterFunc(time.Second, record("1s-a"))
	clock.AfterFunc(time.Second, record("1s-b"))

	clock.Advance(time.Second)
	if got, want := snapshot(), []string{"1s-a", "1s-b"}; !slices.Equal(got, want) {
		t.Fatalf("after Advance(1s) fired %v, want %v", got, want)
	}
	if got := clock.Now(); !got.Equal(start.Add(time.Second)) {
		t.Fatalf("Now() after Advance(1s) = %v, want %v", got, start.Add(time.Second))
	}

	clock.Advance(time.Second)
	if got, want := snapshot(), []string{"1s-a", "1s-b", "2s"}; !slices.Equal(got, want) {
		t.Fatalf("after Advance(2s) fired %v, want %v", got, want)
	}
	if got := clock.Now(); !got.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("Now() after Advance(2s) = %v, want %v", got, start.Add(2*time.Second))
	}

	// Advancing past the last deadline must not fire anything twice.
	clock.Advance(10 * time.Second)
	if got := snapshot(); len(got) != 3 {
		t.Fatalf("timers fired more than once: %v", got)
	}
	if got := clock.Now(); !got.Equal(start.Add(12 * time.Second)) {
		t.Fatalf("Now() after Advance(12s) = %v, want %v", got, start.Add(12*time.Second))
	}

	// Cancel prevents firing and is idempotent.
	cancelled := false
	cancel := clock.AfterFunc(time.Second, func() { cancelled = true })
	cancel()
	cancel()
	clock.Advance(time.Second)
	if cancelled {
		t.Fatal("cancelled timer fired")
	}

	// A timer registered from inside a callback still fires in the same
	// Advance when its deadline falls within the requested window.
	var nested []string
	clock.AfterFunc(time.Second, func() {
		nested = append(nested, "outer")
		clock.AfterFunc(time.Second, func() { nested = append(nested, "inner") })
	})
	clock.Advance(2 * time.Second)
	if want := []string{"outer", "inner"}; !slices.Equal(nested, want) {
		t.Fatalf("nested timers fired %v, want %v", nested, want)
	}
}
