package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/model"
)

type sessionCall struct {
	phone     string
	satellite string
	thread    string
	action    model.SessionAction
}

type fakeSessionApplier struct {
	calls []sessionCall
	err   error
}

func (f *fakeSessionApplier) ApplySessionAction(phone, satellite, thread string, action model.SessionAction) error {
	f.calls = append(f.calls, sessionCall{
		phone:     phone,
		satellite: satellite,
		thread:    thread,
		action:    action,
	})
	return f.err
}

// TestMessageSessionAction verifies that /message applies a session action once
// per addressed recipient and rejects an action the applier refuses.
func TestMessageSessionAction(t *testing.T) {
	const (
		secret    = "session-message-secret"
		sender    = "+15555550100"
		recipient = "+15555550102"
	)

	cfg := &config.Config{
		System: config.SystemConfig{
			Auth:            config.AuthConfig{JWTSecret: secret},
			DefaultProvider: "signal",
		},
		Admin: config.AdminConfig{ApprovedNumbers: []string{sender}},
	}

	token, err := auth.Issue(secret, sender, auth.Admin, time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	tests := []struct {
		name       string
		outbound   model.Outbound
		applierErr error
		wantStatus int
		wantCalls  int
		wantJobs   int
		wantAction string
		wantThread string
		wantTTL    int
	}{
		{
			name: "lock with explicit thread",
			outbound: model.Outbound{
				Recipients: []string{recipient},
				Satellite:  "jellyfin",
				ThreadID:   "main",
				Session:    &model.SessionAction{Action: "lock", TTLSeconds: 60},
			},
			wantStatus: http.StatusAccepted,
			wantCalls:  1,
			wantJobs:   1,
			wantAction: "lock",
			wantThread: "main",
			wantTTL:    60,
		},
		{
			name: "close without text",
			outbound: model.Outbound{
				Recipients: []string{recipient},
				Satellite:  "jellyfin",
				Session:    &model.SessionAction{Action: "close"},
			},
			wantStatus: http.StatusAccepted,
			wantCalls:  1,
			wantJobs:   1,
			wantAction: "close",
		},
		{
			name: "action applies to every recipient",
			outbound: model.Outbound{
				Recipients: []string{recipient, "+15555550103"},
				Satellite:  "jellyfin",
				Session:    &model.SessionAction{Action: "extend", TTLSeconds: 120},
			},
			wantStatus: http.StatusAccepted,
			wantCalls:  2,
			wantJobs:   2,
			wantAction: "extend",
			wantTTL:    120,
		},
		{
			name: "rejected action is a bad request",
			outbound: model.Outbound{
				Recipients: []string{recipient},
				Satellite:  "jellyfin",
				Session:    &model.SessionAction{Action: "explode"},
			},
			applierErr: errors.New("unknown session action"),
			wantStatus: http.StatusBadRequest,
			wantCalls:  1,
			wantJobs:   0,
			wantAction: "explode",
		},
		{
			name: "no session action is not applied",
			outbound: model.Outbound{
				Recipients: []string{recipient},
				Text:       "hello",
				Satellite:  "jellyfin",
			},
			wantStatus: http.StatusAccepted,
			wantCalls:  0,
			wantJobs:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.outbound)
			if err != nil {
				t.Fatalf("marshal outbound: %v", err)
			}

			jobs := &fakeQueue{}
			applier := &fakeSessionApplier{err: tc.applierErr}
			handler := NewMessageHandler(MessageOptions{
				Config:   func() *config.Config { return cfg },
				Queue:    jobs,
				Sessions: applier,
			})

			req := httptest.NewRequest(http.MethodPost, "/message", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if len(applier.calls) != tc.wantCalls {
				t.Fatalf("session calls = %d, want %d: %+v", len(applier.calls), tc.wantCalls, applier.calls)
			}
			for i, call := range applier.calls {
				if call.satellite != "jellyfin" {
					t.Errorf("call %d satellite = %q, want jellyfin", i, call.satellite)
				}
				if call.thread != tc.wantThread {
					t.Errorf("call %d thread = %q, want %q", i, call.thread, tc.wantThread)
				}
				if call.action.Action != tc.wantAction {
					t.Errorf("call %d action = %q, want %q", i, call.action.Action, tc.wantAction)
				}
				if call.action.TTLSeconds != tc.wantTTL {
					t.Errorf("call %d ttl = %d, want %d", i, call.action.TTLSeconds, tc.wantTTL)
				}
			}
			if len(jobs.jobs) != tc.wantJobs {
				t.Fatalf("jobs = %d, want %d: %+v", len(jobs.jobs), tc.wantJobs, jobs.jobs)
			}
		})
	}
}
