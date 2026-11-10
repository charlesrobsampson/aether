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
	"github.com/charlesrobsampson/aether/internal/metrics"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/queue"
)

// fakeQueue records the jobs the /message handler accepts. err, when set, is
// returned by EnqueueBatch so tests can exercise saturation handling.
type fakeQueue struct {
	jobs []queue.Job
	err  error
}

func (q *fakeQueue) Enqueue(job queue.Job) error {
	q.jobs = append(q.jobs, job)
	return nil
}

func (q *fakeQueue) EnqueueBatch(jobs []queue.Job) error {
	if q.err != nil {
		return q.err
	}
	q.jobs = append(q.jobs, jobs...)
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
		wantStyled bool
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
			name: "styled format is propagated",
			body: outbound(t, model.Outbound{
				Recipients: []string{recipient},
				Text:       "*bold*",
				Format:     model.FormatStyled,
			}),
			token:      func(t *testing.T) string { return mint(t, secret, auth.Admin, time.Hour) },
			wantStatus: http.StatusAccepted,
			wantJobs:   1,
			wantRecip:  recipient,
			wantText:   "*bold*",
			wantProvdr: provider,
			wantStyled: true,
		},
		{
			name: "unknown format is rejected",
			body: outbound(t, model.Outbound{
				Recipients: []string{recipient},
				Text:       "hello",
				Format:     "markdown",
			}),
			token:      func(t *testing.T) string { return mint(t, secret, auth.Admin, time.Hour) },
			wantStatus: http.StatusBadRequest,
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
				if job.Styled != tc.wantStyled {
					t.Errorf("job styled = %v, want %v", job.Styled, tc.wantStyled)
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

// TestMessageBulk checks that a multi-recipient /message request fans out to
// one queue job per accepted recipient and counts exactly one bulk request.
// A single-recipient request must not increment the bulk counter.
func TestMessageBulk(t *testing.T) {
	const (
		secret   = "bulk-test-secret"
		sender   = "+15555550100"
		provider = "signal"
	)
	recipients := []string{"+15555550101", "+15555550102", "+15555550103"}

	cfg := &config.Config{
		System: config.SystemConfig{
			Auth:            config.AuthConfig{JWTSecret: secret},
			DefaultProvider: provider,
		},
		Admin: config.AdminConfig{ApprovedNumbers: []string{sender}},
	}

	token, err := auth.Issue(secret, sender, auth.Admin, time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	jobs := &fakeQueue{}
	handler := NewMessageHandler(MessageOptions{
		Config: func() *config.Config { return cfg },
		Queue:  jobs,
	})
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/message", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	before := metrics.Default.Snapshot().BulkMessageRequests

	bulkBody, err := json.Marshal(model.Outbound{
		Recipients: recipients,
		Text:       "release notes",
		Source:     "scheduler",
	})
	if err != nil {
		t.Fatalf("marshal bulk outbound: %v", err)
	}
	if rec := post(string(bulkBody)); rec.Code != http.StatusAccepted {
		t.Fatalf("bulk status = %d, want %d (body %q)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if len(jobs.jobs) != len(recipients) {
		t.Fatalf("bulk jobs = %d, want %d: %+v", len(jobs.jobs), len(recipients), jobs.jobs)
	}
	for i, want := range recipients {
		job := jobs.jobs[i]
		if job.Recipient != want {
			t.Errorf("job %d recipient = %q, want %q", i, job.Recipient, want)
		}
		if job.Text != "release notes" {
			t.Errorf("job %d text = %q, want %q", i, job.Text, "release notes")
		}
		if job.Provider != provider {
			t.Errorf("job %d provider = %q, want %q", i, job.Provider, provider)
		}
	}
	if got := metrics.Default.Snapshot().BulkMessageRequests - before; got != 1 {
		t.Fatalf("bulk counter delta = %d, want 1", got)
	}

	singleBody, err := json.Marshal(model.Outbound{
		Recipients: recipients[:1],
		Text:       "single",
	})
	if err != nil {
		t.Fatalf("marshal single outbound: %v", err)
	}
	if rec := post(string(singleBody)); rec.Code != http.StatusAccepted {
		t.Fatalf("single status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if got := metrics.Default.Snapshot().BulkMessageRequests - before; got != 1 {
		t.Fatalf("bulk counter delta after single send = %d, want 1", got)
	}
	if len(jobs.jobs) != len(recipients)+1 {
		t.Fatalf("jobs after single send = %d, want %d", len(jobs.jobs), len(recipients)+1)
	}
}

// TestMessageGroup covers group sends: an approved group becomes exactly one
// job addressed to the group conversation, an unapproved group is dropped
// silently, and recipients plus group_chat_id is rejected.
func TestMessageGroup(t *testing.T) {
	const (
		secret        = "group-test-secret"
		sender        = "+15555550100"
		provider      = "signal"
		approvedGroup = "group-approved-01"
		otherGroup    = "group-other-02"
	)

	cfg := &config.Config{
		System: config.SystemConfig{
			Auth:            config.AuthConfig{JWTSecret: secret},
			DefaultProvider: provider,
		},
		Admin: config.AdminConfig{
			ApprovedNumbers: []string{sender},
			ApprovedGroups:  []string{approvedGroup},
		},
	}

	token, err := auth.Issue(secret, sender, auth.Admin, time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	tests := []struct {
		name          string
		outbound      model.Outbound
		wantStatus    int
		wantJobs      int
		wantRecipient string
	}{
		{
			name:          "approved group",
			outbound:      model.Outbound{GroupChatID: approvedGroup, Text: "hello group"},
			wantStatus:    http.StatusAccepted,
			wantJobs:      1,
			wantRecipient: approvedGroup,
		},
		{
			name:       "unapproved group is dropped",
			outbound:   model.Outbound{GroupChatID: otherGroup, Text: "hello group"},
			wantStatus: http.StatusAccepted,
			wantJobs:   0,
		},
		{
			name: "recipients and group are mutually exclusive",
			outbound: model.Outbound{
				Recipients:  []string{"+15555550101"},
				GroupChatID: approvedGroup,
				Text:        "hello",
			},
			wantStatus: http.StatusBadRequest,
			wantJobs:   0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.outbound)
			if err != nil {
				t.Fatalf("marshal outbound: %v", err)
			}

			jobs := &fakeQueue{}
			handler := NewMessageHandler(MessageOptions{
				Config: func() *config.Config { return cfg },
				Queue:  jobs,
			})
			req := httptest.NewRequest(http.MethodPost, "/message", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer "+token)
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
				if job.Recipient != tc.wantRecipient {
					t.Errorf("job recipient = %q, want %q", job.Recipient, tc.wantRecipient)
				}
				if job.Text != "hello group" {
					t.Errorf("job text = %q, want %q", job.Text, "hello group")
				}
				if job.Provider != provider {
					t.Errorf("job provider = %q, want %q", job.Provider, provider)
				}
			}
		})
	}
}

// TestMessageQueueFullReturns429 proves a saturated outbound queue pushes back:
// the handler returns 429 with Retry-After and does not enqueue a partial batch
// (plan §3.3).
func TestMessageQueueFullReturns429(t *testing.T) {
	const (
		secret   = "full-test-secret"
		sender   = "+15555550100"
		provider = "signal"
	)

	cfg := &config.Config{
		System: config.SystemConfig{
			Auth:            config.AuthConfig{JWTSecret: secret},
			DefaultProvider: provider,
		},
		Admin: config.AdminConfig{ApprovedNumbers: []string{sender}},
	}

	token, err := auth.Issue(secret, sender, auth.Admin, time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	jobs := &fakeQueue{err: queue.ErrFull}
	handler := NewMessageHandler(MessageOptions{
		Config: func() *config.Config { return cfg },
		Queue:  jobs,
	})

	body, err := json.Marshal(model.Outbound{
		Recipients: []string{"+15555550201", "+15555550202"},
		Text:       "saturated",
	})
	if err != nil {
		t.Fatalf("marshal outbound: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/message", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if len(jobs.jobs) != 0 {
		t.Fatalf("jobs = %d, want 0 when the batch is rejected: %+v", len(jobs.jobs), jobs.jobs)
	}
}
