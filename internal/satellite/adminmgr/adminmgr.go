// Package adminmgr implements Aether's built-in admin-manager satellite
// (plan §3.5, §3.6, and §3.8).
//
// Unknown phone numbers trigger a rotating approval request. The request is
// stored in the SQLite approvals table and, when notify_approvals is enabled,
// queued as an outbound message to the selected maintainer. A maintainer
// resolves it with !approve or !deny; admins manage admin.json with
// !admin add/remove.
//
// admin.json and maintainer.json are written atomically. Rotation advances
// maintainer.json's last_assigned only after the approval row has been written
// successfully.
package adminmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/queue"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

const (
	// approvalTTL is the pending-request lifetime from plan §3.5.
	approvalTTL = 7 * 24 * time.Hour
)

// phonePattern keeps a phone usable as a file/list key and prevents path
// traversal through the admin commands.
var phonePattern = regexp.MustCompile(`^\+[0-9]{1,20}$`)

// Error is a rejected admin-manager request with the HTTP status the built-in
// command should surface. T35 maps it onto the app's response.
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
	var managerErr *Error
	if errors.As(err, &managerErr) {
		return managerErr.Status
	}
	return http.StatusInternalServerError
}

func statusError(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// ApprovalStore is the subset of internal/store used by the manager.
// *store.Store implements it.
type ApprovalStore interface {
	InsertApproval(ctx context.Context, rec store.ApprovalRecord) (int64, error)
	PendingApproval(ctx context.Context, phone string) (*store.ApprovalRecord, error)
	PendingApprovals(ctx context.Context) ([]store.ApprovalRecord, error)
	ResolveApprovals(ctx context.Context, phone, status, resolvedBy string, at time.Time) (int64, error)
	ExpireApprovals(ctx context.Context, before time.Time) (int64, error)
}

// JobQueue is the outbound queue seam. *queue.Queue implements it.
type JobQueue interface {
	Enqueue(job queue.Job) error
}

// Clock is the injectable time source. testutil.FakeClock implements it.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Options configures a Manager.
type Options struct {
	// Config returns the active configuration. It is called per request so a
	// hot reload picks up changed roles and admin lists.
	Config func() *config.Config
	// Store persists approval rows. Nil makes every operation fail with 503.
	Store ApprovalStore
	// Queue receives maintainer notifications. Nil disables notification.
	Queue JobQueue
	// Clock defaults to the system clock.
	Clock Clock
}

// Manager handles approval requests, approval resolution, and admin-list
// maintenance. It is safe for concurrent use.
type Manager struct {
	config func() *config.Config
	store  ApprovalStore
	queue  JobQueue
	clock  Clock
	mu     sync.Mutex
}

// New returns an admin-manager. With no configuration source every command is
// rejected with 503.
func New(options Options) *Manager {
	clock := options.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Manager{
		config: options.Config,
		store:  options.Store,
		queue:  options.Queue,
		clock:  clock,
	}
}

// RequestApproval records one new-number request. It is idempotent while a
// request is pending: a repeat returns nil without notifying again. Expired
// pending rows are marked expired first, so a later message starts a fresh
// request.
func (m *Manager) RequestApproval(ctx context.Context, phone string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := m.requireConfig()
	if err != nil {
		return err
	}
	if m == nil || m.store == nil {
		return statusError(http.StatusServiceUnavailable, "admin: approval store is not configured")
	}

	phone = strings.TrimSpace(phone)
	if !phonePattern.MatchString(phone) {
		return statusError(http.StatusBadRequest, "admin: invalid phone %q", phone)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// A denied number stays denied and an existing user/admin/maintainer needs
	// no approval. Direct callers therefore cannot create noise for known
	// numbers.
	admin := m.loadAdmin(cfg)
	maintainerState := m.loadMaintainer(cfg)
	if hasString(admin.DeniedNumbers, phone) ||
		hasString(admin.ApprovedNumbers, phone) ||
		hasString(admin.Admins, phone) ||
		hasString(maintainerState.Maintainers, phone) {
		return nil
	}

	now := m.now()
	if _, err := m.store.ExpireApprovals(ctx, now.Add(-approvalTTL)); err != nil {
		return statusError(http.StatusInternalServerError, "admin: expire stale requests: %v", err)
	}

	pending, err := m.store.PendingApproval(ctx, phone)
	if err != nil {
		return statusError(http.StatusInternalServerError, "admin: read pending request: %v", err)
	}
	if pending != nil {
		return nil
	}

	maintainer, next := pickMaintainer(maintainerState)
	if _, err := m.store.InsertApproval(ctx, store.ApprovalRecord{
		Phone:       phone,
		Maintainer:  maintainer,
		Status:      store.ApprovalPending,
		RequestedAt: now,
	}); err != nil {
		return statusError(http.StatusInternalServerError, "admin: record request: %v", err)
	}

	// Rotation advances only after the approval row was written successfully.
	if len(maintainerState.Maintainers) > 0 {
		maintainerState.LastAssigned = next
		if err := writeConfigJSON(filepath.Join(cfg.Root, "maintainer.json"), maintainerState); err != nil {
			return statusError(http.StatusInternalServerError, "admin: advance maintainer rotation: %v", err)
		}
	}

	if maintainerState.NotifyApprovals && maintainer != "" && m.queue != nil {
		// Notification is best-effort: the pending row is the source of truth
		// and can still be listed or resolved if the queue is shutting down.
		_ = m.queue.Enqueue(queue.Job{
			Recipient: maintainer,
			Text:      approvalNotification(phone),
		})
	}
	return nil
}

// Approve moves phone to approved_numbers and resolves its pending request.
// Repeating an approval is a no-op success (plan §3.5).
func (m *Manager) Approve(ctx context.Context, actor, phone string) error {
	return m.resolveApproval(ctx, actor, phone, store.ApprovalApproved)
}

// Deny moves phone to denied_numbers and resolves its pending request.
func (m *Manager) Deny(ctx context.Context, actor, phone string) error {
	return m.resolveApproval(ctx, actor, phone, store.ApprovalDenied)
}

// Pending expires stale requests and returns the remaining pending ones,
// oldest first.
func (m *Manager) Pending(ctx context.Context) ([]store.ApprovalRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := m.requireConfig(); err != nil {
		return nil, err
	}
	if m == nil || m.store == nil {
		return nil, statusError(http.StatusServiceUnavailable, "admin: approval store is not configured")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.store.ExpireApprovals(ctx, m.now().Add(-approvalTTL)); err != nil {
		return nil, statusError(http.StatusInternalServerError, "admin: expire stale requests: %v", err)
	}
	pending, err := m.store.PendingApprovals(ctx)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, "admin: list pending requests: %v", err)
	}
	return pending, nil
}

// AddAdmin adds phone to admin.json's admins list. It also removes the number
// from denied_numbers so the new role is effective.
func (m *Manager) AddAdmin(ctx context.Context, actor, phone string) error {
	return m.mutateAdmin(ctx, actor, phone, true)
}

// RemoveAdmin removes phone from admin.json's admins list.
func (m *Manager) RemoveAdmin(ctx context.Context, actor, phone string) error {
	return m.mutateAdmin(ctx, actor, phone, false)
}

// HandleAdmin parses and executes !admin add <phone> / !admin remove <phone>.
func (m *Manager) HandleAdmin(t sdk.Transmission) error {
	action, phone, err := parseAdminCommand(t)
	if err != nil {
		return err
	}
	switch action {
	case "add":
		return m.AddAdmin(context.Background(), t.Sender, phone)
	case "remove":
		return m.RemoveAdmin(context.Background(), t.Sender, phone)
	default:
		return statusError(http.StatusBadRequest, `admin: expected "admin add|remove <phone>"`)
	}
}

func (m *Manager) resolveApproval(ctx context.Context, actor, phone, status string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := m.requireConfig()
	if err != nil {
		return err
	}
	if m == nil || m.store == nil {
		return statusError(http.StatusServiceUnavailable, "admin: approval store is not configured")
	}

	actor = strings.TrimSpace(actor)
	if auth.RoleOf(cfg, actor) < auth.Maintainer {
		return statusError(http.StatusForbidden, "admin: maintainer role required")
	}

	phone = strings.TrimSpace(phone)
	if !phonePattern.MatchString(phone) {
		return statusError(http.StatusBadRequest, "admin: invalid phone %q", phone)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	admin := m.loadAdmin(cfg)
	var changed bool
	switch status {
	case store.ApprovalApproved:
		admin.DeniedNumbers, changed = removeString(admin.DeniedNumbers, phone)
		if !hasString(admin.ApprovedNumbers, phone) {
			admin.ApprovedNumbers = append(admin.ApprovedNumbers, phone)
			changed = true
		}
	case store.ApprovalDenied:
		admin.ApprovedNumbers, changed = removeString(admin.ApprovedNumbers, phone)
		if !hasString(admin.DeniedNumbers, phone) {
			admin.DeniedNumbers = append(admin.DeniedNumbers, phone)
			changed = true
		}
	default:
		return statusError(http.StatusInternalServerError, "admin: unsupported approval status %q", status)
	}

	if changed {
		if err := writeConfigJSON(filepath.Join(cfg.Root, "admin.json"), admin); err != nil {
			return statusError(http.StatusInternalServerError, "admin: write admin.json: %v", err)
		}
	}

	if _, err := m.store.ResolveApprovals(ctx, phone, status, actor, m.now()); err != nil {
		return statusError(http.StatusInternalServerError, "admin: resolve request: %v", err)
	}
	return nil
}

func (m *Manager) mutateAdmin(ctx context.Context, actor, phone string, add bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := m.requireConfig()
	if err != nil {
		return err
	}

	actor = strings.TrimSpace(actor)
	if auth.RoleOf(cfg, actor) < auth.Admin {
		return statusError(http.StatusForbidden, "admin: admin role required")
	}

	phone = strings.TrimSpace(phone)
	if !phonePattern.MatchString(phone) {
		return statusError(http.StatusBadRequest, "admin: invalid phone %q", phone)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	admin := m.loadAdmin(cfg)
	var changed bool
	if add {
		admin.DeniedNumbers, changed = removeString(admin.DeniedNumbers, phone)
		if !hasString(admin.Admins, phone) {
			admin.Admins = append(admin.Admins, phone)
			changed = true
		}
	} else {
		admin.Admins, changed = removeString(admin.Admins, phone)
	}

	if changed {
		if err := writeConfigJSON(filepath.Join(cfg.Root, "admin.json"), admin); err != nil {
			return statusError(http.StatusInternalServerError, "admin: write admin.json: %v", err)
		}
	}
	return nil
}

func (m *Manager) requireConfig() (*config.Config, error) {
	if m == nil || m.config == nil {
		return nil, statusError(http.StatusServiceUnavailable, "admin: configuration is not loaded")
	}
	cfg := m.config()
	if cfg == nil || strings.TrimSpace(cfg.Root) == "" {
		return nil, statusError(http.StatusServiceUnavailable, "admin: configuration is not loaded")
	}
	return cfg, nil
}

func (m *Manager) now() time.Time {
	if m != nil && m.clock != nil {
		return m.clock.Now()
	}
	return time.Now()
}

func (m *Manager) loadAdmin(cfg *config.Config) config.AdminConfig {
	path := filepath.Join(cfg.Root, "admin.json")
	var admin config.AdminConfig
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &admin); err == nil {
			return admin
		}
	}
	return cfg.Admin
}

func (m *Manager) loadMaintainer(cfg *config.Config) config.MaintainerConfig {
	path := filepath.Join(cfg.Root, "maintainer.json")
	var state config.MaintainerConfig
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &state); err == nil {
			return state
		}
	}
	return cfg.Maintainer
}

