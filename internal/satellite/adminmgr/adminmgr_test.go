package adminmgr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/queue"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/internal/testutil"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

const (
	adminPhone       = "+15555550101"
	userPhone        = "+15555550102"
	maintainerPhone1 = "+15555550110"
	maintainerPhone2 = "+15555550111"
	maintainerPhone3 = "+15555550112"
	deniedPhone      = "+15555550999"

	unknownPhone    = "+15555550150"
	unknownPhone2   = "+15555550151"
	unknownPhone3   = "+15555550152"
	unknownPhone4   = "+15555550153"
	unknownPhone5   = "+15555550154"
	newAdminPhone   = "+15555550160"
	otherAdminPhone = "+15555550161"
)

var testNow = time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

// fakeQueue records every outbound job so notification assertions never need a
// real messenger or a sleeping worker.
type fakeQueue struct {
	mu   sync.Mutex
	jobs []queue.Job
}

func (q *fakeQueue) Enqueue(job queue.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, job)
	return nil
}

func (q *fakeQueue) Jobs() []queue.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]queue.Job, len(q.jobs))
	copy(out, q.jobs)
	return out
}

// fakeSender records read-only replies so !pending can be asserted without a
// real messenger.
type fakeSender struct {
	mu        sync.Mutex
	responses []sdk.Response
}

func (s *fakeSender) Send(_ context.Context, resp sdk.Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, resp)
	return nil
}

func (s *fakeSender) Responses() []sdk.Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sdk.Response, len(s.responses))
	copy(out, s.responses)
	return out
}

type fixture struct {
	root    string
	cfg     *config.Config
	store   *store.Store
	clock   *testutil.FakeClock
	queue   *fakeQueue
	out     *fakeSender
	manager *Manager
	config  func() *config.Config
}

