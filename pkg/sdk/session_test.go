package sdk

import (
	"encoding/json"
	"testing"
	"time"
)

// TestSDKSessionManager verifies the helpers satellites use to change an active
// conversation: Lock, Extend, and Close build the /message session envelope,
// default thread_id to the satellite name, and never perform HTTP calls.
func TestSDKSessionManager(t *testing.T) {
	tests := []struct {
		name       string
		threadID   string
		call       func(*SessionManager) Response
		wantAction string
		wantTTL    int
		wantThread string
	}{
		{
			name:       "lock defaults thread to satellite",
			call:       func(m *SessionManager) Response { return m.Lock(2 * time.Minute) },
			wantAction: "lock",
			wantTTL:    120,
			wantThread: "wind-predictor",
		},
		{
			name:       "lock keeps an explicit thread",
			threadID:   "jellyfin",
			call:       func(m *SessionManager) Response { return m.Lock(90 * time.Second) },
			wantAction: "lock",
			wantTTL:    90,
			wantThread: "jellyfin",
		},
		{
			name:       "lock without ttl defers to Aether default",
			call:       func(m *SessionManager) Response { return m.Lock(0) },
			wantAction: "lock",
			wantTTL:    0,
			wantThread: "wind-predictor",
		},
		{
			name:       "extend",
			threadID:   "main",
			call:       func(m *SessionManager) Response { return m.Extend(5 * time.Minute) },
			wantAction: "extend",
			wantTTL:    300,
			wantThread: "main",
		},
		{
			name:       "close",
			call:       func(m *SessionManager) Response { return m.Close() },
			wantAction: "close",
			wantTTL:    0,
			wantThread: "wind-predictor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewSessionManager([]string{"+15555550102"}, "wind-predictor", tt.threadID)

			resp := tt.call(manager)

			if len(resp.Recipients) != 1 || resp.Recipients[0] != "+15555550102" {
				t.Fatalf("Recipients = %v, want [+15555550102]", resp.Recipients)
			}
			if resp.Satellite != "wind-predictor" {
				t.Fatalf("Satellite = %q, want wind-predictor", resp.Satellite)
			}
			if resp.ThreadID != tt.wantThread {
				t.Fatalf("ThreadID = %q, want %q", resp.ThreadID, tt.wantThread)
			}
			if resp.Text != "" {
				t.Fatalf("Text = %q, want empty for a session-only response", resp.Text)
			}
			if resp.Session == nil {
				t.Fatal("Session = nil, want a session action")
			}
			if resp.Session.Action != tt.wantAction {
				t.Fatalf("Session.Action = %q, want %q", resp.Session.Action, tt.wantAction)
			}
			if resp.Session.TTLSeconds != tt.wantTTL {
				t.Fatalf("Session.TTLSeconds = %d, want %d", resp.Session.TTLSeconds, tt.wantTTL)
			}

			// The envelope must marshal with the /message JSON names, and an
			// unspecified TTL must be absent rather than zero.
			raw, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			var envelope map[string]any
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			session, ok := envelope["session"].(map[string]any)
			if !ok {
				t.Fatalf("envelope session = %v, want an object", envelope["session"])
			}
			if got := session["action"]; got != tt.wantAction {
				t.Fatalf("envelope session.action = %v, want %q", got, tt.wantAction)
			}
			if _, present := session["ttl_seconds"]; present != (tt.wantTTL > 0) {
				t.Fatalf("envelope session.ttl_seconds present = %v, want %v (session=%v)",
					present, tt.wantTTL > 0, session)
			}
			if got := envelope["thread_id"]; got != tt.wantThread {
				t.Fatalf("envelope thread_id = %v, want %q", got, tt.wantThread)
			}
		})
	}

	t.Run("sub-second ttl rounds up to one second", func(t *testing.T) {
		resp := NewSessionManager([]string{"+15555550102"}, "test", "").Lock(1500 * time.Millisecond)
		if resp.Session == nil || resp.Session.TTLSeconds != 2 {
			t.Fatalf("Session = %+v, want ttl_seconds 2", resp.Session)
		}
	})

	t.Run("recipients are copied at construction", func(t *testing.T) {
		recipients := []string{"+15555550102"}
		manager := NewSessionManager(recipients, "test", "")
		recipients[0] = "+19999999999"

		if got := manager.Close().Recipients[0]; got != "+15555550102" {
			t.Fatalf("Recipients[0] = %q, want +15555550102", got)
		}
	})
}
