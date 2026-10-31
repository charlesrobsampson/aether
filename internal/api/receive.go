// Package api contains Aether's HTTP endpoints.
//
// Every endpoint is registered on the single ServeMux owned by this package;
// the router never registers handlers itself.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/model"
)

// maxReceiveBody bounds a /receive request body. Inbound messages are small;
// the limit keeps a malformed or hostile transport from tying up memory.
const maxReceiveBody = 1 << 20

// InboundHandler processes one inbound message that passed the transport access
// checks. It is called synchronously, so the transport's 202 means the message
// was accepted, not that any satellite has answered it.
type InboundHandler func(context.Context, model.Inbound)

// ReceiveOptions configures the /receive handler.
type ReceiveOptions struct {
	// Config returns the active configuration. A getter rather than a value
	// lets hot reload swap approved/denied lists and the ingress token.
	Config func() *config.Config
	// Handle dispatches an accepted message. A nil handler acknowledges and
	// drops the message.
	Handle InboundHandler
	// OnUnknown is called once for a sender who is neither approved nor
	// denied. Deduplication against pending approvals belongs to the
	// admin-manager.
	OnUnknown func(phone string)
}

type receiveHandler struct {
	options ReceiveOptions
}

// NewReceiveHandler returns the transport-facing POST /receive handler.
//
// Denied senders are acknowledged with 202 and dropped. Unknown senders also
// receive 202, but the approval callback is invoked first. Parse failures are
// reported to the caller as a generic 400 so decoder details never leak.
func NewReceiveHandler(options ReceiveOptions) http.Handler {
	return &receiveHandler{options: options}
}

func (h *receiveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	cfg := h.config()
	if cfg == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	if !ingressTokenOK(cfg, r) {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}

	inbound, err := decodeInbound(w, r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	switch {
	case auth.IsDenied(cfg, inbound.Sender):
		// Denied wins even when the number also appears in approved_numbers.
		w.WriteHeader(http.StatusAccepted)
	case auth.RoleOf(cfg, inbound.Sender) == auth.Denied:
		if h.options.OnUnknown != nil {
			h.options.OnUnknown(inbound.Sender)
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		if h.options.Handle != nil {
			h.options.Handle(r.Context(), inbound)
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (h *receiveHandler) config() *config.Config {
	if h.options.Config == nil {
		return nil
	}
	return h.options.Config()
}

// ingressTokenOK reports whether the request may enter. An empty configured
// token disables the check; otherwise the header must match exactly.
func ingressTokenOK(cfg *config.Config, r *http.Request) bool {
	want := cfg.System.Auth.IngressToken
	if want == "" {
		return true
	}
	got := r.Header.Get("X-Aether-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// decodeInbound reads exactly one inbound envelope and validates the required
// fields. Error text is for server-side diagnosis only; callers must not echo
// it to the transport.
func decodeInbound(w http.ResponseWriter, r *http.Request) (model.Inbound, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReceiveBody))

	var inbound model.Inbound
	if err := decoder.Decode(&inbound); err != nil {
		return model.Inbound{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return model.Inbound{}, errors.New("receive: body must contain one JSON object")
	}
	if strings.TrimSpace(inbound.Sender) == "" {
		return model.Inbound{}, errors.New("receive: sender is required")
	}
	if strings.TrimSpace(inbound.Text) == "" {
		return model.Inbound{}, errors.New("receive: text is required")
	}
	return inbound, nil
}
