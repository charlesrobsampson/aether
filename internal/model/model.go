package model

import "strings"

// Inbound represents an incoming message payload.
type Inbound struct {
	Sender    string `json:"sender"`
	Text      string `json:"text"`
	Provider  string `json:"provider,omitempty"`
	GroupID   string `json:"group_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	// SenderName is the transport's display name for the sender, when known.
	SenderName string `json:"sender_name,omitempty"`
	// SenderNumber is the sender's phone number when the transport supplies it
	// separately from Sender. Signal may report a UUID as Sender and still know
	// the E.164 number.
	SenderNumber string `json:"sender_number,omitempty"`
	// TimestampMS is the provider timestamp of the inbound message in
	// milliseconds. It is the target of a reaction; zero means unavailable.
	TimestampMS int64 `json:"timestamp_ms,omitempty"`
}

// Outbound represents a message to be sent.
type Outbound struct {
	Recipients  []string `json:"recipients"`
	GroupChatID string   `json:"group_chat_id,omitempty"`
	Text        string   `json:"text,omitempty"`
	// Format selects provider-native text formatting. Empty and "plain" send
	// literal text; "styled" asks the provider to render its markup.
	Format    string         `json:"format,omitempty"`
	Source    string         `json:"source,omitempty"`
	Satellite string         `json:"satellite,omitempty"`
	ThreadID  string         `json:"thread_id,omitempty"`
	Session   *SessionAction `json:"session,omitempty"`
}

// SessionAction describes a session operation.
type SessionAction struct {
	Action     string `json:"action"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// Text format values accepted by the outbound envelope.
const (
	FormatPlain  = ""
	FormatStyled = "styled"
)

// StyledFormat reports whether format asks for provider-native styling.
func StyledFormat(format string) bool {
	return strings.EqualFold(strings.TrimSpace(format), FormatStyled)
}

// ValidFormat reports whether format is one the outbound path accepts.
func ValidFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "plain", FormatStyled:
		return true
	default:
		return false
	}
}
