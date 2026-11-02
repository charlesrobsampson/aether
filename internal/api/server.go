package api

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/metrics"
)

// healthTimeout bounds the store probe behind /healthz so a wedged database
// cannot hang the health check.
const healthTimeout = 2 * time.Second

// Pinger reports whether the backing store is reachable. *store.Store
// implements it; tests inject a fake.
type Pinger interface {
	Ping(ctx context.Context) error
}

// ServerOptions configures the single ServeMux that owns every Aether route.
//
// Config is a getter so a hot reload can swap the snapshot without rebuilding
// the mux. A nil Config getter, or one that returns nil, means the
// configuration is not loaded and /healthz reports 503.
type ServerOptions struct {
	// Config returns the active configuration.
	Config func() *config.Config
	// Store answers /healthz's readiness probe.
	Store Pinger
	// Metrics is the registry served on /metrics. Nil selects the
	// process-wide default registry.
	Metrics *metrics.Registry
	// Inbound receives accepted /receive messages.
	Inbound InboundHandler
	// OnUnknown is called once for a sender who is neither approved nor
	// denied.
	OnUnknown func(phone string)
	// Queue receives outbound jobs from /message.
	Queue Enqueuer
	// Sessions applies session lifecycle actions from /message. Nil leaves
	// them inert.
	Sessions SessionApplier
	// Registry mirrors a registered satellite into SQLite. A nil registry
	// leaves /register-satellite unavailable (503).
	Registry SatelliteStore
}

// NewServer builds the HTTP handler for every Aether endpoint.
//
// Each route is registered exactly once on a fresh ServeMux; the router never
// registers handlers itself, and no package-level mux is shared between
// servers. net/http panics on a duplicate pattern, so an accidental second
// registration fails loudly at construction.
func NewServer(options ServerOptions) http.Handler {
	registry := options.Metrics
	if registry == nil {
		registry = metrics.Default
	}

	mux := http.NewServeMux()
	mux.Handle("POST /receive", NewReceiveHandler(ReceiveOptions{
		Config:    options.Config,
		Handle:    options.Inbound,
		OnUnknown: options.OnUnknown,
	}))
	mux.Handle("POST /message", NewMessageHandler(MessageOptions{
		Config:   options.Config,
		Queue:    options.Queue,
		Sessions: options.Sessions,
	}))
	mux.Handle("POST /register-satellite", NewRegisterHandler(RegisterOptions{
		Config: options.Config,
		Store:  options.Registry,
	}))
	mux.Handle("GET /healthz", healthzHandler(options))
	mux.Handle("GET /metrics", registry.Handler())
	return mux
}

// healthzHandler reports 200 only when the configuration is loaded and the
// store answers Ping. The response never includes the underlying error.
func healthzHandler(options ServerOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if options.Config == nil || options.Config() == nil || options.Store == nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()
		if err := options.Store.Ping(ctx); err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
}
