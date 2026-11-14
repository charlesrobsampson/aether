package configmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/internal/testutil"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

const (
	testSecret  = "configmgr-test-secret"
	adminPhone  = "+15555550101"
	userPhone   = "+15555550102"
	deniedPhone = "+15555550999"
)

// newTestConfig builds a hermetic config tree in t.TempDir() with one admin,
// one approved user, and a wind-predictor satellite. The returned Config is a
// snapshot; callers that change files reload it when they need the new values.
func newTestConfig(t *testing.T) (root string, cfg *config.Config, st *store.Store) {
	t.Helper()

	root = t.TempDir()

	testutil.WriteJSON(t, filepath.Join(root, "system.json"), config.SystemConfig{
		Server: config.ServerConfig{Port: 8080},
		Auth:   config.AuthConfig{JWTSecret: testSecret},
	})
	testutil.WriteJSON(t, filepath.Join(root, "admin.json"), config.AdminConfig{
		Admins:          []string{adminPhone},
		ApprovedNumbers: []string{adminPhone, userPhone},
		DeniedNumbers:   []string{deniedPhone},
		ApprovedGroups:  []string{"group.example"},
	})
	testutil.WriteJSON(t, filepath.Join(root, "maintainer.json"), config.MaintainerConfig{})
	testutil.WriteJSON(t, filepath.Join(root, "users", userPhone+".json"), config.UserConfig{
		Prefixes:     []string{"!"},
		Timezone:     "America/Chicago",
		SatelliteIDs: map[string]string{"wind-predictor": "alice"},
		Aliases: config.Aliases{
			Satellites: map[string]string{"wind": "wind-predictor"},
			Commands:   map[string]string{"w": "fly"},
		},
	})
	testutil.WriteJSON(t, filepath.Join(root, "satellites", "wind-predictor.json"), config.SatelliteConfig{
		Name:     "wind-predictor",
		Trigger:  "fly",
		Endpoint: "http://127.0.0.1:1",
		Prefix:   "wind",
	})

	cfg = reloadConfig(t, root)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open(:memory:): %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	return root, cfg, st
}

func reloadConfig(t *testing.T, root string) *config.Config {
	t.Helper()

	cfg, err := config.LoadConfig(root)
	if err != nil {
		t.Fatalf("config.LoadConfig(%s): %v", root, err)
	}
	return cfg
}

func readUserFile(t *testing.T, root, phone string) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, "users", phone+".json"))
	if err != nil {
		t.Fatalf("read user file for %s: %v", phone, err)
	}
	return raw
}

func readFileMaybe(t *testing.T, path string) ([]byte, bool) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err == nil {
		return raw, true
	}
	if os.IsNotExist(err) {
		return nil, false
	}
	t.Fatalf("read %s: %v", path, err)
	return nil, false
}

func requireStatus(t *testing.T, err error, want int) {
	t.Helper()

	if want == 0 {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}

	var configErr *Error
	if !errors.As(err, &configErr) {
		t.Fatalf("error %v is not *configmgr.Error", err)
	}
	if configErr.Status != want {
		t.Fatalf("error status = %d, want %d (error: %v)", configErr.Status, want, err)
	}
}

