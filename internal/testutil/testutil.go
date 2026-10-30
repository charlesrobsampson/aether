// Package testutil provides hermetic helpers shared by Aether tests.
//
// Nothing in this package reads $HOME or touches the network. Tests should use
// TempConfig to obtain an isolated configuration tree and FakeClock whenever a
// test needs to control time.
package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TempConfig copies the repository's examples/config tree into a fresh
// t.TempDir, points AETHER_CONFIG_DIR at the copy, and returns the copy root.
//
// The environment change is made with t.Setenv, so it is restored automatically
// when the test (or subtest) that called TempConfig finishes. TempConfig never
// reads or writes $HOME.
func TempConfig(t *testing.T) string {
	t.Helper()

	src := repoExamplesConfig(t)
	root := t.TempDir()
	if err := copyTree(src, root); err != nil {
		t.Fatalf("testutil: copy %s to %s: %v", src, root, err)
	}
	t.Setenv("AETHER_CONFIG_DIR", root)
	return root
}

// WriteJSON marshals v as indented JSON, creates any missing parent
// directories, and writes the result to path. It fails the test on error.
func WriteJSON(t *testing.T, path string, v any) {
	t.Helper()

	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("testutil: marshal JSON for %s: %v", path, err)
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("testutil: create parent directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("testutil: write %s: %v", path, err)
	}
}

// FakeClock is a deterministic clock for tests. It is safe for concurrent use.
//
// Advance fires every timer whose deadline falls within the advanced window,
// in deadline order. Ties are broken by registration order. The clock's Now
// moves to each timer's deadline before its callback runs, so callbacks that
// schedule more timers observe a sensible time. Advance runs callbacks
// synchronously; do not call Advance from inside a callback.
type FakeClock struct {
	mu      sync.Mutex
	advance sync.Mutex
	now     time.Time
	seq     uint64
	timers  []*fakeTimer
}

// fakeTimer is the timer handle returned indirectly through the cancel func
// from FakeClock.AfterFunc.
type fakeTimer struct {
	clock    *FakeClock
	deadline time.Time
	seq      uint64
	fn       func()
	stopped  bool
	fired    bool
}

// NewFakeClock returns a FakeClock whose initial time is start.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

// Now returns the clock's current time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc registers f to run after d has elapsed on the fake clock. The
// returned function cancels the timer; it is safe to call more than once.
//
// The unnamed func() return type is deliberate: it lets FakeClock satisfy a
// package-local Clock interface such as
//
//	type Clock interface {
//	    Now() time.Time
//	    AfterFunc(time.Duration, func()) func()
//	}
//
// without an adapter in every test.
func (c *FakeClock) AfterFunc(d time.Duration, f func()) func() {
	if f == nil {
		panic("testutil: nil AfterFunc callback")
	}

	c.mu.Lock()
	c.seq++
	timer := &fakeTimer{
		clock:    c,
		deadline: c.now.Add(d),
		seq:      c.seq,
		fn:       f,
	}
	c.timers = append(c.timers, timer)
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !timer.fired && !timer.stopped {
			timer.stopped = true
		}
	}
}

// Advance moves the clock forward by d and fires all timers that become due.
// Timers fire exactly once, in deadline order.
func (c *FakeClock) Advance(d time.Duration) {
	if d < 0 {
		panic("testutil: negative Advance")
	}

	c.advance.Lock()
	defer c.advance.Unlock()

	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()

	for {
		c.mu.Lock()
		next := c.nextDue(target)
		if next == nil {
			c.now = target
			c.compactLocked()
			c.mu.Unlock()
			return
		}
		next.fired = true
		if next.deadline.After(c.now) {
			c.now = next.deadline
		}
		fn := next.fn
		c.mu.Unlock()

		fn()
	}
}

// nextDue returns the earliest pending timer due at or before target. The
// caller must hold c.mu.
func (c *FakeClock) nextDue(target time.Time) *fakeTimer {
	var next *fakeTimer
	for _, timer := range c.timers {
		if timer.stopped || timer.fired || timer.deadline.After(target) {
			continue
		}
		if next == nil ||
			timer.deadline.Before(next.deadline) ||
			(timer.deadline.Equal(next.deadline) && timer.seq < next.seq) {
			next = timer
		}
	}
	return next
}

// compactLocked drops fired and cancelled timers. The caller must hold c.mu.
func (c *FakeClock) compactLocked() {
	kept := c.timers[:0]
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired {
			kept = append(kept, timer)
		}
	}
	c.timers = kept
}

func repoExamplesConfig(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("testutil: runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	src := filepath.Join(root, "examples", "config")

	info, err := os.Stat(src)
	if err != nil {
		t.Fatalf("testutil: locate examples/config: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("testutil: %s is not a directory", src)
	}
	return src
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are not supported: %s", path)
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
