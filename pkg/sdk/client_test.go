package sdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSDKClient verifies the outbound client used by satellites: it posts a
// Response to Aether's /message endpoint with a bearer token, and treats a
// non-2xx reply as an error.
func TestSDKClient(t *testing.T) {
	t.Run("posts the response with a bearer token", func(t *testing.T) {
		type received struct {
			method      string
			path        string
			authorize   string
			contentType string
			body        Response
			decodeErr   error
		}
		got := make(chan received, 1)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body Response
			err := json.NewDecoder(r.Body).Decode(&body)
			got <- received{
				method:      r.Method,
				path:        r.URL.Path,
				authorize:   r.Header.Get("Authorization"),
				contentType: r.Header.Get("Content-Type"),
				body:        body,
				decodeErr:   err,
			}
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(server.Close)

		client := NewClient(server.URL, "service-token")
		err := client.Send(context.Background(), Response{
			Recipients: []string{"+15555550102"},
			Text:       "wind alert",
			Satellite:  "wind-predictor",
			ThreadID:   "main",
			Session:    &SessionAction{Action: "extend", TTLSeconds: 120},
		})
		if err != nil {
			t.Fatalf("Send() error = %v", err)
		}

		request := <-got
		if request.method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.method)
		}
		if request.path != "/message" {
			t.Errorf("path = %q, want /message", request.path)
		}
		if request.authorize != "Bearer service-token" {
			t.Errorf("Authorization = %q, want %q", request.authorize, "Bearer service-token")
		}
		if request.contentType != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", request.contentType)
		}
		if request.decodeErr != nil {
			t.Fatalf("request body is not a Response: %v", request.decodeErr)
		}
		if len(request.body.Recipients) != 1 || request.body.Recipients[0] != "+15555550102" {
			t.Errorf("recipients = %v", request.body.Recipients)
		}
		if request.body.Text != "wind alert" || request.body.Satellite != "wind-predictor" || request.body.ThreadID != "main" {
			t.Errorf("body = %+v", request.body)
		}
		if request.body.Session == nil || request.body.Session.Action != "extend" || request.body.Session.TTLSeconds != 120 {
			t.Errorf("session = %+v", request.body.Session)
		}
	})

	t.Run("non-2xx is an error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "denied", http.StatusForbidden)
		}))
		t.Cleanup(server.Close)

		client := NewClient(server.URL, "service-token")
		if err := client.Send(context.Background(), Response{Recipients: []string{"+1"}, Text: "hi"}); err == nil {
			t.Fatal("Send() error = nil, want an error for a 403 response")
		}
	})
}
