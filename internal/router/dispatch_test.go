package router

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/internal/testutil"
)

// dispatchEnvelope mirrors the payload in plan §3.6. Args stay raw so each case
// can compare the exact typed JSON the satellite receives.
type dispatchEnvelope struct {
	Command   string                     `json:"command"`
	Trigger   string                     `json:"trigger"`
	Args      map[string]json.RawMessage `json:"args"`
	Text      string                     `json:"text"`
	Sender    string                     `json:"sender"`
	Satellite string                     `json:"satellite"`
	ThreadID  string                     `json:"thread_id"`
	MessageID string                     `json:"message_id"`
}

type dispatchCall struct {
	method      string
	path        string
	contentType string
	envelope    dispatchEnvelope
}

func TestDispatch(t *testing.T) {
	tests := []struct {
		name string
		text string

		// respondStatus is the satellite's HTTP status; zero means 200.
		respondStatus int
		// badEndpoint points the route at a closed listener instead of the
		// test server, exercising the transport-error path.
		badEndpoint bool

		wantCalls     int
		wantPath      string
		wantCommand   string
		wantTrigger   string
		wantSatellite string
		wantText      string
		wantArgs      map[string]any

		wantStatus     string
		wantErrorPart  string
		wantLogText    string
		wantLogSatName string
	}{
		{
			name:          "prefix stripping",
			text:          "!fly FPS",
			wantCalls:     1,
			wantPath:      "/api/v1/predict/text",
			wantCommand:   "fly",
			wantTrigger:   "fly",
			wantSatellite: "wind-predictor",
			wantText:      "fly FPS",
			wantArgs:      map[string]any{"station": "FPS"},
			wantStatus:    "dispatched",
		},
		{
			name:          "persona prefix",
			text:          "Aether fly FPS",
			wantCalls:     1,
			wantPath:      "/api/v1/predict/text",
			wantCommand:   "fly",
			wantTrigger:   "fly",
			wantSatellite: "wind-predictor",
			wantText:      "fly FPS",
			wantArgs:      map[string]any{"station": "FPS"},
			wantStatus:    "dispatched",
		},
		{
			name:          "satellite alias",
			text:          "!wind FPS",
			wantCalls:     1,
			wantPath:      "/api/v1/predict/text",
			wantCommand:   "fly",
			wantTrigger:   "fly",
			wantSatellite: "wind-predictor",
			wantText:      "fly FPS",
			wantArgs:      map[string]any{"station": "FPS"},
			wantStatus:    "dispatched",
		},
		{
			name:          "command alias",
			text:          "!w FPS",
			wantCalls:     1,
			wantPath:      "/api/v1/predict/text",
			wantCommand:   "fly",
			wantTrigger:   "fly",
			wantSatellite: "wind-predictor",
			wantText:      "fly FPS",
			wantArgs:      map[string]any{"station": "FPS"},
			wantStatus:    "dispatched",
		},
		{
			name:          "typed payload posted to endpoint+path",
			text:          "!remind me call bob in 5 minutes",
			wantCalls:     1,
			wantPath:      "/remind",
			wantCommand:   "remind",
			wantTrigger:   "remind",
			wantSatellite: "reminder",
			wantText:      "remind me call bob in 5 minutes",
			wantArgs:      map[string]any{"msg": "call bob", "num": 5, "unit": "minutes"},
			wantStatus:    "dispatched",
		},
		{
			name:          "unknown trigger dropped",
			text:          "!nope x",
			wantCalls:     0,
			wantStatus:    "dropped",
			wantErrorPart: "unknown trigger",
			wantLogText:   "nope x",
		},
		{
			name:           "no matching command dropped",
			text:           "!fly",
			wantCalls:      0,
			wantStatus:     "dropped",
			wantErrorPart:  "no matching command",
			wantLogText:    "fly",
			wantLogSatName: "wind-predictor",
		},
		{
			name:           "non-2xx records failure",
			text:           "!fly FPS",
			respondStatus:  http.StatusInternalServerError,
			wantCalls:      1,
			wantPath:       "/api/v1/predict/text",
			wantCommand:    "fly",
			wantTrigger:    "fly",
			wantSatellite:  "wind-predictor",
			wantText:       "fly FPS",
			wantArgs:       map[string]any{"station": "FPS"},
			wantStatus:     "failed",
			wantErrorPart:  "500",
			wantLogText:    "fly FPS",
			wantLogSatName: "wind-predictor",
		},
		{
			name:           "transport error records failure",
			text:           "!fly FPS",
			badEndpoint:    true,
			wantCalls:      0,
			wantStatus:     "failed",
			wantErrorPart:  "POST",
			wantLogText:    "fly FPS",
			wantLogSatName: "wind-predictor",
		},
		{
			name:          "route logged",
			text:          "!fly FPS",
			wantCalls:     1,
			wantPath:      "/api/v1/predict/text",
			wantCommand:   "fly",
			wantTrigger:   "fly",
			wantSatellite: "wind-predictor",
			wantText:      "fly FPS",
			wantArgs:      map[string]any{"station": "FPS"},
			wantStatus:    "dispatched",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				calls []dispatchCall
			)

			responseStatus := tc.respondStatus
			if responseStatus == 0 {
				responseStatus = http.StatusOK
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var envelope dispatchEnvelope
				if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
					t.Errorf("satellite received invalid JSON: %v", err)
				}
				mu.Lock()
				calls = append(calls, dispatchCall{
					method:      r.Method,
					path:        r.URL.Path,
					contentType: r.Header.Get("Content-Type"),
					envelope:    envelope,
				})
				mu.Unlock()
				w.WriteHeader(responseStatus)
			}))
			t.Cleanup(server.Close)

			endpoint := server.URL
			if tc.badEndpoint {
				endpoint = closedEndpoint(t)
			}

			root := testutil.TempConfig(t)
			cfg, err := config.LoadConfig(root)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			testutil.WriteJSON(t, filepath.Join(root, "routes.json"), routesForTest(cfg, endpoint))
			cfg, err = config.LoadConfig(root)
			if err != nil {
				t.Fatalf("LoadConfig after routes: %v", err)
			}

			st, err := store.Open(":memory:")
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			t.Cleanup(func() {
				if err := st.Close(); err != nil {
					t.Errorf("store.Close: %v", err)
				}
			})

			rt := New(Options{
				Config: func() *config.Config { return cfg },
				Store:  st,
				Client: server.Client(),
			})
			rt.Dispatch(context.Background(), model.Inbound{
				Sender:   "+15555550102",
				Text:     tc.text,
				Provider: "signal",
			})

			mu.Lock()
			gotCalls := append([]dispatchCall(nil), calls...)
			mu.Unlock()

			if len(gotCalls) != tc.wantCalls {
				t.Fatalf("satellite calls = %d, want %d", len(gotCalls), tc.wantCalls)
			}
			if tc.wantCalls > 0 {
				call := gotCalls[0]
				if call.method != http.MethodPost {
					t.Errorf("method = %q, want POST", call.method)
				}
				if call.path != tc.wantPath {
					t.Errorf("path = %q, want %q", call.path, tc.wantPath)
				}
				if !strings.HasPrefix(call.contentType, "application/json") {
					t.Errorf("Content-Type = %q, want application/json", call.contentType)
				}
				if call.envelope.Command != tc.wantCommand {
					t.Errorf("command = %q, want %q", call.envelope.Command, tc.wantCommand)
				}
				if call.envelope.Trigger != tc.wantTrigger {
					t.Errorf("trigger = %q, want %q", call.envelope.Trigger, tc.wantTrigger)
				}
				if call.envelope.Satellite != tc.wantSatellite {
					t.Errorf("satellite = %q, want %q", call.envelope.Satellite, tc.wantSatellite)
				}
				if call.envelope.Text != tc.wantText {
					t.Errorf("text = %q, want %q", call.envelope.Text, tc.wantText)
				}
				if call.envelope.Sender != "+15555550102" {
					t.Errorf("sender = %q, want +15555550102", call.envelope.Sender)
				}
				assertJSONEqual(t, call.envelope.Args, tc.wantArgs)
			}

			direction, sender, satellite, text, status, errText, createdAt := lastRouteLog(t, st)
			if direction != "inbound" {
				t.Errorf("route_log.direction = %q, want inbound", direction)
			}
			if sender != "+15555550102" {
				t.Errorf("route_log.sender = %q, want +15555550102", sender)
			}
			if status != tc.wantStatus {
				t.Errorf("route_log.status = %q, want %q (error %q)", status, tc.wantStatus, errText)
			}
			if tc.wantErrorPart != "" && !strings.Contains(errText, tc.wantErrorPart) {
				t.Errorf("route_log.error = %q, want it to contain %q", errText, tc.wantErrorPart)
			}
			if tc.wantErrorPart == "" && errText != "" {
				t.Errorf("route_log.error = %q, want empty", errText)
			}
			if tc.wantLogSatName != "" && satellite != tc.wantLogSatName {
				t.Errorf("route_log.satellite = %q, want %q", satellite, tc.wantLogSatName)
			}
			if tc.wantLogText != "" && text != tc.wantLogText {
				t.Errorf("route_log.text = %q, want %q", text, tc.wantLogText)
			}
			if createdAt == "" {
				t.Error("route_log.created_at is empty")
			}
		})
	}
}

