package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/store"
)

// maxRegisterBody bounds a /register-satellite request body. A satellite
// document is small; a larger body is malformed or hostile.
const maxRegisterBody = 1 << 20

// satelliteNamePattern keeps a registered name usable as a file name. It is the
// path-traversal guard required by plan §3.15: no slash, no dot segments.
var satelliteNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// builtinSatellites are the internal handlers that take precedence over any
// file-registered satellite (plan §3.6). A file registration may not claim one
// of these names or triggers.
var builtinSatellites = map[string]bool{
	"config": true,
	"admin":  true,
	"remind": true,
	"test":   true,
}

// SatelliteStore mirrors a registered satellite into durable storage.
// *store.Store implements it.
type SatelliteStore interface {
	UpsertSatellite(ctx context.Context, rec store.SatelliteRecord) error
}

// RegisterOptions configures the protected /register-satellite handler.
type RegisterOptions struct {
	// Config returns the active configuration. Its Root is the config
	// directory whose satellites/ and routes.json are updated.
	Config func() *config.Config
	// Store receives the SQLite mirror of the satellite document.
	Store SatelliteStore
}

type registerHandler struct {
	options RegisterOptions
}

// NewRegisterHandler returns the admin/maintainer-facing
// POST /register-satellite handler.
func NewRegisterHandler(options RegisterOptions) http.Handler {
	return &registerHandler{options: options}
}

func (h *registerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	cfg := h.config()
	if cfg == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}

	// Build the authenticator per request so a reloaded jwt_secret takes
	// effect without restarting the server.
	authenticator := auth.Authenticator{Secret: cfg.System.Auth.JWTSecret}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handle(w, r, cfg)
	})
	authenticator.Middleware(next, auth.Admin).ServeHTTP(w, r)
}

func (h *registerHandler) handle(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	satellite, err := decodeSatellite(w, r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := validateSatellite(satellite); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if builtinSatellites[satellite.Name] || builtinSatellites[satellite.Trigger] {
		http.Error(w, "conflict", http.StatusConflict)
		return
	}
	if strings.TrimSpace(cfg.Root) == "" {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	if h.options.Store == nil {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}

	dir := filepath.Join(cfg.Root, "satellites")
	existing, err := loadSatelliteFiles(dir)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if _, ok := existing[satellite.Name]; ok {
		http.Error(w, "conflict", http.StatusConflict)
		return
	}
	for _, other := range existing {
		if other.Trigger == satellite.Trigger {
			http.Error(w, "conflict", http.StatusConflict)
			return
		}
	}

	raw, err := encodeSatellite(satellite)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	path := filepath.Join(dir, satellite.Name+".json")
	if err := writeFileAtomic(dir, path, raw); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	record := store.SatelliteRecord{
		Name:         satellite.Name,
		Trigger:      satellite.Trigger,
		Endpoint:     satellite.Endpoint,
		Config:       raw,
		RegisteredBy: subjectOf(r),
	}
	if err := h.options.Store.UpsertSatellite(r.Context(), record); err != nil {
		// Do not leave a file whose registry mirror failed.
		_ = os.Remove(path)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	if err := rebuildRoutes(cfg.Root); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func (h *registerHandler) config() *config.Config {
	if h.options.Config == nil {
		return nil
	}
	return h.options.Config()
}

func subjectOf(r *http.Request) string {
	if claims, ok := auth.ClaimsFromContext(r.Context()); ok {
		return claims.Sub
	}
	return ""
}

// decodeSatellite reads exactly one satellite document.
func decodeSatellite(w http.ResponseWriter, r *http.Request) (config.SatelliteConfig, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRegisterBody))

	var satellite config.SatelliteConfig
	if err := decoder.Decode(&satellite); err != nil {
		return config.SatelliteConfig{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return config.SatelliteConfig{}, errors.New("register: body must contain one JSON object")
	}
	return satellite, nil
}

// validateSatellite enforces plan §3.15: a safe, unique name, a non-empty
// trigger, an absolute http(s) endpoint, and well-formed commands.
func validateSatellite(satellite config.SatelliteConfig) error {
	if !satelliteNamePattern.MatchString(satellite.Name) {
		return errors.New("register: invalid satellite name")
	}
	if strings.TrimSpace(satellite.Trigger) == "" {
		return errors.New("register: trigger is required")
	}

	endpoint, err := url.Parse(satellite.Endpoint)
	if err != nil || endpoint.Host == "" ||
		(!strings.EqualFold(endpoint.Scheme, "http") && !strings.EqualFold(endpoint.Scheme, "https")) {
		return errors.New("register: endpoint must be an absolute http(s) URL")
	}

	for _, command := range satellite.Commands {
		if strings.TrimSpace(command.Name) == "" {
			return errors.New("register: command name is required")
		}
		if !strings.HasPrefix(command.Path, "/") {
			return errors.New("register: command path must start with /")
		}
		if len(command.Patterns) == 0 {
			return errors.New("register: command needs at least one pattern")
		}
		for _, pattern := range command.Patterns {
			if strings.TrimSpace(pattern) == "" {
				return errors.New("register: command pattern is blank")
			}
		}
	}
	return nil
}

// encodeSatellite renders a satellite document the same way the example files
// are written: indented JSON with a trailing newline.
func encodeSatellite(satellite config.SatelliteConfig) ([]byte, error) {
	raw, err := json.MarshalIndent(satellite, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("register: marshal satellite: %w", err)
	}
	return append(raw, '\n'), nil
}

// loadSatelliteFiles reads every satellites/*.json document, keyed by file
// name without the .json suffix. A missing directory is empty, not an error.
func loadSatelliteFiles(dir string) (map[string]config.SatelliteConfig, error) {
	satellites := make(map[string]config.SatelliteConfig)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return satellites, nil
		}
		return nil, fmt.Errorf("register: read %s: %w", dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("register: read %s: %w", path, err)
		}
		var satellite config.SatelliteConfig
		if err := json.Unmarshal(raw, &satellite); err != nil {
			return nil, fmt.Errorf("register: parse %s: %w", path, err)
		}
		satellites[strings.TrimSuffix(entry.Name(), ".json")] = satellite
	}
	return satellites, nil
}

// rebuildRoutes regenerates routes.json from every satellite file on disk.
// Registration never appends to a stale routes.json (plan §3.14).
func rebuildRoutes(root string) error {
	satellites, err := loadSatelliteFiles(filepath.Join(root, "satellites"))
	if err != nil {
		return err
	}

	names := make([]string, 0, len(satellites))
	for name := range satellites {
		names = append(names, name)
	}
	sort.Strings(names)

	routes := config.RoutesConfig{Routes: make([]config.RouteEntry, 0, len(names))}
	for _, key := range names {
		satellite := satellites[key]
		name := satellite.Name
		if name == "" {
			name = key
		}
		routes.Routes = append(routes.Routes, config.RouteEntry{
			Trigger:   satellite.Trigger,
			Satellite: name,
			Endpoint:  satellite.Endpoint,
			Commands:  satellite.Commands,
		})
	}

	raw, err := json.MarshalIndent(routes, "", "  ")
	if err != nil {
		return fmt.Errorf("register: marshal routes: %w", err)
	}
	if err := writeFileAtomic(root, filepath.Join(root, "routes.json"), append(raw, '\n')); err != nil {
		return fmt.Errorf("register: write routes: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to a temporary file in dir and renames it over
// path, so a reader never observes a partially written document.
func writeFileAtomic(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
