package configmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

// satelliteReadTarget reports whether a read command addresses a satellite's
// own settings. list/export carry the satellite name in command.phone; get
// carries it as the <satellite>.<key> target.
func satelliteReadTarget(cfg *config.Config, cmd command) (name, key string, ok bool) {
	switch cmd.action {
	case "list", "export":
		target := strings.TrimSpace(cmd.phone)
		if target == "" || strings.HasPrefix(target, "@") {
			return "", "", false
		}
		name, ok = findSatelliteKey(cfg, target)
		return name, "", ok
	case "get":
		return splitSatelliteTarget(cfg, cmd.target)
	default:
		return "", "", false
	}
}

// satelliteSetting is one key/value pair returned by a satellite's config read
// endpoint. Note marks values that are defaults or read-only; export skips
// them because they are not valid set targets.
type satelliteSetting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Note  string `json:"note,omitempty"`
}

type satelliteSettingsResponse struct {
	Settings []satelliteSetting `json:"settings"`
}

// handleSatelliteRead fetches one satellite's settings and renders them with
// the same list/get/export format as Aether's own config command. Satellite
// config is admin-only and private-chat-only, matching writes.
func (m *Manager) handleSatelliteRead(t sdk.Transmission, cfg *config.Config, actor, sender string, role auth.Role, name, action, key string) error {
	if strings.TrimSpace(t.GroupID) != "" {
		return sdk.ErrIgnored
	}
	if role < auth.Admin {
		return statusError(http.StatusForbidden, "config: admin role required to view satellite configuration")
	}

	settings, err := m.fetchSatelliteSettings(context.Background(), cfg, actor, name)
	if err != nil {
		return err
	}

	var text string
	switch action {
	case "list":
		text = renderSatelliteList(settings, cfg, actor, name)
	case "export":
		text = renderSatelliteExport(settings, cfg, actor, name)
	case "get":
		text, err = renderSatelliteGet(settings, cfg, actor, name, key)
		if err != nil {
			return err
		}
	default:
		return statusBadCommand()
	}
	return m.send(t, text)
}

// fetchSatelliteSettings reads the actor's satellite-owned settings. The
// satellite resolves its own schema and defaults; Aether only renders the
// returned key/value pairs.
func (m *Manager) fetchSatelliteSettings(ctx context.Context, cfg *config.Config, actor, name string) ([]setting, error) {
	satellite, ok := cfg.Satellites[name]
	if !ok {
		return nil, statusError(http.StatusBadRequest, "config: unknown satellite %q", name)
	}

	userID := ""
	if user, ok := cfg.Users[actor]; ok {
		userID = strings.TrimSpace(user.SatelliteIDs[name])
	}
	if userID == "" {
		return nil, statusError(http.StatusBadRequest, "config: sender %s is not mapped to satellite %q", actor, name)
	}

	endpoint := strings.TrimRight(strings.TrimSpace(satellite.Endpoint), "/")
	if endpoint == "" {
		return nil, statusError(http.StatusBadRequest, "config: satellite %q has no endpoint", name)
	}

	token, err := auth.Issue(cfg.System.Auth.JWTSecret, actor, auth.Admin, forwardTokenTTL)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, "config: mint forwarding token: %v", err)
	}

	requestURL := endpoint + forwardPath + "?user=" + url.QueryEscape(userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, "config: build read request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, statusError(http.StatusBadGateway, "config: read %s: %v", name, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxForwardBody))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, statusError(http.StatusBadGateway, "config: read %s: status %d", name, resp.StatusCode)
	}

	var payload satelliteSettingsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, statusError(http.StatusBadGateway, "config: read %s: invalid settings response", name)
	}

	settings := make([]setting, 0, len(payload.Settings))
	for _, item := range payload.Settings {
		if strings.TrimSpace(item.Key) == "" {
			continue
		}
		settings = append(settings, setting{
			key:   strings.TrimSpace(item.Key),
			value: item.Value,
			note:  strings.TrimSpace(item.Note),
		})
	}
	return settings, nil
}

// renderSatelliteList prints every returned setting in the same key = value
// format as the Aether-level list.
func renderSatelliteList(settings []setting, cfg *config.Config, actor, name string) string {
	var b strings.Builder
	for _, s := range settings {
		b.WriteString(formatSetting(s))
		b.WriteString("\n")
	}
	if len(settings) == 0 {
		b.WriteString("(no settings)\n")
	}
	example := actorPrefix(cfg, actor) + satelliteCommandPrefix(cfg, name) + "config get <key>"
	fmt.Fprintf(&b, "Send %q for one setting.", example)
	return b.String()
}

// renderSatelliteGet expands one key or a group such as alert_schedule.
func renderSatelliteGet(settings []setting, cfg *config.Config, actor, name, key string) (string, error) {
	matched := matchSettings(settings, key)
	if len(matched) == 0 {
		return "", statusError(http.StatusBadRequest, "config: unknown satellite key %q", key)
	}

	var b strings.Builder
	for _, s := range matched {
		b.WriteString(formatSetting(s))
		b.WriteString("\n")
	}
	for _, s := range matched {
		if s.note != "" {
			continue
		}
		b.WriteString("Set with: ")
		b.WriteString(pasteSatelliteSetCommand(cfg, actor, name, s.key, s.value))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// renderSatelliteExport prints pasteable scoped set lines. Defaults and
// read-only values are skipped because they cannot be written back.
func renderSatelliteExport(settings []setting, cfg *config.Config, actor, name string) string {
	lines := make([]string, 0, len(settings))
	for _, s := range settings {
		if s.note != "" {
			continue
		}
		lines = append(lines, pasteSatelliteSetCommand(cfg, actor, name, s.key, s.value))
	}
	if len(lines) == 0 {
		return "No satellite settings to export."
	}
	return strings.Join(lines, "\n")
}

// pasteSatelliteSetCommand renders the scoped form the user can paste back.
func pasteSatelliteSetCommand(cfg *config.Config, actor, name, key, value string) string {
	return actorPrefix(cfg, actor) + satelliteCommandPrefix(cfg, name) + "config set " + key + " " + value
}

// satelliteCommandPrefix returns the short prefix when configured, otherwise
// the canonical name, with a trailing space.
func satelliteCommandPrefix(cfg *config.Config, name string) string {
	satellite, ok := cfg.Satellites[name]
	if !ok {
		return name + " "
	}
	if prefix := strings.TrimSpace(satellite.Prefix); prefix != "" {
		return prefix + " "
	}
	if canonical := strings.TrimSpace(satellite.Name); canonical != "" {
		return canonical + " "
	}
	return name + " "
}
