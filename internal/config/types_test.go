package config

import (
    "encoding/json"
    "reflect"
    "testing"
)

func TestStructs(t *testing.T) {
    // Minimal JSON covering top‑level Config with nested structs.
    data := []byte(`{
        "root":"/tmp/aether",
        "system":{
            "server":{"port":8080,"db_path":"/tmp/db"},
            "persona":{"name":"test","prefixes":["!","/"]},
            "auth":{"jwt_secret":"secret","ingress_token":"token"},
            "limits":{"rate_per_second":10,"burst":5,"queue_depth":100},
            "default_provider":"test",
            "providers":{},
            "schedules":[]
        },
        "admin":{"admins":["+15555550102"],"approved_numbers":[],"denied_numbers":[],"approved_groups":[]},
        "maintainer":{"maintainers":[],"last_assigned":0,"notify_approvals":false},
        "users":{},
        "satellites":{},
        "routes":{ "routes":[] }
    }`)

    var cfg Config
    if err := json.Unmarshal(data, &cfg); err != nil {
        t.Fatalf("unmarshal failed: %v", err)
    }

    // Verify a couple of nested fields have expected values.
    if cfg.System.Server.Port != 8080 {
        t.Errorf("expected port 8080, got %d", cfg.System.Server.Port)
    }
    if cfg.System.Persona.Name != "test" {
        t.Errorf("expected persona name 'test', got %s", cfg.System.Persona.Name)
    }
    if cfg.Admin.Admins[0] != "+15555550102" {
        t.Errorf("expected admin '+15555550102', got %s", cfg.Admin.Admins[0])
    }

    // Round‑trip test: marshal back and compare JSON structures (ignoring field order).
    out, err := json.Marshal(cfg)
    if err != nil {
        t.Fatalf("marshal failed: %v", err)
    }
    var cfg2 Config
    if err := json.Unmarshal(out, &cfg2); err != nil {
        t.Fatalf("unmarshal round‑trip failed: %v", err)
    }
    if !reflect.DeepEqual(cfg, cfg2) {
        t.Errorf("config round‑trip mismatch")
    }
}
