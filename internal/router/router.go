// Package router resolves inbound messages to registered satellites.
package router

import (
	"context"
	"net/http"
	"time"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/store"
)

// defaultClientTimeout bounds one dispatch POST the way plan §3.13/§3.6
// requires: a slow satellite fails the route, it does not stall /receive.
const defaultClientTimeout = 5 * time.Second

// RouteLogger records dispatch attempts. *store.Store implements it.
type RouteLogger interface {
	LogRoute(ctx context.Context, rec store.RouteLog) error
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
}

// Router dispatches inbound messages to registered satellites.
type Router struct {
	config func() *config.Config
	store  RouteLogger
	client *http.Client
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
	if option.Client != nil {
		r.client = option.Client
	}
	return r
}
