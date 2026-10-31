package config

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/testutil"
)

const watchDebounceForTest = 50 * time.Millisecond

// fakeWatcher is a deterministic event source for exercising Watch without a
// real filesystem watcher or any wall-clock delay.
type fakeWatcher struct {
	events chan struct{}
}

func (f *fakeWatcher) Events() <-chan struct{} { return f.events }

// signalClock wraps testutil.FakeClock and reports every timer registration so
// the test can advance the clock only after Watch has armed its debounce timer.
type signalClock struct {
	*testutil.FakeClock
	scheduled chan struct{}
}

func newSignalClock(start time.Time) *signalClock {
	return &signalClock{
		FakeClock: testutil.NewFakeClock(start),
		scheduled: make(chan struct{}, 16),
	}
}

func (c *signalClock) AfterFunc(d time.Duration, f func()) func() {
	cancel := c.FakeClock.AfterFunc(d, f)
	c.scheduled <- struct{}{}
	return cancel
}

func waitReload(t *testing.T, reloads <-chan *Config) *Config {
	t.Helper()
	select {
	case cfg := <-reloads:
		return cfg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for config reload")
		return nil
	}
}

func waitLoadError(t *testing.T, errs <-chan error) error {
	t.Helper()
	select {
	case err := <-errs:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a load error")
		return nil
	}
}

func TestWatchReload(t *testing.T) {
	root := t.TempDir()
	systemPath := filepath.Join(root, "system.json")
	writeConfigFile(t, systemPath, `{"server": {"port": 8080}}`)

	initial, err := LoadConfig(root)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	snapshot := NewSnapshot(initial)

	source := &fakeWatcher{events: make(chan struct{})}
	clock := newSignalClock(time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC))
	reloads := make(chan *Config, 8)
	loadErrors := make(chan error, 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Watch(
			ctx,
			source,
			func() (*Config, error) { return LoadConfig(root) },
			func(cfg *Config) {
				snapshot.Store(cfg)
				reloads <- cfg
			},
			WithClock(clock),
			WithDebounce(watchDebounceForTest),
			WithErrorHandler(func(err error) { loadErrors <- err }),
		)
	}()

	// A burst of notifications must collapse into exactly one reload once the
	// debounce window elapses.
	writeConfigFile(t, systemPath, `{"server": {"port": 9090}}`)
	for i := 0; i < 3; i++ {
		source.events <- struct{}{}
		<-clock.scheduled
	}
	clock.Advance(watchDebounceForTest)

	reloaded := waitReload(t, reloads)
	if got := reloaded.System.Server.Port; got != 9090 {
		t.Fatalf("reloaded port = %d, want 9090", got)
	}
	if got := snapshot.Load().System.Server.Port; got != 9090 {
		t.Fatalf("snapshot port = %d, want 9090", got)
	}
	select {
	case extra := <-reloads:
		t.Fatalf("burst produced more than one reload: port %d", extra.System.Server.Port)
	default:
	}
	select {
	case err := <-loadErrors:
		t.Fatalf("unexpected load error: %v", err)
	default:
	}

	// A malformed edit must report the load error but leave the previous
	// snapshot untouched.
	writeConfigFile(t, systemPath, `{"server": `)
	source.events <- struct{}{}
	<-clock.scheduled
	clock.Advance(watchDebounceForTest)

	loadErr := waitLoadError(t, loadErrors)
	if !strings.Contains(loadErr.Error(), "system.json") {
		t.Fatalf("load error = %q, want a system.json parse error", loadErr)
	}
	if got := snapshot.Load().System.Server.Port; got != 9090 {
		t.Fatalf("malformed edit replaced snapshot: port = %d, want 9090", got)
	}
	select {
	case cfg := <-reloads:
		t.Fatalf("malformed edit triggered onReload with port %d", cfg.System.Server.Port)
	default:
	}

	// Fixing the file resumes reloading against the same snapshot.
	writeConfigFile(t, systemPath, `{"server": {"port": 9091}}`)
	source.events <- struct{}{}
	<-clock.scheduled
	clock.Advance(watchDebounceForTest)

	reloaded = waitReload(t, reloads)
	if got := reloaded.System.Server.Port; got != 9091 {
		t.Fatalf("recovered port = %d, want 9091", got)
	}
	if got := snapshot.Load().System.Server.Port; got != 9091 {
		t.Fatalf("snapshot after recovery = %d, want 9091", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch returned error after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}
}

func TestSnapshotAtomicSwap(t *testing.T) {
	first := &Config{System: SystemConfig{Server: ServerConfig{Port: 8080}}}
	second := &Config{System: SystemConfig{Server: ServerConfig{Port: 9090}}}
	snapshot := NewSnapshot(first)

	stop := make(chan struct{})
	bad := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			cfg := snapshot.Load()
			if cfg == nil {
				select {
				case bad <- "snapshot was nil":
				default:
				}
				return
			}
			if port := cfg.System.Server.Port; port != 8080 && port != 9090 {
				select {
				case bad <- "snapshot held an unknown config":
				default:
				}
				return
			}
		}
	}()

	for i := 0; i < 1000; i++ {
		if i%2 == 0 {
			snapshot.Store(second)
		} else {
			snapshot.Store(first)
		}
	}
	close(stop)
	<-done

	select {
	case msg := <-bad:
		t.Fatal(msg)
	default:
	}
	if got := snapshot.Load().System.Server.Port; got != 8080 {
		t.Fatalf("final snapshot port = %d, want 8080", got)
	}
}

func TestFSWatcherLifecycle(t *testing.T) {
	if _, err := NewFSWatcher(""); err == nil {
		t.Fatal("NewFSWatcher(\"\") returned no error")
	}
	if _, err := NewFSWatcher(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("NewFSWatcher(missing) returned no error")
	}

	root := t.TempDir()
	testutil.WriteJSON(t, filepath.Join(root, "system.json"), map[string]any{"server": map[string]any{"port": 8080}})
	watcher, err := NewFSWatcher(root)
	if err != nil {
		t.Fatalf("NewFSWatcher(%q): %v", root, err)
	}
	if watcher.Events() == nil {
		t.Fatal("Events() returned a nil channel")
	}
	if err := watcher.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatalf("second Close(): %v", err)
	}
}
