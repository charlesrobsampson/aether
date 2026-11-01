package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/queue"
)

// fakeQueue records the jobs the /message handler accepts.
type fakeQueue struct {
	jobs []queue.Job
}

func (q *fakeQueue) Enqueue(job queue.Job) error {
	q.jobs = append(q.jobs, job)
	return nil
}

// TestMessageSingle covers the satellite-facing /message contract for a
// single recipient: JWT enforcement, payload validation, denied-recipient
// dropping, and the exact job handed to the outbound queue.
func TestMessageSingle(t *testing.T) {
	const (
		secret    = "message-test-secret"
		sender    = "+15555550100"
		recipient = "+15555550102"
		denied    = "+15555550999"
		provider  = "signal"
	)

	cfg := &config.Config{
		System: config.SystemConfig{
			Auth:            config.AuthConfig{JWTSecret: secret},
			DefaultProvider: provider,
		},
		Admin: config.AdminConfig{
			ApprovedNumbers: []string{sender},
			DeniedNumbers:   []string{denied},
		},
	}

	mint := func(t *testing.T, key string, role auth.Role, ttl time.Duration) string {
		t.Helper()
		token, err := auth.Issue(key, sender, role, ttl)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		return token
	}
	outbound := func(t *testing.T, msg model.Outbound) string {
		t.Helper()
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal outbound: %v", err)
		}
		return string(raw)
	}

	tests := []struct {
		name       string
		body       string
		token      func(*testing.T) string
		wantStatus int
		wantJobs   int
		wantRecip  string
		wantText   string
		wantProvdr string
	}{
		{
			name: "valid single recipient",
			body: outbound(t, model.Outbound{
				Recipients: []string{recipient},
				Text:       "hello",
				Source:     "satellite",
				Satellite:  "wind-predictor",
			}),
			token:      func(t *testing.T) string { return mint(t, secret, auth.Admin, time.Hour) },
			wantStatus: http.StatusAccepted,
			wantJobs:   1,
			wantRecip:  recipient,
			wantText:   "hello",
			wantProvdr: provider,
		},
		{
			name:       "malformed json",
			body:       `{"recipients":`,
			token:      func(t *testing.T) string { return mint(t, secret, auth.Admin, time.Hour) },
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing recipients",
			body:       `{"text":"hello"}`,
			token:      func(t *testing.T) string { return mint(t, secret, auth.Admin, time.Hour) },
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "blank text",
			body:       `{"recipients":["` + recipient + `"],"text":"   "}`,
			token:      func(t *testing.T) string { return mint(t, secret, auth.Admin, time.Hour) },
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "recipients and group are mutually exclusive",
			body:       `{"recipients":["` + recipient + `"],"group_chat_id":"group-1","text":"hello"}`,
			token:      func(t *testing.T) string { return mint(t, secret, auth.Admin, time.Hour) },
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing token",
			body:       outbound(t, model.Outbound{Recipients: []string{recipient}, Text: "hello"}),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid token",
			body:       outbound(t, model.Outbound{Recipients: []string{recipient}, Text: "hello"}),
			token:      func(t *testing.T) string { return "not-a-jwt" },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "expired token",
			body:       outbound(t, model.Outbound{Recipients: []string{recipient}, Text: "hello"}),
			token:      func(t *testing.T) string { return mint(t, secret, auth.Admin, -time.Minute) },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong secret",
			body:       outbound(t, model.Outbound{Recipients: []string{recipient}, Text: "hello"}),
			token:      func(t *testing.T) string { return mint(t, "wrong-secret", auth.Admin, time.Hour) },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "user role is insufficient",
			body:       outbound(t, model.Outbound{Recipients: []string{recipient}, Text: "hello"}),
			token:      func(t *testing.T) string { return mint(t, secret, auth.User, time.Hour) },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "denied recipient is dropped",
			body:       outbound(t, model.Outbound{Recipients: []string{denied}, Text: "hello"}),
			token:      func(t *testing.T) string { return mint(t, secret, auth.Admin, time.Hour) },
			wantStatus: http.StatusAccepted,
			wantJobs:   0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jobs := &fakeQueue{}
			handler := NewMessageHandler(MessageOptions{
				Config: func() *config.Config { return cfg },
				Queue:  jobs,
			})

			req := httptest.NewRequest(http.MethodPost, "/message", strings.NewReader(tc.body))
			if tc.token != nil {
				req.Header.Set("Authorization", "Bearer "+tc.token(t))
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if len(jobs.jobs) != tc.wantJobs {
				t.Fatalf("jobs = %d, want %d: %+v", len(jobs.jobs), tc.wantJobs, jobs.jobs)
			}
			if tc.wantJobs == 1 {
				job := jobs.jobs[0]
				if job.Recipient != tc.wantRecip {
					t.Errorf("job recipient = %q, want %q", job.Recipient, tc.wantRecip)
				}
				if job.Text != tc.wantText {
					t.Errorf("job text = %q, want %q", job.Text, tc.wantText)
				}
				if job.Provider != tc.wantProvdr {
					t.Errorf("job provider = %q, want %q", job.Provider, tc.wantProvdr)
				}
			}
			if tc.wantStatus == http.StatusBadRequest {
				body := rec.Body.String()
				for _, leak := range []string{"unexpected end", "invalid character", "EOF"} {
					if strings.Contains(body, leak) {
						t.Errorf("parse error leaked to caller: %q", body)
					}
				}
			}
		})
	}
}
