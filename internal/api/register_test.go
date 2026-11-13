package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/internal/testutil"
)

// TestRegisterSatellite covers plan §3.15: a protected admin/maintainer
// endpoint that validates a satellite document, writes it atomically, mirrors
// it into SQLite, and rebuilds routes.json from the files on disk.
func TestRegisterSatellite(t *testing.T) {
	const (
		secret = "register-test-secret"
		admin  = "+15555550101"
	)

	valid := config.SatelliteConfig{
		Name:     "weather",
		Trigger:  "weather",
		Endpoint: "http://127.0.0.1:9999",
		Commands: []config.CommandConfig{{
			Name:     "forecast",
			Path:     "/forecast",
			Patterns: []string{"weather <city>"},
			Args:     []config.ArgDef{{Name: "city", Type: "string"}},
		}},
	}

	withEndpoint := func(endpoint string) config.SatelliteConfig {
		clone := valid
		clone.Endpoint = endpoint
		return clone
	}
	named := func(name, trigger string) config.SatelliteConfig {
		clone := valid
		clone.Name = name
		clone.Trigger = trigger
		return clone
	}
	withCommand := func(name, trigger string, command config.CommandConfig) config.SatelliteConfig {
		clone := named(name, trigger)
		clone.Commands = []config.CommandConfig{command}
		return clone
	}
	badPath := withCommand("badpath", "badpath", config.CommandConfig{
		Name:     "bad",
		Path:     "bad",
		Patterns: []string{"bad"},
	})
	noPattern := withCommand("nopattern", "nopattern", config.CommandConfig{
		Name: "bad",
		Path: "/bad",
	})
	badPrefix := named("badprefix", "badprefix")
	badPrefix.Prefix = "../evil"

	tests := []struct {
		name       string
		satellite  config.SatelliteConfig
		rawBody    string
		role       auth.Role
		token      bool
		wantStatus int
	}{
		{
			name:       "valid satellite",
			satellite:  valid,
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusCreated,
		},
		{
			name:       "duplicate trigger",
			satellite:  named("dupe", "fly"),
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "duplicate name",
			satellite:  named("wind-predictor", "other-trigger"),
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "built-in name",
			satellite:  named("config", "cfg"),
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "built-in trigger",
			satellite:  named("custom", "test"),
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "built-in remind trigger",
			satellite:  named("custom-remind", "remind"),
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "path traversal name",
			satellite:  named("../evil", "evil"),
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "non-absolute endpoint",
			satellite:  withEndpoint("127.0.0.1:9999"),
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "command path without leading slash",
			satellite:  badPath,
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "command without patterns",
			satellite:  noPattern,
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid satellite prefix",
			satellite:  badPrefix,
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing jwt",
			satellite:  valid,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "user role",
			satellite:  valid,
			role:       auth.User,
			token:      true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "malformed json",
			rawBody:    `{"name":`,
			role:       auth.Admin,
			token:      true,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := testutil.TempConfig(t)

			// A stale routes.json proves registration rebuilds rather than
			// appends to the existing file.
			stale := config.RoutesConfig{Routes: []config.RouteEntry{{
				Trigger:   "stale",
				Satellite: "stale",
				Endpoint:  "http://127.0.0.1:1",
			}}}
			testutil.WriteJSON(t, filepath.Join(root, "routes.json"), stale)

			cfg := &config.Config{
				Root: root,
				System: config.SystemConfig{
					Auth: config.AuthConfig{JWTSecret: secret},
				},
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

			handler := NewRegisterHandler(RegisterOptions{
				Config: func() *config.Config { return cfg },
				Store:  st,
			})

			body := tc.rawBody
			if body == "" {
				raw, err := json.Marshal(tc.satellite)
				if err != nil {
					t.Fatalf("marshal satellite: %v", err)
				}
				body = string(raw)
			}

			req := httptest.NewRequest(http.MethodPost, "/register-satellite", strings.NewReader(body))
			if tc.token {
				minted, err := auth.Issue(secret, admin, tc.role, time.Hour)
				if err != nil {
					t.Fatalf("issue token: %v", err)
				}
				req.Header.Set("Authorization", "Bearer "+minted)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}

			if tc.wantStatus != http.StatusCreated {
				// A rejected registration must not leave a new file behind.
				if tc.satellite.Name != "" {
					path := filepath.Join(root, "satellites", tc.satellite.Name+".json")
					if _, err := os.Stat(path); err == nil && tc.satellite.Name != "wind-predictor" {
						t.Errorf("rejected registration created %s", path)
					}
				}
				return
			}

			assertRegisteredSatellite(t, root, st, tc.satellite, admin)
		})
	}
}

