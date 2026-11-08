package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/metrics"
	"github.com/charlesrobsampson/aether/internal/model"
)

// TestReceiveHandler covers the transport-facing /receive contract: parse and
// access errors never reach the caller as details, approved senders dispatch,
// denied senders are dropped silently, and unknown senders trigger exactly one
// approval request.
func TestReceiveHandler(t *testing.T) {
	const (
		approved = "+15555550102"
		denied   = "+15555550999"
		unknown  = "+15555550104"
		token    = "ingress-secret"
	)

	baseConfig := func() *config.Config {
		return &config.Config{
			Admin: config.AdminConfig{
				ApprovedNumbers: []string{approved},
				DeniedNumbers:   []string{denied},
			},
		}
	}
	tokenConfig := func() *config.Config {
		cfg := baseConfig()
		cfg.System.Auth.IngressToken = token
		return cfg
	}
	inbound := func(t *testing.T, sender, text string) string {
		t.Helper()
		raw, err := json.Marshal(model.Inbound{
			Sender:    sender,
			Text:      text,
			Provider:  "signal",
			GroupID:   "group-1",
			MessageID: "upstream-1",
		})
		if err != nil {
			t.Fatalf("marshal inbound: %v", err)
		}
		return string(raw)
	}

	tests := []struct {
		name         string
		cfg          func() *config.Config
		body         string
		token        string
		wantStatus   int
		wantHandled  int
		wantUnknown  int
		wantSender   string
		wantVanished bool
	}{
		{
			name:       "malformed json",
			cfg:        baseConfig,
			body:       `{"sender":`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing sender",
			cfg:        baseConfig,
			body:       `{"text":"!test"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing text",
			cfg:        baseConfig,
			body:       `{"sender":"` + approved + `"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "blank text",
			cfg:        baseConfig,
			body:       `{"sender":"` + approved + `","text":"   "}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "wrong token",
			cfg:        tokenConfig,
			body:       inbound(t, approved, "!test"),
			token:      "not-the-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing token when configured",
			cfg:        tokenConfig,
			body:       inbound(t, approved, "!test"),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:        "valid approved sender",
			cfg:         baseConfig,
			body:        inbound(t, approved, "!test"),
			wantStatus:  http.StatusAccepted,
			wantHandled: 1,
			wantSender:  approved,
		},
		{
			name:        "valid approved sender with token",
			cfg:         tokenConfig,
			body:        inbound(t, approved, "!test"),
			token:       token,
			wantStatus:  http.StatusAccepted,
			wantHandled: 1,
			wantSender:  approved,
		},
		{
			name:         "denied sender drops silently",
			cfg:          baseConfig,
			body:         inbound(t, denied, "!test"),
			wantStatus:   http.StatusAccepted,
			wantVanished: true,
		},
		{
			name:         "unknown sender asks for approval once",
			cfg:          baseConfig,
			body:         inbound(t, unknown, "!test"),
			wantStatus:   http.StatusAccepted,
			wantUnknown:  1,
			wantSender:   unknown,
			wantVanished: true,
		},
		{
			name:       "wrong method",
			cfg:        baseConfig,
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var handled []model.Inbound
			var unknownCalls []string
			handler := NewReceiveHandler(ReceiveOptions{
				Config: tc.cfg,
				Handle: func(_ context.Context, msg model.Inbound) error {
					handled = append(handled, msg)
					return nil
				},
				OnUnknown: func(phone string) {
					unknownCalls = append(unknownCalls, phone)
				},
			})

			method := http.MethodPost
			if tc.name == "wrong method" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, "/receive", strings.NewReader(tc.body))
			if tc.token != "" {
				req.Header.Set("X-Aether-Token", tc.token)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if len(handled) != tc.wantHandled {
				t.Fatalf("handled calls = %d, want %d", len(handled), tc.wantHandled)
			}
			if len(unknownCalls) != tc.wantUnknown {
				t.Fatalf("unknown calls = %d, want %d: %v", len(unknownCalls), tc.wantUnknown, unknownCalls)
			}
			if tc.wantUnknown == 1 && unknownCalls[0] != tc.wantSender {
				t.Errorf("unknown phone = %q, want %q", unknownCalls[0], tc.wantSender)
			}
			if tc.wantHandled == 1 {
				got := handled[0]
				if got.Sender != tc.wantSender {
					t.Errorf("handled sender = %q, want %q", got.Sender, tc.wantSender)
				}
				if got.Text != "!test" || got.Provider != "signal" ||
					got.GroupID != "group-1" || got.MessageID != "upstream-1" {
					t.Errorf("handled payload = %+v, want the full inbound envelope", got)
				}
			}
			if tc.wantVanished && len(handled) != 0 {
				t.Errorf("dropped sender was dispatched: %+v", handled)
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

// TestReceiveQueueFull proves an accepted sender whose dispatch queue cannot
// take the message receives 429 with a Retry-After hint instead of an ACK. Any
// other submit failure is a 500 because the message was not accepted.
func TestReceiveQueueFull(t *testing.T) {
	const approved = "+15555550102"

	cfg := func() *config.Config {
		return &config.Config{
			Admin: config.AdminConfig{ApprovedNumbers: []string{approved}},
		}
	}
	body := `{"sender":"` + approved + `","text":"!test"}`

	t.Run("queue full", func(t *testing.T) {
		var handled int
		handler := NewReceiveHandler(ReceiveOptions{
			Config: cfg,
			Handle: func(_ context.Context, _ model.Inbound) error {
				handled++
				return ErrQueueFull
			},
		})

		req := httptest.NewRequest(http.MethodPost, "/receive", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 (body %q)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Fatalf("Retry-After = %q, want 1", got)
		}
		if handled != 1 {
			t.Fatalf("submit calls = %d, want 1", handled)
		}
	})

	t.Run("other submit failure is 500", func(t *testing.T) {
		handler := NewReceiveHandler(ReceiveOptions{
			Config: cfg,
			Handle: func(_ context.Context, _ model.Inbound) error {
				return errors.New("dispatch unavailable")
			},
		})

		req := httptest.NewRequest(http.MethodPost, "/receive", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Fatalf("Retry-After = %q, want it unset for a non-retryable failure", got)
		}
	})
}

// TestReceiveDedupeHitReturnsAccepted proves a duplicate claim is acknowledged
// without enqueuing the message again.
func TestReceiveDedupeHitReturnsAccepted(t *testing.T) {
	const approved = "+15555550102"

	cfg := func() *config.Config {
		return &config.Config{
			Admin: config.AdminConfig{ApprovedNumbers: []string{approved}},
		}
	}

	var (
		claimed bool
		handled int
	)
	handler := NewReceiveHandler(ReceiveOptions{
		Config: cfg,
		Claim: func(_ context.Context, sender, messageID string) (bool, error) {
			claimed = true
			if sender != approved || messageID != "dup-1" {
				t.Errorf("Claim(%q, %q), want (%q, dup-1)", sender, messageID, approved)
			}
			return false, nil
		},
		Handle: func(_ context.Context, _ model.Inbound) error {
			handled++
			return nil
		},
	})

	body := `{"sender":"` + approved + `","text":"!test","message_id":"dup-1"}`
	req := httptest.NewRequest(http.MethodPost, "/receive", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	if !claimed {
		t.Fatal("Claim was not called")
	}
	if handled != 0 {
		t.Fatalf("submit calls = %d, want 0 for a duplicate", handled)
	}
}

// TestReceiveDedupeErrorFailsOpen proves a dedupe failure never drops a
// message: the error is logged and the message is submitted anyway.
func TestReceiveDedupeErrorFailsOpen(t *testing.T) {
	const approved = "+15555550102"

	cfg := func() *config.Config {
		return &config.Config{
			Admin: config.AdminConfig{ApprovedNumbers: []string{approved}},
		}
	}

	var (
		handled int
		logged  []string
	)
	handler := NewReceiveHandler(ReceiveOptions{
		Config: cfg,
		Claim: func(_ context.Context, _, _ string) (bool, error) {
			return false, errors.New("store unavailable")
		},
		Logf: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
		Handle: func(_ context.Context, _ model.Inbound) error {
			handled++
			return nil
		},
	})

	body := `{"sender":"` + approved + `","text":"!test","message_id":"dup-2"}`
	req := httptest.NewRequest(http.MethodPost, "/receive", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	if handled != 1 {
		t.Fatalf("submit calls = %d, want 1 when the claim fails open", handled)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "dedupe") {
		t.Fatalf("log lines = %v, want one dedupe failure line", logged)
	}
}

// TestReceiveDedupeReleasedOnSubmitFailure proves a claim is released when the
// dispatch queue rejects the message, so the transport's 429 retry is not
// mistaken for a duplicate and silently dropped.
func TestReceiveDedupeReleasedOnSubmitFailure(t *testing.T) {
	const approved = "+15555550102"

	cfg := func() *config.Config {
		return &config.Config{
			Admin: config.AdminConfig{ApprovedNumbers: []string{approved}},
		}
	}

	var released bool
	handler := NewReceiveHandler(ReceiveOptions{
		Config: cfg,
		Claim: func(_ context.Context, _, _ string) (bool, error) {
			return true, nil
		},
		Release: func(_ context.Context, sender, messageID string) error {
			released = true
			if sender != approved || messageID != "retry-1" {
				t.Errorf("Release(%q, %q), want (%q, retry-1)", sender, messageID, approved)
			}
			return nil
		},
		Handle: func(_ context.Context, _ model.Inbound) error {
			return ErrQueueFull
		},
		Logf: func(string, ...any) {},
	})

	body := `{"sender":"` + approved + `","text":"!test","message_id":"retry-1"}`
	req := httptest.NewRequest(http.MethodPost, "/receive", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %q)", rec.Code, rec.Body.String())
	}
	if !released {
		t.Fatal("claim was not released after the queue rejected the message")
	}
}

// TestReceiveMetrics proves every documented /receive outcome increments its
// result counter and that a suppressed duplicate also increments the dedupe
// counter (plan §3.19).
func TestReceiveMetrics(t *testing.T) {
	const approved = "+15555550102"

	body := `{"sender":"` + approved + `","text":"!test","message_id":"metric-1"}`
	cfg := func() *config.Config {
		return &config.Config{
			Admin: config.AdminConfig{ApprovedNumbers: []string{approved}},
		}
	}
	tokenConfig := func() *config.Config {
		config := cfg()
		config.System.Auth.IngressToken = "ingress-secret"
		return config
	}

	tests := []struct {
		name       string
		cfg        func() *config.Config
		body       string
		token      string
		configure  func(*ReceiveOptions)
		wantStatus int
		wantResult string
		wantDedupe uint64
	}{
		{
			name:       "accepted",
			cfg:        cfg,
			body:       body,
			wantStatus: http.StatusAccepted,
			wantResult: metrics.ReceiveAccepted,
		},
		{
			name:       "duplicate",
			cfg:        cfg,
			body:       body,
			wantStatus: http.StatusAccepted,
			wantResult: metrics.ReceiveDuplicate,
			wantDedupe: 1,
			configure: func(options *ReceiveOptions) {
				options.Claim = func(context.Context, string, string) (bool, error) {
					return false, nil
				}
			},
		},
		{
			name: "queue full",
			cfg:  cfg,
			body: body,
			configure: func(options *ReceiveOptions) {
				options.Handle = func(context.Context, model.Inbound) error {
					return ErrQueueFull
				}
			},
			wantStatus: http.StatusTooManyRequests,
			wantResult: metrics.ReceiveQueueFull,
		},
		{
			name:       "bad request",
			cfg:        cfg,
			body:       `{"sender":`,
			wantStatus: http.StatusBadRequest,
			wantResult: metrics.ReceiveBadRequest,
		},
		{
			name:       "unauthorized",
			cfg:        tokenConfig,
			body:       body,
			wantStatus: http.StatusUnauthorized,
			wantResult: metrics.ReceiveUnauthorized,
		},
	}

	results := []string{
		metrics.ReceiveAccepted,
		metrics.ReceiveDuplicate,
		metrics.ReceiveQueueFull,
		metrics.ReceiveBadRequest,
		metrics.ReceiveUnauthorized,
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry := metrics.New()
			options := ReceiveOptions{
				Config:  tc.cfg,
				Metrics: registry,
				Handle:  func(context.Context, model.Inbound) error { return nil },
			}
			if tc.configure != nil {
				tc.configure(&options)
			}

			req := httptest.NewRequest(http.MethodPost, "/receive", strings.NewReader(tc.body))
			if tc.token != "" {
				req.Header.Set("X-Aether-Token", tc.token)
			}
			rec := httptest.NewRecorder()
			NewReceiveHandler(options).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			for _, result := range results {
				want := uint64(0)
				if result == tc.wantResult {
					want = 1
				}
				if got := registry.ReceiveRequests(result); got != want {
					t.Errorf("receive_requests_total{result=%q} = %d, want %d", result, got, want)
				}
			}
			if got := registry.DedupeHits(); got != tc.wantDedupe {
				t.Errorf("dedupe hits = %d, want %d", got, tc.wantDedupe)
			}
			if tc.wantResult == metrics.ReceiveQueueFull {
				if got := registry.QueueFull(metrics.QueueInbound); got != 1 {
					t.Errorf("queue_full_total{queue=inbound} = %d, want 1", got)
				}
			}
		})
	}
}