// newFixture builds a hermetic config tree and SQLite database. The config
// getter rereads the tree on every call, which mirrors hot reload and keeps
// role checks honest after a file mutation.
func newFixture(t *testing.T, maintainers []string, notify bool) *fixture {
	t.Helper()

	root := t.TempDir()

	testutil.WriteJSON(t, filepath.Join(root, "system.json"), config.SystemConfig{
		Auth: config.AuthConfig{JWTSecret: "adminmgr-test-secret"},
	})

	approved := []string{adminPhone, userPhone}
	approved = append(approved, maintainers...)
	testutil.WriteJSON(t, filepath.Join(root, "admin.json"), config.AdminConfig{
		Admins:          []string{adminPhone},
		ApprovedNumbers: approved,
		DeniedNumbers:   []string{deniedPhone},
	})
	testutil.WriteJSON(t, filepath.Join(root, "maintainer.json"), config.MaintainerConfig{
		Maintainers:     maintainers,
		NotifyApprovals: notify,
	})

	cfg, err := config.LoadConfig(root)
	if err != nil {
		t.Fatalf("config.LoadConfig(%s): %v", root, err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open(:memory:): %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	clock := testutil.NewFakeClock(testNow)
	q := &fakeQueue{}
	out := &fakeSender{}

	configFn := func() *config.Config {
		loaded, err := config.LoadConfig(root)
		if err != nil {
			return cfg
		}
		return loaded
	}

	manager := New(Options{
		Config: configFn,
		Store:  st,
		Queue:  q,
		Out:    out,
		Clock:  clock,
	})

	return &fixture{
		root:    root,
		cfg:     cfg,
		store:   st,
		clock:   clock,
		queue:   q,
		out:     out,
		manager: manager,
		config:  configFn,
	}
}

func readJSONFile[T any](t *testing.T, path string, v *T) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func (f *fixture) admin(t *testing.T) config.AdminConfig {
	t.Helper()

	var admin config.AdminConfig
	readJSONFile(t, filepath.Join(f.root, "admin.json"), &admin)
	return admin
}

func (f *fixture) maintainer(t *testing.T) config.MaintainerConfig {
	t.Helper()

	var maintainer config.MaintainerConfig
	readJSONFile(t, filepath.Join(f.root, "maintainer.json"), &maintainer)
	return maintainer
}

func (f *fixture) approvalsFor(t *testing.T, phone string) []store.ApprovalRecord {
	t.Helper()

	rows, err := f.store.DB().QueryContext(context.Background(), `
		SELECT id, phone, maintainer, status, requested_at,
		       COALESCE(resolved_at, ''), COALESCE(resolved_by, '')
		FROM approvals
		WHERE phone = ?
		ORDER BY id`, phone)
	if err != nil {
		t.Fatalf("query approvals for %s: %v", phone, err)
	}
	defer rows.Close()

	var out []store.ApprovalRecord
	for rows.Next() {
		var rec store.ApprovalRecord
		var requestedAt, resolvedAt string
		if err := rows.Scan(&rec.ID, &rec.Phone, &rec.Maintainer, &rec.Status, &requestedAt, &resolvedAt, &rec.ResolvedBy); err != nil {
			t.Fatalf("scan approval row: %v", err)
		}
		rec.RequestedAt, err = time.Parse(time.RFC3339, requestedAt)
		if err != nil {
			t.Fatalf("parse requested_at %q: %v", requestedAt, err)
		}
		if resolvedAt != "" {
			rec.ResolvedAt, err = time.Parse(time.RFC3339, resolvedAt)
			if err != nil {
				t.Fatalf("parse resolved_at %q: %v", resolvedAt, err)
			}
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate approvals: %v", err)
	}
	return out
}

func (f *fixture) maintainerAssignments(t *testing.T) []string {
	t.Helper()

	rows, err := f.store.DB().QueryContext(context.Background(),
		`SELECT maintainer FROM approvals ORDER BY id`)
	if err != nil {
		t.Fatalf("query maintainer assignments: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var maintainer string
		if err := rows.Scan(&maintainer); err != nil {
			t.Fatalf("scan maintainer: %v", err)
		}
		out = append(out, maintainer)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate maintainer assignments: %v", err)
	}
	return out
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func countOf(list []string, value string) int {
	count := 0
	for _, item := range list {
		if item == value {
			count++
		}
	}
	return count
}

func requireStatus(t *testing.T, err error, want int) {
	t.Helper()

	if want == 0 {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}

	var managerErr *Error
	if !errors.As(err, &managerErr) {
		t.Fatalf("error %v is not *adminmgr.Error", err)
	}
	if managerErr.Status != want {
		t.Fatalf("error status = %d, want %d (error: %v)", managerErr.Status, want, err)
	}
}

// TestApprovalFlow covers request assignment, the approval write, idempotency,
// notification, and denial.
func TestApprovalFlow(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []string{maintainerPhone1}, true)

	if err := f.manager.RequestApproval(ctx, unknownPhone); err != nil {
		t.Fatalf("RequestApproval(%s): %v", unknownPhone, err)
	}

	pending, err := f.store.PendingApproval(ctx, unknownPhone)
	if err != nil {
		t.Fatalf("PendingApproval(%s): %v", unknownPhone, err)
	}
	if pending == nil {
		t.Fatalf("no pending approval for %s", unknownPhone)
	}
	if pending.Maintainer != maintainerPhone1 {
		t.Fatalf("maintainer = %q, want %q", pending.Maintainer, maintainerPhone1)
	}
	if pending.Status != store.ApprovalPending {
		t.Fatalf("status = %q, want %q", pending.Status, store.ApprovalPending)
	}

	jobs := f.queue.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("notification jobs = %d, want 1", len(jobs))
	}
	if jobs[0].Recipient != maintainerPhone1 {
		t.Fatalf("notification recipient = %q, want %q", jobs[0].Recipient, maintainerPhone1)
	}
	if !strings.Contains(jobs[0].Text, unknownPhone) || !strings.Contains(jobs[0].Text, "!approve") {
		t.Fatalf("notification text %q does not name the phone and approve command", jobs[0].Text)
	}

	if err := f.manager.Approve(ctx, maintainerPhone1, unknownPhone); err != nil {
		t.Fatalf("Approve(%s): %v", unknownPhone, err)
	}

	admin := f.admin(t)
	if !contains(admin.ApprovedNumbers, unknownPhone) {
		t.Fatalf("approved_numbers = %v, want %s", admin.ApprovedNumbers, unknownPhone)
	}
	if contains(admin.DeniedNumbers, unknownPhone) {
		t.Fatalf("denied_numbers = %v must not contain approved %s", admin.DeniedNumbers, unknownPhone)
	}

	rows := f.approvalsFor(t, unknownPhone)
	if len(rows) != 1 {
		t.Fatalf("approval rows = %d, want 1", len(rows))
	}
	if rows[0].Status != store.ApprovalApproved {
		t.Fatalf("status = %q, want %q", rows[0].Status, store.ApprovalApproved)
	}
	if rows[0].ResolvedBy != maintainerPhone1 {
		t.Fatalf("resolved_by = %q, want %q", rows[0].ResolvedBy, maintainerPhone1)
	}
	if rows[0].ResolvedAt.IsZero() {
		t.Fatal("resolved_at is zero")
	}

	// Approving twice is a no-op success and must not duplicate the number.
	if err := f.manager.Approve(ctx, maintainerPhone1, unknownPhone); err != nil {
		t.Fatalf("second Approve(%s): %v", unknownPhone, err)
	}
	admin = f.admin(t)
	if got := countOf(admin.ApprovedNumbers, unknownPhone); got != 1 {
		t.Fatalf("approved_numbers contains %s %d times, want 1", unknownPhone, got)
	}

	t.Run("deny", func(t *testing.T) {
		if err := f.manager.RequestApproval(ctx, unknownPhone2); err != nil {
			t.Fatalf("RequestApproval(%s): %v", unknownPhone2, err)
		}
		if err := f.manager.Deny(ctx, maintainerPhone1, unknownPhone2); err != nil {
			t.Fatalf("Deny(%s): %v", unknownPhone2, err)
		}

		admin := f.admin(t)
		if !contains(admin.DeniedNumbers, unknownPhone2) {
			t.Fatalf("denied_numbers = %v, want %s", admin.DeniedNumbers, unknownPhone2)
		}
		if contains(admin.ApprovedNumbers, unknownPhone2) {
			t.Fatalf("approved_numbers = %v must not contain denied %s", admin.ApprovedNumbers, unknownPhone2)
		}

		rows := f.approvalsFor(t, unknownPhone2)
		if len(rows) != 1 || rows[0].Status != store.ApprovalDenied {
			t.Fatalf("approval rows = %+v, want one denied row", rows)
		}
	})
}

// TestApprovalRotation covers maintainers[last_assigned % len] rotation and the
// rule that last_assigned advances only after the approval write succeeds.
func TestApprovalRotation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []string{maintainerPhone1, maintainerPhone2, maintainerPhone3}, false)

	for _, phone := range []string{unknownPhone, unknownPhone2, unknownPhone3, unknownPhone4} {
		if err := f.manager.RequestApproval(ctx, phone); err != nil {
			t.Fatalf("RequestApproval(%s): %v", phone, err)
		}
	}

	got := f.maintainerAssignments(t)
	want := []string{maintainerPhone1, maintainerPhone2, maintainerPhone3, maintainerPhone1}
	if len(got) != len(want) {
		t.Fatalf("assignments = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("assignments = %v, want %v", got, want)
		}
	}

	if got := f.maintainer(t).LastAssigned; got != 4 {
		t.Fatalf("last_assigned = %d, want 4", got)
	}

	t.Run("failed write does not advance rotation", func(t *testing.T) {
		f := newFixture(t, []string{maintainerPhone1, maintainerPhone2}, true)
		f.manager = New(Options{
			Config: f.config,
			Store:  failingInsertStore{Store: f.store},
			Queue:  f.queue,
			Clock:  f.clock,
		})

		if err := f.manager.RequestApproval(ctx, unknownPhone5); err == nil {
			t.Fatal("RequestApproval succeeded with a failing store")
		}
		if got := f.maintainer(t).LastAssigned; got != 0 {
			t.Fatalf("last_assigned = %d after a failed write, want 0", got)
		}
	})
}

// failingInsertStore fails approval inserts while delegating reads to the real
// store, so the rotation-write ordering can be asserted.
type failingInsertStore struct {
	*store.Store
}

func (failingInsertStore) InsertApproval(context.Context, store.ApprovalRecord) (int64, error) {
	return 0, errors.New("insert failed")
}

// TestApprovalExpiry covers the seven-day pending TTL and the fact that a later
// message starts a fresh request while the expired row remains as history.
func TestApprovalExpiry(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []string{maintainerPhone1}, false)

	if err := f.manager.RequestApproval(ctx, unknownPhone); err != nil {
		t.Fatalf("RequestApproval(%s): %v", unknownPhone, err)
	}

	f.clock.Advance(7*24*time.Hour + time.Second)

	pending, err := f.manager.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending after expiry: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none after seven days", pending)
	}

	rows := f.approvalsFor(t, unknownPhone)
	if len(rows) != 1 {
		t.Fatalf("approval rows = %d, want 1", len(rows))
	}
	if rows[0].Status != store.ApprovalExpired {
		t.Fatalf("status = %q, want %q", rows[0].Status, store.ApprovalExpired)
	}
	if rows[0].ResolvedAt.IsZero() {
		t.Fatal("expired row has no resolved_at")
	}

	if err := f.manager.RequestApproval(ctx, unknownPhone); err != nil {
		t.Fatalf("RequestApproval after expiry(%s): %v", unknownPhone, err)
	}

	rows = f.approvalsFor(t, unknownPhone)
	if len(rows) != 2 {
		t.Fatalf("approval rows = %d, want 2 (expired + fresh)", len(rows))
	}
	if rows[0].Status != store.ApprovalExpired {
		t.Fatalf("old row status = %q, want %q", rows[0].Status, store.ApprovalExpired)
	}
	if rows[1].Status != store.ApprovalPending {
		t.Fatalf("new row status = %q, want %q", rows[1].Status, store.ApprovalPending)
	}
	if !rows[1].RequestedAt.Equal(f.clock.Now()) {
		t.Fatalf("new row requested_at = %s, want %s", rows[1].RequestedAt, f.clock.Now())
	}

	pending, err = f.manager.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending after re-request: %v", err)
	}
	if len(pending) != 1 || pending[0].Phone != unknownPhone {
		t.Fatalf("pending = %+v, want one fresh request for %s", pending, unknownPhone)
	}
}