// routesForTest rebuilds routes.json from the example satellites, pointing every
// endpoint at endpoint.
func routesForTest(cfg *config.Config, endpoint string) config.RoutesConfig {
	var routes config.RoutesConfig
	for _, name := range []string{"reminder", "wind-predictor"} {
		satellite, ok := cfg.Satellites[name]
		if !ok {
			continue
		}
		routes.Routes = append(routes.Routes, config.RouteEntry{
			Trigger:   satellite.Trigger,
			Satellite: satellite.Name,
			Endpoint:  endpoint,
			Commands:  satellite.Commands,
		})
	}
	return routes
}

// closedEndpoint returns an http URL for a listener that is already closed, so
// the dial fails without touching the network.
func closedEndpoint(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return "http://" + addr
}

func lastRouteLog(t *testing.T, st *store.Store) (direction, sender, satellite, text, status, errText, createdAt string) {
	t.Helper()

	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM route_log`).Scan(&count); err != nil {
		t.Fatalf("count route_log: %v", err)
	}
	if count != 1 {
		t.Fatalf("route_log rows = %d, want 1", count)
	}

	err := st.DB().QueryRow(`
		SELECT direction, sender, satellite, text, status, error, created_at
		FROM route_log ORDER BY id DESC LIMIT 1`).
		Scan(&direction, &sender, &satellite, &text, &status, &errText, &createdAt)
	if err != nil {
		t.Fatalf("read route_log: %v", err)
	}
	return direction, sender, satellite, text, status, errText, createdAt
}

func assertJSONEqual(t *testing.T, got map[string]json.RawMessage, want map[string]any) {
	t.Helper()

	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got args: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want args: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("args = %s, want %s", gotJSON, wantJSON)
	}
}

// TestEnvelopeAndRouteLogCarryMessageID proves the inbound correlation id is
// forwarded to the satellite and audited in route_log (plan §3.6).
func TestEnvelopeAndRouteLogCarryMessageID(t *testing.T) {
	const (
		sender    = "+15555550102"
		messageID = "upstream-42"
	)

	var (
		mu       sync.Mutex
		envelope dispatchEnvelope
		calls    int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got dispatchEnvelope
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("satellite received invalid JSON: %v", err)
		}
		mu.Lock()
		envelope = got
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	root := testutil.TempConfig(t)
	cfg, err := config.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	testutil.WriteJSON(t, filepath.Join(root, "routes.json"), routesForTest(cfg, server.URL))
	cfg, err = config.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig after routes: %v", err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	rt := New(Options{
		Config: func() *config.Config { return cfg },
		Store:  st,
		Client: server.Client(),
	})
	rt.Dispatch(context.Background(), model.Inbound{
		Sender:    sender,
		Text:      "!fly FPS",
		Provider:  "signal",
		MessageID: messageID,
	})

	mu.Lock()
	gotEnvelope := envelope
	gotCalls := calls
	mu.Unlock()

	if gotCalls != 1 {
		t.Fatalf("satellite calls = %d, want 1", gotCalls)
	}
	if gotEnvelope.MessageID != messageID {
		t.Fatalf("envelope message_id = %q, want %q", gotEnvelope.MessageID, messageID)
	}

	var loggedID string
	if err := st.DB().QueryRow(
		`SELECT message_id FROM route_log ORDER BY id DESC LIMIT 1`,
	).Scan(&loggedID); err != nil {
		t.Fatalf("read route_log.message_id: %v", err)
	}
	if loggedID != messageID {
		t.Fatalf("route_log.message_id = %q, want %q", loggedID, messageID)
	}
}
