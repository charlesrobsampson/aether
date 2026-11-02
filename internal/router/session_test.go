package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/session"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/internal/testutil"
)

const (
	sessionPhone      = "+15555550102"
	jellyfinSatellite = "jellyfin"
)

// sessionEnv is one hermetic dispatch environment: a recording satellite, a
// temp config tree with a jellyfin route, a FakeClock-backed session store, and
// a Router wired to all of them.
type sessionEnv struct {
	phone    string
	clock    *testutil.FakeClock
	sessions *session.Store
	router   *Router
	store    *store.Store
	server   *httptest.Server

	mu    sync.Mutex
	calls []dispatchCall
}

func newSessionEnv(t *testing.T) *sessionEnv {
	t.Helper()

	env := &sessionEnv{phone: sessionPhone}
	env.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope dispatchEnvelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Errorf("satellite received invalid JSON: %v", err)
		}
		env.mu.Lock()
		env.calls = append(env.calls, dispatchCall{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			envelope:    envelope,
		})
		env.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(env.server.Close)

	root := testutil.TempConfig(t)
	testutil.WriteJSON(t, filepath.Join(root, "routes.json"), config.RoutesConfig{
		Routes: []config.RouteEntry{
			{
				Trigger:   "watch",
				Satellite: jellyfinSatellite,
				Endpoint:  env.server.URL,
				Commands: []config.CommandConfig{
					{
						Name:     "select",
						Path:     "/select",
						Patterns: []string{"<choice>"},
						Args:     []config.ArgDef{{Name: "choice", Type: "int"}},
					},
					{
						Name:     "watch",
						Path:     "/watch",
						Patterns: []string{"watch <genre>"},
						Args:     []config.ArgDef{{Name: "genre", Type: "string"}},
					},
				},
			},
			{
				Trigger:   "fly",
				Satellite: "wind-predictor",
				Endpoint:  env.server.URL,
				Commands: []config.CommandConfig{
					{
						Name:     "fly",
						Path:     "/api/v1/predict/text",
						Patterns: []string{"fly <station>"},
						Args:     []config.ArgDef{{Name: "station", Type: "string"}},
					},
					{
						Name:     "choose",
						Path:     "/wind-choose",
						Patterns: []string{"<choice>"},
						Args:     []config.ArgDef{{Name: "choice", Type: "int"}},
					},
				},
			},
		},
	})

	cfg, err := config.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	env.store, err = store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := env.store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	env.clock = testutil.NewFakeClock(time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC))
	env.sessions = session.New(env.clock)
	env.router = New(Options{
		Config:   func() *config.Config { return cfg },
		Store:    env.store,
		Client:   env.server.Client(),
		Sessions: env.sessions,
	})
	return env
}

func (e *sessionEnv) takeCalls() []dispatchCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]dispatchCall(nil), e.calls...)
	e.calls = nil
	return out
}