// TestAdminMaintenance covers !admin add/remove and the admin-role gate.
func TestAdminMaintenance(t *testing.T) {
	f := newFixture(t, []string{maintainerPhone1}, false)

	if err := f.manager.HandleAdmin(sdk.Transmission{
		Command: "admin",
		Trigger: "admin",
		Text:    "!admin add " + newAdminPhone,
		Sender:  adminPhone,
	}); err != nil {
		t.Fatalf("HandleAdmin(add): %v", err)
	}
	if admin := f.admin(t); !contains(admin.Admins, newAdminPhone) {
		t.Fatalf("admins = %v, want %s", admin.Admins, newAdminPhone)
	}

	if err := f.manager.HandleAdmin(sdk.Transmission{
		Command: "admin",
		Trigger: "admin",
		Text:    "!admin remove " + newAdminPhone,
		Sender:  adminPhone,
	}); err != nil {
		t.Fatalf("HandleAdmin(remove): %v", err)
	}
	if admin := f.admin(t); contains(admin.Admins, newAdminPhone) {
		t.Fatalf("admins = %v must not contain removed %s", admin.Admins, newAdminPhone)
	}

	err := f.manager.HandleAdmin(sdk.Transmission{
		Command: "admin",
		Trigger: "admin",
		Text:    "!admin add " + otherAdminPhone,
		Sender:  userPhone,
	})
	requireStatus(t, err, http.StatusForbidden)
	if admin := f.admin(t); contains(admin.Admins, otherAdminPhone) {
		t.Fatalf("user request modified admins: %v", admin.Admins)
	}
}

