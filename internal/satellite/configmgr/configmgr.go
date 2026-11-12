// Package configmgr implements Aether's built-in config satellite (plan §3.6).
//
// A user inspects their settings with
//
//	!config list
//	!config get <key>
//	!config export
//
// and edits their own users/<phone>.json with
//
//	!config set <key> <value>
//
// and an admin edits or inspects another user with
//
//	!config list|get|export|set @<phone> ...
//
// Read-only output is plain key = value text with defaults marked in
// parentheses; export prints pasteable !config set lines.
//
// A <satellite>.<key> target is treated as satellite-owned configuration: the
// sender is resolved through users/<phone>.json's satellite_ids, and the change
// is forwarded as an RFC6902 patch to the satellite's POST /api/v1/config
// (plan §3.12). Local writes are atomic and every successful change is audited
// in the SQLite patches table.
//
// Local keys are timezone, prefixes, muted, aliases.satellites.<name>, and
// aliases.commands.<name>. The list is deliberately extensible: add a case to
// buildLocalPatch and a test. satellite_ids is admin-managed and is never part
// of this command.
package configmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

const (
	// defaultHTTPTimeout bounds one forwarded config patch. It mirrors the
	// router's dispatch timeout: a slow satellite fails the command, it does
	// not stall the built-in handler.
	defaultHTTPTimeout = 5 * time.Second
	// forwardPath is the wind-predictor config endpoint from plan §3.12.
	forwardPath = "/api/v1/config"
	// forwardTokenTTL is deliberately short; the token only authenticates
	// one local forwarding call.
	forwardTokenTTL = time.Minute
	// maxForwardBody bounds the response body we drain from a satellite.
	maxForwardBody = 1 << 20
)

// Error is a rejected config-manager request with the HTTP status the built-in
// command should surface. T35 maps it onto whatever response the app sends;
// tests assert the status directly.
type Error struct {
	Status  int
	Message string
}

// Error implements error.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// UserMessage exposes client-correctable failures to the router so they can be
// sent back to the sender. Server-side failures stay in the route log.
func (e *Error) UserMessage() string {
	if e == nil || e.Status >= http.StatusInternalServerError {
		return ""
	}
	return e.Message
}

// StatusOf returns the HTTP status carried by err, or 500 for an untyped error.
func StatusOf(err error) int {
	var configErr *Error
	if errors.As(err, &configErr) {
		return configErr.Status
	}
	return http.StatusInternalServerError
}