// pickMaintainer returns maintainers[last_assigned % len] and the next counter
// value. A negative counter is normalized rather than wrapping oddly.
func pickMaintainer(state config.MaintainerConfig) (string, int) {
	if len(state.Maintainers) == 0 {
		return "", state.LastAssigned
	}
	last := state.LastAssigned
	if last < 0 {
		last = 0
	}
	return state.Maintainers[last%len(state.Maintainers)], last + 1
}

func approvalNotification(phone string) string {
	return fmt.Sprintf(
		`New Aether access request from %s. Reply "!approve %s" or "!deny %s".`,
		phone, phone, phone,
	)
}

func parseAdminCommand(t sdk.Transmission) (string, string, error) {
	text := strings.TrimSpace(t.Text)
	if text != "" {
		text = stripCommandPrefix(text)
		fields := strings.Fields(text)
		if len(fields) > 0 && (fields[0] == "admin" || (t.Trigger != "" && fields[0] == t.Trigger)) {
			fields = fields[1:]
		}
		if len(fields) != 2 {
			return "", "", statusError(http.StatusBadRequest, `admin: expected "admin add|remove <phone>"`)
		}
		action := strings.ToLower(fields[0])
		if action != "add" && action != "remove" {
			return "", "", statusError(http.StatusBadRequest, `admin: expected "admin add|remove <phone>"`)
		}
		return action, fields[1], nil
	}

	action := strings.ToLower(argString(t.Args, "action"))
	phone := strings.TrimSpace(argString(t.Args, "phone"))
	if (action != "add" && action != "remove") || phone == "" {
		return "", "", statusError(http.StatusBadRequest, `admin: expected "admin add|remove <phone>"`)
	}
	return action, phone, nil
}

func stripCommandPrefix(text string) string {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"!", "/", "Aether ", "aether "} {
		if strings.HasPrefix(text, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(text, prefix))
		}
	}
	return text
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

func hasString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// removeString returns list without value and reports whether anything changed.
func removeString(list []string, value string) ([]string, bool) {
	changed := false
	out := list[:0]
	for _, item := range list {
		if item == value {
			changed = true
			continue
		}
		out = append(out, item)
	}
	return out, changed
}

func writeConfigJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeFileAtomic(path, raw)
}

// writeFileAtomic writes data to a temporary file beside path and renames it
// into place, so a reader never observes a partially written document.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
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