// TestSessionRouting covers plan §3.7: an active session intercepts a reply
// before trigger parsing, a #thread-id tag selects one thread, an expired
// session falls through to normal routing, and a session action can close it.
func TestSessionRouting(t *testing.T) {
	t.Run("active session intercepts before trigger parsing", func(t *testing.T) {
		env := newSessionEnv(t)
		env.sessions.Start(env.phone, jellyfinSatellite, "")

		env.router.Dispatch(context.Background(), model.Inbound{
			Sender: env.phone,
			Text:   "2",
		})

		calls := env.takeCalls()
		if len(calls) != 1 {
			t.Fatalf("satellite calls = %d, want 1", len(calls))
		}
		call := calls[0]
		if call.path != "/select" {
			t.Errorf("path = %q, want /select", call.path)
		}
		if call.envelope.Satellite != jellyfinSatellite {
			t.Errorf("satellite = %q, want %q", call.envelope.Satellite, jellyfinSatellite)
		}
		if call.envelope.Command != "select" {
			t.Errorf("command = %q, want select", call.envelope.Command)
		}
		if call.envelope.Text != "2" {
			t.Errorf("text = %q, want 2", call.envelope.Text)
		}
		if call.envelope.ThreadID != "" {
			t.Errorf("thread_id = %q, want empty (untagged session)", call.envelope.ThreadID)
		}
		assertJSONEqual(t, call.envelope.Args, map[string]any{"choice": 2})

		_, _, satellite, text, status, errText, _ := lastRouteLog(t, env.store)
		if satellite != jellyfinSatellite || text != "2" || status != "dispatched" || errText != "" {
			t.Errorf("route_log = (satellite %q, text %q, status %q, error %q), want (%q, %q, %q, empty)",
				satellite, text, status, errText, jellyfinSatellite, "2", "dispatched")
		}
	})

	t.Run("#thread-id targets that thread", func(t *testing.T) {
		env := newSessionEnv(t)
		env.sessions.Start(env.phone, jellyfinSatellite, "jellyfin")
		env.clock.Advance(time.Second)
		// Make a different satellite's thread more recent: the tag, not
		// MostRecent, must decide where this message goes.
		env.sessions.Start(env.phone, "wind-predictor", "other")

		env.router.Dispatch(context.Background(), model.Inbound{
			Sender: env.phone,
			Text:   "#jellyfin 2",
		})

		calls := env.takeCalls()
		if len(calls) != 1 {
			t.Fatalf("satellite calls = %d, want 1", len(calls))
		}
		call := calls[0]
		if call.envelope.Satellite != jellyfinSatellite {
			t.Fatalf("satellite = %q, want %q (tag was ignored)", call.envelope.Satellite, jellyfinSatellite)
		}
		if call.path != "/select" {
			t.Errorf("path = %q, want /select", call.path)
		}
		if call.envelope.ThreadID != "jellyfin" {
			t.Errorf("thread_id = %q, want jellyfin", call.envelope.ThreadID)
		}
		if call.envelope.Text != "2" {
			t.Errorf("text = %q, want 2 (tag should be stripped)", call.envelope.Text)
		}
		assertJSONEqual(t, call.envelope.Args, map[string]any{"choice": 2})

		_, _, satellite, text, status, errText, _ := lastRouteLog(t, env.store)
		if satellite != jellyfinSatellite || text != "2" || status != "dispatched" || errText != "" {
			t.Errorf("route_log = (satellite %q, text %q, status %q, error %q), want (%q, %q, %q, empty)",
				satellite, text, status, errText, jellyfinSatellite, "2", "dispatched")
		}
	})

	t.Run("expired session falls through to trigger parsing", func(t *testing.T) {
		env := newSessionEnv(t)
		env.sessions.StartWithTTL(env.phone, jellyfinSatellite, "", time.Minute)
		env.clock.Advance(2 * time.Minute)

		env.router.Dispatch(context.Background(), model.Inbound{
			Sender: env.phone,
			Text:   "!fly FPS",
		})

		calls := env.takeCalls()
		if len(calls) != 1 {
			t.Fatalf("satellite calls = %d, want 1", len(calls))
		}
		call := calls[0]
		if call.envelope.Satellite != "wind-predictor" {
			t.Fatalf("satellite = %q, want wind-predictor (expired session intercepted)", call.envelope.Satellite)
		}
		if call.path != "/api/v1/predict/text" {
			t.Errorf("path = %q, want /api/v1/predict/text", call.path)
		}
		if call.envelope.Command != "fly" {
			t.Errorf("command = %q, want fly", call.envelope.Command)
		}
		if call.envelope.Text != "fly FPS" {
			t.Errorf("text = %q, want %q", call.envelope.Text, "fly FPS")
		}
		assertJSONEqual(t, call.envelope.Args, map[string]any{"station": "FPS"})
	})

	t.Run("session action lock defaults thread and close removes it", func(t *testing.T) {
		env := newSessionEnv(t)

		if err := env.router.ApplySessionAction(env.phone, jellyfinSatellite, "",
			model.SessionAction{Action: "lock", TTLSeconds: 300}); err != nil {
			t.Fatalf("lock: %v", err)
		}
		if _, ok := env.sessions.Get(env.phone, jellyfinSatellite, jellyfinSatellite); !ok {
			t.Fatal("lock did not create a session keyed by the satellite name")
		}

		if err := env.router.ApplySessionAction(env.phone, jellyfinSatellite, "",
			model.SessionAction{Action: "explode"}); err == nil {
			t.Fatal("unknown session action did not return an error")
		}

		if err := env.router.ApplySessionAction(env.phone, jellyfinSatellite, "",
			model.SessionAction{Action: "close"}); err != nil {
			t.Fatalf("close: %v", err)
		}
		if _, ok := env.sessions.Get(env.phone, jellyfinSatellite, jellyfinSatellite); ok {
			t.Fatal("close did not remove the session")
		}

		// With no live session, "2" is parsed as a trigger and dropped.
		env.router.Dispatch(context.Background(), model.Inbound{
			Sender: env.phone,
			Text:   "2",
		})
		if calls := env.takeCalls(); len(calls) != 0 {
			t.Fatalf("satellite calls after close = %d, want 0", len(calls))
		}

		_, _, _, _, status, errText, _ := lastRouteLog(t, env.store)
		if status != "dropped" {
			t.Errorf("route_log.status = %q, want dropped", status)
		}
		if errText == "" {
			t.Error("route_log.error is empty, want the unknown-trigger reason")
		}
	})

	t.Run("session action extend resets the deadline", func(t *testing.T) {
		env := newSessionEnv(t)
		env.sessions.StartWithTTL(env.phone, jellyfinSatellite, "main", time.Minute)
		env.clock.Advance(30 * time.Second)

		if err := env.router.ApplySessionAction(env.phone, jellyfinSatellite, "main",
			model.SessionAction{Action: "extend", TTLSeconds: 120}); err != nil {
			t.Fatalf("extend: %v", err)
		}

		got, ok := env.sessions.Get(env.phone, jellyfinSatellite, "main")
		if !ok {
			t.Fatal("extend removed or failed to find the session")
		}
		if want := env.clock.Now().Add(120 * time.Second); !got.ExpiresAt.Equal(want) {
			t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want)
		}
	})
}