// TestApplyPatch covers the on-disk atomic patch and the SQLite audit row
// required by plan §3.14 and §3.8.
func TestApplyPatch(t *testing.T) {
	t.Run("timezone and audit", func(t *testing.T) {
		root, cfg, st := newTestConfig(t)
		manager := New(Options{
			Config: func() *config.Config { return cfg },
			Store:  st,
		})

		err := manager.HandlePatch(sdk.Transmission{
			Command: "set",
			Trigger: "config",
			Text:    "config set timezone Europe/London",
			Sender:  userPhone,
		})
		requireStatus(t, err, 0)

		var user config.UserConfig
		raw := readUserFile(t, root, userPhone)
		if err := json.Unmarshal(raw, &user); err != nil {
			t.Fatalf("decode patched user: %v", err)
		}
		if user.Timezone != "Europe/London" {
			t.Fatalf("timezone = %q, want Europe/London", user.Timezone)
		}
		if !reflect.DeepEqual(user.Prefixes, []string{"!"}) {
			t.Fatalf("unrelated prefixes changed: %v", user.Prefixes)
		}
		if got := user.SatelliteIDs["wind-predictor"]; got != "alice" {
			t.Fatalf("satellite_ids changed: %q", got)
		}

		rows := queryPatches(t, st)
		if len(rows) != 1 {
			t.Fatalf("patches rows = %d, want 1", len(rows))
		}
		if rows[0].phone != userPhone {
			t.Fatalf("patches.phone = %q, want %q", rows[0].phone, userPhone)
		}
		if rows[0].appliedBy != userPhone {
			t.Fatalf("patches.applied_by = %q, want %q", rows[0].appliedBy, userPhone)
		}

		var ops []sdk.Operation
		if err := json.Unmarshal([]byte(rows[0].patch), &ops); err != nil {
			t.Fatalf("decode audit patch: %v", err)
		}
		if len(ops) != 1 || ops[0].Path != "/timezone" {
			t.Fatalf("audit patch = %+v, want one /timezone op", ops)
		}
		if ops[0].Value != "Europe/London" {
			t.Fatalf("audit patch value = %v, want Europe/London", ops[0].Value)
		}

		// The atomic write must not leave a temp file behind.
		entries, err := os.ReadDir(filepath.Join(root, "users"))
		if err != nil {
			t.Fatalf("read users dir: %v", err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), ".tmp") {
				t.Fatalf("temporary file left behind: %s", entry.Name())
			}
		}
	})

	t.Run("nested alias creates missing parents", func(t *testing.T) {
		root, cfg, st := newTestConfig(t)

		// Replace the user file with a document that has no aliases object at
		// all, so the patch has to create the intermediate containers.
		testutil.WriteJSON(t, filepath.Join(root, "users", userPhone+".json"), map[string]any{
			"timezone": "America/Chicago",
		})
		cfg = reloadConfig(t, root)

		manager := New(Options{
			Config: func() *config.Config { return cfg },
			Store:  st,
		})
		for _, text := range []string{
			"config set aliases.satellites.home wind-predictor",
			"config set aliases.commands.h fly",
		} {
			if err := manager.HandlePatch(sdk.Transmission{
				Command: "set",
				Trigger: "config",
				Text:    text,
				Sender:  userPhone,
			}); err != nil {
				t.Fatalf("HandlePatch(%q): %v", text, err)
			}
		}

		var user config.UserConfig
		if err := json.Unmarshal(readUserFile(t, root, userPhone), &user); err != nil {
			t.Fatalf("decode patched user: %v", err)
		}
		if got := user.Aliases.Satellites["home"]; got != "wind-predictor" {
			t.Fatalf("aliases.satellites.home = %q, want wind-predictor", got)
		}
		if got := user.Aliases.Commands["h"]; got != "fly" {
			t.Fatalf("aliases.commands.h = %q, want fly", got)
		}
		if user.Timezone != "America/Chicago" {
			t.Fatalf("timezone changed: %q", user.Timezone)
		}

		if rows := queryPatches(t, st); len(rows) != 2 {
			t.Fatalf("patches rows = %d, want 2", len(rows))
		}
	})
}

