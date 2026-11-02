package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/metrics"
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

// SessionApplier applies a session lifecycle action. *router.Router implements
// it; /message uses the same in-memory store as inbound dispatch.
type SessionApplier interface {
	ApplySessionAction(phone, satellite, thread string, action model.SessionAction) error
}

// MessageOptions configures the /message handler.
type MessageOptions struct {
	// Config returns the active configuration. A getter lets hot reload swap
	// the JWT secret, default provider, and denied-number list.
	Config func() *config.Config
	// Queue receives one job per accepted recipient.
	Queue Enqueuer
	// Sessions applies lock/extend/close actions carried by the envelope. Nil
	// leaves session actions inert.
	Sessions SessionApplier
}

type messageHandler struct {
	options MessageOptions
}

// NewMessageHandler returns the satellite-facing POST /message handler.
//
// Requests are authenticated as admin (or maintainer) before the envelope is
// read. A multi-recipient request fans out to one job per accepted recipient
// and increments aether_bulk_message_requests_total once. Denied recipients and
// unapproved groups are dropped silently: the request still succeeds but no job
// is enqueued for them.
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

	// Session actions are conversation state, not delivery: apply them once
	// per accepted recipient before the outbound fan-out (plan §3.3, §3.7).
	if outbound.Session != nil && h.options.Sessions != nil {
		for _, recipient := range outbound.Recipients {
			if auth.IsDenied(cfg, recipient) {
				continue
			}
			if err := h.options.Sessions.ApplySessionAction(recipient, outbound.Satellite, outbound.ThreadID, *outbound.Session); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
		}
	}

	provider := cfg.System.DefaultProvider

	// A group send is one job addressed to the group conversation. The group
	// must be approved; otherwise it is dropped silently like a denied
	// recipient.
	if strings.TrimSpace(outbound.GroupChatID) != "" {
		if !groupApproved(cfg, outbound.GroupChatID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		job := queue.Job{
			Recipient: outbound.GroupChatID,
			Text:      outbound.Text,
			Provider:  provider,
		}
		if err := h.options.Queue.Enqueue(job); err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

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

	// One increment per request, regardless of how many recipients survived
	// the denied filter. Single-recipient sends are not bulk.
	if len(outbound.Recipients) > 1 {
		metrics.IncBulkMessageRequests()
	}

	w.WriteHeader(http.StatusAccepted)
}

// groupApproved reports whether groupID appears verbatim in admin.json's
// approved_groups. Group identifiers are opaque and matched literally.
func groupApproved(cfg *config.Config, groupID string) bool {
	for _, approved := range cfg.Admin.ApprovedGroups {
		if approved == groupID {
			return true
		}
	}
	return false
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
// mutually exclusive, exactly one of them is present, every recipient is
// non-blank, and text is required unless a session action is present.
func validateOutbound(outbound model.Outbound) error {
	hasGroup := strings.TrimSpace(outbound.GroupChatID) != ""
	if hasGroup && len(outbound.Recipients) > 0 {
		return errors.New("message: recipients and group_chat_id are mutually exclusive")
	}
	if !hasGroup && len(outbound.Recipients) == 0 {
		return errors.New("message: recipients or group_chat_id are required")
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
