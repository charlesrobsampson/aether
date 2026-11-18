package testsat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/scheduler"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/internal/testutil"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

// fakeReminderStore is the durable-reminder seam. It keeps rows in memory and
// lets each test simulate a restart by building a second Manager over it.
type fakeReminderStore struct {
	mu         sync.Mutex
	nextID     int64
	records    map[int64]store.ReminderRecord
	insertErr  error
	pendingErr error
	markErr    error
	advanceErr error
	cancelErr  error
}

func newFakeReminderStore() *fakeReminderStore {
	return &fakeReminderStore{records: make(map[int64]store.ReminderRecord)}
}

func (f *fakeReminderStore) InsertReminder(_ context.Context, rec store.ReminderRecord) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	f.nextID++
	rec.ID = f.nextID
	f.records[rec.ID] = rec
	return rec.ID, nil
}

func (f *fakeReminderStore) InsertSchedule(ctx context.Context, rec store.ReminderRecord) (int64, error) {
	return f.InsertReminder(ctx, rec)
}

func (f *fakeReminderStore) PendingReminders(_ context.Context) ([]store.ReminderRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pendingErr != nil {
		return nil, f.pendingErr
	}
	out := make([]store.ReminderRecord, 0, len(f.records))
	for _, rec := range f.records {
		if rec.FiredAt.IsZero() {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DueAt.Before(out[j].DueAt) })
	return out, nil
}

func (f *fakeReminderStore) PendingRemindersFor(_ context.Context, principal string) ([]store.ReminderRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pendingErr != nil {
		return nil, f.pendingErr
	}
	principal = strings.TrimSpace(principal)
	var out []store.ReminderRecord
	for _, rec := range f.records {
		if !rec.FiredAt.IsZero() {
			continue
		}
		if rec.GroupID != "" {
			if rec.GroupID != principal {
				continue
			}
		} else if rec.Sender != principal {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DueAt.Before(out[j].DueAt) })
	return out, nil
}

func (f *fakeReminderStore) MarkReminderFired(_ context.Context, id int64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	rec, ok := f.records[id]
	if !ok || !rec.FiredAt.IsZero() {
		return nil
	}
	rec.FiredAt = at
	f.records[id] = rec
	return nil
}

func (f *fakeReminderStore) AdvanceReminder(_ context.Context, id int64, nextDue time.Time, remaining int, lastFired time.Time, done bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.advanceErr != nil {
		return false, f.advanceErr
	}
	rec, ok := f.records[id]
	if !ok || !rec.FiredAt.IsZero() {
		return false, nil
	}
	rec.DueAt = nextDue
	rec.Remaining = remaining
	rec.LastFiredAt = lastFired
	if done {
		rec.FiredAt = lastFired
	}
	f.records[id] = rec
	return true, nil
}

func (f *fakeReminderStore) CancelReminder(_ context.Context, id int64, principal string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return false, f.cancelErr
	}
	rec, ok := f.records[id]
	if !ok || !rec.FiredAt.IsZero() {
		return false, nil
	}
	if rec.GroupID != "" {
		if rec.GroupID != principal {
			return false, nil
		}
	} else if rec.Sender != principal {
		return false, nil
	}
	delete(f.records, id)
	return true, nil
}

func (f *fakeReminderStore) only(t *testing.T) store.ReminderRecord {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.records) != 1 {
		t.Fatalf("records = %d, want 1", len(f.records))
	}
	for _, rec := range f.records {
		return rec
	}
	return store.ReminderRecord{}
}

func TestReminderPersistsAndMarksFired(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewFakeClock(start)
	sender := &recordingSender{}
	reminders := newFakeReminderStore()
	manager := New(Options{
		Client:    sender,
		Scheduler: scheduler.New(clock),
		Store:     reminders,
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
	})

	if err := manager.HandleRemind(sdk.Transmission{
		Trigger:   "remind",
		Text:      "remind me call bob in 30 seconds",
		Sender:    testSender,
		Satellite: testSatellite,
		ThreadID:  "errand",
	}); err != nil {
		t.Fatalf("HandleRemind: %v", err)
	}

	rec := reminders.only(t)
	if rec.Sender != testSender || rec.ThreadID != "errand" || rec.Satellite != testSatellite {
		t.Fatalf("stored reminder = %+v", rec)
	}
	if rec.Text != "call bob" {
		t.Errorf("text = %q, want %q", rec.Text, "call bob")
	}
	if want := start.Add(30 * time.Second); !rec.DueAt.Equal(want) {
		t.Errorf("due = %v, want %v", rec.DueAt, want)
	}
	if !rec.FiredAt.IsZero() {
		t.Fatalf("fired_at = %v before the timer, want zero", rec.FiredAt)
	}

	clock.Advance(30 * time.Second)
	requireResponses(t, sender, 1)

	rec = reminders.only(t)
	if want := start.Add(30 * time.Second); !rec.FiredAt.Equal(want) {
		t.Errorf("fired_at = %v, want %v", rec.FiredAt, want)
	}
}

