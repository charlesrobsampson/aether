package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadConfig reads the JSON configuration tree rooted at root.
//
// system.json is required. admin.json, maintainer.json, users/*.json,
// satellites/*.json, and routes.json are optional; when absent they leave the
// corresponding fields at their zero values. Config.Root is set to root and is
// never serialised.
func LoadConfig(root string) (*Config, error) {
	if root == "" {
		return nil, fmt.Errorf("config: root is empty")
	}

	cfg := &Config{
		Root:       root,
		Users:      make(map[string]UserConfig),
		Satellites: make(map[string]SatelliteConfig),
	}

	if err := readRequiredJSON(filepath.Join(root, "system.json"), &cfg.System); err != nil {
		return nil, err
	}
	if err := readOptionalJSON(filepath.Join(root, "admin.json"), &cfg.Admin); err != nil {
		return nil, err
	}
	if err := readOptionalJSON(filepath.Join(root, "maintainer.json"), &cfg.Maintainer); err != nil {
		return nil, err
	}

	if err := loadJSONDir(filepath.Join(root, "users"), func(key string, data []byte) error {
		var user UserConfig
		if err := json.Unmarshal(data, &user); err != nil {
			return err
		}
		cfg.Users[key] = user
		return nil
	}); err != nil {
		return nil, err
	}

	if err := loadJSONDir(filepath.Join(root, "satellites"), func(key string, data []byte) error {
		var satellite SatelliteConfig
		if err := json.Unmarshal(data, &satellite); err != nil {
			return err
		}
		cfg.Satellites[key] = satellite
		return nil
	}); err != nil {
		return nil, err
	}

	if err := readOptionalJSON(filepath.Join(root, "routes.json"), &cfg.Routes); err != nil {
		return nil, err
	}

	return cfg, nil
}

// ResolveRoot returns the config root directory: AETHER_CONFIG_DIR when set,
// otherwise ~/.config/aether.
func ResolveRoot() string {
	if dir := os.Getenv("AETHER_CONFIG_DIR"); dir != "" {
		return dir
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "aether")
	}
	return filepath.Join(".config", "aether")
}

// readRequiredJSON reads path and unmarshals it into v. A missing file is an
// error.
func readRequiredJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

// readOptionalJSON reads path and unmarshals it into v. A missing file is not
// an error; any other read or parse failure is.
func readOptionalJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

// loadJSONDir calls onFile for every *.json file directly inside dir, with key
// set to the file name without its .json suffix. A missing directory is not an
// error.
func loadJSONDir(dir string, onFile func(key string, data []byte) error) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config: read directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := onFile(strings.TrimSuffix(entry.Name(), ".json"), data); err != nil {
			return fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	return nil
}
