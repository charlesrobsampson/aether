// Package app wires Aether's configuration, storage, routing, scheduling, and
// HTTP surface into one runnable process.
//
// Run owns the lifecycle: it loads and watches the config tree, opens SQLite,
// starts the outbound queue and scheduler, registers the built-in satellites,
// serves the single API mux, and drains everything on context cancellation.
// Tests inject a fake transport.Messenger, a Clock, and a config.Watcher, so
// nothing here reads $HOME, sleeps, or touches the network.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesrobsampson/aether/internal/api"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/metrics"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/queue"
	"github.com/charlesrobsampson/aether/internal/router"
	"github.com/charlesrobsampson/aether/internal/satellite/adminmgr"
	"github.com/charlesrobsampson/aether/internal/satellite/configmgr"
	"github.com/charlesrobsampson/aether/internal/satellite/testsat"
	"github.com/charlesrobsampson/aether/internal/scheduler"
	"github.com/charlesrobsampson/aether/internal/session"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/internal/transport"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

const (
	// shutdownTimeout bounds the drain of the HTTP server and outbound queue.
	// The base Run context may already be cancelled, so shutdown uses a fresh
	// background context.
	shutdownTimeout = 10 * time.Second
	// dispatchTimeout bounds one scheduler dispatch, matching the router's
	// satellite POST timeout.
	dispatchTimeout = 5 * time.Second
	// defaultPort is used when system.json does not set server.port.
	defaultPort = 8080
)

// Clock is the injectable time source shared by sessions, the queue's rate
// limiter, the scheduler, the admin manager, and config watching.
// *testutil.FakeClock satisfies it.
type Clock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) func()
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, fn func()) func() {
	timer := time.AfterFunc(d, fn)
	return func() { timer.Stop() }
}

// Options configures Run.
type Options struct {
	// ConfigDir is the AETHER_CONFIG_DIR root. Empty uses config.ResolveRoot.
	ConfigDir string
	// Addr is the HTTP listen address. Empty uses server.port from
	// system.json. ":0" asks the kernel for a free port.
	Addr string
	// Messenger performs outbound delivery. Nil builds the configured msg
	// provider, so production needs no injection.
	Messenger transport.Messenger
	// Clock defaults to the system clock.
	Clock Clock
	// Watcher is the config change source. Nil starts an fsnotify watcher on
	// ConfigDir; tests inject a fake to drive reloads deterministically.
	Watcher config.Watcher
	// OnReady, when set, receives the bound listen address after the listener
	// is open and all wiring is complete. Tests use it instead of sleeping.
	OnReady func(addr string)
	// OnReload, when set, receives each successfully reloaded configuration.
	OnReload func(*config.Config)
}