const unknownUUID = "2aea5c40-0f8e-4ad7-8099-880b50064454"

// TestApprovalFlowUUID covers a sender known only by a Signal UUID: the
// request is recorded, the notification names it, and !approve authorizes it.
func TestApprovalFlowUUID(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []string{maintainerPhone1}, true)

	if err := f.manager.RequestApprovalFrom(ctx, unknownUUID, "Tara Schick"); err != nil {
		t.Fatalf("RequestApprovalFrom(%s): %v", unknownUUID, err)
	}

	pending, err := f.store.PendingApproval(ctx, unknownUUID)
	if err != nil {
		t.Fatalf("PendingApproval(%s): %v", unknownUUID, err)
	}
	if pending == nil || pending.Status != store.ApprovalPending {
		t.Fatalf("pending = %+v, want a pending request", pending)
	}

	jobs := f.queue.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("notification jobs = %d, want 1", len(jobs))
	}
	text := jobs[0].Text
	for _, want := range []string{unknownUUID, "Tara Schick", "#1", "!approve 1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notification text %q does not contain %q", text, want)
		}
	}

	if err := f.manager.HandleApprove(sdk.Transmission{
		Trigger: "approve",
		Text:    "!approve " + unknownUUID,
		Sender:  maintainerPhone1,
	}); err != nil {
		t.Fatalf("HandleApprove(%s): %v", unknownUUID, err)
	}
	if admin := f.admin(t); !contains(admin.ApprovedNumbers, unknownUUID) {
		t.Fatalf("approved_numbers = %v, want %s", admin.ApprovedNumbers, unknownUUID)
	}
	rows := f.approvalsFor(t, unknownUUID)
	if len(rows) != 1 || rows[0].Status != store.ApprovalApproved {
		t.Fatalf("approval rows = %+v, want one approved row", rows)
	}
}

