package router

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestModuleInit(t *testing.T) {
	if got := New(); got == nil {
		t.Fatal("New() returned nil")
	}
}

func TestScaffoldLayout(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))

	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if !strings.Contains(string(mod), "module github.com/charlesrobsampson/aether") {
		t.Fatalf("go.mod does not declare github.com/charlesrobsampson/aether:\n%s", mod)
	}

	dirs := []string{
		"cmd/aether",
		"internal/app",
		"internal/config",
		"internal/model",
		"internal/auth",
		"internal/router",
		"internal/session",
		"internal/store",
		"internal/queue",
		"internal/transport",
		"internal/metrics",
		"internal/api",
		"internal/scheduler",
		"internal/testutil",
		"internal/integration",
		"internal/deploy",
		"internal/docs",
		"internal/satellite/configmgr",
		"internal/satellite/adminmgr",
		"internal/satellite/testsat",
		"pkg/sdk",
		"examples/config/users",
		"examples/config/satellites",
		"scripts",
		".github/workflows",
	}
	for _, dir := range dirs {
		info, err := os.Stat(filepath.Join(root, dir))
		if err != nil {
			t.Errorf("missing directory %s: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}

	configRoot := filepath.Join(root, "examples", "config")
	var jsonFiles int
	err = filepath.WalkDir(configRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		jsonFiles++
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("example config is not valid JSON: %v", err)
	}
	if jsonFiles == 0 {
		t.Fatal("no example config JSON files found")
	}
}
