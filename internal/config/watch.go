package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultWatchDebounce is the quiet period Watch waits for after a change
// notification before it reloads the configuration. It collapses the several
// filesystem events a single editor save can produce into one reload.
const defaultWatchDebounce = 100 * time.Millisecond

// fsWatcherDebounce is the equivalent quiet period used by FSWatcher before it
// publishes one notification on Events.
const fsWatcherDebounce = 100 * time.Millisecond

// Watcher is a source of configuration change notifications. Every value
// received on Events is treated as a hint that the configuration tree changed.
type Watcher interface {
	Events() <-chan struct{}
}

// Clock abstracts the timer used by Watch's debounce loop so tests can drive it
// with a fake clock instead of sleeping. testutil.FakeClock satisfies this
// interface.
type Clock interface {
	AfterFunc(time.Duration, func()) func()
}

// Snapshot holds the active configuration and swaps it atomically. Readers see
// either the previous whole config or the new whole config, never a partially
// updated value.
type Snapshot struct {
	current atomic.Pointer[Config]
}

// NewSnapshot returns a Snapshot initialized with cfg.
func NewSnapshot(cfg *Config) *Snapshot {
	snapshot := &Snapshot{}
	snapshot.Store(cfg)
	return snapshot
}

// Load returns the current configuration. It returns nil only when the
// snapshot was created or stored with a nil config.
func (s *Snapshot) Load() *Config {
	if s == nil {
		return nil
	}
	return s.current.Load()
}

// Store atomically replaces the current configuration.
func (s *Snapshot) Store(cfg *Config) {
	if s == nil {
		return
	}
	s.current.Store(cfg)
}

// WatchOption configures Watch.
type WatchOption func(*watchOptions)

type watchOptions struct {
	debounce time.Duration
	clock    Clock
	onError  func(error)
}

// WithDebounce sets the quiet period Watch waits for after the last change
// notification before reloading. A negative duration is treated as zero.
func WithDebounce(d time.Duration) WatchOption {
	return func(options *watchOptions) {
		options.debounce = d
	}
}

// WithClock replaces the timer used by Watch. It is intended for tests.
func WithClock(clock Clock) WatchOption {
	return func(options *watchOptions) {
		options.clock = clock
	}
}

// WithErrorHandler installs a callback for load failures. When a reload fails,
// Watch keeps the previous snapshot and continues waiting for changes.
func WithErrorHandler(handler func(error)) WatchOption {
	return func(options *watchOptions) {
		options.onError = handler
	}
}

type realClock struct{}

func (realClock) AfterFunc(d time.Duration, f func()) func() {
	timer := time.AfterFunc(d, f)
	return func() { timer.Stop() }
}

// Watch reloads the configuration whenever source reports a change. Bursts of
// notifications are debounced into one load call. A successful load is passed
// to onReload; a failed load is reported to the handler installed with
// WithErrorHandler and leaves the previous snapshot untouched.
//
// Watch blocks until ctx is cancelled or source closes its Events channel. It
// returns nil in both cases so callers can treat cancellation as a clean
// shutdown.
func Watch(ctx context.Context, source Watcher, load func() (*Config, error), onReload func(*Config), opts ...WatchOption) error {
	if source == nil {
		return errors.New("config: watch source is nil")
	}
	if load == nil {
		return errors.New("config: watch load function is nil")
	}

	options := watchOptions{
		debounce: defaultWatchDebounce,
		clock:    realClock{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if options.clock == nil {
		options.clock = realClock{}
	}
	if options.debounce < 0 {
		options.debounce = 0
	}

	// pending holds the newest generation whose debounce timer fired. notify
	// is a one-slot edge trigger, so a stale generation can never hide a
	// newer one: the loop always re-reads pending after waking.
	var pending atomic.Uint64
	notify := make(chan struct{}, 1)

	var generation uint64
	var cancel func()
	stopTimer := func() {
		if cancel != nil {
			cancel()
			cancel = nil
		}
	}
	defer stopTimer()

	events := source.Events()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-events:
			if !ok {
				return nil
			}
			generation++
			current := generation
			stopTimer()
			cancel = options.clock.AfterFunc(options.debounce, func() {
				pending.Store(current)
				select {
				case notify <- struct{}{}:
				default:
				}
			})
		case <-notify:
			if pending.Load() != generation {
				continue
			}
			cancel = nil
			cfg, err := load()
			if err != nil {
				if options.onError != nil {
					options.onError(err)
				}
				continue
			}
			if onReload != nil {
				onReload(cfg)
			}
		}
	}
}

// FSWatcher watches a configuration directory tree with fsnotify and publishes
// one debounced notification per burst of filesystem changes.
type FSWatcher struct {
	watcher   *fsnotify.Watcher
	events    chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

var _ Watcher = (*FSWatcher)(nil)

// NewFSWatcher starts watching root and all of its subdirectories. The caller
// must call Close when done.
func NewFSWatcher(root string) (*FSWatcher, error) {
	if root == "" {
		return nil, errors.New("config: watcher root is empty")
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("config: watch root %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("config: watch root %s is not a directory", root)
	}

	raw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("config: create fsnotify watcher: %w", err)
	}
	if err := addWatchDirs(raw, root); err != nil {
		raw.Close()
		return nil, err
	}

	watcher := &FSWatcher{
		watcher: raw,
		events:  make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	watcher.wg.Add(1)
	go watcher.run()
	return watcher, nil
}

// Events returns the channel of debounced change notifications.
func (w *FSWatcher) Events() <-chan struct{} {
	return w.events
}

// Close stops watching. It is safe to call more than once.
func (w *FSWatcher) Close() error {
	w.closeOnce.Do(func() {
		close(w.done)
		w.closeErr = w.watcher.Close()
		w.wg.Wait()
		close(w.events)
	})
	return w.closeErr
}

func (w *FSWatcher) run() {
	defer w.wg.Done()

	timer := time.NewTimer(fsWatcherDebounce)
	if !timer.Stop() {
		<-timer.C
	}
	var timerC <-chan time.Time
	defer timer.Stop()

	for {
		select {
		case <-w.done:
			return
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					// A new subdirectory appears; watch it too so files
					// created inside it produce notifications.
					_ = w.watcher.Add(event.Name)
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(fsWatcherDebounce)
			timerC = timer.C
		case _, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
		case <-timerC:
			timerC = nil
			select {
			case w.events <- struct{}{}:
			default:
			}
		}
	}
}

// addWatchDirs adds root and every directory below it to raw.
func addWatchDirs(raw *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("config: walk watch root %s: %w", root, err)
		}
		if !entry.IsDir() {
			return nil
		}
		if err := raw.Add(path); err != nil {
			return fmt.Errorf("config: watch %s: %w", path, err)
		}
		return nil
	})
}