// TestConfigSetCommand covers the role checks and key whitelist from plan §3.5,
// §3.6, and docs/open-questions.md #3.
func TestConfigSetCommand(t *testing.T) {
	tests := []struct {
		name string

		sender string
		text   string

		wantStatus int
		// unchangedPhone, when set, must have identical bytes (or remain
		// absent) before and after the call.
		unchangedPhone string
		check          func(t *testing.T, root string)
	}{
		{
			name:   "self edit allowed",
			sender: userPhone,
			text:   "config set timezone Europe/Paris",
			check: func(t *testing.T, root string) {
				var user config.UserConfig
				if err := json.Unmarshal(readUserFile(t, root, userPhone), &user); err != nil {
					t.Fatalf("decode user: %v", err)
				}
				if user.Timezone != "Europe/Paris" {
					t.Fatalf("timezone = %q, want Europe/Paris", user.Timezone)
				}
			},
		},
		{
			name:           "other user denied for user",
			sender:         userPhone,
			text:           "config set @+15555550101 timezone Europe/Paris",
			wantStatus:     http.StatusForbidden,
			unchangedPhone: adminPhone,
		},
		{
			name:   "other user allowed for admin",
			sender: adminPhone,
			text:   "config set @+15555550102 timezone Europe/Paris",
			check: func(t *testing.T, root string) {
				var user config.UserConfig
				if err := json.Unmarshal(readUserFile(t, root, userPhone), &user); err != nil {
					t.Fatalf("decode user: %v", err)
				}
				if user.Timezone != "Europe/Paris" {
					t.Fatalf("timezone = %q, want Europe/Paris", user.Timezone)
				}
			},
		},
		{
			name:   "uuid target allowed for admin",
			sender: adminPhone,
			text:   "config set @2aea5c40-0f8e-4ad7-8099-880b50064454 timezone Europe/Paris",
			check: func(t *testing.T, root string) {
				var user config.UserConfig
				if err := json.Unmarshal(readUserFile(t, root, "2aea5c40-0f8e-4ad7-8099-880b50064454"), &user); err != nil {
					t.Fatalf("decode user: %v", err)
				}
				if user.Timezone != "Europe/Paris" {
					t.Fatalf("timezone = %q, want Europe/Paris", user.Timezone)
				}
			},
		},
		{
			name:           "disallowed key rejected",
			sender:         userPhone,
			text:           "config set default_provider openmessage",
			wantStatus:     http.StatusBadRequest,
			unchangedPhone: userPhone,
		},
		{
			name:           "satellite_ids not user editable",
			sender:         userPhone,
			text:           "config set satellite_ids.wind-predictor mallory",
			wantStatus:     http.StatusBadRequest,
			unchangedPhone: userPhone,
		},
		{
			name:           "satellite_ids not admin editable via set",
			sender:         adminPhone,
			text:           "config set @+15555550102 satellite_ids.wind-predictor mallory",
			wantStatus:     http.StatusBadRequest,
			unchangedPhone: userPhone,
		},
		{
			name:   "muted",
			sender: userPhone,
			text:   "config set muted true",
			check: func(t *testing.T, root string) {
				var user config.UserConfig
				if err := json.Unmarshal(readUserFile(t, root, userPhone), &user); err != nil {
					t.Fatalf("decode user: %v", err)
				}
				if !user.Muted {
					t.Fatal("muted = false, want true")
				}
			},
		},
		{
			name:   "prefixes comma separated",
			sender: userPhone,
			text:   "config set prefixes !,/",
			check: func(t *testing.T, root string) {
				var user config.UserConfig
				if err := json.Unmarshal(readUserFile(t, root, userPhone), &user); err != nil {
					t.Fatalf("decode user: %v", err)
				}
				if !reflect.DeepEqual(user.Prefixes, []string{"!", "/"}) {
					t.Fatalf("prefixes = %v, want [! /]", user.Prefixes)
				}
			},
		},
		{
			name:   "command alias",
			sender: userPhone,
			text:   "config set aliases.commands.g fly",
			check: func(t *testing.T, root string) {
				var user config.UserConfig
				if err := json.Unmarshal(readUserFile(t, root, userPhone), &user); err != nil {
					t.Fatalf("decode user: %v", err)
				}
				if got := user.Aliases.Commands["g"]; got != "fly" {
					t.Fatalf("aliases.commands.g = %q, want fly", got)
				}
			},
		},
		{
			name:           "invalid timezone rejected",
			sender:         userPhone,
			text:           "config set timezone Not/AZone",
			wantStatus:     http.StatusBadRequest,
			unchangedPhone: userPhone,
		},
		{
			name:           "invalid bool rejected",
			sender:         userPhone,
			text:           "config set muted maybe",
			wantStatus:     http.StatusBadRequest,
			unchangedPhone: userPhone,
		},
		{
			name:           "malformed command",
			sender:         userPhone,
			text:           "config set",
			wantStatus:     http.StatusBadRequest,
			unchangedPhone: userPhone,
		},
		{
			name:       "denied sender",
			sender:     deniedPhone,
			text:       "config set timezone Europe/Paris",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "unknown sender",
			sender:     "+15555550199",
			text:       "config set timezone Europe/Paris",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, cfg, st := newTestConfig(t)
			manager := New(Options{
				Config: func() *config.Config { return cfg },
				Store:  st,
			})

			var before []byte
			var beforeExists bool
			if tc.unchangedPhone != "" {
				before, beforeExists = readFileMaybe(t, filepath.Join(root, "users", tc.unchangedPhone+".json"))
			}

			err := manager.HandlePatch(sdk.Transmission{
				Command: "set",
				Trigger: "config",
				Text:    tc.text,
				Sender:  tc.sender,
			})
			requireStatus(t, err, tc.wantStatus)

			if tc.unchangedPhone != "" {
				after, afterExists := readFileMaybe(t, filepath.Join(root, "users", tc.unchangedPhone+".json"))
				if beforeExists != afterExists || !bytes.Equal(before, after) {
					t.Fatalf("user file %s changed after rejected command", tc.unchangedPhone)
				}
			}
			if tc.check != nil {
				tc.check(t, root)
			}
		})
	}
}

