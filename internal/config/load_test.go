package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfigFile writes content to path, creating parent directories as
// needed.
func writeConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadConfig(t *testing.T) {
	const validSystem = `{
		"server": {"port": 9090, "db_path": "/tmp/aether-test.db"},
		"persona": {"name": "Aether", "prefixes": ["!", "/"]},
		"auth": {"jwt_secret": "test-secret", "ingress_token": "ingress"},
		"limits": {"rate_per_second": 2, "burst": 3, "queue_depth": 16, "dispatch_queue_depth": 17, "send_timeout_seconds": 7, "dedupe_ttl_seconds": 90, "send_attempts": 4, "retry_backoff_ms": 750},
		"default_provider": "signal",
		"providers": {"signal": {"type": "msg", "account": "+15555550100"}},
		"schedules": [
			{"name": "morning", "cron": "0 7 * * 1-5", "satellite": "wind-predictor", "command": "fly", "args": {"station": "FPS"}}
		]
	}`

	tests := []struct {
		name      string
		emptyRoot bool
		setup     func(t *testing.T, root string)
		wantErr   string // substring of the expected error; empty means success
		check     func(t *testing.T, cfg *Config, root string)
	}{
		{
			name:    "missing system.json",
			setup:   func(t *testing.T, root string) {},
			wantErr: "system.json",
		},
		{
			name:    "malformed system.json",
			setup:   func(t *testing.T, root string) { writeConfigFile(t, filepath.Join(root, "system.json"), `{"server":`) },
			wantErr: "system.json",
		},
		{
			name: "valid tree loads",
			setup: func(t *testing.T, root string) {
				writeConfigFile(t, filepath.Join(root, "system.json"), validSystem)
				writeConfigFile(t, filepath.Join(root, "admin.json"), `{
					"admins": ["+15555550101"],
					"approved_numbers": ["+15555550101", "+15555550102"],
					"denied_numbers": ["+15555550999"],
					"approved_groups": ["group.example-opaque-id"]
				}`)
				writeConfigFile(t, filepath.Join(root, "maintainer.json"), `{
					"maintainers": ["+15555550101"],
					"last_assigned": 2,
					"notify_approvals": true
				}`)
				writeConfigFile(t, filepath.Join(root, "users", "+15555550102.json"), `{
					"prefixes": ["!"],
					"timezone": "America/Chicago",
					"muted": false,
					"satellite_ids": {"wind-predictor": "alice"},
					"aliases": {"satellites": {"wind": "wind-predictor"}, "commands": {"w": "fly"}}
				}`)
				writeConfigFile(t, filepath.Join(root, "users", "+15555550103.json"), `{
					"timezone": "Europe/London",
					"muted": true
				}`)
				writeConfigFile(t, filepath.Join(root, "satellites", "wind-predictor.json"), `{
					"name": "wind-predictor",
					"trigger": "fly",
					"endpoint": "http://127.0.0.1:9463",
					"commands": [
						{"name": "fly", "path": "/api/v1/predict/text", "patterns": ["fly <station>"], "args": [{"name": "station", "type": "string"}]}
					]
				}`)
				writeConfigFile(t, filepath.Join(root, "routes.json"), `{
					"routes": [
						{
							"trigger": "fly",
							"satellite": "wind-predictor",
							"endpoint": "http://127.0.0.1:9463",
							"commands": [
								{"name": "fly", "path": "/api/v1/predict/text", "patterns": ["fly <station>"]}
							]
						}
					]
				}`)
			},
			check: func(t *testing.T, cfg *Config, root string) {
				if cfg.Root != root {
					t.Errorf("Root = %q, want %q", cfg.Root, root)
				}
				if got := cfg.System.Server.Port; got != 9090 {
					t.Errorf("System.Server.Port = %d, want 9090", got)
				}
				if got := cfg.System.Auth.JWTSecret; got != "test-secret" {
					t.Errorf("System.Auth.JWTSecret = %q, want %q", got, "test-secret")
				}
				if got := cfg.System.Limits.QueueDepth; got != 16 {
					t.Errorf("System.Limits.QueueDepth = %d, want 16", got)
				}
				if got := cfg.System.Limits.SendTimeoutSeconds; got != 7 {
					t.Errorf("System.Limits.SendTimeoutSeconds = %d, want 7", got)
				}
				if got := cfg.System.Limits.DedupeTTLSeconds; got != 90 {
					t.Errorf("System.Limits.DedupeTTLSeconds = %d, want 90", got)
				}
				if got := cfg.System.Limits.DispatchQueueDepth; got != 17 {
					t.Errorf("System.Limits.DispatchQueueDepth = %d, want 17", got)
				}
				if got := cfg.System.Limits.SendAttempts; got != 4 {
					t.Errorf("System.Limits.SendAttempts = %d, want 4", got)
				}
				if got := cfg.System.Limits.RetryBackoffMS; got != 750 {
					t.Errorf("System.Limits.RetryBackoffMS = %d, want 750", got)
				}
				if got := len(cfg.System.Schedules); got != 1 {
					t.Fatalf("len(System.Schedules) = %d, want 1", got)
				}
				if got := cfg.System.Schedules[0].Args["station"]; got != "FPS" {
					t.Errorf("Schedule args[station] = %v, want FPS", got)
				}
				if got := len(cfg.Admin.Admins); got != 1 || cfg.Admin.Admins[0] != "+15555550101" {
					t.Errorf("Admin.Admins = %v, want [+15555550101]", cfg.Admin.Admins)
				}
				if got := cfg.Maintainer.LastAssigned; got != 2 {
					t.Errorf("Maintainer.LastAssigned = %d, want 2", got)
				}
				if got := len(cfg.Users); got != 2 {
					t.Fatalf("len(Users) = %d, want 2", got)
				}
				alice, ok := cfg.Users["+15555550102"]
				if !ok {
					t.Fatalf("Users missing key %q: %v", "+15555550102", cfg.Users)
				}
				if alice.Timezone != "America/Chicago" {
					t.Errorf("user timezone = %q, want America/Chicago", alice.Timezone)
				}
				if got := alice.SatelliteIDs["wind-predictor"]; got != "alice" {
					t.Errorf("user satellite id = %q, want alice", got)
				}
				if !cfg.Users["+15555550103"].Muted {
					t.Errorf("user +15555550103 should be muted")
				}
				if got := len(cfg.Satellites); got != 1 {
					t.Fatalf("len(Satellites) = %d, want 1", got)
				}
				sat, ok := cfg.Satellites["wind-predictor"]
				if !ok {
					t.Fatalf("Satellites missing key wind-predictor: %v", cfg.Satellites)
				}
				if sat.Trigger != "fly" {
					t.Errorf("satellite trigger = %q, want fly", sat.Trigger)
				}
				if len(sat.Commands) != 1 || sat.Commands[0].Path != "/api/v1/predict/text" {
					t.Errorf("satellite commands = %+v", sat.Commands)
				}
				if got := len(cfg.Routes.Routes); got != 1 {
					t.Fatalf("len(Routes.Routes) = %d, want 1", got)
				}
				if cfg.Routes.Routes[0].Trigger != "fly" {
					t.Errorf("route trigger = %q, want fly", cfg.Routes.Routes[0].Trigger)
				}
			},
		},
		{
			name: "optional files may be absent",
			setup: func(t *testing.T, root string) {
				writeConfigFile(t, filepath.Join(root, "system.json"), `{"server": {"port": 8080}}`)
			},
			check: func(t *testing.T, cfg *Config, root string) {
				if cfg.Root != root {
					t.Errorf("Root = %q, want %q", cfg.Root, root)
				}
				if got := cfg.System.Server.Port; got != 8080 {
					t.Errorf("System.Server.Port = %d, want 8080", got)
				}
				if got := len(cfg.Admin.Admins); got != 0 {
					t.Errorf("Admin.Admins = %v, want empty", cfg.Admin.Admins)
				}
				if got := len(cfg.Maintainer.Maintainers); got != 0 {
					t.Errorf("Maintainer.Maintainers = %v, want empty", cfg.Maintainer.Maintainers)
				}
				if got := len(cfg.Users); got != 0 {
					t.Errorf("len(Users) = %d, want 0", got)
				}
				if got := len(cfg.Satellites); got != 0 {
					t.Errorf("len(Satellites) = %d, want 0", got)
				}
				if got := len(cfg.Routes.Routes); got != 0 {
					t.Errorf("len(Routes.Routes) = %d, want 0", got)
				}
			},
		},
		{
			name: "malformed optional admin.json",
			setup: func(t *testing.T, root string) {
				writeConfigFile(t, filepath.Join(root, "system.json"), validSystem)
				writeConfigFile(t, filepath.Join(root, "admin.json"), `{"admins": [`)
			},
			wantErr: "admin.json",
		},
		{
			name: "malformed user file",
			setup: func(t *testing.T, root string) {
				writeConfigFile(t, filepath.Join(root, "system.json"), validSystem)
				writeConfigFile(t, filepath.Join(root, "users", "+15555550102.json"), `{"timezone":`)
			},
			wantErr: "+15555550102.json",
		},
		{
			name:      "empty root is rejected",
			emptyRoot: true,
			wantErr:   "root",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.setup != nil {
				tt.setup(t, root)
			}
			queryRoot := root
			if tt.emptyRoot {
				queryRoot = ""
			}

			cfg, err := LoadConfig(queryRoot)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadConfig(%q) = %+v, want error containing %q", queryRoot, cfg, tt.wantErr)
				}
				if cfg != nil {
					t.Fatalf("LoadConfig(%q) returned config %+v alongside error %v; want nil", queryRoot, cfg, err)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadConfig(%q) error = %q, want substring %q", queryRoot, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig(%q) returned unexpected error: %v", queryRoot, err)
			}
			if cfg == nil {
				t.Fatalf("LoadConfig(%q) returned nil config", queryRoot)
			}
			if tt.check != nil {
				tt.check(t, cfg, root)
			}
		})
	}

	t.Run("ResolveRoot honours AETHER_CONFIG_DIR", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AETHER_CONFIG_DIR", dir)
		if got := ResolveRoot(); got != dir {
			t.Fatalf("ResolveRoot() = %q, want %q", got, dir)
		}
	})

	t.Run("ResolveRoot defaults to HOME/.config/aether", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AETHER_CONFIG_DIR", "")
		t.Setenv("HOME", home)
		want := filepath.Join(home, ".config", "aether")
		if got := ResolveRoot(); got != want {
			t.Fatalf("ResolveRoot() = %q, want %q", got, want)
		}
	})
}