func statusError(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// PatchStore records applied patches. *store.Store implements it.
type PatchStore interface {
	LogPatch(ctx context.Context, rec store.PatchRecord) error
}

// Sender is the outbound seam used to reply with read-only settings output.
// The app's queue sender satisfies it; tests inject a recorder.
type Sender interface {
	Send(ctx context.Context, resp sdk.Response) error
}

// Options configures a Manager.
type Options struct {
	// Config returns the active configuration. It is called per request so a
	// hot reload picks up new roles and user files.
	Config func() *config.Config
	// Store receives the patches audit row. Nil disables auditing.
	Store PatchStore
	// Client is the HTTP client used for forwarding. Nil uses a client with a
	// 5 s timeout; tests inject httptest's client.
	Client *http.Client
	// Out replies with read-only settings output. Nil makes list/get/export
	// fail with 503; set does not need it because the router reacts instead.
	Out Sender
}

// Manager handles built-in !config commands.
type Manager struct {
	config func() *config.Config
	store  PatchStore
	client *http.Client
	out    Sender
}

// New returns a config-manager. With no configuration source every command is
// rejected with 503.
func New(options Options) *Manager {
	manager := &Manager{
		config: options.Config,
		store:  options.Store,
		client: options.Client,
		out:    options.Out,
	}
	if manager.client == nil {
		manager.client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return manager
}

type setCommand struct {
	// phone is the explicit @<phone> target, if any.
	phone string
	// target is either a local key or <satellite>.<key>.
	target string
	value  string
}

// HandlePatch applies one !config set command. It never mutates t.
func (m *Manager) HandlePatch(t sdk.Transmission) error {
	command, err := parseSet(t)
	if err != nil {
		return err
	}
	return m.handleSet(t, command)
}

// handleSet applies one parsed set command.
func (m *Manager) handleSet(t sdk.Transmission, command setCommand) error {
	cfg, actor, sender, role, err := m.authorize(t)
	if err != nil {
		return err
	}

	if name, key, ok := splitSatelliteTarget(cfg, command.target); ok {
		if role < auth.Admin {
			if strings.TrimSpace(t.GroupID) != "" {
				return sdk.ErrIgnored
			}
			return statusError(http.StatusForbidden, "config: admin role required to edit satellite configuration")
		}
		return m.forward(context.Background(), cfg, actor, sender, name, key, command.value)
	}

	// A group is its own principal. Targeting anyone else, or editing the
	// admin-managed satellite mapping, is an administrative action: groups
	// ignore it without exposing that the command exists.
	if groupAdminAttempt(t.GroupID, actor, command.phone, command.target) {
		return sdk.ErrIgnored
	}

	targetPhone, err := resolveTargetPhone(actor, role, command.phone, "edit")
	if err != nil {
		return err
	}

	return m.applyLocal(context.Background(), cfg, sender, targetPhone, command.target, command.value)
}

// authorize resolves the active config and checks that the conversation may
// use !config. The actor is the conversation principal (the group itself, or
// the sender in a direct chat); the sender is kept for auditing.
func (m *Manager) authorize(t sdk.Transmission) (*config.Config, string, string, auth.Role, error) {
	cfg := m.configSnapshot()
	if cfg == nil || strings.TrimSpace(cfg.Root) == "" {
		return nil, "", "", auth.Denied, statusError(http.StatusServiceUnavailable, "config: configuration is not loaded")
	}

	sender := strings.TrimSpace(t.Sender)
	if sender == "" {
		return nil, "", "", auth.Denied, statusError(http.StatusUnauthorized, "config: sender is required")
	}
	actor := t.Actor()
	if actor == "" {
		actor = sender
	}
	role := auth.ChatRoleOf(cfg, actor, t.GroupID)
	if role < auth.User {
		return nil, "", "", auth.Denied, statusError(http.StatusForbidden, "config: sender is not authorized")
	}
	return cfg, actor, sender, role, nil
}

// groupAdminAttempt reports whether a group message targets anything other
// than the group's own settings. Those requests are administrative and must be
// ignored silently.
func groupAdminAttempt(groupID, actor, phone, key string) bool {
	if strings.TrimSpace(groupID) == "" {
		return false
	}
	if target := strings.TrimSpace(phone); target != "" && target != actor {
		return true
	}
	key = strings.TrimSpace(key)
	return key == "satellite_ids" || strings.HasPrefix(key, "satellite_ids.")
}

// resolveTargetPhone returns the user file a set command edits and enforces
// the admin-only rule for editing someone else.
func resolveTargetPhone(sender string, role auth.Role, phone, verb string) (string, error) {
	target := strings.TrimSpace(phone)
	if target == "" {
		target = sender
	}
	if target != sender && role < auth.Admin {
		return "", statusError(http.StatusForbidden, "config: admin role required to %s another user", verb)
	}
	if !auth.ValidIdentity(target) {
		return "", statusError(http.StatusBadRequest, "config: invalid user %q", target)
	}
	return target, nil
}

// configSnapshot returns the active configuration, if any.
func (m *Manager) configSnapshot() *config.Config {
	if m == nil || m.config == nil {
		return nil
	}
	return m.config()
}

// parseSet reads either the canonical text envelope ("config set ...") or the
// typed args a router-side built-in matcher would produce.
func parseSet(t sdk.Transmission) (setCommand, error) {
	if text := strings.TrimSpace(t.Text); text != "" {
		fields := strings.Fields(text)
		if len(fields) > 0 && (strings.EqualFold(fields[0], "config") || (t.Trigger != "" && strings.EqualFold(fields[0], t.Trigger))) {
			fields = fields[1:]
		}
		if len(fields) == 0 || !strings.EqualFold(fields[0], "set") {
			return setCommand{}, statusBadCommand()
		}
		return parseSetFields(fields[1:])
	}

	// Typed-args fallback: key/value plus an optional phone.
	key := argString(t.Args, "key")
	value, ok := argValue(t.Args, "value")
	if key == "" || !ok {
		return setCommand{}, statusBadCommand()
	}
	return setCommand{
		phone:  argString(t.Args, "phone"),
		target: key,
		value:  value,
	}, nil
}

func parseSetFields(fields []string) (setCommand, error) {
	if len(fields) < 2 {
		return setCommand{}, statusBadCommand()
	}

	if strings.HasPrefix(fields[0], "@") {
		phone := strings.TrimPrefix(fields[0], "@")
		if phone == "" || len(fields) < 3 {
			return setCommand{}, statusBadCommand()
		}
		return setCommand{
			phone:  phone,
			target: fields[1],
			value:  strings.Join(fields[2:], " "),
		}, nil
	}

	return setCommand{
		target: fields[0],
		value:  strings.Join(fields[1:], " "),
	}, nil
}

func statusBadCommand() *Error {
	return statusError(http.StatusBadRequest, `config: expected "config set <key> <value>"`)
}

func argString(args map[string]any, name string) string {
	value, ok := args[name]
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(raw)
}

func argValue(args map[string]any, name string) (string, bool) {
	value, ok := args[name]
	if !ok {
		return "", false
	}
	if text, ok := value.(string); ok {
		return text, true
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// splitSatelliteTarget reports whether target names a configured satellite.
// The first dot separates <satellite> from <key>; a satellite name without a
// key is not a forwarding target because the key identifies the patch path.
func splitSatelliteTarget(cfg *config.Config, target string) (name, key string, ok bool) {
	dot := strings.Index(target, ".")
	if dot <= 0 || dot == len(target)-1 {
		return "", "", false
	}
	name = target[:dot]
	if _, exists := cfg.Satellites[name]; !exists {
		return "", "", false
	}
	return name, target[dot+1:], true
}

// applyLocal patches one user or group settings file and records the audit row.
func (m *Manager) applyLocal(ctx context.Context, cfg *config.Config, appliedBy, phone, key, raw string) error {
	path := config.UserFilePath(cfg.Root, phone)
	dir := filepath.Dir(path)

	existing := []byte("{}\n")
	if data, err := os.ReadFile(path); err == nil {
		existing = data
	} else if !os.IsNotExist(err) {
		return statusError(http.StatusInternalServerError, "config: read user file: %v", err)
	}

	ops, err := buildLocalPatch(existing, key, raw)
	if err != nil {
		return err
	}

	patched, err := sdk.Apply(existing, ops)
	if err != nil {
		return statusError(http.StatusInternalServerError, "config: apply patch: %v", err)
	}

	// Reject a patch that would produce a document the rest of Aether cannot
	// decode (for example, a string where prefixes must be an array).
	var check config.UserConfig
	if err := json.Unmarshal(patched, &check); err != nil {
		return statusError(http.StatusInternalServerError, "config: patched user file is invalid: %v", err)
	}

	pretty, err := indentJSON(patched)
	if err != nil {
		return statusError(http.StatusInternalServerError, "config: format user file: %v", err)
	}
	if err := writeFileAtomic(dir, path, pretty); err != nil {
		return statusError(http.StatusInternalServerError, "config: write user file: %v", err)
	}

	m.audit(ctx, phone, appliedBy, ops)
	return nil
}

// buildLocalPatch turns one whitelisted key/value pair into RFC6902 operations.
// It inspects existing so nested alias writes create only the containers that
// are actually missing.
func buildLocalPatch(existing []byte, key, raw string) ([]sdk.Operation, error) {
	raw = strings.TrimSpace(raw)

	switch {
	case key == "timezone":
		timezone := unquote(raw)
		if timezone == "" {
			return nil, statusError(http.StatusBadRequest, "config: timezone cannot be empty")
		}
		if _, err := time.LoadLocation(timezone); err != nil {
			return nil, statusError(http.StatusBadRequest, "config: unknown timezone %q", timezone)
		}
		return []sdk.Operation{{Op: "add", Path: "/timezone", Value: timezone}}, nil

	case key == "prefixes":
		prefixes, err := parseStringList(raw)
		if err != nil {
			return nil, err
		}
		return []sdk.Operation{{Op: "add", Path: "/prefixes", Value: prefixes}}, nil

	case key == "muted":
		muted, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, statusError(http.StatusBadRequest, "config: muted must be true or false")
		}
		return []sdk.Operation{{Op: "add", Path: "/muted", Value: muted}}, nil

	case strings.HasPrefix(key, "aliases.satellites."):
		return buildAliasPatch(existing, "satellites", strings.TrimPrefix(key, "aliases.satellites."), raw)

	case strings.HasPrefix(key, "aliases.commands."):
		return buildAliasPatch(existing, "commands", strings.TrimPrefix(key, "aliases.commands."), raw)

	case key == "satellite_ids" || strings.HasPrefix(key, "satellite_ids."):
		return nil, statusError(http.StatusBadRequest,
			"config: satellite_ids is admin-managed and cannot be edited with !config set")

	default:
		return nil, statusError(http.StatusBadRequest, "config: key %q is not user-editable", key)
	}
}

func buildAliasPatch(existing []byte, group, alias, raw string) ([]sdk.Operation, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil, statusError(http.StatusBadRequest, "config: alias name is required")
	}
	value := unquote(raw)
	if value == "" {
		return nil, statusError(http.StatusBadRequest, "config: alias value is required")
	}

	doc := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, statusError(http.StatusInternalServerError, "config: existing user file is not valid JSON")
		}
	}

	ops := make([]sdk.Operation, 0, 3)
	aliases, ok := doc["aliases"]
	if !ok || aliases == nil {
		ops = append(ops, sdk.Operation{Op: "add", Path: "/aliases", Value: map[string]any{}})
		aliases = map[string]any{}
	}
	aliasesMap, ok := aliases.(map[string]any)
	if !ok {
		return nil, statusError(http.StatusInternalServerError, "config: existing aliases field is not an object")
	}

	if current, ok := aliasesMap[group]; !ok || current == nil {
		ops = append(ops, sdk.Operation{Op: "add", Path: "/aliases/" + group, Value: map[string]any{}})
	} else if _, ok := current.(map[string]any); !ok {
		return nil, statusError(http.StatusInternalServerError, "config: existing aliases.%s field is not an object", group)
	}

	ops = append(ops, sdk.Operation{
		Op:    "add",
		Path:  "/aliases/" + group + "/" + escapePointerToken(alias),
		Value: value,
	})
	return ops, nil
}

