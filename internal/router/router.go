// Package router resolves inbound messages to registered satellites.
package router

// Router dispatches inbound messages to registered satellites.
type Router struct{}

// New returns an empty Router.
func New() *Router {
	return &Router{}
}
