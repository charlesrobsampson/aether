package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charlesrobsampson/aether/internal/config"
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