type forwardPayload struct {
	User  string          `json:"user"`
	Patch []sdk.Operation `json:"patch"`
}

// TestForwardSatelliteConfig covers plan §3.12: a <satellite>.<key> target is
// resolved through satellite_ids and forwarded to the satellite's config
// endpoint with a Bearer token, without touching the local user file.
func TestForwardSatelliteConfig(t *testing.T) {
	t.Run("mapped satellite is forwarded", func(t *testing.T) {
		var (
			gotMethod        string
			gotPath          string
			gotContentType   string
			gotAuthorization string
			gotBody          forwardPayload
		)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotContentType = r.Header.Get("Content-Type")
			gotAuthorization = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode forwarded body: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()

		root, cfg, st := newTestConfig(t)

		// The sender needs the admin role for forwarding, and this user
		// already has satellite_ids["wind-predictor"] = "alice".
		cfg.Admin.Admins = append(cfg.Admin.Admins, userPhone)
		satellite := cfg.Satellites["wind-predictor"]
		satellite.Endpoint = server.URL
		cfg.Satellites["wind-predictor"] = satellite

		manager := New(Options{
			Config: func() *config.Config { return cfg },
			Store:  st,
		})

		before, beforeExists := readFileMaybe(t, filepath.Join(root, "users", userPhone+".json"))

		err := manager.HandlePatch(sdk.Transmission{
			Command: "set",
			Trigger: "config",
			Text:    `config set wind-predictor.subscriptions.0.personal_constraints.no_fly_days ["Sunday"]`,
			Sender:  userPhone,
		})
		requireStatus(t, err, 0)

		if gotMethod != http.MethodPost {
			t.Fatalf("forwarded method = %q, want POST", gotMethod)
		}
		if gotPath != "/api/v1/config" {
			t.Fatalf("forwarded path = %q, want /api/v1/config", gotPath)
		}
		if gotContentType != "application/json" {
			t.Fatalf("forwarded content type = %q, want application/json", gotContentType)
		}
		if !strings.HasPrefix(gotAuthorization, "Bearer ") {
			t.Fatalf("Authorization = %q, want Bearer token", gotAuthorization)
		}
		claims, err := auth.Verify(testSecret, strings.TrimPrefix(gotAuthorization, "Bearer "))
		if err != nil {
			t.Fatalf("forwarded token does not verify: %v", err)
		}
		if claims.Sub != userPhone {
			t.Fatalf("token subject = %q, want %q", claims.Sub, userPhone)
		}
		if claims.Role != auth.Admin {
			t.Fatalf("token role = %v, want admin", claims.Role)
		}

		if gotBody.User != "alice" {
			t.Fatalf("forwarded user = %q, want alice", gotBody.User)
		}
		if len(gotBody.Patch) != 1 {
			t.Fatalf("forwarded patch length = %d, want 1", len(gotBody.Patch))
		}
		op := gotBody.Patch[0]
		if op.Path != "/subscriptions/0/personal_constraints/no_fly_days" {
			t.Fatalf("forwarded path = %q", op.Path)
		}
		if !reflect.DeepEqual(op.Value, []any{"Sunday"}) {
			t.Fatalf("forwarded value = %#v, want [Sunday]", op.Value)
		}

		after, afterExists := readFileMaybe(t, filepath.Join(root, "users", userPhone+".json"))
		if beforeExists != afterExists || !bytes.Equal(before, after) {
			t.Fatal("forwarding modified the local user file")
		}
	})

	t.Run("unmapped satellite returns 400", func(t *testing.T) {
		var calls int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()

		_, cfg, st := newTestConfig(t)
		satellite := cfg.Satellites["wind-predictor"]
		satellite.Endpoint = server.URL
		cfg.Satellites["wind-predictor"] = satellite

		manager := New(Options{
			Config: func() *config.Config { return cfg },
			Store:  st,
		})

		// adminPhone is an admin but has no satellite_ids mapping.
		err := manager.HandlePatch(sdk.Transmission{
			Command: "set",
			Trigger: "config",
			Text:    "config set wind-predictor.display_name Alice",
			Sender:  adminPhone,
		})
		requireStatus(t, err, http.StatusBadRequest)

		if got := atomic.LoadInt32(&calls); got != 0 {
			t.Fatalf("unmapped satellite made %d HTTP calls, want 0", got)
		}
	})
}

