package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/queue"
)

// maxMessageBody bounds a /message request body. The envelope is small, so a
// larger body is either malformed or hostile.
const maxMessageBody = 1 << 20

// Enqueuer accepts one outbound job. *queue.Queue implements it; tests inject
// a recorder instead of starting workers.
type Enqueuer interface {
	Enqueue(queue.Job) error
}

// MessageOptions configures the /message handler.
type MessageOptions struct {
	// Config returns the active configuration. A getter lets hot reload swap
	// the JWT secret, default provider, and denied-number list.
	Config func() *config.Config
	// Queue receives one job per accepted recipient.
	Queue Enqueuer
}

type messageHandler struct {
	options MessageOptions
}

// NewMessageHandler returns the satellite-facing POST /message handler.
//
// Requests are authenticated as admin (or maintainer) before the envelope is
// read. Denied recipients are dropped silently: the request still succeeds but
// no job is enqueued for them.
func NewMessageHandler(options MessageOptions) http.Handler {
	return &messageHandler{options: options}
}

func (h *messageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	// Build the authenticator per request so a reloaded jwt_secret takes
	// effect without restarting the server.
	authenticator := auth.Authenticator{Secret: cfg.System.Auth.JWTSecret}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handle(w, r, cfg)
	})
	authenticator.Middleware(next, auth.Admin).ServeHTTP(w, r)
}

func (h *messageHandler) handle(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	outbound, err := decodeOutbound(w, r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := validateOutbound(outbound); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if h.options.Queue == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}

	provider := cfg.System.DefaultProvider
	for _, recipient := range outbound.Recipients {
		if auth.IsDenied(cfg, recipient) {
			continue
		}
		job := queue.Job{
			Recipient: recipient,
			Text:      outbound.Text,
			Provider:  provider,
		}
		if err := h.options.Queue.Enqueue(job); err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
	}

	w.WriteHeader(http.StatusAccepted)
}

func (h *messageHandler) config() *config.Config {
	if h.options.Config == nil {
		return nil
	}
	return h.options.Config()
}

// decodeOutbound reads exactly one outbound envelope.
func decodeOutbound(w http.ResponseWriter, r *http.Request) (model.Outbound, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessageBody))

	var outbound model.Outbound
	if err := decoder.Decode(&outbound); err != nil {
		return model.Outbound{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return model.Outbound{}, errors.New("message: body must contain one JSON object")
	}
	return outbound, nil
}

// validateOutbound enforces plan §3.3: recipients and group_chat_id are
// mutually exclusive, at least one recipient is present, every recipient is
// non-blank, and text is required unless a session action is present.
func validateOutbound(outbound model.Outbound) error {
	if outbound.GroupChatID != "" && len(outbound.Recipients) > 0 {
		return errors.New("message: recipients and group_chat_id are mutually exclusive")
	}
	if len(outbound.Recipients) == 0 {
		return errors.New("message: recipients are required")
	}
	for _, recipient := range outbound.Recipients {
		if strings.TrimSpace(recipient) == "" {
			return errors.New("message: recipient is blank")
		}
	}
	if strings.TrimSpace(outbound.Text) == "" && outbound.Session == nil {
		return errors.New("message: text is required unless a session action is present")
	}
	return nil
}
