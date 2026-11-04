// Package router resolves inbound messages to registered satellites.
package router

import (
	"context"
	"net/http"
	"time"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/session"
	"github.com/charlesrobsampson/aether/internal/store"
)

// defaultClientTimeout bounds one dispatch POST the way plan §3.13/§3.6
// requires: a slow satellite fails the route, it does not stall /receive.
const defaultClientTimeout = 5 * time.Second

// RouteLogger records dispatch attempts. *store.Store implements it.
type RouteLogger interface {
	LogRoute(ctx context.Context, rec store.RouteLog) error
}

// Builtin is an internal command handler (plan §3.6). Built-ins take
// precedence over file-registered satellites with the same trigger, and their
// handlers run in-process instead of over HTTP.
//
// Handle receives the canonical text after prefix stripping, including the
// trigger, and must return an error when the command is rejected. The app owns
// the concrete handlers; the router only resolves precedence and audits the
// outcome.
type Builtin struct {
	// Satellite is the internal satellite name recorded in route_log.
	Satellite string
	// Trigger is the first token that selects this handler.
	Trigger string
	// Handle runs the command.
	Handle func(context.Context, model.Inbound, string) error
}

// Options configures a Router. Every field is optional so New() remains usable
// as an inert value in scaffold tests.
type Options struct {
	// Config returns the active configuration. A getter lets hot reload swap
	// users, aliases, and routes without rebuilding the Router.
	Config func() *config.Config
	// Store receives the route_log audit row for every dispatch attempt.
	Store RouteLogger
	// Client is the outbound HTTP client. Nil uses a client with a 5 s
	// timeout; tests inject httptest's client so no real network is used.
	Client *http.Client
	// Sessions holds the in-memory conversation state used to intercept a
	// reply before trigger parsing (plan §3.7). Nil disables session routing.
	Sessions *session.Store
	// Builtins are internal command handlers, checked in order before any
	// file-registered route.
	Builtins []Builtin
}

// Router dispatches inbound messages to registered satellites.
type Router struct {
	config   func() *config.Config
	store    RouteLogger
	client   *http.Client
	sessions *session.Store
	builtins []Builtin
}

// New returns a Router. With no options it has no configuration source and
// drops every message; pass Options to wire it.
func New(options ...Options) *Router {
	r := &Router{client: &http.Client{Timeout: defaultClientTimeout}}
	if len(options) == 0 {
		return r
	}

	option := options[0]
	r.config = option.Config
	r.store = option.Store
	r.sessions = option.Sessions
	r.builtins = option.Builtins
	if option.Client != nil {
		r.client = option.Client
	}
	return r
}
