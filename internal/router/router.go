// Package router resolves inbound messages to registered satellites.
package router

import (
	"context"
	"net/http"
	"time"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/metrics"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/session"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/pkg/sdk"
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
	// RequiredRole is the minimum role that may run the command. A group chat
	// silently drops any command above User before Handle runs, so
	// administrative commands are neither exposed nor acknowledged there.
	RequiredRole auth.Role
	// Handle runs the command.
	Handle func(context.Context, model.Inbound, string) error
	// SuccessReaction, when set, is attached to the inbound message after a
	// successful command instead of sending a text acknowledgement.
	SuccessReaction string
	// SuccessReactionFor, when set, chooses the success reaction from the
	// command text. Returning "" means the command replies itself and must not
	// be acknowledged with a reaction.
	SuccessReactionFor func(text string) string
	// SuccessText is the fallback acknowledgement sent when SuccessReaction is
	// set but the provider cannot attach reactions.
	SuccessText string
}

// Replier sends a text reply back to the conversation an inbound message came
// from. It is used for safe, user-correctable errors and help output.
type Replier func(ctx context.Context, in model.Inbound, text string) error

// Reactor attaches an emoji reaction to an inbound message. Providers that do
// not support reactions return transport.ErrReactionsUnsupported.
type Reactor func(ctx context.Context, in model.Inbound, emoji string) error

// Sender delivers a satellite response envelope. It is the same outbound seam
// the /message handler uses, so a satellite can answer its dispatch with a
// response body instead of a second HTTP call (plan §3.6, §3.11).
type Sender interface {
	Send(ctx context.Context, resp sdk.Response) error
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
	// Reply sends safe user-facing errors and help output back to the sender.
	// Nil disables user replies.
	Reply Replier
	// React attaches success reactions. Nil disables reactions.
	React Reactor
	// Out delivers a satellite response envelope. Nil drops response bodies.
	Out Sender
	// Metrics receives dispatch latency and failure counters. Nil selects the
	// process-wide default registry.
	Metrics *metrics.Registry
}

// Router dispatches inbound messages to registered satellites.
type Router struct {
	config   func() *config.Config
	store    RouteLogger
	client   *http.Client
	sessions *session.Store
	builtins []Builtin
	reply    Replier
	react    Reactor
	out      Sender
	metrics  *metrics.Registry
}

// New returns a Router. With no options it has no configuration source and
// drops every message; pass Options to wire it.
func New(options ...Options) *Router {
	r := &Router{
		client:  &http.Client{Timeout: defaultClientTimeout},
		metrics: metrics.Default,
	}
	if len(options) == 0 {
		return r
	}

	option := options[0]
	r.config = option.Config
	r.store = option.Store
	r.sessions = option.Sessions
	r.builtins = option.Builtins
	r.reply = option.Reply
	r.react = option.React
	r.out = option.Out
	if option.Client != nil {
		r.client = option.Client
	}
	if option.Metrics != nil {
		r.metrics = option.Metrics
	}
	return r
}
