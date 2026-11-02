package sdk

import (
	"math"
	"time"
)

// SessionManager builds the session-change part of the /message envelope for one
// conversation. It performs no I/O: pass the returned Response to SendResponse
// (in reply to Aether) or to Client.Send (when pushing a new message).
//
// An empty thread ID defaults to the satellite name whenever a session action is
// present (plan §3.3), and a non-positive TTL leaves ttl_seconds unset so Aether
// applies its configured default (plan §3.7).
type SessionManager struct {
	recipients []string
	satellite  string
	threadID   string
}

// NewSessionManager returns a manager for recipients on satellite. The recipient
// slice is copied, so later changes by the caller do not affect the manager.
func NewSessionManager(recipients []string, satellite, threadID string) *SessionManager {
	return &SessionManager{
		recipients: append([]string(nil), recipients...),
		satellite:  satellite,
		threadID:   threadID,
	}
}

// Lock starts (or restarts) the session. A non-positive ttl defers to Aether's
// default session lifetime.
func (m *SessionManager) Lock(ttl time.Duration) Response {
	return m.response("lock", ttl)
}

// Extend pushes the session's expiry out by ttl. A non-positive ttl defers to
// Aether's default session lifetime.
func (m *SessionManager) Extend(ttl time.Duration) Response {
	return m.response("extend", ttl)
}

// Close ends the session. It never sends a TTL.
func (m *SessionManager) Close() Response {
	return m.response("close", 0)
}

// response assembles a copy of the manager's addressing plus the requested
// session action. A nil manager yields an empty response rather than a panic.
func (m *SessionManager) response(action string, ttl time.Duration) Response {
	if m == nil {
		return Response{Session: &SessionAction{Action: action, TTLSeconds: ttlSeconds(ttl)}}
	}

	threadID := m.threadID
	if threadID == "" {
		threadID = m.satellite
	}

	return Response{
		Recipients: append([]string(nil), m.recipients...),
		Satellite:  m.satellite,
		ThreadID:   threadID,
		Session:    &SessionAction{Action: action, TTLSeconds: ttlSeconds(ttl)},
	}
}

// ttlSeconds converts a duration to the whole seconds the envelope carries,
// rounding up so a sub-second TTL cannot become an omitted default. Zero means
// "unset", which is why non-positive durations map to zero.
func ttlSeconds(ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	return int(math.Ceil(ttl.Seconds()))
}
