package sdk

import "context"

// Reaction targets one existing inbound message. ConversationID is the
// provider conversation (the sender for a 1:1, the group ID for a group),
// TargetAuthor is the author of that message, and TargetTimestampMS is the
// provider timestamp from the inbound envelope.
type Reaction struct {
	ConversationID    string
	TargetAuthor      string
	TargetTimestampMS int64
	Emoji             string
	Remove            bool
}

// Reactor attaches a reaction to an existing message. It is optional: a
// provider that cannot react returns an error, and callers fall back to text.
type Reactor interface {
	React(ctx context.Context, reaction Reaction) error
}