// forward posts {user, patch} to the satellite's config endpoint (plan §3.12).
// The role check happened in handleSet; the satellite owns schema validation.
// actor supplies the satellite mapping and the token subject, while appliedBy
// is the human sender recorded in the audit row.
func (m *Manager) forward(ctx context.Context, cfg *config.Config, actor, appliedBy, name, key, raw string) error {
	satellite, ok := cfg.Satellites[name]
	if !ok {
		return statusError(http.StatusBadRequest, "config: unknown satellite %q", name)
	}

	userID := ""
	if user, ok := cfg.Users[actor]; ok {
		userID = strings.TrimSpace(user.SatelliteIDs[name])
	}
	if userID == "" {
		return statusError(http.StatusBadRequest, "config: sender %s is not mapped to satellite %q", actor, name)
	}

	endpoint := strings.TrimSpace(satellite.Endpoint)
	if endpoint == "" {
		return statusError(http.StatusBadRequest, "config: satellite %q has no endpoint", name)
	}

	pointer := pointerFromKey(key)
	if pointer == "" {
		return statusError(http.StatusBadRequest, "config: satellite key is required")
	}

	value, err := parseForwardValue(raw)
	if err != nil {
		return err
	}
	ops := []sdk.Operation{{Op: "add", Path: pointer, Value: value}}

	token, err := auth.Issue(cfg.System.Auth.JWTSecret, actor, auth.Admin, forwardTokenTTL)
	if err != nil {
		return statusError(http.StatusInternalServerError, "config: mint forwarding token: %v", err)
	}

	body, err := json.Marshal(forwardRequest{User: userID, Patch: ops})
	if err != nil {
		return statusError(http.StatusInternalServerError, "config: encode forwarding request: %v", err)
	}

	url := strings.TrimRight(endpoint, "/") + forwardPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return statusError(http.StatusInternalServerError, "config: build forwarding request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := m.client.Do(req)
	if err != nil {
		return statusError(http.StatusBadGateway, "config: forward to %s: %v", name, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxForwardBody))

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return statusError(http.StatusBadGateway, "config: forward to %s: status %d", name, resp.StatusCode)
	}

	m.audit(ctx, actor, appliedBy, ops)
	return nil
}

