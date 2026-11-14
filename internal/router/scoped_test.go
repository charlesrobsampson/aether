package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/internal/testutil"
)

// scopedFixture builds a config with one satellite split across three files,
// plus a recorder for the in-process config and help built-ins.
type scopedFixture struct {
	root       string
	cfg        *config.Config
	store      *store.Store
	server     *httptest.Server
	mu         sync.Mutex
	calls      []dispatchEnvelope
	builtinSat string
	builtinTxt string
	replies    []string
}

func newScopedFixture(t *testing.T) *scopedFixture {
	t.Helper()

	f := &scopedFixture{root: testutil.TempConfig(t)}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope dispatchEnvelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Errorf("decode envelope: %v", err)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, envelope)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.server.Close)

	testutil.WriteJSON(t, filepath.Join(f.root, "satellites", "wind-predictor.json"), config.SatelliteConfig{
		Name:        "wind-predictor",
		Trigger:     "fly",
		Endpoint:    f.server.URL,
		Prefix:      "wind",
		Description: "Run-aware wind and gust predictions for flying sites",
		Commands: []config.CommandConfig{{
			Name:     "fly",
			Path:     "/fly",
			Patterns: []string{"fly <station>"},
			Args:     []config.ArgDef{{Name: "station", Type: "string"}},
		}},
	})
	testutil.WriteJSON(t, filepath.Join(f.root, "satellites", "glider.json"), config.SatelliteConfig{
		Name:        "wind-predictor",
		Trigger:     "glider",
		Endpoint:    f.server.URL,
		Prefix:      "wind",
		Description: "Check the glider",
		Commands: []config.CommandConfig{{
			Name:     "glider_action",
			Path:     "/glider",
			Patterns: []string{"glider <action>"},
			Args:     []config.ArgDef{{Name: "action", Type: "string"}},
		}},
	})
	testutil.WriteJSON(t, filepath.Join(f.root, "satellites", "loaded.json"), config.SatelliteConfig{
		Name:        "wind-predictor",
		Trigger:     "loaded",
		Endpoint:    f.server.URL,
		Prefix:      "wind",
		Description: "Mark the glider loaded",
		Commands: []config.CommandConfig{{
			Name:     "glider_loaded",
			Path:     "/loaded",
			Patterns: []string{"loaded <rest>"},
			Args:     []config.ArgDef{{Name: "rest", Type: "string"}},
		}},
	})

	cfg, err := config.LoadConfig(f.root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	var routes config.RoutesConfig
	for _, name := range []string{"reminder", "wind-predictor", "glider", "loaded"} {
		satellite, ok := cfg.Satellites[name]
		if !ok {
			continue
		}
		satellite.Endpoint = f.server.URL
		routes.Routes = append(routes.Routes, config.RouteEntry{
			Trigger:   satellite.Trigger,
			Satellite: satellite.Name,
			Endpoint:  satellite.Endpoint,
			Commands:  satellite.Commands,
		})
	}
	testutil.WriteJSON(t, filepath.Join(f.root, "routes.json"), routes)

	f.cfg, err = config.LoadConfig(f.root)
	if err != nil {
		t.Fatalf("LoadConfig after routes: %v", err)
	}
	f.store, err = store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := f.store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return f
}

func (f *scopedFixture) dispatch(t *testing.T, text string) {
	t.Helper()

	f.mu.Lock()
	f.calls = nil
	f.builtinSat = ""
	f.builtinTxt = ""
	f.replies = nil
	f.mu.Unlock()

	rt := New(Options{
		Config: func() *config.Config { return f.cfg },
		Store:  f.store,
		Client: f.server.Client(),
		Builtins: []Builtin{
			{
				Satellite:    "config",
				Trigger:      "config",
				RequiredRole: auth.User,
				Handle: func(_ context.Context, _ model.Inbound, text string) error {
					f.mu.Lock()
					f.builtinSat = "config"
					f.builtinTxt = text
					f.mu.Unlock()
					return nil
				},
			},
			{
				Satellite:    "help",
				Trigger:      "help",
				RequiredRole: auth.User,
				Handle: func(_ context.Context, _ model.Inbound, text string) error {
					f.mu.Lock()
					f.builtinSat = "help"
					f.builtinTxt = text
					f.mu.Unlock()
					return nil
				},
			},
		},
		Reply: func(_ context.Context, _ model.Inbound, text string) error {
			f.mu.Lock()
			f.replies = append(f.replies, text)
			f.mu.Unlock()
			return nil
		},
	})
	rt.Dispatch(context.Background(), model.Inbound{
		Sender:   "+15555550102",
		Text:     text,
		Provider: "signal",
	})
}

func (f *scopedFixture) lastCall(t *testing.T) dispatchEnvelope {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 {
		t.Fatalf("satellite calls = %d, want 1", len(f.calls))
	}
	return f.calls[0]
}

func (f *scopedFixture) builtin(t *testing.T) (satellite, text string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.builtinSat, f.builtinTxt
}

