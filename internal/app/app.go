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
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charlesrobsampson/aether/internal/api"
	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/dispatch"
	"github.com/charlesrobsampson/aether/internal/metrics"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/queue"
	"github.com/charlesrobsampson/aether/internal/router"
	"github.com/charlesrobsampson/aether/internal/satellite/adminmgr"
	"github.com/charlesrobsampson/aether/internal/satellite/configmgr"
	"github.com/charlesrobsampson/aether/internal/satellite/helpsat"
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
	dedupeTTL := time.Duration(limits.DedupeTTLSeconds) * time.Second
	if dedupeTTL <= 0 {
		dedupeTTL = store.DefaultDedupeTTL
	}
	q, err := queue.New(queue.Options{
		Depth:         limits.QueueDepth,
		RatePerSecond: limits.RatePerSecond,
		Burst:         limits.Burst,
		SendTimeout:   time.Duration(limits.SendTimeoutSeconds) * time.Second,
		Attempts:      limits.SendAttempts,
		RetryBackoff:  time.Duration(limits.RetryBackoffMS) * time.Millisecond,
		Messenger:     messenger,
		Clock:         clock,
		Muted: func(recipient string) bool {
			cfg := snapshot.Load()
			if cfg == nil {
				return false
			}
			return cfg.Users[recipient].Muted
		},
		OnError: func(job queue.Job, err error) {
			log.Printf("queue: send to %s via %s failed: %v", job.Recipient, job.Provider, err)
		},
		OnDeadLetter: func(job queue.Job, err error) {
			// The queue stopped retrying; persist one audit row so the loss is
			// visible (plan §3.9, §3.19).
			log.Printf("queue: dead-letter %s via %s: %v", job.Recipient, job.Provider, err)
			record := store.RouteLog{
				Direction: "outbound",
				Recipient: job.Recipient,
				Text:      job.Text,
				Status:    "dead_letter",
			}
			if err != nil {
				record.Error = err.Error()
			}
			if logErr := st.LogRoute(context.Background(), record); logErr != nil {
				log.Printf("queue: record dead letter: %v", logErr)
			}
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

	sender := &queueSender{
		queue:    q,
		provider: initial.System.DefaultProvider,
	}
	adminManager := adminmgr.New(adminmgr.Options{
		Config: snapshot.Load,
		Store:  st,
		Queue:  q,
		Out:    sender,
		Clock:  clock,
	})
	configManager := configmgr.New(configmgr.Options{
		Config: snapshot.Load,
		Store:  st,
		Out:    sender,
	})
	testManager := testsat.New(testsat.Options{
		Client:    sender,
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
	helpManager := helpsat.New(helpsat.Options{
		Config:   snapshot.Load,
		Client:   sender,
		Commands: helpCommands(),
	})

	rt := router.New(router.Options{
		Config:   snapshot.Load,
		Store:    st,
		Sessions: sessions,
		Builtins: builtins(configManager, adminManager, testManager, helpManager),
		Reply:    sender.Reply,
		React:    sender.React,
		Out:      sender,
	})

	// Inbound dispatch is asynchronous: /receive enqueues and returns 202, and
	// one worker runs router.Dispatch (plan §3.2, §3.9).
	dispatches, err := dispatch.New(dispatch.Options{
		Depth:   limits.DispatchQueueDepth,
		Handler: rt.Dispatch,
	})
	if err != nil {
		return err
	}
	// This defer covers early returns after construction; the normal path
	// below shuts the inbound queue down before the outbound queue.
	defer func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = dispatches.Shutdown(drainCtx)
	}()

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
		Inbound: func(ctx context.Context, in model.Inbound) error {
			if err := dispatches.Submit(ctx, in); err != nil {
				if errors.Is(err, dispatch.ErrFull) {
					return api.ErrQueueFull
				}
				return err
			}
			metrics.IncInboundMessages()
			return nil
		},
		Claim: func(ctx context.Context, sender, messageID string) (bool, error) {
			return st.ClaimInbound(ctx, sender, messageID, dedupeTTL)
		},
		Release: func(ctx context.Context, sender, messageID string) error {
			return st.ReleaseInbound(ctx, sender, messageID)
		},
		OnUnknown: func(unknown api.UnknownSender) {
			// The admin-manager deduplicates against pending approvals, so a
			// repeat message does not create a second request (plan §3.2).
			_ = adminManager.RequestApprovalFrom(context.Background(), unknown.Sender, unknown.Name)
		},
		OnUnknownGroup: func(unknown api.UnknownGroup) {
			// A group must be approved before the bot can send there. The
			// admin-manager deduplicates pending group requests.
			_ = adminManager.RequestGroupApproval(context.Background(), unknown.GroupID, unknown.Name, unknown.Sender)
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
	// Stop inbound work first so no dispatch can enqueue new outbound jobs
	// while the outbound queue is draining (plan §3.2, §3.9).
	if err := dispatches.Shutdown(stopCtx); err != nil && runErr == nil {
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
func builtins(configManager *configmgr.Manager, adminManager *adminmgr.Manager, testManager *testsat.Manager, helpManager *helpsat.Manager) []router.Builtin {
	wrap := func(satellite, trigger string, role auth.Role, reaction, fallback string, run func(sdk.Transmission) error) router.Builtin {
		return router.Builtin{
			Satellite:       satellite,
			Trigger:         trigger,
			RequiredRole:    role,
			SuccessReaction: reaction,
			SuccessText:     fallback,
			Handle: func(_ context.Context, in model.Inbound, text string) error {
				return run(sdk.Transmission{
					Command:      trigger,
					Trigger:      trigger,
					Text:         text,
					Sender:       in.Sender,
					SenderName:   in.SenderName,
					SenderNumber: in.SenderNumber,
					Satellite:    satellite,
					MessageID:    in.MessageID,
					GroupID:      in.GroupID,
					GroupName:    in.GroupName,
					MentionsMe:   in.MentionsMe,
					TimestampMS:  in.TimestampMS,
				})
			},
		}
	}

	return []router.Builtin{
		withReactionFor(
			wrap("config", "config", auth.User, "👍", "Configuration updated.", configManager.Handle),
			configmgr.SuccessReactionFor,
		),
		withReactionFor(
			wrap("admin", "admin", auth.Admin, "👍", "Done.", adminManager.HandleAdmin),
			adminmgr.SuccessReactionFor,
		),
		wrap("approve", "approve", auth.Maintainer, "👍", "Approved.", adminManager.HandleApprove),
		wrap("deny", "deny", auth.Maintainer, "👍", "Denied.", adminManager.HandleDeny),
		wrap("pending", "pending", auth.Maintainer, "", "", adminManager.HandlePending),
		wrap("remind", "remind", auth.User, "👍", "Reminder set.", testManager.HandleRemind),
		wrap("test", "test", auth.User, "", "", testManager.HandleTest),
		wrap("help", "help", auth.User, "", "", helpManager.HandleHelp),
	}
}

// withReactionFor installs a text-dependent reaction selector on a built-in.
func withReactionFor(builtin router.Builtin, choose func(string) string) router.Builtin {
	builtin.SuccessReactionFor = choose
	return builtin
}

// helpCommands describes the built-ins for !help. Satellite entries are added
// by the help manager from live configuration.
func helpCommands() []helpsat.Entry {
	return []helpsat.Entry{
		{
			Name:        "help",
			Trigger:     "help",
			Usage:       "help [command]",
			Description: "List available commands",
			Role:        auth.User,
			Details:     []string{`Send "help <command>" to expand one command.`},
		},
		{
			Name:        "test",
			Trigger:     "test",
			Usage:       "test",
			Description: "Check the connection",
			Role:        auth.User,
			Details:     []string{`Replies with "test received".`},
		},
		{
			Name:        "remind",
			Trigger:     "remind",
			Usage:       "remind me <text> in <count> <unit>",
			Description: "Set a reminder",
			Role:        auth.User,
			Details: []string{
				"Absolute form: remind me <text> at <datetime>",
				"Units: seconds, minutes, hours, days (s, min, h, d).",
				"Times: 2026-11-08 09:00, 09:00, Monday 09:00, or RFC3339.",
				"Times use your configured timezone; past times are rejected.",
			},
		},
		{
			Name:        "config",
			Trigger:     "config",
			Usage:       "config [list|get|export|set] ...",
			Description: "View or change your settings",
			Role:        auth.User,
			Details: []string{
				"config list [@phone] — show settings and defaults.",
				"config get [@phone] <key> — show one setting.",
				"config export [@phone] — pasteable config set lines.",
				"config set [@phone] <key> <value> — change a setting.",
				"Keys: timezone, prefixes, muted, aliases.",
				`Use "config set prefixes []" for no command prefix.`,
				"Admins can target another user with @<phone>.",
			},
		},
		{
			Name:        "admin",
			Trigger:     "admin",
			Usage:       "admin <approve|deny|list|add|remove> ...",
			Description: "Manage users and approvals",
			Role:        auth.Admin,
			Details: []string{
				"admin approve|deny [id[,id...]|phone|uuid] — resolve requests.",
				"admin list — show pending requests.",
				"admin add|remove <phone|uuid> — change the admin list.",
			},
		},
		{
			Name:        "approve",
			Trigger:     "approve",
			Usage:       "approve [id[,id...]|phone|uuid]",
			Description: "Approve an access request",
			Role:        auth.Maintainer,
			Details: []string{
				`Use "pending" to list requests before resolving one.`,
				`A bare "approve" resolves the newest pending request.`,
			},
		},
		{
			Name:        "deny",
			Trigger:     "deny",
			Usage:       "deny [id[,id...]|phone|uuid]",
			Description: "Deny an access request",
			Role:        auth.Maintainer,
			Details: []string{
				`Use "pending" to list requests before resolving one.`,
				`A bare "deny" resolves the newest pending request.`,
			},
		},
		{
			Name:        "pending",
			Trigger:     "pending",
			Usage:       "pending",
			Description: "List pending access requests",
			Role:        auth.Maintainer,
			Details:     []string{`Resolve with "approve <id>", "approve 1,3,4", or "deny <id>".`},
		},
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
	if !model.ValidFormat(resp.Format) {
		return fmt.Errorf("app: unsupported message format %q", resp.Format)
	}
	if group := strings.TrimSpace(resp.GroupChatID); group != "" {
		return s.queue.Enqueue(queue.Job{
			Recipient: group,
			Text:      resp.Text,
			Styled:    model.StyledFormat(resp.Format),
			Provider:  s.provider,
		})
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
			Styled:    model.StyledFormat(resp.Format),
			Provider:  s.provider,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Reply sends a plain text answer to the conversation the inbound message came
// from. Groups answer in the group; direct chats answer the sender.
func (s *queueSender) Reply(_ context.Context, in model.Inbound, text string) error {
	if s == nil || s.queue == nil {
		return errors.New("app: outbound queue is not configured")
	}
	recipient := strings.TrimSpace(in.GroupID)
	if recipient == "" {
		recipient = strings.TrimSpace(in.Sender)
	}
	if recipient == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	return s.queue.Enqueue(queue.Job{
		Recipient: recipient,
		Text:      text,
		Provider:  s.provider,
	})
}

// React attaches an emoji reaction to the inbound message. A missing provider
// timestamp means the reaction cannot target anything, so it reports an error
// and the router falls back to text.
func (s *queueSender) React(_ context.Context, in model.Inbound, emoji string) error {
	if s == nil || s.queue == nil {
		return errors.New("app: outbound queue is not configured")
	}
	recipient := strings.TrimSpace(in.GroupID)
	if recipient == "" {
		recipient = strings.TrimSpace(in.Sender)
	}
	if recipient == "" {
		return errors.New("app: reaction recipient is unavailable")
	}
	if in.TimestampMS == 0 {
		return errors.New("app: inbound message has no timestamp for a reaction")
	}
	return s.queue.Enqueue(queue.Job{
		Reaction: &transport.Reaction{
			Recipient:         recipient,
			TargetAuthor:      in.Sender,
			TargetTimestampMS: in.TimestampMS,
			Emoji:             emoji,
		},
		Provider: s.provider,
	})
}