func TestRestoreRemindersSurvivesRestart(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	reminders := newFakeReminderStore()

	firstClock := testutil.NewFakeClock(start)
	firstSender := &recordingSender{}
	first := New(Options{
		Client:    firstSender,
		Scheduler: scheduler.New(firstClock),
		Store:     reminders,
		Clock:     firstClock,
		Timezone:  func(string) string { return "America/Chicago" },
	})
	if err := first.HandleRemind(sdk.Transmission{
		Trigger:   "remind",
		Text:      "remind me stretch at 2026-01-05 12:30",
		Sender:    testSender,
		GroupID:   "group.example",
		Satellite: testSatellite,
		ThreadID:  "fitness",
	}); err != nil {
		t.Fatalf("HandleRemind: %v", err)
	}
	requireResponses(t, firstSender, 1) // the absolute-time acknowledgement

	// A restart one hour later must re-arm the still-pending row.
	restart := start.Add(time.Hour)
	secondClock := testutil.NewFakeClock(restart)
	secondSender := &recordingSender{}
	second := New(Options{
		Client:    secondSender,
		Scheduler: scheduler.New(secondClock),
		Store:     reminders,
		Clock:     secondClock,
		Timezone:  func(string) string { return "America/Chicago" },
	})
	if err := second.RestoreReminders(context.Background()); err != nil {
		t.Fatalf("RestoreReminders: %v", err)
	}

	secondClock.Advance(5*time.Hour + 29*time.Minute)
	requireResponses(t, secondSender, 0)
	secondClock.Advance(time.Minute)

	got := requireResponses(t, secondSender, 1)
	if got[0].Text != "stretch" {
		t.Errorf("text = %q, want stretch", got[0].Text)
	}
	if len(got[0].Recipients) != 1 || got[0].Recipients[0] != "group.example" {
		t.Errorf("recipients = %v, want [group.example]", got[0].Recipients)
	}
	if got[0].ThreadID != "fitness" {
		t.Errorf("thread = %q, want fitness", got[0].ThreadID)
	}

	// A third start after the send must not deliver it again.
	thirdClock := testutil.NewFakeClock(start.Add(7 * time.Hour))
	thirdSender := &recordingSender{}
	third := New(Options{
		Client:    thirdSender,
		Scheduler: scheduler.New(thirdClock),
		Store:     reminders,
		Clock:     thirdClock,
		Timezone:  func(string) string { return "America/Chicago" },
	})
	if err := third.RestoreReminders(context.Background()); err != nil {
		t.Fatalf("RestoreReminders after fire: %v", err)
	}
	thirdClock.Advance(24 * time.Hour)
	requireResponses(t, thirdSender, 0)
}

func TestRestoreFiresOverdueReminder(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	reminders := newFakeReminderStore()
	if _, err := reminders.InsertReminder(context.Background(), store.ReminderRecord{
		Sender:    testSender,
		Text:      "late reminder",
		DueAt:     start.Add(-time.Hour),
		Satellite: testSatellite,
	}); err != nil {
		t.Fatalf("InsertReminder: %v", err)
	}

	clock := testutil.NewFakeClock(start)
	sender := &recordingSender{}
	manager := New(Options{
		Client:    sender,
		Scheduler: scheduler.New(clock),
		Store:     reminders,
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
	})
	if err := manager.RestoreReminders(context.Background()); err != nil {
		t.Fatalf("RestoreReminders: %v", err)
	}

	clock.Advance(0)
	got := requireResponses(t, sender, 1)
	if got[0].Text != "late reminder" {
		t.Errorf("text = %q, want late reminder", got[0].Text)
	}
	if rec := reminders.only(t); rec.FiredAt.IsZero() {
		t.Error("overdue reminder was sent but not marked fired")
	}
}