func TestSatelliteConfigRead(t *testing.T) {
	settingsBody := `{"settings":[
		{"key":"display_name","value":"Alice"},
		{"key":"alert_schedule.nightly_local","value":"22:00"},
		{"key":"alert_schedule.morning_local","value":"(station default)","note":"default"},
		{"key":"aether_recipient","value":"+15555550100","note":"read-only"}
	]}`

	newServer := func(t *testing.T) *httptest.Server {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %q, want GET", r.Method)
			}
			if r.URL.Path != "/api/v1/config" {
				t.Errorf("path = %q, want /api/v1/config", r.URL.Path)
			}
			if got := r.URL.Query().Get("user"); got != "alice" {
				t.Errorf("user = %q, want alice", got)
			}
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				t.Errorf("Authorization = %q, want Bearer token", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(settingsBody))
		}))
		t.Cleanup(server.Close)
		return server
	}

	t.Run("list renders satellite settings", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		cfg.Admin.Admins = append(cfg.Admin.Admins, userPhone)
		satellite := cfg.Satellites["wind-predictor"]
		satellite.Endpoint = newServer(t).URL
		cfg.Satellites["wind-predictor"] = satellite
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config list wind-predictor",
			Sender:  userPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		got := recorder.last(t)
		for _, want := range []string{
			"display_name = Alice",
			"alert_schedule.nightly_local = 22:00",
			"alert_schedule.morning_local = (station default) (default)",
			"aether_recipient = +15555550100 (read-only)",
			`Send "!wind config get <key>" for one setting.`,
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("list lacks %q:\n%s", want, got)
			}
		}
	})

	t.Run("get adds a scoped set line", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		cfg.Admin.Admins = append(cfg.Admin.Admins, userPhone)
		satellite := cfg.Satellites["wind-predictor"]
		satellite.Endpoint = newServer(t).URL
		cfg.Satellites["wind-predictor"] = satellite
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config get wind-predictor.alert_schedule.nightly_local",
			Sender:  userPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		want := "alert_schedule.nightly_local = 22:00\nSet with: !wind config set alert_schedule.nightly_local 22:00"
		if got := recorder.last(t); got != want {
			t.Fatalf("get =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("export skips defaults and read-only values", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		cfg.Admin.Admins = append(cfg.Admin.Admins, userPhone)
		satellite := cfg.Satellites["wind-predictor"]
		satellite.Endpoint = newServer(t).URL
		cfg.Satellites["wind-predictor"] = satellite
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config export wind-predictor",
			Sender:  userPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		want := strings.Join([]string{
			"!wind config set display_name Alice",
			"!wind config set alert_schedule.nightly_local 22:00",
		}, "\n")
		if got := recorder.last(t); got != want {
			t.Fatalf("export =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("group reads are ignored", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		satellite := cfg.Satellites["wind-predictor"]
		satellite.Endpoint = newServer(t).URL
		cfg.Satellites["wind-predictor"] = satellite
		manager, _ := readManager(t, cfg)

		err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config list wind-predictor",
			Sender:  userPhone,
			GroupID: "group.example",
		})
		if !errors.Is(err, sdk.ErrIgnored) {
			t.Fatalf("error = %v, want ErrIgnored", err)
		}
	})

	t.Run("non-admin is forbidden", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		satellite := cfg.Satellites["wind-predictor"]
		satellite.Endpoint = newServer(t).URL
		cfg.Satellites["wind-predictor"] = satellite
		manager, _ := readManager(t, cfg)

		err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config list wind-predictor",
			Sender:  userPhone,
		})
		requireStatus(t, err, http.StatusForbidden)
	})
}