type forwardRequest struct {
	User  string          `json:"user"`
	Patch []sdk.Operation `json:"patch"`
}

// parseForwardValue accepts JSON for arrays, objects, numbers, booleans, and
// quoted strings; anything else is a literal string. This lets
// `no_fly_days ["Sunday"]` carry an array while `display_name Alice` stays text.
func parseForwardValue(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, statusError(http.StatusBadRequest, "config: value is required")
	}

	var value any
	if err := json.Unmarshal([]byte(raw), &value); err == nil {
		return value, nil
	}
	return raw, nil
}

// pointerFromKey converts a dot path (subscriptions.0.no_fly_days) to an
// RFC6901 pointer (/subscriptions/0/no_fly_days). A key already written as a
// pointer is passed through unchanged.
func pointerFromKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if strings.HasPrefix(key, "/") {
		return key
	}

	var builder strings.Builder
	for _, part := range strings.Split(key, ".") {
		if part == "" {
			return ""
		}
		builder.WriteByte('/')
		builder.WriteString(escapePointerToken(part))
	}
	return builder.String()
}

func escapePointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	return strings.ReplaceAll(token, "/", "~1")
}

// parseStringList accepts a JSON string array or a comma-separated list.
func parseStringList(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}, nil
	}

	if strings.HasPrefix(raw, "[") {
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil, statusError(http.StatusBadRequest, "config: prefixes must be a JSON string array or a comma-separated list")
		}
		return list, nil
	}

	if unquoted := unquote(raw); unquoted != raw {
		if unquoted == "" {
			return []string{}, nil
		}
		return []string{unquoted}, nil
	}

	parts := strings.Split(raw, ",")
	list := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			list = append(list, part)
		}
	}
	return list, nil
}

func unquote(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		var value string
		if err := json.Unmarshal([]byte(raw), &value); err == nil {
			return value
		}
	}
	return raw
}

func indentJSON(raw []byte) ([]byte, error) {
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, raw, "", "  "); err != nil {
		return nil, err
	}
	buffer.WriteByte('\n')
	return buffer.Bytes(), nil
}

// audit records a successful local or forwarded patch. Audit failures are
// swallowed the same way router route_log failures are: a logging problem must
// not turn an applied change into an error.
func (m *Manager) audit(ctx context.Context, phone, appliedBy string, ops []sdk.Operation) {
	if m == nil || m.store == nil {
		return
	}
	raw, err := json.Marshal(ops)
	if err != nil {
		return
	}
	_ = m.store.LogPatch(ctx, store.PatchRecord{
		Phone:     phone,
		Patch:     raw,
		AppliedBy: appliedBy,
	})
}

// writeFileAtomic writes data to a temporary file in dir and renames it over
// path, so a reader never observes a partially written document.
func writeFileAtomic(dir, path string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

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