// assertRegisteredSatellite verifies the three durable effects of a successful
// registration: the satellite file, the SQLite mirror, and the regenerated
// routes.json.
func assertRegisteredSatellite(t *testing.T, root string, st *store.Store, want config.SatelliteConfig, registeredBy string) {
	t.Helper()

	path := filepath.Join(root, "satellites", want.Name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read satellite file: %v", err)
	}
	var onDisk config.SatelliteConfig
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parse satellite file: %v", err)
	}
	if !reflect.DeepEqual(onDisk, want) {
		t.Errorf("satellite file = %+v, want %+v", onDisk, want)
	}

	// Atomic writes use a temp file in the same directory; none may survive.
	entries, err := os.ReadDir(filepath.Join(root, "satellites"))
	if err != nil {
		t.Fatalf("read satellites dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}

	var (
		gotName, gotTrigger, gotEndpoint, gotConfig, gotBy string
	)
	err = st.DB().QueryRow(
		`SELECT name, "trigger", endpoint, config, registered_by FROM satellites WHERE name = ?`,
		want.Name,
	).Scan(&gotName, &gotTrigger, &gotEndpoint, &gotConfig, &gotBy)
	if err != nil {
		t.Fatalf("read SQLite mirror: %v", err)
	}
	if gotName != want.Name || gotTrigger != want.Trigger || gotEndpoint != want.Endpoint {
		t.Errorf("SQLite mirror = (%q, %q, %q), want (%q, %q, %q)",
			gotName, gotTrigger, gotEndpoint, want.Name, want.Trigger, want.Endpoint)
	}
	if gotBy != registeredBy {
		t.Errorf("registered_by = %q, want %q", gotBy, registeredBy)
	}
	var mirrored config.SatelliteConfig
	if err := json.Unmarshal([]byte(gotConfig), &mirrored); err != nil {
		t.Fatalf("parse mirrored config: %v", err)
	}
	if !reflect.DeepEqual(mirrored, want) {
		t.Errorf("mirrored config = %+v, want %+v", mirrored, want)
	}

	routesPath := filepath.Join(root, "routes.json")
	routesRaw, err := os.ReadFile(routesPath)
	if err != nil {
		t.Fatalf("read routes.json: %v", err)
	}
	var routes config.RoutesConfig
	if err := json.Unmarshal(routesRaw, &routes); err != nil {
		t.Fatalf("parse routes.json: %v", err)
	}

	var found *config.RouteEntry
	for i := range routes.Routes {
		if routes.Routes[i].Trigger == "stale" || routes.Routes[i].Satellite == "stale" {
			t.Errorf("routes.json still contains the stale entry: %+v", routes.Routes[i])
		}
		if routes.Routes[i].Satellite == want.Name {
			found = &routes.Routes[i]
		}
	}
	if found == nil {
		t.Fatalf("routes.json is missing %q: %+v", want.Name, routes.Routes)
	}
	if found.Trigger != want.Trigger || found.Endpoint != want.Endpoint {
		t.Errorf("route = %+v, want trigger %q endpoint %q", *found, want.Trigger, want.Endpoint)
	}
	if !reflect.DeepEqual(found.Commands, want.Commands) {
		t.Errorf("route commands = %+v, want %+v", found.Commands, want.Commands)
	}

	// Existing file-registered satellites survive a rebuild.
	var sawExisting bool
	for _, route := range routes.Routes {
		if route.Satellite == "wind-predictor" {
			sawExisting = true
		}
	}
	if !sawExisting {
		t.Error("routes.json dropped the existing wind-predictor satellite")
	}
}
