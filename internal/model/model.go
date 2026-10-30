package model

// Inbound represents an incoming message payload.
type Inbound struct {
    Sender   string `json:"sender"`
    Text     string `json:"text"`
    Provider string `json:"provider,omitempty"`
    GroupID  string `json:"group_id,omitempty"`
    MessageID string `json:"message_id,omitempty"`
}

// Outbound represents a message to be sent.
type Outbound struct {
    Recipients []string      `json:"recipients"`
    GroupChatID string       `json:"group_chat_id,omitempty"`
    Text        string       `json:"text,omitempty"`
    Source      string       `json:"source,omitempty"`
    Satellite   string       `json:"satellite,omitempty"`
    ThreadID    string       `json:"thread_id,omitempty"`
    Session     *SessionAction `json:"session,omitempty"`
}

// SessionAction describes a session operation.
type SessionAction struct {
    Action    string `json:"action"`
    TTLSeconds int    `json:"ttl_seconds,omitempty"`
}