func TestReminderSendFailureStaysPending(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewFakeClock(start)
	sender := &recordingSender{err: errors.New("queue full")}
	reminders := newFakeReminderStore()
	var observed error
	manager := New(Options{
		Client:    sender,
		Scheduler: scheduler.New(clock),
		Store:     reminders,
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
		OnError:   func(err error) { observed = err },
	})

	if err := manager.HandleRemind(sdk.Transmission{
		Trigger:   "remind",
		Text:      "remind me call bob in 30 seconds",
		Sender:    testSender,
		Satellite: testSatellite,
	}); err != nil {
		t.Fatalf("HandleRemind: %v", err)
	}

	clock.Advance(30 * time.Second)
	if observed == nil {
		t.Fatal("send failure was not observed")
	}
	if rec := reminders.only(t); !rec.FiredAt.IsZero() {
		t.Fatalf("fired_at = %v after a failed send, want zero", rec.FiredAt)
	}
}

func TestReminderStoreFailureIsUserSafe(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewFakeClock(start)
	reminders := newFakeReminderStore()
	reminders.insertErr = errors.New("database unavailable")
	sender := &recordingSender{}
	manager := New(Options{
		Client:    sender,
		Scheduler: scheduler.New(clock),
		Store:     reminders,
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
	})

	err := manager.HandleRemind(sdk.Transmission{
		Trigger:   "remind",
		Text:      "remind me call bob in 30 seconds",
		Sender:    testSender,
		Satellite: testSatellite,
	})
	if _, ok := sdk.AsUserMessage(err); !ok {
		t.Fatalf("error %v is not user-facing", err)
	}

	clock.Advance(time.Minute)
	requireResponses(t, sender, 0)
}

func TestHandleRemindersListsConversationPending(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewFakeClock(start)
	sender := &recordingSender{}
	reminders := newFakeReminderStore()
	manager := New(Options{
		Client:    sender,
		Scheduler: scheduler.New(clock),
		Store:     reminders,
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
	})

	seed := []store.ReminderRecord{
		{Sender: testSender, Text: "first", DueAt: start.Add(time.Hour)},
		{Sender: testSender, Text: "second\nline", DueAt: start.Add(2 * time.Hour)},
		{Sender: "+15555550199", Text: "not mine", DueAt: start},
		{Sender: testSender, GroupID: "group.example", Text: "group only", DueAt: start},
	}
	for _, rec := range seed {
		if _, err := reminders.InsertReminder(ctx, rec); err != nil {
			t.Fatalf("InsertReminder(%q): %v", rec.Text, err)
		}
	}
	firedID, err := reminders.InsertReminder(ctx, store.ReminderRecord{
		Sender: testSender, Text: "already done", DueAt: start.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("InsertReminder(fired): %v", err)
	}
	if err := reminders.MarkReminderFired(ctx, firedID, start); err != nil {
		t.Fatalf("MarkReminderFired: %v", err)
	}

	if err := manager.HandleReminders(sdk.Transmission{
		Sender:    testSender,
		Satellite: testSatellite,
	}); err != nil {
		t.Fatalf("HandleReminders: %v", err)
	}
	got := requireResponses(t, sender, 1)
	text := got[0].Text
	if !strings.HasPrefix(text, "Pending reminders:") {
		t.Fatalf("reply = %q, want a pending-reminders header", text)
	}
	for _, want := range []string{"#", "first", "second line"} {
		if !strings.Contains(text, want) {
			t.Errorf("reply %q missing %q", text, want)
		}
	}
	for _, unwanted := range []string{"not mine", "group only", "already done"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("reply %q leaked %q", text, unwanted)
		}
	}
	if strings.Index(text, "first") > strings.Index(text, "second line") {
		t.Errorf("reply is not ordered by due time: %q", text)
	}

	// A group lists only the group's reminders, not its members' direct ones.
	groupSender := &recordingSender{}
	groupManager := New(Options{
		Client:    groupSender,
		Scheduler: scheduler.New(clock),
		Store:     reminders,
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
	})
	if err := groupManager.HandleReminders(sdk.Transmission{
		Sender:    testSender,
		GroupID:   "group.example",
		Satellite: testSatellite,
	}); err != nil {
		t.Fatalf("HandleReminders(group): %v", err)
	}
	got = requireResponses(t, groupSender, 1)
	if !strings.Contains(got[0].Text, "group only") {
		t.Errorf("group reply = %q, want the group reminder", got[0].Text)
	}
	if strings.Contains(got[0].Text, "first") {
		t.Errorf("group reply = %q, leaked a direct reminder", got[0].Text)
	}
	if len(got[0].Recipients) != 1 || got[0].Recipients[0] != "group.example" {
		t.Errorf("recipients = %v, want [group.example]", got[0].Recipients)
	}

	// A conversation with nothing pending gets a plain text answer.
	emptySender := &recordingSender{}
	emptyManager := New(Options{
		Client:    emptySender,
		Scheduler: scheduler.New(clock),
		Store:     reminders,
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
	})
	if err := emptyManager.HandleReminders(sdk.Transmission{
		Sender:    "+15555550198",
		Satellite: testSatellite,
	}); err != nil {
		t.Fatalf("HandleReminders(empty): %v", err)
	}
	got = requireResponses(t, emptySender, 1)
	if got[0].Text != "No pending reminders." {
		t.Errorf("empty reply = %q, want %q", got[0].Text, "No pending reminders.")
	}
}