type patchRow struct {
	phone     string
	patch     string
	appliedBy string
}

func queryPatches(t *testing.T, st *store.Store) []patchRow {
	t.Helper()

	rows, err := st.DB().QueryContext(context.Background(),
		`SELECT phone, patch, applied_by FROM patches ORDER BY id`)
	if err != nil {
		t.Fatalf("query patches: %v", err)
	}
	defer rows.Close()

	var out []patchRow
	for rows.Next() {
		var row patchRow
		if err := rows.Scan(&row.phone, &row.patch, &row.appliedBy); err != nil {
			t.Fatalf("scan patch row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate patches: %v", err)
	}
	return out
}

// recordingSender captures read-only replies so tests can assert the exact
// text a sender would receive.
type recordingSender struct {
	responses []sdk.Response
}

func (s *recordingSender) Send(_ context.Context, resp sdk.Response) error {
	s.responses = append(s.responses, resp)
	return nil
}

func (s *recordingSender) last(t *testing.T) string {
	t.Helper()
	if len(s.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(s.responses))
	}
	return s.responses[0].Text
}

func readManager(t *testing.T, cfg *config.Config) (*Manager, *recordingSender) {
	t.Helper()
	recorder := &recordingSender{}
	return New(Options{
		Config: func() *config.Config { return cfg },
		Out:    recorder,
	}), recorder
}

func TestConfigList(t *testing.T) {
	const otherPhone = "+15555550103"

	t.Run("self shows values and defaults", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config list",
			Sender:  userPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}

		want := strings.Join([]string{
			"timezone = America/Chicago",
			"prefixes = !",
			"muted = false (default)",
			"aliases.satellites.wind = wind-predictor",
			"aliases.commands.w = fly",
			`Send "!config get <key>" for one setting.`,
		}, "\n")
		if got := recorder.last(t); got != want {
			t.Fatalf("list =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("bare config lists defaults", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config",
			Sender:  userPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if got := recorder.last(t); !strings.Contains(got, "timezone = America/Chicago") {
			t.Fatalf("bare config did not list settings: %q", got)
		}
	})

	t.Run("admin sees another user's effective defaults", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config list @" + otherPhone,
			Sender:  adminPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}

		want := strings.Join([]string{
			"Settings for " + otherPhone + ":",
			"timezone = UTC (default)",
			"prefixes = ! (default)",
			"muted = false (default)",
			"aliases = (none)",
			`Send "!config get @` + otherPhone + ` <key>" for one setting.`,
		}, "\n")
		if got := recorder.last(t); got != want {
			t.Fatalf("list =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("user cannot view another user", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		manager, _ := readManager(t, cfg)

		err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config list @" + adminPhone,
			Sender:  userPhone,
		})
		requireStatus(t, err, http.StatusForbidden)
	})
}

func TestConfigGet(t *testing.T) {
	t.Run("exact key includes a pasteable set line", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config get timezone",
			Sender:  userPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}

		want := strings.Join([]string{
			"timezone = America/Chicago",
			"Set with: !config set timezone America/Chicago",
		}, "\n")
		if got := recorder.last(t); got != want {
			t.Fatalf("get =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("default value is marked", func(t *testing.T) {
		const otherPhone = "+15555550103"
		_, cfg, _ := newTestConfig(t)
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config get @" + otherPhone + " muted",
			Sender:  adminPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}

		want := strings.Join([]string{
			"muted = false (default)",
			"Set with: !config set @" + otherPhone + " muted false",
		}, "\n")
		if got := recorder.last(t); got != want {
			t.Fatalf("get =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("alias group matches every alias", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config get aliases.satellites",
			Sender:  userPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}

		want := strings.Join([]string{
			"aliases.satellites.wind = wind-predictor",
			"Set with: !config set aliases.satellites.wind wind-predictor",
		}, "\n")
		if got := recorder.last(t); got != want {
			t.Fatalf("get =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("empty alias group is explicit", func(t *testing.T) {
		const otherPhone = "+15555550103"
		_, cfg, _ := newTestConfig(t)
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config get @" + otherPhone + " aliases",
			Sender:  adminPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if got := recorder.last(t); got != "aliases = (none)" {
			t.Fatalf("get aliases = %q, want aliases = (none)", got)
		}
	})

	t.Run("unknown key is a safe user error", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		manager, _ := readManager(t, cfg)

		err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config get nope",
			Sender:  userPhone,
		})
		requireStatus(t, err, http.StatusBadRequest)
		if message := (&Error{Status: http.StatusBadRequest, Message: err.Error()}).UserMessage(); !strings.Contains(message, "unknown key") {
			t.Fatalf("error = %q, want an unknown-key hint", err)
		}
	})

	t.Run("admin target uses the configured prefix", func(t *testing.T) {
		root, cfg, _ := newTestConfig(t)
		testutil.WriteJSON(t, filepath.Join(root, "users", adminPhone+".json"), config.UserConfig{
			Prefixes: []string{"?"},
		})
		cfg = reloadConfig(t, root)
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config get @" + userPhone + " timezone",
			Sender:  adminPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		want := "Set with: ?config set @" + userPhone + " timezone America/Chicago"
		if got := recorder.last(t); !strings.Contains(got, want) {
			t.Fatalf("get = %q, want %q", got, want)
		}
	})
}