func TestScopedSatelliteCommands(t *testing.T) {
	f := newScopedFixture(t)

	t.Run("satellite name prefixes a trigger", func(t *testing.T) {
		f.dispatch(t, "!wind-predictor fly FPS")
		call := f.lastCall(t)
		if call.Trigger != "fly" || call.Text != "fly FPS" || call.Satellite != "wind-predictor" {
			t.Fatalf("envelope = %+v", call)
		}
	})

	t.Run("short prefix selects the satellite", func(t *testing.T) {
		f.dispatch(t, "!wind fly FPS")
		call := f.lastCall(t)
		if call.Trigger != "fly" || call.Text != "fly FPS" || call.Satellite != "wind-predictor" {
			t.Fatalf("envelope = %+v", call)
		}
	})

	t.Run("short prefix selects a sibling file", func(t *testing.T) {
		f.dispatch(t, "!wind loaded the glider")
		call := f.lastCall(t)
		if call.Trigger != "loaded" || call.Text != "loaded the glider" {
			t.Fatalf("envelope = %+v", call)
		}
	})

	t.Run("short prefix forwards config to the primary file", func(t *testing.T) {
		f.dispatch(t, "!wind config set alert_schedule.nightly_local 21:45")
		satellite, text := f.builtin(t)
		if satellite != "config" {
			t.Fatalf("builtin = %q, want config", satellite)
		}
		want := "config set wind-predictor.alert_schedule.nightly_local 21:45"
		if text != want {
			t.Fatalf("builtin text = %q, want %q", text, want)
		}
	})

	t.Run("short prefix help names the canonical satellite", func(t *testing.T) {
		f.dispatch(t, "!wind help")
		satellite, text := f.builtin(t)
		if satellite != "help" || text != "help wind-predictor" {
			t.Fatalf("builtin = %q, text = %q", satellite, text)
		}
	})

	t.Run("satellite alias prefixes a trigger", func(t *testing.T) {
		f.dispatch(t, "!wind glider status")
		call := f.lastCall(t)
		if call.Trigger != "glider" || call.Text != "glider status" {
			t.Fatalf("envelope = %+v", call)
		}
	})

	t.Run("satellite prefix selects a sibling file", func(t *testing.T) {
		f.dispatch(t, "!wind-predictor loaded the glider")
		call := f.lastCall(t)
		if call.Trigger != "loaded" || call.Text != "loaded the glider" {
			t.Fatalf("envelope = %+v", call)
		}
	})

	t.Run("unprefixed trigger still works", func(t *testing.T) {
		f.dispatch(t, "!fly FPS")
		if call := f.lastCall(t); call.Trigger != "fly" || call.Text != "fly FPS" {
			t.Fatalf("envelope = %+v", call)
		}
	})

	t.Run("legacy satellite shorthand still works", func(t *testing.T) {
		f.dispatch(t, "!wind FPS")
		if call := f.lastCall(t); call.Trigger != "fly" || call.Text != "fly FPS" {
			t.Fatalf("envelope = %+v", call)
		}
	})

	t.Run("scoped config forwards the satellite target", func(t *testing.T) {
		f.dispatch(t, "!wind-predictor config set alert_schedule.nightly_local 21:45")
		satellite, text := f.builtin(t)
		if satellite != "config" {
			t.Fatalf("builtin = %q, want config", satellite)
		}
		want := "config set wind-predictor.alert_schedule.nightly_local 21:45"
		if text != want {
			t.Fatalf("builtin text = %q, want %q", text, want)
		}
	})

	t.Run("scoped config preserves an explicit phone", func(t *testing.T) {
		f.dispatch(t, "!wind config set @+15555550101 timezone Europe/Paris")
		_, text := f.builtin(t)
		want := "config set @+15555550101 wind-predictor.timezone Europe/Paris"
		if text != want {
			t.Fatalf("builtin text = %q, want %q", text, want)
		}
	})

	t.Run("scoped help names the satellite", func(t *testing.T) {
		f.dispatch(t, "!wind-predictor help")
		satellite, text := f.builtin(t)
		if satellite != "help" || text != "help wind-predictor" {
			t.Fatalf("builtin = %q, text = %q", satellite, text)
		}
	})

	t.Run("scoped config list forwards the satellite target", func(t *testing.T) {
		f.dispatch(t, "!wind config list")
		satellite, text := f.builtin(t)
		if satellite != "config" {
			t.Fatalf("builtin = %q, want config", satellite)
		}
		if text != "config list wind-predictor" {
			t.Fatalf("builtin text = %q", text)
		}
	})

	t.Run("scoped config get forwards the satellite key", func(t *testing.T) {
		f.dispatch(t, "!wind config get alert_schedule.nightly_local")
		_, text := f.builtin(t)
		want := "config get wind-predictor.alert_schedule.nightly_local"
		if text != want {
			t.Fatalf("builtin text = %q, want %q", text, want)
		}
	})

	t.Run("scoped config export forwards the satellite target", func(t *testing.T) {
		f.dispatch(t, "!wind config export")
		_, text := f.builtin(t)
		if text != "config export wind-predictor" {
			t.Fatalf("builtin text = %q", text)
		}
	})

	t.Run("scoped config rejects unknown actions", func(t *testing.T) {
		f.dispatch(t, "!wind config delete alert_schedule")
		if satellite, _ := f.builtin(t); satellite != "" {
			t.Fatalf("builtin = %q, want no built-in", satellite)
		}
		f.mu.Lock()
		replies := append([]string(nil), f.replies...)
		f.mu.Unlock()
		if len(replies) != 1 || !strings.Contains(replies[0], "wind config list") {
			t.Fatalf("replies = %v", replies)
		}
	})
}

