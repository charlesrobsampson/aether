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
	"strconv"
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
	ResolveApprovalsByIDs(ctx context.Context, ids []int64, status, resolvedBy string, at time.Time) (int64, error)
	ExpireApprovals(ctx context.Context, before time.Time) (int64, error)
}

// JobQueue is the outbound queue seam. *queue.Queue implements it.
type JobQueue interface {
	Enqueue(job queue.Job) error
}

// Sender is the outbound seam used to answer read-only admin commands. The
// app's queue sender satisfies it; tests inject a recorder.
type Sender interface {
	Send(ctx context.Context, resp sdk.Response) error
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
	// Out replies with read-only command output such as !pending. Nil makes
	// !pending fail with 503.
	Out Sender
	// Clock defaults to the system clock.
	Clock Clock
}

// Manager handles approval requests, approval resolution, and admin-list
// maintenance. It is safe for concurrent use.
type Manager struct {
	config func() *config.Config
	store  ApprovalStore
	queue  JobQueue
	out    Sender
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
		out:    options.Out,
		clock:  clock,
	}
}

// RequestApproval records one new-sender request without a display name.
func (m *Manager) RequestApproval(ctx context.Context, identity string) error {
	return m.RequestApprovalFrom(ctx, identity, "")
}

// RequestApprovalFrom records one new-sender request. Signal sometimes hides a
// sender's phone number, so identity may be either a phone number or a UUID.
// displayName is optional and only enriches the maintainer notification.
//
// It is idempotent while a request is pending: a repeat returns nil without
// notifying again. Expired pending rows are marked expired first, so a later
// message starts a fresh request.
func (m *Manager) RequestApprovalFrom(ctx context.Context, identity, displayName string) error {
	return m.requestApproval(ctx, identity, store.ApprovalKindNumber, displayName, "")
}

// RequestGroupApproval records one unapproved-group request. groupName and
// requester only enrich the maintainer notification; groupID is the identity
// that an approval writes to approved_groups. It is idempotent while a request
// is pending.
func (m *Manager) RequestGroupApproval(ctx context.Context, groupID, groupName, requester string) error {
	return m.requestApproval(ctx, groupID, store.ApprovalKindGroup, groupName, requester)
}