// Run starts Aether and blocks until ctx is cancelled or the HTTP server stops.
// It returns nil after a clean shutdown.
func Run(ctx context.Context, opts Options) error {
	if ctx == nil {
		ctx = context.Background()
	}

	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}

	root := strings.TrimSpace(opts.ConfigDir)
	if root == "" {
		root = config.ResolveRoot()
	}

	initial, err := loadConfig(root)
	if err != nil {
		return err
	}

	st, err := openStore(initial)
	if err != nil {
		return err
	}
	defer st.Close()

	messenger := opts.Messenger
	if messenger == nil {
		messenger, err = newMessenger(initial)
		if err != nil {
			return err
		}
	}

	snapshot := config.NewSnapshot(initial)
	sessions := session.New(clock)
	sched := scheduler.New(clock)

	limits := initial.System.Limits
	q, err := queue.New(queue.Options{
		Depth:         limits.QueueDepth,
		RatePerSecond: limits.RatePerSecond,
		Burst:         limits.Burst,
		Messenger:     messenger,
		Clock:         clock,
		Muted: func(recipient string) bool {
			cfg := snapshot.Load()
			if cfg == nil {
				return false
			}
			return cfg.Users[recipient].Muted
		},
	})
	if err != nil {
		return err
	}
	// This defer also covers an early return after the queue was built; the
	// normal path below calls Shutdown explicitly first.
	defer func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = q.Shutdown(drainCtx)
	}()

	adminManager := adminmgr.New(adminmgr.Options{
		Config: snapshot.Load,
		Store:  st,
		Queue:  q,
		Clock:  clock,
	})
	configManager := configmgr.New(configmgr.Options{
		Config: snapshot.Load,
		Store:  st,
	})
	testManager := testsat.New(testsat.Options{
		Client: &queueSender{
			queue:    q,
			provider: initial.System.DefaultProvider,
		},
		Scheduler: sched,
		Clock:     clock,
		Timezone: func(phone string) string {
			cfg := snapshot.Load()
			if cfg == nil {
				return "UTC"
			}
			return cfg.Users[phone].Timezone
		},
	})

	rt := router.New(router.Options{
		Config:   snapshot.Load,
		Store:    st,
		Sessions: sessions,
		Builtins: builtins(configManager, adminManager, testManager),
	})

	dispatcher := &scheduleDispatcher{
		config: snapshot.Load,
		store:  st,
		client: &http.Client{Timeout: dispatchTimeout},
	}
	scheduleIDs, err := registerSchedules(sched, initial, dispatcher.dispatch)
	if err != nil {
		return err
	}
	defer func() {
		for _, id := range scheduleIDs {
			sched.Cancel(id)
		}
	}()

	watchSource := opts.Watcher
	var ownedWatcher *config.FSWatcher
	if watchSource == nil {
		ownedWatcher, err = config.NewFSWatcher(root)
		if err != nil {
			return err
		}
		watchSource = ownedWatcher
	}

	watchCtx, stopWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		_ = config.Watch(watchCtx, watchSource,
			func() (*config.Config, error) { return loadConfig(root) },
			func(cfg *config.Config) {
				snapshot.Store(cfg)
				if opts.OnReload != nil {
					opts.OnReload(cfg)
				}
			},
			config.WithClock(clock),
		)
	}()
	defer func() {
		stopWatch()
		if ownedWatcher != nil {
			_ = ownedWatcher.Close()
		}
		select {
		case <-watchDone:
		case <-time.After(shutdownTimeout):
		}
	}()

	addr := strings.TrimSpace(opts.Addr)
	if addr == "" {
		port := initial.System.Server.Port
		if port == 0 {
			port = defaultPort
		}
		addr = fmt.Sprintf(":%d", port)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("app: listen %s: %w", addr, err)
	}

	handler := api.NewServer(api.ServerOptions{
		Config: snapshot.Load,
		Store:  st,
		Inbound: func(ctx context.Context, in model.Inbound) {
			metrics.IncInboundMessages()
			rt.Dispatch(ctx, in)
		},
		OnUnknown: func(phone string) {
			// The admin-manager deduplicates against pending approvals, so a
			// repeat message does not create a second request (plan §3.2).
			_ = adminManager.RequestApproval(context.Background(), phone)
		},
		Queue:    q,
		Sessions: rt,
		Registry: st,
	})

	server := &http.Server{Handler: handler}
	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	if opts.OnReady != nil {
		opts.OnReady(listener.Addr().String())
	}

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		runErr = err
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(stopCtx); err != nil && !errors.Is(err, http.ErrServerClosed) && runErr == nil {
		runErr = err
	}
	// Draining the queue after the HTTP server stops accepts no new work and
	// delivers everything already buffered (plan §3.9).
	if err := q.Shutdown(stopCtx); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

// loadConfig reads root and applies the AETHER_JWT_SECRET override from plan
// §3.4. It is used for the initial load and every hot reload.
func loadConfig(root string) (*config.Config, error) {
	cfg, err := config.LoadConfig(root)
	if err != nil {
		return nil, err
	}
	if secret := os.Getenv("AETHER_JWT_SECRET"); secret != "" {
		cfg.System.Auth.JWTSecret = secret
	}
	return cfg, nil
}

