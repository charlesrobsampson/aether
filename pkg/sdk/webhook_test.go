package sdk

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSDKWebhook covers the contract satellites rely on when Aether dispatches
// a command: the transmission envelope, request validation, and the response
// helper used to answer Aether.
func TestSDKWebhook(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		want        *Transmission
		wantErr     bool
	}{
		{
			name:        "valid JSON",
			method:      http.MethodPost,
			contentType: "application/json",
			body: `{
				"command": "remind",
				"trigger": "remind",
				"args": {"msg": "call bob", "num": 5, "unit": "minutes"},
				"text": "remind me call bob in 5 minutes",
				"sender": "+15555550102",
				"satellite": "reminder",
				"thread_id": "main"
			}`,
			want: &Transmission{
				Command:   "remind",
				Trigger:   "remind",
				Args:      map[string]any{"msg": "call bob", "num": float64(5), "unit": "minutes"},
				Text:      "remind me call bob in 5 minutes",
				Sender:    "+15555550102",
				Satellite: "reminder",
				ThreadID:  "main",
			},
		},
		{
			name:        "valid JSON with charset",
			method:      http.MethodPost,
			contentType: "application/json; charset=utf-8",
			body:        `{"command":"test","text":"test","sender":"+15555550102"}`,
			want: &Transmission{
				Command: "test",
				Text:    "test",
				Sender:  "+15555550102",
			},
		},
		{
			name:        "wrong method",
			method:      http.MethodGet,
			contentType: "application/json",
			body:        `{"command":"test"}`,
			wantErr:     true,
		},
		{
			name:        "wrong content type",
			method:      http.MethodPost,
			contentType: "text/plain",
			body:        `{"command":"test"}`,
			wantErr:     true,
		},
		{
			name:    "missing content type",
			method:  http.MethodPost,
			body:    `{"command":"test"}`,
			wantErr: true,
		},
		{
			name:        "malformed body",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        `{"command":`,
			wantErr:     true,
		},
		{
			name:        "trailing JSON value",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        `{"command":"test"} {"command":"other"}`,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/", strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}

			got, err := ParseWebhook(req)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseWebhook() error = nil, want an error (got %+v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWebhook() error = %v", err)
			}
			if got.Command != tt.want.Command ||
				got.Trigger != tt.want.Trigger ||
				got.Text != tt.want.Text ||
				got.Sender != tt.want.Sender ||
				got.Satellite != tt.want.Satellite ||
				got.ThreadID != tt.want.ThreadID {
				t.Fatalf("ParseWebhook() = %+v, want %+v", got, tt.want)
			}
			if len(got.Args) != len(tt.want.Args) {
				t.Fatalf("ParseWebhook() args = %v, want %v", got.Args, tt.want.Args)
			}
			for key, value := range tt.want.Args {
				if got.Args[key] != value {
					t.Fatalf("ParseWebhook() args[%q] = %v, want %v", key, got.Args[key], value)
				}
			}
		})
	}

	t.Run("SendResponse writes JSON and the requested status", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		resp := Response{
			Recipients: []string{"+15555550102"},
			Text:       "test received",
			Satellite:  "test",
			ThreadID:   "main",
			Session:    &SessionAction{Action: "lock", TTLSeconds: 60},
			Status:     http.StatusCreated,
		}

		SendResponse(recorder, resp)

		if recorder.Code != http.StatusCreated {
			t.Fatalf("SendResponse() status = %d, want %d", recorder.Code, http.StatusCreated)
		}
		if got := recorder.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("SendResponse() Content-Type = %q, want application/json", got)
		}

		var got Response
		if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
			t.Fatalf("SendResponse() body is not JSON: %v", err)
		}
		if len(got.Recipients) != 1 || got.Recipients[0] != "+15555550102" {
			t.Fatalf("SendResponse() recipients = %v", got.Recipients)
		}
		if got.Text != "test received" || got.Satellite != "test" || got.ThreadID != "main" {
			t.Fatalf("SendResponse() body = %+v", got)
		}
		if got.Session == nil || got.Session.Action != "lock" || got.Session.TTLSeconds != 60 {
			t.Fatalf("SendResponse() session = %+v", got.Session)
		}
	})

	t.Run("SendResponse defaults to 200", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		SendResponse(recorder, Response{Recipients: []string{"+15555550102"}, Text: "ok"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("SendResponse() status = %d, want %d", recorder.Code, http.StatusOK)
		}
	})
}