func (m *Manager) requestApproval(ctx context.Context, identity, kind, displayName, requester string) error {
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

	identity = strings.TrimSpace(identity)
	switch kind {
	case store.ApprovalKindNumber:
		if !auth.ValidIdentity(identity) {
			return statusError(http.StatusBadRequest, "admin: invalid sender %q", identity)
		}
	case store.ApprovalKindGroup:
		if identity == "" {
			return statusError(http.StatusBadRequest, "admin: invalid group %q", identity)
		}
	default:
		return statusError(http.StatusInternalServerError, "admin: unsupported approval kind %q", kind)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// A denied identity stays denied and an existing user/admin/maintainer or
	// approved group needs no approval. Direct callers therefore cannot create
	// noise for known identities.
	admin := m.loadAdmin(cfg)
	maintainerState := m.loadMaintainer(cfg)
	switch kind {
	case store.ApprovalKindNumber:
		if hasString(admin.DeniedNumbers, identity) ||
			hasString(admin.ApprovedNumbers, identity) ||
			hasString(admin.Admins, identity) ||
			hasString(maintainerState.Maintainers, identity) {
			return nil
		}
	case store.ApprovalKindGroup:
		if hasString(admin.DeniedGroups, identity) || hasString(admin.ApprovedGroups, identity) {
			return nil
		}
	}

	now := m.now()
	if _, err := m.store.ExpireApprovals(ctx, now.Add(-approvalTTL)); err != nil {
		return statusError(http.StatusInternalServerError, "admin: expire stale requests: %v", err)
	}

	pending, err := m.store.PendingApproval(ctx, identity)
	if err != nil {
		return statusError(http.StatusInternalServerError, "admin: read pending request: %v", err)
	}
	if pending != nil {
		return nil
	}

	maintainer, next := pickMaintainer(maintainerState)
	displayName = strings.TrimSpace(displayName)
	id, err := m.store.InsertApproval(ctx, store.ApprovalRecord{
		Phone:       identity,
		Kind:        kind,
		DisplayName: displayName,
		Maintainer:  maintainer,
		Status:      store.ApprovalPending,
		RequestedAt: now,
	})
	if err != nil {
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
		text := approvalNotification(id, identity, displayName)
		if kind == store.ApprovalKindGroup {
			text = groupApprovalNotification(id, identity, displayName, requester)
		}
		_ = m.queue.Enqueue(queue.Job{
			Recipient: maintainer,
			Text:      text,
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

// SuccessReactionFor keeps read-only approval listings from being acknowledged
// with a reaction while approve/deny/add/remove still are.
func SuccessReactionFor(text string) string {
	fields := strings.Fields(stripCommandPrefix(text))
	if len(fields) == 0 {
		return "👍"
	}
	if strings.EqualFold(fields[0], "pending") {
		return ""
	}
	if strings.EqualFold(fields[0], "admin") && len(fields) > 1 {
		switch strings.ToLower(fields[1]) {
		case "list", "pending":
			return ""
		}
	}
	return "👍"
}

// HandleAdmin parses and executes !admin approve|deny|list|add|remove.
func (m *Manager) HandleAdmin(t sdk.Transmission) error {
	action, identity, err := parseAdminCommand(t)
	if err != nil {
		return err
	}
	switch action {
	case "approve":
		return m.resolveApprovalRefs(context.Background(), t.Sender, identity, store.ApprovalApproved, true)
	case "deny":
		return m.resolveApprovalRefs(context.Background(), t.Sender, identity, store.ApprovalDenied, true)
	case "list":
		return m.HandlePending(t)
	case "add":
		return m.AddAdmin(context.Background(), t.Sender, identity)
	case "remove":
		return m.RemoveAdmin(context.Background(), t.Sender, identity)
	default:
		return statusError(http.StatusBadRequest, `admin: expected "admin approve|deny|list|add|remove [id[,id...]|phone|uuid]"`)
	}
}

// HandleApprove implements !approve [id[,id...]|phone|uuid]. With no target it
// resolves the newest pending request, which is normally the one that was just
// notified.
func (m *Manager) HandleApprove(t sdk.Transmission) error {
	target, err := parseTargetCommand(t, "approve")
	if err != nil {
		return err
	}
	return m.resolveApprovalRefs(context.Background(), t.Sender, target, store.ApprovalApproved, true)
}

// HandleDeny implements !deny [id[,id...]|phone|uuid].
func (m *Manager) HandleDeny(t sdk.Transmission) error {
	target, err := parseTargetCommand(t, "deny")
	if err != nil {
		return err
	}
	return m.resolveApprovalRefs(context.Background(), t.Sender, target, store.ApprovalDenied, true)
}

// HandlePending implements !pending and !admin list.
func (m *Manager) HandlePending(t sdk.Transmission) error {
	cfg, err := m.requireConfig()
	if err != nil {
		return err
	}
	if auth.RoleOf(cfg, strings.TrimSpace(t.Sender)) < auth.Maintainer {
		return statusError(http.StatusForbidden, "admin: maintainer role required")
	}
	pending, err := m.Pending(context.Background())
	if err != nil {
		return err
	}
	return m.send(t, renderPending(pending))
}

func (m *Manager) resolveApproval(ctx context.Context, actor, phone, status string) error {
	if strings.TrimSpace(phone) == "" {
		return statusError(http.StatusBadRequest, "admin: invalid sender %q", phone)
	}
	return m.resolveApprovalRefs(ctx, actor, phone, status, false)
}

// resolveApprovalRefs resolves one or more targets. A target is a pending
// approval row id, a comma/space separated list of row ids, or a phone/UUID.
// allowEmpty makes a bare command resolve the newest pending request.
func (m *Manager) resolveApprovalRefs(ctx context.Context, actor, raw, status string, allowEmpty bool) error {
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

	refs, err := parseApprovalRefs(raw)
	if err != nil {
		return err
	}
	if len(refs) == 0 && !allowEmpty {
		return statusError(http.StatusBadRequest, "admin: approval id, phone, or uuid is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if _, err := m.store.ExpireApprovals(ctx, now.Add(-approvalTTL)); err != nil {
		return statusError(http.StatusInternalServerError, "admin: expire stale requests: %v", err)
	}
	pending, err := m.store.PendingApprovals(ctx)
	if err != nil {
		return statusError(http.StatusInternalServerError, "admin: list pending requests: %v", err)
	}

	if len(refs) == 0 {
		if len(pending) == 0 {
			return statusError(http.StatusBadRequest, "admin: no pending access requests")
		}
		// PendingApprovals is oldest first, so the last row is the newest
		// request and normally the one whose notification prompted this reply.
		refs = []approvalRef{{id: pending[len(pending)-1].ID}}
	}

	byID := make(map[int64]store.ApprovalRecord, len(pending))
	kindByIdentity := make(map[string]string, len(pending))
	for _, rec := range pending {
		byID[rec.ID] = rec
		kindByIdentity[rec.Phone] = rec.Kind
	}

	var (
		targets []approvalTarget
		ids     []int64
		missing []string
	)
	for _, ref := range refs {
		if ref.id == 0 {
			kind := kindByIdentity[ref.identity]
			if kind == "" {
				kind = inferApprovalKind(ref.identity)
			}
			targets = append(targets, approvalTarget{identity: ref.identity, kind: kind})
			continue
		}
		rec, ok := byID[ref.id]
		if !ok {
			missing = append(missing, fmt.Sprintf("#%d", ref.id))
			continue
		}
		kind := rec.Kind
		if kind == "" {
			kind = store.ApprovalKindNumber
		}
		ids = append(ids, ref.id)
		targets = append(targets, approvalTarget{identity: rec.Phone, kind: kind})
	}
	if len(missing) > 0 {
		return statusError(http.StatusBadRequest, "admin: no pending request %s", strings.Join(missing, ", "))
	}

	admin := m.loadAdmin(cfg)
	var changed bool
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		key := target.kind + "\x00" + target.identity
		if seen[key] {
			continue
		}
		seen[key] = true
		identity := target.identity
		switch target.kind {
		case store.ApprovalKindGroup:
			switch status {
			case store.ApprovalApproved:
				admin.DeniedGroups, changed = removeStringChanged(admin.DeniedGroups, identity, changed)
				if !hasString(admin.ApprovedGroups, identity) {
					admin.ApprovedGroups = append(admin.ApprovedGroups, identity)
					changed = true
				}
			case store.ApprovalDenied:
				admin.ApprovedGroups, changed = removeStringChanged(admin.ApprovedGroups, identity, changed)
				if !hasString(admin.DeniedGroups, identity) {
					admin.DeniedGroups = append(admin.DeniedGroups, identity)
					changed = true
				}
			default:
				return statusError(http.StatusInternalServerError, "admin: unsupported approval status %q", status)
			}
		case store.ApprovalKindNumber:
			switch status {
			case store.ApprovalApproved:
				admin.DeniedNumbers, changed = removeStringChanged(admin.DeniedNumbers, identity, changed)
				if !hasString(admin.ApprovedNumbers, identity) {
					admin.ApprovedNumbers = append(admin.ApprovedNumbers, identity)
					changed = true
				}
			case store.ApprovalDenied:
				admin.ApprovedNumbers, changed = removeStringChanged(admin.ApprovedNumbers, identity, changed)
				if !hasString(admin.DeniedNumbers, identity) {
					admin.DeniedNumbers = append(admin.DeniedNumbers, identity)
					changed = true
				}
			default:
				return statusError(http.StatusInternalServerError, "admin: unsupported approval status %q", status)
			}
		default:
			return statusError(http.StatusInternalServerError, "admin: unsupported approval kind %q", target.kind)
		}
	}
	if changed {
		if err := writeConfigJSON(filepath.Join(cfg.Root, "admin.json"), admin); err != nil {
			return statusError(http.StatusInternalServerError, "admin: write admin.json: %v", err)
		}
	}

	if len(ids) > 0 {
		if _, err := m.store.ResolveApprovalsByIDs(ctx, ids, status, actor, now); err != nil {
			return statusError(http.StatusInternalServerError, "admin: resolve requests: %v", err)
		}
	}
	// A phone/UUID/group target may refer to a request that was not in the
	// pending set (for example an old row already resolved). Resolving it is a
	// no-op, which keeps repeated approvals idempotent.
	for _, target := range targets {
		if _, err := m.store.ResolveApprovals(ctx, target.identity, status, actor, now); err != nil {
			return statusError(http.StatusInternalServerError, "admin: resolve request: %v", err)
		}
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
	if !auth.ValidIdentity(phone) {
		return statusError(http.StatusBadRequest, "admin: invalid sender %q", phone)
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

func approvalNotification(id int64, identity, displayName string) string {
	who := identity
	if name := strings.TrimSpace(displayName); name != "" && name != identity {
		who = fmt.Sprintf("%s (%s)", name, identity)
	}
	return fmt.Sprintf(
		`New Aether access request #%d from %s. Reply "!approve %d" or "!deny %d".`,
		id, who, id, id,
	)
}

func groupApprovalNotification(id int64, groupID, groupName, requester string) string {
	who := groupID
	if name := strings.TrimSpace(groupName); name != "" && name != groupID {
		who = fmt.Sprintf("%s (%s)", name, groupID)
	}
	from := ""
	if requester = strings.TrimSpace(requester); requester != "" {
		from = fmt.Sprintf(" for a message from %s", requester)
	}
	return fmt.Sprintf(
		`New Aether group access request #%d%s: %s. Reply "!approve %d" or "!deny %d".`,
		id, from, who, id, id,
	)
}

// renderPending formats the pending queue as text. Identities may be phone
// numbers or Signal UUIDs; row IDs are short references accepted by !approve
// and !deny, including as a comma-separated batch.
func renderPending(records []store.ApprovalRecord) string {
	if len(records) == 0 {
		return "No pending access requests."
	}
	var b strings.Builder
	b.WriteString("Pending access requests:\n")
	for _, rec := range records {
		fmt.Fprintf(&b, "  #%d %s", rec.ID, approvalLabel(rec))
		if rec.Kind == store.ApprovalKindGroup {
			b.WriteString(" [group]")
		}
		if rec.Maintainer != "" {
			fmt.Fprintf(&b, " (assigned to %s)", rec.Maintainer)
		}
		if !rec.RequestedAt.IsZero() {
			fmt.Fprintf(&b, " — %s", rec.RequestedAt.UTC().Format("2006-01-02 15:04 UTC"))
		}
		b.WriteString("\n")
	}
	b.WriteString(`Reply "!approve <id>", "!approve 1,3,4", or "!deny <id>".`)
	return b.String()
}

func approvalLabel(rec store.ApprovalRecord) string {
	name := strings.TrimSpace(rec.DisplayName)
	switch {
	case name != "" && name != rec.Phone:
		return fmt.Sprintf("%s (%s)", name, rec.Phone)
	case name != "":
		return name
	default:
		return rec.Phone
	}
}

// send emits one read-only admin answer to the originating sender.
func (m *Manager) send(t sdk.Transmission, text string) error {
	if m == nil || m.out == nil {
		return statusError(http.StatusServiceUnavailable, "admin: outbound client is not configured")
	}
	recipient := t.ReplyTo()
	if recipient == "" {
		return statusError(http.StatusUnauthorized, "admin: sender is required")
	}
	satellite := strings.TrimSpace(t.Satellite)
	if satellite == "" {
		satellite = "admin"
	}
	return m.out.Send(context.Background(), sdk.Response{
		Recipients: []string{recipient},
		Text:       text,
		Satellite:  satellite,
		ThreadID:   t.ThreadID,
	})
}

func parseAdminCommand(t sdk.Transmission) (string, string, error) {
	const usage = `admin: expected "admin approve|deny|list|add|remove [id[,id...]|phone|uuid]"`

	text := strings.TrimSpace(t.Text)
	if text != "" {
		text = stripCommandPrefix(text)
		fields := strings.Fields(text)
		if len(fields) > 0 && (strings.EqualFold(fields[0], "admin") || (t.Trigger != "" && strings.EqualFold(fields[0], t.Trigger))) {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			return "", "", statusError(http.StatusBadRequest, usage)
		}

		action := strings.ToLower(fields[0])
		target := strings.Join(fields[1:], " ")
		switch action {
		case "list":
			if target != "" {
				return "", "", statusError(http.StatusBadRequest, usage)
			}
			return "list", "", nil
		case "approve", "deny":
			// An empty target resolves the newest pending request.
			return action, target, nil
		case "add", "remove":
			if target == "" {
				return "", "", statusError(http.StatusBadRequest, usage)
			}
			return action, target, nil
		default:
			return "", "", statusError(http.StatusBadRequest, usage)
		}
	}

	action := strings.ToLower(argString(t.Args, "action"))
	target := strings.TrimSpace(argString(t.Args, "ids"))
	if target == "" {
		target = strings.TrimSpace(argString(t.Args, "id"))
	}
	if target == "" {
		target = strings.TrimSpace(argString(t.Args, "phone"))
	}
	switch action {
	case "list":
		return "list", "", nil
	case "approve", "deny":
		return action, target, nil
	case "add", "remove":
		if target == "" {
			return "", "", statusError(http.StatusBadRequest, usage)
		}
		return action, target, nil
	default:
		return "", "", statusError(http.StatusBadRequest, usage)
	}
}

// parseTargetCommand reads "[id[,id...]|phone|uuid]" for the standalone
// !approve and !deny built-ins. An empty result is a bare command.
func parseTargetCommand(t sdk.Transmission, name string) (string, error) {
	if text := strings.TrimSpace(t.Text); text != "" {
		fields := strings.Fields(stripCommandPrefix(text))
		if len(fields) > 0 && (strings.EqualFold(fields[0], name) || (t.Trigger != "" && strings.EqualFold(fields[0], t.Trigger))) {
			fields = fields[1:]
		}
		return strings.Join(fields, " "), nil
	}

	target := strings.TrimSpace(argString(t.Args, "ids"))
	if target == "" {
		target = strings.TrimSpace(argString(t.Args, "id"))
	}
	if target == "" {
		target = strings.TrimSpace(argString(t.Args, "phone"))
	}
	return target, nil
}

// approvalRef is either a pending-row ID or a phone/UUID/group identity.
type approvalRef struct {
	id       int64
	identity string
}

// approvalTarget pairs an identity with the list it belongs to. Row IDs carry
// the kind from the stored record; raw identities are looked up in the pending
// set first and otherwise inferred from the Signal group prefix.
type approvalTarget struct {
	identity string
	kind     string
}

func inferApprovalKind(identity string) string {
	if strings.HasPrefix(strings.TrimSpace(identity), "group.") {
		return store.ApprovalKindGroup
	}
	return store.ApprovalKindNumber
}

// parseApprovalRefs accepts "1", "1,3,4", "1 3 4", "+15551234567", or a
// Signal UUID. Commas and whitespace are interchangeable separators.
func parseApprovalRefs(raw string) ([]approvalRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	fields := strings.Fields(strings.ReplaceAll(raw, ",", " "))
	refs := make([]approvalRef, 0, len(fields))
	for _, field := range fields {
		if field == "" {
			continue
		}
		if strings.HasPrefix(field, "#") {
			field = strings.TrimPrefix(field, "#")
		}
		if isDigits(field) {
			id, err := strconv.ParseInt(field, 10, 64)
			if err != nil || id <= 0 {
				return nil, statusError(http.StatusBadRequest, "admin: invalid approval id %q", field)
			}
			refs = append(refs, approvalRef{id: id})
			continue
		}
		if !auth.ValidIdentity(field) && !strings.HasPrefix(field, "group.") {
			return nil, statusError(http.StatusBadRequest, "admin: invalid approval target %q", field)
		}
		refs = append(refs, approvalRef{identity: field})
	}
	return refs, nil
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func stripCommandPrefix(text string) string {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"!", "/", "Aether "} {
		if len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix) {
			return strings.TrimSpace(text[len(prefix):])
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

// removeStringChanged is removeString with an accumulated changed flag, which
// keeps multi-target resolution readable.
func removeStringChanged(list []string, value string, changed bool) ([]string, bool) {
	out, removed := removeString(list, value)
	return out, changed || removed
}

// dedupeStrings preserves order while dropping repeats.
func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
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