// openStore resolves the configured database path. Relative paths are anchored
// at the config root so a temp config tree never writes into the working
// directory; an empty path is an in-memory database.
func openStore(cfg *config.Config) (*store.Store, error) {
	dsn := strings.TrimSpace(cfg.System.Server.DBPath)
	if dsn == "" {
		dsn = ":memory:"
	}
	if dsn != ":memory:" && !strings.HasPrefix(dsn, "file:") && !filepath.IsAbs(dsn) {
		dsn = filepath.Join(cfg.Root, dsn)
	}
	if dsn != ":memory:" && !strings.HasPrefix(dsn, "file:") {
		if err := os.MkdirAll(filepath.Dir(dsn), 0o755); err != nil {
			return nil, fmt.Errorf("app: create database directory: %w", err)
		}
	}

	st, err := store.Open(dsn)
	if err != nil {
		return nil, fmt.Errorf("app: open store: %w", err)
	}
	return st, nil
}

// newMessenger builds the transport for system.json's default provider. The
// real msg adapter is used in production; tests inject a fake instead.
func newMessenger(cfg *config.Config) (transport.Messenger, error) {
	name := strings.TrimSpace(cfg.System.DefaultProvider)
	if name == "" && len(cfg.System.Providers) == 1 {
		for key := range cfg.System.Providers {
			name = key
		}
	}
	if name == "" {
		return nil, errors.New("app: no default provider is configured")
	}

	provider, ok := cfg.System.Providers[name]
	if !ok {
		return nil, fmt.Errorf("app: default provider %q is not defined", name)
	}
	if !strings.EqualFold(strings.TrimSpace(provider.Type), "msg") {
		return nil, fmt.Errorf("app: provider %q has unsupported type %q", name, provider.Type)
	}
	return transport.NewMsgMessenger(name, provider)
}

// builtins returns the internal command handlers in precedence order. They run
// before any file-registered satellite with the same trigger (plan §3.6).
func builtins(configManager *configmgr.Manager, adminManager *adminmgr.Manager, testManager *testsat.Manager) []router.Builtin {
	wrap := func(satellite, trigger string, run func(sdk.Transmission) error) router.Builtin {
		return router.Builtin{
			Satellite: satellite,
			Trigger:   trigger,
			Handle: func(_ context.Context, in model.Inbound, text string) error {
				return run(sdk.Transmission{
					Command:   trigger,
					Trigger:   trigger,
					Text:      text,
					Sender:    in.Sender,
					Satellite: satellite,
				})
			},
		}
	}

	return []router.Builtin{
		wrap("config", "config", configManager.HandlePatch),
		wrap("admin", "admin", adminManager.HandleAdmin),
		wrap("remind", "remind", testManager.HandleRemind),
		wrap("test", "test", testManager.HandleTest),
	}
}

// queueSender adapts the SDK's Response envelope to the outbound queue. It is
// the in-process reply path for built-in satellites: no JWT or self HTTP call
// is needed, but delivery still goes through the same queue and metrics as
// /message.
type queueSender struct {
	queue    *queue.Queue
	provider string
}

// Send enqueues one job per recipient.
func (s *queueSender) Send(_ context.Context, resp sdk.Response) error {
	if s == nil || s.queue == nil {
		return errors.New("app: outbound queue is not configured")
	}
	if len(resp.Recipients) > 1 {
		metrics.IncBulkMessageRequests()
	}
	for _, recipient := range resp.Recipients {
		if strings.TrimSpace(recipient) == "" {
			continue
		}
		if err := s.queue.Enqueue(queue.Job{
			Recipient: recipient,
			Text:      resp.Text,
			Provider:  s.provider,
		}); err != nil {
			return err
		}
	}
	return nil
}