// TestPendingCommand covers the read-only listing, its role gate, and the
// text-dependent reaction policy.
func TestPendingCommand(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []string{maintainerPhone1}, false)

	if err := f.manager.RequestApproval(ctx, unknownPhone); err != nil {
		t.Fatalf("RequestApproval(%s): %v", unknownPhone, err)
	}

	err := f.manager.HandlePending(sdk.Transmission{
		Trigger: "pending",
		Text:    "!pending",
		Sender:  userPhone,
	})
	requireStatus(t, err, http.StatusForbidden)

	if err := f.manager.HandlePending(sdk.Transmission{
		Trigger: "pending",
		Text:    "!pending",
		Sender:  maintainerPhone1,
	}); err != nil {
		t.Fatalf("HandlePending: %v", err)
	}
	responses := f.out.Responses()
	if len(responses) != 1 {
		t.Fatalf("pending responses = %d, want 1", len(responses))
	}
	if responses[0].Recipients[0] != maintainerPhone1 {
		t.Fatalf("pending recipient = %v, want %s", responses[0].Recipients, maintainerPhone1)
	}
	for _, want := range []string{unknownPhone, "!approve", "!deny"} {
		if !strings.Contains(responses[0].Text, want) {
			t.Fatalf("pending text %q does not contain %q", responses[0].Text, want)
		}
	}

	if got := SuccessReactionFor("!pending"); got != "" {
		t.Errorf("SuccessReactionFor(!pending) = %q, want no reaction", got)
	}
	if got := SuccessReactionFor("admin list"); got != "" {
		t.Errorf("SuccessReactionFor(admin list) = %q, want no reaction", got)
	}
	if got := SuccessReactionFor("!admin approve " + unknownPhone); got != "👍" {
		t.Errorf("SuccessReactionFor(admin approve) = %q, want 👍", got)
	}
}

// TestAdminApproveDeny covers resolving requests through !admin.
func TestAdminApproveDeny(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []string{maintainerPhone1}, false)

	if err := f.manager.RequestApproval(ctx, unknownPhone3); err != nil {
		t.Fatalf("RequestApproval(%s): %v", unknownPhone3, err)
	}
	if err := f.manager.HandleAdmin(sdk.Transmission{
		Trigger: "admin",
		Text:    "!Admin Approve " + unknownPhone3,
		Sender:  maintainerPhone1,
	}); err != nil {
		t.Fatalf("HandleAdmin(approve): %v", err)
	}
	if admin := f.admin(t); !contains(admin.ApprovedNumbers, unknownPhone3) {
		t.Fatalf("approved_numbers = %v, want %s", admin.ApprovedNumbers, unknownPhone3)
	}

	if err := f.manager.RequestApproval(ctx, unknownPhone4); err != nil {
		t.Fatalf("RequestApproval(%s): %v", unknownPhone4, err)
	}
	if err := f.manager.HandleAdmin(sdk.Transmission{
		Trigger: "admin",
		Text:    "!admin deny " + unknownPhone4,
		Sender:  maintainerPhone1,
	}); err != nil {
		t.Fatalf("HandleAdmin(deny): %v", err)
	}
	if admin := f.admin(t); !contains(admin.DeniedNumbers, unknownPhone4) {
		t.Fatalf("denied_numbers = %v, want %s", admin.DeniedNumbers, unknownPhone4)
	}
}

// TestRequestApprovalRejectsInvalidIdentity keeps path traversal and malformed
// identifiers out of the approvals table and the config tree.
func TestRequestApprovalRejectsInvalidIdentity(t *testing.T) {
	f := newFixture(t, []string{maintainerPhone1}, false)

	err := f.manager.RequestApproval(context.Background(), "../admin")
	requireStatus(t, err, http.StatusBadRequest)
}

