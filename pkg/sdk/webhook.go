package sdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// maxWebhookBody bounds a satellite webhook request. The dispatch envelope is
// small, so a larger body is either malformed or hostile.
const maxWebhookBody = 1 << 20

// Transmission is the envelope Aether posts to a satellite for a matched
// command (plan §3.6). Args holds the converted, typed values.
type Transmission struct {
	Command   string         `json:"command"`
	Trigger   string         `json:"trigger"`
	Args      map[string]any `json:"args"`
	Text      string         `json:"text"`
	Sender    string         `json:"sender"`
	Satellite string         `json:"satellite"`
	ThreadID  string         `json:"thread_id,omitempty"`
	MessageID string         `json:"message_id,omitempty"`
	GroupID   string         `json:"group_id,omitempty"`
	GroupName string         `json:"group_name,omitempty"`
	// MentionsMe reports that the transport detected a native mention of the
	// bot account.
	MentionsMe bool `json:"mentions_me,omitempty"`
	// SenderName and SenderNumber carry the transport's display name and
	// E.164 number when they are known separately from Sender.
	SenderName   string `json:"sender_name,omitempty"`
	SenderNumber string `json:"sender_number,omitempty"`
	// TimestampMS is the provider timestamp of the inbound message. Reactions
	// use it as the target, so zero means the provider did not supply one.
	TimestampMS int64 `json:"timestamp_ms,omitempty"`
}

// ReplyTo returns the conversation a response should target. Group messages
// answer in the group; direct messages answer the sender. Satellites that need
// an explicit individual target should use Sender instead.
func (t Transmission) ReplyTo() string {
	if group := strings.TrimSpace(t.GroupID); group != "" {
		return group
	}
	return strings.TrimSpace(t.Sender)
}

// SessionAction accompanies a Response that changes conversation state.
type SessionAction struct {
	Action     string `json:"action"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// Response is the outbound envelope a satellite returns to Aether. It carries
// the same JSON tags as internal/model.Outbound (plan §3.3 and §3.11).
type Response struct {
	Recipients []string `json:"recipients"`
	// GroupChatID targets one group conversation instead of individual
	// recipients. It is mutually exclusive with Recipients.
	GroupChatID string `json:"group_chat_id,omitempty"`
	Text        string `json:"text,omitempty"`
	// Format selects provider-native text formatting. Empty or "plain" sends
	// literal text; "styled" asks the provider to render its markup (for
	// Signal: *bold*, _italic_, ~strike~, ||spoiler||, and `monospace`).
	Format    string         `json:"format,omitempty"`
	Satellite string         `json:"satellite,omitempty"`
	ThreadID  string         `json:"thread_id,omitempty"`
	Session   *SessionAction `json:"session,omitempty"`
	// Status is the HTTP status SendResponse should write. It is not part of
	// the /message envelope, so it never appears on the wire.
	Status int `json:"-"`
}

// ParseWebhook decodes and validates the standard Aether dispatch envelope
// received by a satellite. Only POST requests carrying application/json are
// accepted.
func ParseWebhook(r *http.Request) (*Transmission, error) {
	if r == nil {
		return nil, errors.New("sdk: nil webhook request")
	}
	if r.Method != http.MethodPost {
		return nil, fmt.Errorf("sdk: webhook method %s, want POST", r.Method)
	}

	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return nil, fmt.Errorf("sdk: webhook content type %q, want application/json", contentType)
	}

	decoder := json.NewDecoder(io.LimitReader(r.Body, maxWebhookBody))

	var transmission Transmission
	if err := decoder.Decode(&transmission); err != nil {
		return nil, fmt.Errorf("sdk: parse webhook: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("sdk: webhook body must contain one JSON object")
	}
	return &transmission, nil
}

// SendResponse writes a satellite response to Aether as JSON. A zero or invalid
// Response.Status defaults to 200.
func SendResponse(w http.ResponseWriter, resp Response) {
	status := resp.Status
	if status < 100 || status > 599 {
		status = http.StatusOK
	}

	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "response encoding failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