func TestConfigExport(t *testing.T) {
	t.Run("self exports stored and effective values", func(t *testing.T) {
		_, cfg, _ := newTestConfig(t)
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config export",
			Sender:  userPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}

		want := strings.Join([]string{
			"!config set timezone America/Chicago",
			"!config set prefixes !",
			"!config set muted false",
			"!config set aliases.satellites.wind wind-predictor",
			"!config set aliases.commands.w fly",
		}, "\n")
		if got := recorder.last(t); got != want {
			t.Fatalf("export =\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("admin export targets the user", func(t *testing.T) {
		const otherPhone = "+15555550103"
		_, cfg, _ := newTestConfig(t)
		manager, recorder := readManager(t, cfg)

		if err := manager.Handle(sdk.Transmission{
			Trigger: "config",
			Text:    "config export @" + otherPhone,
			Sender:  adminPhone,
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		want := "!config set @" + otherPhone + " timezone UTC"
		if got := recorder.last(t); !strings.HasPrefix(got, want) {
			t.Fatalf("export = %q, want prefix %q", got, want)
		}
	})
}

// TestConfigNoPrefix covers the explicit empty prefix list: it is written as
// [], displayed as (none), exported as a pasteable set command, and removes
// the prefix from generated examples.
func TestConfigNoPrefix(t *testing.T) {
	root, cfg, st := newTestConfig(t)
	manager := New(Options{
		Config: func() *config.Config { return cfg },
		Store:  st,
	})

	if err := manager.HandlePatch(sdk.Transmission{
		Command: "set",
		Trigger: "config",
		Text:    `Config Set prefixes []`,
		Sender:  userPhone,
	}); err != nil {
		t.Fatalf("HandlePatch(prefixes []): %v", err)
	}

	var user config.UserConfig
	if err := json.Unmarshal(readUserFile(t, root, userPhone), &user); err != nil {
		t.Fatalf("decode patched user: %v", err)
	}
	if user.Prefixes == nil {
		t.Fatal("prefixes = nil, want an explicit empty list")
	}
	if len(user.Prefixes) != 0 {
		t.Fatalf("prefixes = %v, want none", user.Prefixes)
	}

	cfg = reloadConfig(t, root)
	manager, recorder := readManager(t, cfg)

	if err := manager.Handle(sdk.Transmission{
		Trigger: "config",
		Text:    "Config List",
		Sender:  userPhone,
	}); err != nil {
		t.Fatalf("Handle(list): %v", err)
	}
	if got := recorder.last(t); !strings.Contains(got, "prefixes = (none)") {
		t.Fatalf("list = %q, want prefixes = (none)", got)
	}

	manager, recorder = readManager(t, cfg)
	if err := manager.Handle(sdk.Transmission{
		Trigger: "config",
		Text:    "Config Export",
		Sender:  userPhone,
	}); err != nil {
		t.Fatalf("Handle(export): %v", err)
	}
	if got := recorder.last(t); !strings.Contains(got, "config set prefixes []") {
		t.Fatalf("export = %q, want config set prefixes []", got)
	}
}

func TestConfigReadRequiresSender(t *testing.T) {
	_, cfg, _ := newTestConfig(t)
	manager := New(Options{Config: func() *config.Config { return cfg }})

	err := manager.Handle(sdk.Transmission{
		Trigger: "config",
		Text:    "config list",
		Sender:  userPhone,
	})
	requireStatus(t, err, http.StatusServiceUnavailable)
}

func TestConfigSuccessReactionFor(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{text: "config set timezone UTC", want: "👍"},
		{text: "config list", want: ""},
		{text: "config", want: ""},
		{text: "config get timezone", want: ""},
		{text: "config export", want: ""},
	}
	for _, tt := range tests {
		if got := SuccessReactionFor(tt.text); got != tt.want {
			t.Errorf("SuccessReactionFor(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

// TestConfigGroupPrincipal proves a group is its own settings identity: any
// member can edit the group's settings, they are stored under groups/, and
// administrative targets are ignored without a user-facing error.
func TestConfigGroupPrincipal(t *testing.T) {
	const groupID = "group.FyZ5D/fEjYhE0+7AvLZgAD3w88fRJ08xx1otqxDCVZE="

	root, cfg, st := newTestConfig(t)
	cfg.Admin.ApprovedGroups = []string{groupID}
	manager := New(Options{
		Config: func() *config.Config { return cfg },
		Store:  st,
	})

	// A plain member edits the group's settings.
	err := manager.HandlePatch(sdk.Transmission{
		Command: "set",
		Trigger: "config",
		Text:    "config set timezone Europe/Paris",
		Sender:  userPhone,
		GroupID: groupID,
	})
	requireStatus(t, err, 0)

	groupPath := config.UserFilePath(root, groupID)
	raw, err := os.ReadFile(groupPath)
	if err != nil {
		t.Fatalf("read group settings: %v", err)
	}
	var groupUser config.UserConfig
	if err := json.Unmarshal(raw, &groupUser); err != nil {
		t.Fatalf("decode group settings: %v", err)
	}
	if groupUser.Timezone != "Europe/Paris" {
		t.Fatalf("group timezone = %q, want Europe/Paris", groupUser.Timezone)
	}

	// The same edit must not touch the sender's own file.
	if got := readUserFile(t, root, userPhone); strings.Contains(string(got), "Europe/Paris") {
		t.Fatalf("group edit leaked into the sender's user file: %s", got)
	}

	// Administrative targets are ignored silently, not rejected with text.
	ignored := []sdk.Transmission{
		{
			Command: "set",
			Trigger: "config",
			Text:    "config set @" + adminPhone + " timezone Europe/Paris",
			Sender:  userPhone,
			GroupID: groupID,
		},
		{
			Command: "list",
			Trigger: "config",
			Text:    "config list @" + adminPhone,
			Sender:  userPhone,
			GroupID: groupID,
		},
		{
			Command: "set",
			Trigger: "config",
			Text:    "config set satellite_ids.wind alice",
			Sender:  userPhone,
			GroupID: groupID,
		},
	}
	for _, transmission := range ignored {
		if err := manager.Handle(transmission); !errors.Is(err, sdk.ErrIgnored) {
			t.Fatalf("Handle(%q) = %v, want sdk.ErrIgnored", transmission.Text, err)
		}
	}
}

func TestSplitSatelliteTargetResolvesNameAndCase(t *testing.T) {
	cfg := &config.Config{Satellites: map[string]config.SatelliteConfig{
		"wp": {Name: "wind-predictor"},
	}}

	name, key, ok := splitSatelliteTarget(cfg, "wind-predictor.alert_schedule.nightly_local")
	if !ok || name != "wp" || key != "alert_schedule.nightly_local" {
		t.Fatalf("splitSatelliteTarget by name = %q, %q, %v", name, key, ok)
	}

	name, key, ok = splitSatelliteTarget(cfg, "Wind-Predictor.muted")
	if !ok || name != "wp" || key != "muted" {
		t.Fatalf("splitSatelliteTarget case-insensitive = %q, %q, %v", name, key, ok)
	}

	if _, _, ok := splitSatelliteTarget(cfg, "unknown.key"); ok {
		t.Fatal("unknown satellite matched")
	}
}