// TestApproveBatchByID covers short row references, comma-separated batches,
// and the fact that unlisted requests stay pending.
func TestApproveBatchByID(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []string{maintainerPhone1}, false)

	for _, phone := range []string{unknownPhone, unknownPhone2, unknownPhone3} {
		if err := f.manager.RequestApproval(ctx, phone); err != nil {
			t.Fatalf("RequestApproval(%s): %v", phone, err)
		}
	}

	if err := f.manager.HandleApprove(sdk.Transmission{
		Trigger: "approve",
		Text:    "!Approve #1,3",
		Sender:  maintainerPhone1,
	}); err != nil {
		t.Fatalf("HandleApprove(batch): %v", err)
	}

	admin := f.admin(t)
	if !contains(admin.ApprovedNumbers, unknownPhone) || !contains(admin.ApprovedNumbers, unknownPhone3) {
		t.Fatalf("approved_numbers = %v, want #1 and #3", admin.ApprovedNumbers)
	}
	if contains(admin.ApprovedNumbers, unknownPhone2) {
		t.Fatalf("approved_numbers = %v must not contain #2", admin.ApprovedNumbers)
	}
	for _, phone := range []string{unknownPhone, unknownPhone3} {
		rows := f.approvalsFor(t, phone)
		if len(rows) != 1 || rows[0].Status != store.ApprovalApproved {
			t.Fatalf("%s rows = %+v, want approved", phone, rows)
		}
	}
	rows := f.approvalsFor(t, unknownPhone2)
	if len(rows) != 1 || rows[0].Status != store.ApprovalPending {
		t.Fatalf("%s rows = %+v, want still pending", unknownPhone2, rows)
	}
}

// TestApproveBareUsesNewestRequest covers the context-free reply path: a bare
// !approve resolves the newest pending request.
func TestApproveBareUsesNewestRequest(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []string{maintainerPhone1}, false)

	if err := f.manager.RequestApproval(ctx, unknownPhone); err != nil {
		t.Fatalf("RequestApproval(%s): %v", unknownPhone, err)
	}
	if err := f.manager.RequestApproval(ctx, unknownPhone2); err != nil {
		t.Fatalf("RequestApproval(%s): %v", unknownPhone2, err)
	}

	if err := f.manager.HandleApprove(sdk.Transmission{
		Trigger: "approve",
		Text:    "!approve",
		Sender:  maintainerPhone1,
	}); err != nil {
		t.Fatalf("HandleApprove(bare): %v", err)
	}

	admin := f.admin(t)
	if contains(admin.ApprovedNumbers, unknownPhone) {
		t.Fatalf("approved_numbers = %v must not contain the older request", admin.ApprovedNumbers)
	}
	if !contains(admin.ApprovedNumbers, unknownPhone2) {
		t.Fatalf("approved_numbers = %v, want newest request %s", admin.ApprovedNumbers, unknownPhone2)
	}
}

// TestApproveBatchRejectsUnknownIDAtomically keeps a typo from applying part
// of a batch.
func TestApproveBatchRejectsUnknownIDAtomically(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []string{maintainerPhone1}, false)

	if err := f.manager.RequestApproval(ctx, unknownPhone); err != nil {
		t.Fatalf("RequestApproval(%s): %v", unknownPhone, err)
	}

	err := f.manager.HandleApprove(sdk.Transmission{
		Trigger: "approve",
		Text:    "!approve 1,99",
		Sender:  maintainerPhone1,
	})
	requireStatus(t, err, http.StatusBadRequest)

	rows := f.approvalsFor(t, unknownPhone)
	if len(rows) != 1 || rows[0].Status != store.ApprovalPending {
		t.Fatalf("%s rows = %+v, want still pending after a rejected batch", unknownPhone, rows)
	}
}

// TestPendingShowsNamesAndBatchHint covers the richer listing maintainers use
// to choose short references.
func TestPendingShowsNamesAndBatchHint(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, []string{maintainerPhone1}, false)

	if err := f.manager.RequestApprovalFrom(ctx, unknownUUID, "Tara Schick"); err != nil {
		t.Fatalf("RequestApprovalFrom(%s): %v", unknownUUID, err)
	}
	if err := f.manager.RequestApproval(ctx, unknownPhone); err != nil {
		t.Fatalf("RequestApproval(%s): %v", unknownPhone, err)
	}
	if err := f.manager.HandlePending(sdk.Transmission{
		Trigger: "pending",
		Text:    "!pending",
		Sender:  maintainerPhone1,
	}); err != nil {
		t.Fatalf("HandlePending: %v", err)
	}

	responses := f.out.Responses()
	if len(responses) != 1 {
		t.Fatalf("pending responses = %d, want 1", len(responses))
	}
	text := responses[0].Text
	for _, want := range []string{"#1", "Tara Schick", unknownUUID, "#2", unknownPhone, "1,3,4"} {
		if !strings.Contains(text, want) {
			t.Fatalf("pending text %q does not contain %q", text, want)
		}
	}
}
