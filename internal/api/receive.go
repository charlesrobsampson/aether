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
	"log"
	"net/http"
	"strings"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/metrics"
	"github.com/charlesrobsampson/aether/internal/model"
)

// maxReceiveBody bounds a /receive request body. Inbound messages are small;
// the limit keeps a malformed or hostile transport from tying up memory.
const maxReceiveBody = 1 << 20

// ErrQueueFull reports that the inbound dispatch queue could not accept a
// message. The handler maps it to 429 so the transport retries with backoff.
var ErrQueueFull = errors.New("api: inbound dispatch queue is full")

// InboundHandler accepts one inbound message that passed the transport access
// checks. It must return promptly: the transport's 202 means the message was
// stored or queued, not that any satellite has answered it. ErrQueueFull
// becomes 429 with Retry-After.
type InboundHandler func(context.Context, model.Inbound) error

// InboundClaim reports whether an inbound (sender, message_id) pair is fresh.
// False means a duplicate within the dedupe TTL. An empty messageID is always
// fresh.
type InboundClaim func(ctx context.Context, sender, messageID string) (bool, error)

// InboundRelease removes a claim after a message could not be queued, so the
// transport's retry is not mistaken for a duplicate. Implementations must
// ignore an empty messageID.
type InboundRelease func(ctx context.Context, sender, messageID string) error

// UnknownSender describes a sender who is neither approved nor denied. Name
// and Number are best-effort transport metadata used to make an approval
// notification more useful.
type UnknownSender struct {
	Sender string
	Name   string
	Number string
}

// UnknownGroup describes a group that is neither approved nor denied. Name and
// Sender make the maintainer notification more useful; GroupID is the identity
// that an approval writes to admin.json's approved_groups.
type UnknownGroup struct {
	GroupID    string
	Name       string
	Sender     string
	SenderName string
}

// ReceiveOptions configures the /receive handler.
type ReceiveOptions struct {
	// Config returns the active configuration. A getter rather than a value
	// lets hot reload swap approved/denied lists and the ingress token.
	Config func() *config.Config
	// Handle enqueues an accepted message. A nil handler acknowledges and
	// drops the message.
	Handle InboundHandler
	// Claim deduplicates an accepted message by (sender, message_id). A nil
	// claim disables dedupe. Errors fail open: the message is still queued.
	Claim InboundClaim
	// Release undoes a successful Claim when Handle rejects the message. It
	// is optional; without it a retried message could be mistaken for a
	// duplicate after a 429.
	Release InboundRelease
	// Logf records operational failures. Nil uses the standard logger.
	Logf func(format string, args ...any)
	// OnUnknown is called once for a sender who is neither approved nor
	// denied. Deduplication against pending approvals belongs to the
	// admin-manager.
	OnUnknown func(UnknownSender)
	// OnUnknownGroup is called once for a group that is neither approved nor
	// denied. Deduplication against pending approvals belongs to the
	// admin-manager.
	OnUnknownGroup func(UnknownGroup)
	// Metrics receives receive outcome counters. Nil selects the
	// process-wide default registry.
	Metrics *metrics.Registry
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
	if options.Metrics == nil {
		options.Metrics = metrics.Default
	}
	return &receiveHandler{options: options}
}

func (h *receiveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.options.Metrics.IncReceiveRequest(metrics.ReceiveBadRequest)
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
		h.options.Metrics.IncReceiveRequest(metrics.ReceiveUnauthorized)
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}

	inbound, err := decodeInbound(w, r)
	if err != nil {
		h.options.Metrics.IncReceiveRequest(metrics.ReceiveBadRequest)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	inbound = canonicalizeSender(inbound)

	groupID := strings.TrimSpace(inbound.GroupID)
	switch {
	case auth.IsDenied(cfg, inbound.Sender):
		// Denied wins everywhere, including in an approved group.
		h.options.Metrics.IncReceiveRequest(metrics.ReceiveAccepted)
		w.WriteHeader(http.StatusAccepted)
	case groupID != "" && auth.GroupDenied(cfg, groupID):
		// A denied group is dropped silently, just like a denied sender.
		h.options.Metrics.IncReceiveRequest(metrics.ReceiveAccepted)
		w.WriteHeader(http.StatusAccepted)
	case groupID != "" && !auth.GroupApproved(cfg, groupID):
		// An unapproved group asks the maintainer to approve the group. The
		// message is not dispatched until it is approved; the sender does not
		// need a separate approval because the group is the principal.
		if h.options.OnUnknownGroup != nil {
			h.options.OnUnknownGroup(UnknownGroup{
				GroupID:    inbound.GroupID,
				Name:       inbound.GroupName,
				Sender:     inbound.Sender,
				SenderName: inbound.SenderName,
			})
		}
		h.options.Metrics.IncReceiveRequest(metrics.ReceiveAccepted)
		w.WriteHeader(http.StatusAccepted)
	case groupID == "" && auth.RoleOf(cfg, inbound.Sender) == auth.Denied:
		if h.options.OnUnknown != nil {
			h.options.OnUnknown(UnknownSender{
				Sender: inbound.Sender,
				Name:   inbound.SenderName,
				Number: inbound.SenderNumber,
			})
		}
		h.options.Metrics.IncReceiveRequest(metrics.ReceiveAccepted)
		w.WriteHeader(http.StatusAccepted)
	default:
		claimed := false
		if h.options.Claim != nil {
			fresh, err := h.options.Claim(r.Context(), inbound.Sender, inbound.MessageID)
			switch {
			case err != nil:
				// Dedupe is best-effort: a store problem must never drop a
				// message the transport already delivered.
				h.logf("receive: dedupe claim for %q message %q: %v",
					inbound.Sender, inbound.MessageID, err)
			case !fresh:
				// A duplicate within the TTL is acknowledged without
				// dispatching it again.
				h.options.Metrics.IncDedupeHits()
				h.options.Metrics.IncReceiveRequest(metrics.ReceiveDuplicate)
				w.WriteHeader(http.StatusAccepted)
				return
			default:
				claimed = true
			}
		}

		if h.options.Handle != nil {
			if err := h.options.Handle(r.Context(), inbound); err != nil {
				// The message was not queued, so release the claim: a 429
				// retry must be allowed to dispatch it.
				if claimed && h.options.Release != nil {
					if releaseErr := h.options.Release(r.Context(), inbound.Sender, inbound.MessageID); releaseErr != nil {
						h.logf("receive: release claim for %q message %q: %v",
							inbound.Sender, inbound.MessageID, releaseErr)
					}
				}
				if errors.Is(err, ErrQueueFull) {
					h.options.Metrics.IncQueueFull(metrics.QueueInbound)
					h.options.Metrics.IncReceiveRequest(metrics.ReceiveQueueFull)
					w.Header().Set("Retry-After", "1")
					http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
					return
				}
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
		}
		h.options.Metrics.IncReceiveRequest(metrics.ReceiveAccepted)
		w.WriteHeader(http.StatusAccepted)
	}
}

func (h *receiveHandler) logf(format string, args ...any) {
	if h.options.Logf != nil {
		h.options.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
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

// canonicalizeSender prefers a transport-reported phone number over a UUID.
// Signal hides some senders behind a UUID; when sourceNumber is present the
// phone is the identity users and admins actually know.
func canonicalizeSender(in model.Inbound) model.Inbound {
	if number := strings.TrimSpace(in.SenderNumber); auth.IsPhone(number) {
		in.Sender = number
	}
	return in
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