func TestAetherPrefixSelectsBuiltins(t *testing.T) {
	f := newScopedFixture(t)

	t.Run("config is dispatched to the built-in", func(t *testing.T) {
		f.dispatch(t, "!aether config list")
		satellite, text := f.builtin(t)
		if satellite != "config" || text != "config list" {
			t.Fatalf("builtin = %q, text = %q", satellite, text)
		}
	})

	t.Run("help is dispatched to the built-in", func(t *testing.T) {
		f.dispatch(t, "!aether help wind")
		satellite, text := f.builtin(t)
		if satellite != "help" || text != "help wind" {
			t.Fatalf("builtin = %q, text = %q", satellite, text)
		}
	})

	t.Run("bare aether shows help", func(t *testing.T) {
		f.dispatch(t, "!aether")
		satellite, text := f.builtin(t)
		if satellite != "help" || text != "help" {
			t.Fatalf("builtin = %q, text = %q", satellite, text)
		}
	})

	t.Run("satellite triggers are not reachable through aether", func(t *testing.T) {
		f.dispatch(t, "!aether fly FPS")
		if satellite, _ := f.builtin(t); satellite != "" {
			t.Fatalf("builtin = %q, want no built-in", satellite)
		}
		f.mu.Lock()
		calls := len(f.calls)
		replies := append([]string(nil), f.replies...)
		f.mu.Unlock()
		if calls != 0 {
			t.Fatalf("satellite calls = %d, want 0", calls)
		}
		if len(replies) != 1 || !strings.Contains(replies[0], "Unknown command") {
			t.Fatalf("replies = %v", replies)
		}
	})
}

func TestAmbiguousTriggerRequiresSatellitePrefix(t *testing.T) {
	root := testutil.TempConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	prefixes := map[string]string{"alpha": "a", "beta": "be"}
	for _, name := range []string{"alpha", "beta"} {
		testutil.WriteJSON(t, filepath.Join(root, "satellites", name+".json"), config.SatelliteConfig{
			Name:     name,
			Trigger:  "ping",
			Endpoint: server.URL,
			Prefix:   prefixes[name],
			Commands: []config.CommandConfig{{
				Name:     "ping",
				Path:     "/ping",
				Patterns: []string{"ping"},
			}},
		})
	}
	testutil.WriteJSON(t, filepath.Join(root, "routes.json"), config.RoutesConfig{Routes: []config.RouteEntry{
		{Trigger: "ping", Satellite: "alpha", Endpoint: server.URL, Commands: []config.CommandConfig{{Name: "ping", Path: "/ping", Patterns: []string{"ping"}}}},
		{Trigger: "ping", Satellite: "beta", Endpoint: server.URL, Commands: []config.CommandConfig{{Name: "ping", Path: "/ping", Patterns: []string{"ping"}}}},
	}})
	cfg, err := config.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
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

	var replies []string
	rt := New(Options{
		Config: func() *config.Config { return cfg },
		Store:  st,
		Client: server.Client(),
		Reply: func(_ context.Context, _ model.Inbound, text string) error {
			replies = append(replies, text)
			return nil
		},
	})

	rt.Dispatch(context.Background(), model.Inbound{Sender: "+15555550102", Text: "!ping"})
	if len(replies) != 1 || !strings.Contains(replies[0], "ambiguous") {
		t.Fatalf("replies = %v, want an ambiguity hint", replies)
	}
	if !strings.Contains(replies[0], "a ping") || !strings.Contains(replies[0], "be ping") {
		t.Fatalf("reply = %q, want both short prefixed forms", replies[0])
	}

	var status, errText string
	if err := st.DB().QueryRow(`SELECT status, error FROM route_log ORDER BY id DESC LIMIT 1`).Scan(&status, &errText); err != nil {
		t.Fatalf("read route_log: %v", err)
	}
	if status != "dropped" || !strings.Contains(errText, "ambiguous") {
		t.Fatalf("route_log = %q, %q", status, errText)
	}

	// An explicit short prefix resolves the conflict.
	rt.Dispatch(context.Background(), model.Inbound{Sender: "+15555550102", Text: "!a ping"})
	var satellite string
	if err := st.DB().QueryRow(`SELECT satellite FROM route_log ORDER BY id DESC LIMIT 1`).Scan(&satellite); err != nil {
		t.Fatalf("read route_log: %v", err)
	}
	if satellite != "alpha" {
		t.Fatalf("satellite = %q, want alpha", satellite)
	}
}