func TestRemindListAliasDoesNotSchedule(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewFakeClock(start)
	sender := &recordingSender{}
	reminders := newFakeReminderStore()
	manager := New(Options{
		Client:    sender,
		Scheduler: scheduler.New(clock),
		Store:     reminders,
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
	})
	if _, err := reminders.InsertReminder(ctx, store.ReminderRecord{
		Sender: testSender, Text: "existing", DueAt: start.Add(time.Hour),
	}); err != nil {
		t.Fatalf("InsertReminder: %v", err)
	}

	if err := manager.HandleRemind(sdk.Transmission{
		Trigger:   "remind",
		Text:      "remind list",
		Sender:    testSender,
		Satellite: testSatellite,
	}); err != nil {
		t.Fatalf("HandleRemind(list): %v", err)
	}
	got := requireResponses(t, sender, 1)
	if !strings.Contains(got[0].Text, "existing") {
		t.Errorf("reply = %q, want the existing reminder", got[0].Text)
	}
	if records := reminders.only(t); records.Text != "existing" {
		t.Errorf("alias created a reminder: %+v", records)
	}
}

func TestHandleRemindersStoreFailureIsUserSafe(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewFakeClock(start)
	sender := &recordingSender{}
	reminders := newFakeReminderStore()
	reminders.pendingErr = errors.New("database unavailable")
	manager := New(Options{
		Client:    sender,
		Scheduler: scheduler.New(clock),
		Store:     reminders,
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
	})

	err := manager.HandleReminders(sdk.Transmission{
		Sender:    testSender,
		Satellite: testSatellite,
	})
	if _, ok := sdk.AsUserMessage(err); !ok {
		t.Fatalf("error %v is not user-facing", err)
	}
	requireResponses(t, sender, 0)
}

func TestHandleRemindersCapsLongList(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewFakeClock(start)
	sender := &recordingSender{}
	reminders := newFakeReminderStore()
	manager := New(Options{
		Client:    sender,
		Scheduler: scheduler.New(clock),
		Store:     reminders,
		Clock:     clock,
		Timezone:  func(string) string { return "America/Chicago" },
	})
	for i := 0; i < maxReminderList+3; i++ {
		if _, err := reminders.InsertReminder(ctx, store.ReminderRecord{
			Sender: testSender,
			Text:   fmt.Sprintf("reminder %d", i),
			DueAt:  start.Add(time.Duration(i+1) * time.Hour),
		}); err != nil {
			t.Fatalf("InsertReminder(%d): %v", i, err)
		}
	}

	if err := manager.HandleReminders(sdk.Transmission{
		Sender:    testSender,
		Satellite: testSatellite,
	}); err != nil {
		t.Fatalf("HandleReminders: %v", err)
	}
	got := requireResponses(t, sender, 1)
	if !strings.Contains(got[0].Text, "... and 3 more") {
		t.Errorf("reply = %q, want an overflow line", got[0].Text)
	}
}
