// Package testsat implements Aether's built-in test and reminder satellite
// (plan §3.16).
//
// It is a handler, not an HTTP server: the app dispatches !test and !remind to
// HandleTest and HandleRemind, which emit the standard /message envelope through
// an injected SDK client. Reminders are scheduled on internal/scheduler, so the
// same injectable clock drives both relative (`in <num> <unit>`) and absolute
// (`at <datetime>`) timers and tests never sleep.
package testsat

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charlesrobsampson/aether/internal/scheduler"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

const (
	// SatelliteName identifies the built-in satellite when a transmission does
	// not already carry a name.
	SatelliteName = "testsat"
	// defaultReminderText is sent when the user supplies no reminder body.
	defaultReminderText = "Reminder"
	// testReply is the fixed echo required by plan §3.16.
	testReply = "test received"
	// maxReminderList caps one list reply so a long backlog does not flood the
	// chat. The remainder is reported as a count.
	maxReminderList = 20
	// minRecurringInterval keeps "every N minutes" from becoming a spam loop.
	minRecurringInterval = time.Minute
	// scheduleRetryDelay is how long a recurring reminder waits before retrying
	// a failed send.
	scheduleRetryDelay = time.Minute
	// maxScheduleAttempts bounds those retries so a broken send cannot loop.
	maxScheduleAttempts = 3
)

// Sender is the outbound seam used to reply through Aether's /message. The
// concrete *sdk.Client satisfies it; tests inject a recorder.
type Sender interface {
	Send(ctx context.Context, resp sdk.Response) error
}

// Clock supplies the current time for absolute-reminder conversion. It is the
// same clock the injected scheduler uses, so a test can advance one FakeClock.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// ReminderStore is the subset of internal/store used for durable reminders.
// *store.Store implements it. A nil store keeps reminders in memory only.
type ReminderStore interface {
	InsertReminder(ctx context.Context, rec store.ReminderRecord) (int64, error)
	InsertSchedule(ctx context.Context, rec store.ReminderRecord) (int64, error)
	PendingReminders(ctx context.Context) ([]store.ReminderRecord, error)
	PendingRemindersFor(ctx context.Context, principal string) ([]store.ReminderRecord, error)
	MarkReminderFired(ctx context.Context, id int64, at time.Time) error
	AdvanceReminder(ctx context.Context, id int64, nextDue time.Time, remaining int, lastFired time.Time, done bool) (bool, error)
	CancelReminder(ctx context.Context, id int64, principal string) (bool, error)
}

// Options configures a Manager.
type Options struct {
	// Client posts responses to Aether. A nil client makes every command fail.
	Client Sender
	// Scheduler owns the reminder timers. A nil scheduler rejects reminders.
	Scheduler *scheduler.Scheduler
	// Store persists reminders so a restart does not drop them. Nil keeps
	// reminders in memory only.
	Store ReminderStore
	// Clock is the reference time for datetimes that omit a date. Defaults to
	// the system clock.
	Clock Clock
	// Timezone resolves the sender's IANA zone for absolute reminders. A nil
	// resolver uses UTC.
	Timezone func(sender string) string
	// RecurringLimit returns the principal's default repeat count, for example
	// "5 times". An empty string or "forever" means unlimited.
	RecurringLimit func(principal string) string
	// OnError observes a delivery failure from a fired reminder. The scheduler
	// callback cannot return an error, so it is reported here instead. Nil
	// discards it.
	OnError func(error)
}

// activeReminder is the in-memory handle for one pending reminder. cancelled is
// set by remind cancel so a timer that already fired cannot re-arm itself.
type activeReminder struct {
	cancelled bool
}

// Manager handles the built-in test and reminder commands.
type Manager struct {
	client         Sender
	scheduler      *scheduler.Scheduler
	store          ReminderStore
	clock          Clock
	timezone       func(string) string
	recurringLimit func(string) string
	onError        func(error)

	mu     sync.Mutex
	active map[int64]*activeReminder
}

// New returns a test/reminder handler.
func New(options Options) *Manager {
	clock := options.Clock
	if clock == nil {
		clock = systemClock{}
	}
	timezone := options.Timezone
	if timezone == nil {
		timezone = func(string) string { return "UTC" }
	}
	recurringLimit := options.RecurringLimit
	if recurringLimit == nil {
		recurringLimit = func(string) string { return "" }
	}
	return &Manager{
		client:         options.Client,
		scheduler:      options.Scheduler,
		store:          options.Store,
		clock:          clock,
		timezone:       timezone,
		recurringLimit: recurringLimit,
		onError:        options.OnError,
		active:         make(map[int64]*activeReminder),
	}
}

// HandleTest replies "test received" to the originating sender (plan §3.16).
func (m *Manager) HandleTest(t sdk.Transmission) error {
	if err := requireSender(t); err != nil {
		return err
	}
	return m.send(t, testReply)
}

// HandleRemind schedules one reminder back to the originating conversation. It
// accepts the typed form produced by a router matcher (num/unit or at plus msg)
// and the canonical text form:
//
//	remind me <msg> in <num> <unit>
//	remind me <msg> at <datetime>
//	remind me <msg> every day at <time>
//	remind me <msg> every weekday at <time>
//	remind me <msg> every <weekday> at <time>
//	remind me <msg> every <num> <unit>
//	remind list
//	remind cancel <id>
//
// A recurring form may end with "<num> times" or "until <date>". A message
// omitted by the short form defaults to "Reminder".
func (m *Manager) HandleRemind(t sdk.Transmission) error {
	if err := requireSender(t); err != nil {
		return err
	}
	switch ReminderCommand(t.Text, t.Trigger) {
	case "list":
		return m.HandleReminders(t)
	case "cancel":
		return m.HandleCancel(t)
	}
	if m == nil || m.scheduler == nil {
		return errors.New("testsat: reminder scheduler is not configured")
	}

	reminder, err := m.parseReminder(t)
	if err != nil {
		return err
	}
	if reminder.recurring != nil {
		return m.createRecurring(t, reminder.text, *reminder.recurring)
	}

	dueAt := reminder.at
	if !reminder.absolute {
		dueAt = m.clock.Now().Add(reminder.delay)
	}
	id, err := m.persistReminder(t, reminder.text, dueAt)
	if err != nil {
		return err
	}
	m.scheduleOneShot(t, reminder.text, dueAt, id)

	if reminder.absolute {
		// The reaction acknowledges every reminder; an absolute time also gets
		// one short normalized line so the user can verify the instant.
		if err := m.send(t, fmt.Sprintf("Reminder set for %s: %s",
			m.formatReminderTime(reminder.at, t.Actor()), reminder.text)); err != nil && m.onError != nil {
			m.onError(err)
		}
	}
	return nil
}

// createRecurring persists and arms a recurring reminder, then acknowledges the
// rule and its first occurrence.
func (m *Manager) createRecurring(t sdk.Transmission, text string, rec recurrence) error {
	actor := t.Actor()
	first, ok := nextOccurrence(rec, m.location(actor), m.clock.Now())
	if !ok || (!rec.endsAt.IsZero() && !first.Before(rec.endsAt)) {
		return sdk.Userf("That schedule has no occurrences before it ends. Choose a later end date.")
	}

	var id int64
	if m.store != nil {
		var err error
		id, err = m.store.InsertSchedule(context.Background(), store.ReminderRecord{
			Sender:          t.Sender,
			GroupID:         t.GroupID,
			ThreadID:        t.ThreadID,
			Satellite:       t.Satellite,
			Text:            text,
			DueAt:           first,
			CreatedAt:       m.clock.Now(),
			Kind:            rec.kind,
			Weekdays:        rec.weekdays,
			AtMinutes:       rec.atMinutes,
			IntervalSeconds: int(rec.interval / time.Second),
			Remaining:       rec.remaining,
			EndsAt:          rec.endsAt,
		})
		if err != nil {
			return sdk.Userf("I couldn't save that reminder. Try again.")
		}
	}
	m.scheduleRecurring(t, text, rec, id, first)

	ack := fmt.Sprintf("Recurring reminder set: %s — %s", text, rec.rule())
	switch {
	case rec.remaining > 0:
		ack += fmt.Sprintf(" (%d times)", rec.remaining)
	case !rec.endsAt.IsZero():
		ack += " until " + m.formatReminderDate(rec.endsAt, actor)
	}
	ack += ". First: " + m.formatReminderTime(first, actor) + "."
	if err := m.send(t, ack); err != nil && m.onError != nil {
		m.onError(err)
	}
	return nil
}

// HandleReminders lists the pending reminders owned by the originating
// conversation. A group lists the group's reminders; a direct chat lists the
// sender's. The reply is text because this command only reads.
func (m *Manager) HandleReminders(t sdk.Transmission) error {
	if err := requireSender(t); err != nil {
		return err
	}
	if ReminderCommand(t.Text, t.Trigger) == "cancel" {
		return m.HandleCancel(t)
	}
	if m == nil || m.store == nil {
		return errors.New("testsat: reminder store is not configured")
	}

	actor := t.Actor()
	records, err := m.store.PendingRemindersFor(context.Background(), actor)
	if err != nil {
		return sdk.Userf("I couldn't load your reminders. Try again.")
	}
	return m.send(t, m.renderReminders(records, actor))
}

// renderReminders formats pending reminders in due order. Text is collapsed to
// one line so each reminder stays scannable in chat.
func (m *Manager) renderReminders(records []store.ReminderRecord, actor string) string {
	if len(records) == 0 {
		return "No pending reminders."
	}

	var b strings.Builder
	b.WriteString("Pending reminders:\n")
	limit := len(records)
	if limit > maxReminderList {
		limit = maxReminderList
	}
	for _, rec := range records[:limit] {
		fmt.Fprintf(&b, "  #%d %s — %s",
			rec.ID, m.formatReminderTime(rec.DueAt, actor), oneLine(rec.Text))
		if rec.Recurring() {
			fmt.Fprintf(&b, " [%s]", m.scheduleDescription(rec, actor))
		}
		b.WriteString("\n")
	}
	if remaining := len(records) - limit; remaining > 0 {
		fmt.Fprintf(&b, "  ... and %d more\n", remaining)
	}
	return strings.TrimRight(b.String(), "\n")
}

// HandleCancel deletes one pending reminder owned by the conversation.
func (m *Manager) HandleCancel(t sdk.Transmission) error {
	if err := requireSender(t); err != nil {
		return err
	}
	if m == nil || m.store == nil {
		return errors.New("testsat: reminder store is not configured")
	}

	id, err := reminderCancelID(t.Text, t.Trigger)
	if err != nil {
		return err
	}
	ok, err := m.store.CancelReminder(context.Background(), id, t.Actor())
	if err != nil {
		return sdk.Userf("I couldn't cancel that reminder. Try again.")
	}
	if !ok {
		return sdk.Userf("No pending reminder #%d in this conversation.", id)
	}
	m.cancel(id)
	return nil
}

// ReminderCommand classifies the text after the trigger: "list", "cancel", or
// "" for a reminder to schedule. The app uses it to choose reactions, and the
// handlers use it to route the convenience forms.
func ReminderCommand(text, trigger string) string {
	rest := strings.ToLower(normalizeWhitespace(stripTrigger(text, trigger)))
	switch {
	case rest == "list" || rest == "ls":
		return "list"
	case rest == "cancel" || strings.HasPrefix(rest, "cancel "):
		return "cancel"
	default:
		return ""
	}
}

// reminderCancelID parses the id from "cancel <id>".
func reminderCancelID(text, trigger string) (int64, error) {
	fields := strings.Fields(normalizeWhitespace(stripTrigger(text, trigger)))
	if len(fields) < 2 || !strings.EqualFold(fields[0], "cancel") {
		return 0, sdk.Userf(`Use "remind cancel <id>", for example "remind cancel 3".`)
	}
	if len(fields) != 2 {
		return 0, sdk.Userf(`Use one reminder id, for example "remind cancel 3".`)
	}
	id, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || id <= 0 {
		return 0, sdk.Userf("Reminder id %q is not a positive number.", fields[1])
	}
	return id, nil
}

// oneLine collapses newlines and runs of spaces into single spaces.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// RestoreReminders re-arms every reminder that was persisted but never fired.
// A reminder whose due time passed while Aether was down is scheduled at the
// next clock tick, so an outage delays it instead of dropping it. A recurring
// reminder that missed several occurrences fires once and then resumes at the
// next future occurrence.
func (m *Manager) RestoreReminders(ctx context.Context) error {
	if m == nil || m.store == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	records, err := m.store.PendingReminders(ctx)
	if err != nil {
		return err
	}
	for _, rec := range records {
		t := sdk.Transmission{
			Sender:    rec.Sender,
			GroupID:   rec.GroupID,
			ThreadID:  rec.ThreadID,
			Satellite: rec.Satellite,
		}
		if rec.Recurring() {
			m.scheduleRecurring(t, rec.Text, recurrenceFromRecord(rec), rec.ID, rec.DueAt)
			continue
		}
		m.scheduleOneShot(t, rec.Text, rec.DueAt, rec.ID)
	}
	return nil
}

// persistReminder saves the reminder before its timer is armed. A save failure
// fails the command, because acknowledging a reminder that cannot survive a
// restart would be misleading.
func (m *Manager) persistReminder(t sdk.Transmission, text string, dueAt time.Time) (int64, error) {
	if m.store == nil {
		return 0, nil
	}
	id, err := m.store.InsertReminder(context.Background(), store.ReminderRecord{
		Sender:    t.Sender,
		GroupID:   t.GroupID,
		ThreadID:  t.ThreadID,
		Satellite: t.Satellite,
		Text:      text,
		DueAt:     dueAt,
		CreatedAt: m.clock.Now(),
	})
	if err != nil {
		return 0, sdk.Userf("I couldn't save that reminder. Try again.")
	}
	return id, nil
}

// scheduleOneShot arms the timer and marks the row fired only after a
// successful send. A failed send stays pending, so the next startup retries it
// instead of silently losing it.
func (m *Manager) scheduleOneShot(t sdk.Transmission, text string, dueAt time.Time, id int64) {
	active := m.track(id)
	m.scheduler.At(dueAt, func() {
		defer m.forget(id)
		if m.isCancelled(active) {
			return
		}
		if err := m.send(t, text); err != nil {
			if m.onError != nil {
				m.onError(err)
			}
			return
		}
		if id == 0 || m.store == nil {
			return
		}
		if err := m.store.MarkReminderFired(context.Background(), id, m.clock.Now()); err != nil && m.onError != nil {
			m.onError(err)
		}
	})
}

// scheduleRecurring arms the next occurrence of a recurring reminder.
func (m *Manager) scheduleRecurring(t sdk.Transmission, text string, rec recurrence, id int64, dueAt time.Time) {
	m.armRecurring(t, text, rec, id, dueAt, m.track(id), 0)
}

// armRecurring arms one occurrence. A successful send advances the row and
// arms the next occurrence; a failed send retries a few times and then leaves
// the row pending for the next startup.
func (m *Manager) armRecurring(t sdk.Transmission, text string, rec recurrence, id int64, dueAt time.Time, active *activeReminder, attempts int) {
	m.scheduler.At(dueAt, func() {
		if m.isCancelled(active) {
			m.forget(id)
			return
		}
		if err := m.send(t, text); err != nil {
			if m.onError != nil {
				m.onError(err)
			}
			if attempts+1 < maxScheduleAttempts {
				m.armRecurring(t, text, rec, id, m.clock.Now().Add(scheduleRetryDelay), active, attempts+1)
				return
			}
			m.forget(id)
			return
		}
		m.advanceRecurring(t, text, rec, id, active)
	})
}

// advanceRecurring records a successful occurrence and arms the next one. A
// count or end date that has run out completes the series.
func (m *Manager) advanceRecurring(t sdk.Transmission, text string, rec recurrence, id int64, active *activeReminder) {
	now := m.clock.Now()
	remaining := rec.remaining
	if remaining != store.UnlimitedReminders {
		remaining--
	}
	done := remaining == 0

	var nextDue time.Time
	if !done {
		var ok bool
		nextDue, ok = nextOccurrence(rec, m.location(t.Actor()), now)
		if !ok || (!rec.endsAt.IsZero() && !nextDue.Before(rec.endsAt)) {
			done = true
		}
	}

	if m.isCancelled(active) {
		m.forget(id)
		return
	}
	if id != 0 && m.store != nil {
		found, err := m.store.AdvanceReminder(context.Background(), id, nextDue, remaining, now, done)
		if err != nil && m.onError != nil {
			m.onError(err)
		}
		if !found {
			m.forget(id)
			return
		}
	}
	if done {
		m.forget(id)
		return
	}
	rec.remaining = remaining
	m.armRecurring(t, text, rec, id, nextDue, active, 0)
}

// track registers the in-memory handle used by remind cancel.
func (m *Manager) track(id int64) *activeReminder {
	active := &activeReminder{}
	if m == nil || id == 0 {
		return active
	}
	m.mu.Lock()
	m.active[id] = active
	m.mu.Unlock()
	return active
}

// forget drops a finished reminder's handle.
func (m *Manager) forget(id int64) {
	if m == nil || id == 0 {
		return
	}
	m.mu.Lock()
	delete(m.active, id)
	m.mu.Unlock()
}

// cancel marks a reminder cancelled so a timer already in flight cannot send or
// re-arm after remind cancel deletes the row.
func (m *Manager) cancel(id int64) {
	if m == nil || id == 0 {
		return
	}
	m.mu.Lock()
	if active, ok := m.active[id]; ok {
		active.cancelled = true
		delete(m.active, id)
	}
	m.mu.Unlock()
}

// isCancelled reports whether remind cancel ran for this reminder.
func (m *Manager) isCancelled(active *activeReminder) bool {
	if m == nil || active == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return active.cancelled
}

// recurrenceFromRecord restores the in-memory form from a stored row.
func recurrenceFromRecord(rec store.ReminderRecord) recurrence {
	return recurrence{
		kind:      rec.Kind,
		weekdays:  rec.Weekdays,
		atMinutes: rec.AtMinutes,
		interval:  time.Duration(rec.IntervalSeconds) * time.Second,
		remaining: rec.Remaining,
		endsAt:    rec.EndsAt,
	}
}

// formatReminderTime renders an absolute reminder in the sender's timezone. An
// unknown zone falls back to UTC rather than hiding the acknowledgement.
func (m *Manager) formatReminderTime(at time.Time, sender string) string {
	return at.In(m.location(sender)).Format("Mon 2006-01-02 15:04 MST")
}

// formatReminderDate renders a date in the sender's timezone.
func (m *Manager) formatReminderDate(at time.Time, sender string) string {
	return at.In(m.location(sender)).Format("2006-01-02")
}

// location resolves the sender's live timezone. An unknown zone falls back to
// UTC so a bad setting never hides a reminder.
func (m *Manager) location(principal string) *time.Location {
	if m != nil {
		if timezone := strings.TrimSpace(m.timezone(principal)); timezone != "" {
			if loaded, err := time.LoadLocation(timezone); err == nil {
				return loaded
			}
		}
	}
	return time.UTC
}

// recurrence is the parsed form of a recurring reminder.
type recurrence struct {
	kind      string
	weekdays  int
	atMinutes int
	interval  time.Duration
	remaining int
	endsAt    time.Time
}

// rule renders the repeat rule, for example "every weekday at 07:30".
func (r recurrence) rule() string {
	switch r.kind {
	case store.ReminderDaily:
		return "every day at " + formatMinutes(r.atMinutes)
	case store.ReminderWeekdays:
		return "every weekday at " + formatMinutes(r.atMinutes)
	case store.ReminderWeekly:
		return "every " + weekdayName(r.weekdays) + " at " + formatMinutes(r.atMinutes)
	case store.ReminderInterval:
		return "every " + formatInterval(r.interval)
	default:
		return ""
	}
}

// reminder is a parsed reminder ready to hand to the scheduler.
type reminder struct {
	text      string
	delay     time.Duration
	at        time.Time
	absolute  bool
	recurring *recurrence
}

// parseReminder prefers the typed arguments a router matcher supplies and falls
// back to parsing the original text, which is what the built-in path uses.
func (m *Manager) parseReminder(t sdk.Transmission) (reminder, error) {
	if parsed, ok, err := m.parseArgs(t); ok || err != nil {
		return parsed, err
	}
	return m.parseText(t)
}

// parseArgs reads the typed form. ok reports whether any scheduling argument was
// present; a malformed argument returns ok=true with an error.
func (m *Manager) parseArgs(t sdk.Transmission) (reminder, bool, error) {
	msg := strings.TrimSpace(argString(t.Args, "msg"))

	if raw, present := t.Args["at"]; present && raw != nil {
		at, err := m.absoluteTime(raw, t.Actor())
		if err != nil {
			return reminder{}, true, err
		}
		return reminder{text: messageOr(msg), at: at, absolute: true}, true, nil
	}

	num, ok := argInt(t.Args, "num")
	if !ok {
		return reminder{}, false, nil
	}
	unit, ok := t.Args["unit"]
	if !ok {
		return reminder{}, true, sdk.Userf(`Reminder unit is required. Try "in 10 minutes".`)
	}
	delay, err := relativeDelay(num, fmt.Sprint(unit))
	if err != nil {
		return reminder{}, true, err
	}
	return reminder{text: messageOr(msg), delay: delay}, true, nil
}

// parseText parses the user-facing reminder text.
func (m *Manager) parseText(t sdk.Transmission) (reminder, error) {
	text := strings.TrimSpace(t.Text)
	if text == "" {
		return reminder{}, sdk.Userf(`Try "remind me <text> in <count> <unit>" or "remind me <text> at <datetime>".`)
	}

	text = stripTrigger(text, t.Trigger)
	text = normalizeWhitespace(text)
	text = stripMe(text)

	if msg, spec, ok := splitRecurring(text); ok {
		rec, err := m.parseRecurrence(spec, t.Actor())
		if err != nil {
			return reminder{}, err
		}
		return reminder{text: messageOr(msg), recurring: &rec}, nil
	}

	if index := strings.LastIndex(text, " in "); index >= 0 {
		return m.relativeReminder(strings.TrimSpace(text[:index]), strings.TrimSpace(text[index+len(" in "):]))
	}
	if rest, ok := strings.CutPrefix(text, "in "); ok {
		return m.relativeReminder("", strings.TrimSpace(rest))
	}

	if index := strings.LastIndex(text, " at "); index >= 0 {
		return m.absoluteReminder(strings.TrimSpace(text[:index]), strings.TrimSpace(text[index+len(" at "):]), t.Actor())
	}
	if rest, ok := strings.CutPrefix(text, "at "); ok {
		return m.absoluteReminder("", strings.TrimSpace(rest), t.Actor())
	}

	return reminder{}, sdk.Userf(`I couldn't find a schedule in that. Try "remind me <text> in 10 minutes", "remind me <text> at 2026-11-08 09:00", or "remind me <text> every weekday at 07:30".`)
}

// splitRecurring separates "<msg> every <rule>" into the message and rule. It
// only recognizes a rule that starts with a repeat word, so ordinary text such
// as "every detail" falls through to the one-shot parser.
func splitRecurring(text string) (msg, spec string, ok bool) {
	lower := strings.ToLower(text)
	const marker = " every "
	for from := len(lower); from > 0; {
		index := strings.LastIndex(lower[:from], marker)
		if index < 0 {
			break
		}
		candidate := strings.TrimSpace(text[index+len(marker):])
		if recurrenceStarter(candidate) {
			return strings.TrimSpace(text[:index]), candidate, true
		}
		from = index
	}
	if strings.HasPrefix(lower, "every ") {
		candidate := strings.TrimSpace(text[len("every "):])
		if recurrenceStarter(candidate) {
			return "", candidate, true
		}
	}
	return "", "", false
}

// recurrenceStarter reports whether spec begins with a repeat rule.
func recurrenceStarter(spec string) bool {
	fields := strings.Fields(strings.ToLower(spec))
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "day", "weekday", "weekdays":
		return true
	}
	if _, ok := parseWeekday(fields[0]); ok {
		return true
	}
	if _, err := strconv.Atoi(fields[0]); err == nil {
		return true
	}
	return false
}

// parseRecurrence parses the rule after "every", including an optional
// "until <date>" or "<count> times" suffix.
func (m *Manager) parseRecurrence(spec, actor string) (recurrence, error) {
	rec := recurrence{kind: "", remaining: store.UnlimitedReminders}
	spec = strings.TrimSpace(spec)

	if index := strings.LastIndex(strings.ToLower(spec), " until "); index >= 0 {
		value := strings.TrimSpace(spec[index+len(" until "):])
		endsAt, err := m.untilTime(value, actor)
		if err != nil {
			return recurrence{}, err
		}
		rec.endsAt = endsAt
		spec = strings.TrimSpace(spec[:index])
	}

	if count, rest, ok, err := splitCountSuffix(spec); err != nil {
		return recurrence{}, err
	} else if ok {
		if !rec.endsAt.IsZero() {
			return recurrence{}, sdk.Userf(`Use either "<count> times" or "until <date>", not both.`)
		}
		rec.remaining = count
		spec = rest
	}

	fields := strings.Fields(strings.ToLower(spec))
	switch {
	case len(fields) >= 2 && fields[0] == "day" && fields[1] == "at":
		minutes, err := parseClockFields(fields[2:])
		if err != nil {
			return recurrence{}, err
		}
		rec.kind = store.ReminderDaily
		rec.atMinutes = minutes

	case len(fields) >= 2 && (fields[0] == "weekday" || fields[0] == "weekdays") && fields[1] == "at":
		minutes, err := parseClockFields(fields[2:])
		if err != nil {
			return recurrence{}, err
		}
		rec.kind = store.ReminderWeekdays
		rec.atMinutes = minutes

	case len(fields) >= 2 && fields[1] == "at":
		weekday, ok := parseWeekday(fields[0])
		if !ok {
			return recurrence{}, recurrenceUsage()
		}
		minutes, err := parseClockFields(fields[2:])
		if err != nil {
			return recurrence{}, err
		}
		rec.kind = store.ReminderWeekly
		rec.weekdays = 1 << weekday
		rec.atMinutes = minutes

	case len(fields) == 2:
		num, err := strconv.Atoi(fields[0])
		if err != nil {
			return recurrence{}, recurrenceUsage()
		}
		delay, err := relativeDelay(num, fields[1])
		if err != nil {
			return recurrence{}, err
		}
		if delay < minRecurringInterval {
			return recurrence{}, sdk.Userf("Recurring reminders must be at least one minute apart.")
		}
		rec.kind = store.ReminderInterval
		rec.interval = delay

	default:
		return recurrence{}, recurrenceUsage()
	}

	if rec.remaining == store.UnlimitedReminders && rec.endsAt.IsZero() {
		if count, ok := parseCountPhrase(m.recurringLimit(actor)); ok && count > 0 {
			rec.remaining = count
		}
	}
	return rec, nil
}

func recurrenceUsage() error {
	return sdk.Userf(`Try "every day at 09:00", "every weekday at 07:30", "every monday at 09:00", or "every 2 hours".`)
}

// splitCountSuffix recognizes a trailing "<count> times" or "<count> time".
func splitCountSuffix(spec string) (count int, rest string, ok bool, err error) {
	fields := strings.Fields(spec)
	if len(fields) < 2 {
		return 0, spec, false, nil
	}
	unit := strings.ToLower(fields[len(fields)-1])
	if unit != "times" && unit != "time" {
		return 0, spec, false, nil
	}
	count, convErr := strconv.Atoi(fields[len(fields)-2])
	if convErr != nil {
		return 0, spec, false, sdk.Userf("Repeat count %q is not a number.", fields[len(fields)-2])
	}
	if count <= 0 {
		return 0, spec, false, sdk.Userf("How many times should it repeat? Use a positive number.")
	}
	return count, strings.TrimSpace(strings.Join(fields[:len(fields)-2], " ")), true, nil
}

// parseClockFields reads the clock after "at".
func parseClockFields(fields []string) (int, error) {
	value := strings.Join(fields, " ")
	if strings.TrimSpace(value) == "" {
		return 0, sdk.Userf(`Add a time, for example "every day at 09:00".`)
	}
	minutes, ok := parseClockOfDay(value)
	if !ok {
		return 0, sdk.Userf(`I couldn't parse the time %q. Try "09:00", "7:30pm", or "7 pm".`, value)
	}
	return minutes, nil
}

// parseClockOfDay converts a conversational clock to minutes after midnight.
func parseClockOfDay(value string) (int, bool) {
	v := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), " ", ""))
	if v == "" {
		return 0, false
	}
	if hour, err := strconv.Atoi(v); err == nil && hour >= 0 && hour <= 23 {
		return hour * 60, true
	}
	for _, layout := range []string{"15:04", "3:04pm", "3pm", "3:04"} {
		if parsed, err := time.Parse(layout, v); err == nil {
			return parsed.Hour()*60 + parsed.Minute(), true
		}
	}
	return 0, false
}

// parseWeekday maps full and three-letter weekday names.
func parseWeekday(value string) (time.Weekday, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sunday", "sun":
		return time.Sunday, true
	case "monday", "mon":
		return time.Monday, true
	case "tuesday", "tue", "tues":
		return time.Tuesday, true
	case "wednesday", "wed":
		return time.Wednesday, true
	case "thursday", "thu", "thur", "thurs":
		return time.Thursday, true
	case "friday", "fri":
		return time.Friday, true
	case "saturday", "sat":
		return time.Saturday, true
	default:
		return 0, false
	}
}

// parseCountPhrase reads a configured default such as "5 times" or "forever".
func parseCountPhrase(value string) (int, bool) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
	if len(fields) == 0 {
		return 0, false
	}
	if len(fields) == 1 {
		switch fields[0] {
		case "forever", "never", "unlimited", "none":
			return store.UnlimitedReminders, true
		}
	}
	if len(fields) == 2 && (fields[1] == "times" || fields[1] == "time") {
		if count, err := strconv.Atoi(fields[0]); err == nil && count > 0 {
			return count, true
		}
	}
	return 0, false
}

// untilTime parses the end of a recurring reminder. A bare year means the start
// of that year, so "until 2027" includes all of 2026.
func (m *Manager) untilTime(value, actor string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, sdk.Userf(`Add an end date, for example "until 2027-01-01".`)
	}
	at, err := sdk.ToUTC(value, m.timezoneName(actor), m.clock.Now())
	if err != nil {
		loc := m.location(actor)
		if date, dateErr := time.ParseInLocation("2006-01-02", value, loc); dateErr == nil {
			// A bare date means the start of that day, so "until 2027-01-01"
			// includes all of 2026.
			at = date.UTC()
		} else if year, yearErr := strconv.Atoi(value); yearErr == nil && year >= 1970 && year <= 9999 {
			at = time.Date(year, 1, 1, 0, 0, 0, 0, loc).UTC()
		} else {
			return time.Time{}, sdk.Userf(`I couldn't parse the end date %q. Try "until 2027-01-01".`, value)
		}
	}
	if !at.After(m.clock.Now()) {
		return time.Time{}, sdk.Userf("That end date is in the past.")
	}
	return at, nil
}

// timezoneName returns the principal's configured zone, defaulting to UTC.
func (m *Manager) timezoneName(principal string) string {
	if m != nil {
		if timezone := strings.TrimSpace(m.timezone(principal)); timezone != "" {
			return timezone
		}
	}
	return "UTC"
}

// nextOccurrence computes the first occurrence strictly after after. Calendar
// kinds use the principal's live timezone, so moving keeps the wall-clock time.
func nextOccurrence(rec recurrence, loc *time.Location, after time.Time) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	switch rec.kind {
	case store.ReminderInterval:
		if rec.interval <= 0 {
			return time.Time{}, false
		}
		return after.Add(rec.interval), true

	case store.ReminderDaily, store.ReminderWeekdays, store.ReminderWeekly:
		local := after.In(loc)
		for day := 0; day <= 7; day++ {
			date := local.AddDate(0, 0, day)
			candidate := time.Date(date.Year(), date.Month(), date.Day(),
				rec.atMinutes/60, rec.atMinutes%60, 0, 0, loc)
			if !candidate.After(after) {
				continue
			}
			switch rec.kind {
			case store.ReminderDaily:
				return candidate.UTC(), true
			case store.ReminderWeekdays:
				if candidate.Weekday() != time.Saturday && candidate.Weekday() != time.Sunday {
					return candidate.UTC(), true
				}
			case store.ReminderWeekly:
				if rec.weekdays&(1<<candidate.Weekday()) != 0 {
					return candidate.UTC(), true
				}
			}
		}
	}
	return time.Time{}, false
}

// scheduleDescription renders a stored recurring row for the list command.
func (m *Manager) scheduleDescription(rec store.ReminderRecord, actor string) string {
	rule := recurrenceFromRecord(rec).rule()
	switch {
	case rec.Remaining > 0:
		rule += fmt.Sprintf(", %d left", rec.Remaining)
	case !rec.EndsAt.IsZero():
		rule += ", until " + m.formatReminderDate(rec.EndsAt, actor)
	}
	return rule
}

// formatMinutes renders minutes after midnight as HH:MM.
func formatMinutes(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// weekdayName renders the single weekday in a weekly bitmask.
func weekdayName(mask int) string {
	for day := time.Sunday; day <= time.Saturday; day++ {
		if mask&(1<<day) != 0 {
			return day.String()
		}
	}
	return ""
}

// formatInterval renders an elapsed interval in its largest whole unit.
func formatInterval(interval time.Duration) string {
	switch {
	case interval%(24*time.Hour) == 0:
		return plural(int(interval/(24*time.Hour)), "day")
	case interval%time.Hour == 0:
		return plural(int(interval/time.Hour), "hour")
	default:
		return plural(int(interval/time.Minute), "minute")
	}
}

func plural(count int, unit string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", count, unit)
}

func (m *Manager) relativeReminder(msg, spec string) (reminder, error) {
	fields := strings.Fields(spec)
	if len(fields) != 2 {
		return reminder{}, sdk.Userf(`Use "in <count> <unit>", for example "in 10 minutes".`)
	}
	num, err := strconv.Atoi(fields[0])
	if err != nil {
		return reminder{}, sdk.Userf("Reminder count %q is not a number.", fields[0])
	}
	delay, err := relativeDelay(num, fields[1])
	if err != nil {
		return reminder{}, err
	}
	return reminder{text: messageOr(msg), delay: delay}, nil
}

func (m *Manager) absoluteReminder(msg, value, sender string) (reminder, error) {
	at, err := m.absoluteTime(value, sender)
	if err != nil {
		return reminder{}, err
	}
	return reminder{text: messageOr(msg), at: at, absolute: true}, nil
}

// absoluteTime converts a datetime using the sender's configured timezone and
// the manager's clock, which keeps the "today"/"next Monday" layouts testable.
func (m *Manager) absoluteTime(value any, sender string) (time.Time, error) {
	var text string
	switch v := value.(type) {
	case time.Time:
		if v.IsZero() {
			return time.Time{}, sdk.Userf("That reminder time is empty.")
		}
		return v.UTC(), nil
	case string:
		text = strings.TrimSpace(v)
	default:
		text = strings.TrimSpace(fmt.Sprint(v))
	}
	if text == "" {
		return time.Time{}, sdk.Userf("That reminder time is empty.")
	}

	timezone := strings.TrimSpace(m.timezone(sender))
	if timezone == "" {
		timezone = "UTC"
	}
	at, err := sdk.ToUTC(text, timezone, m.clock.Now())
	if err != nil {
		return time.Time{}, sdk.Userf(`I couldn't parse the reminder time %q. Try "2026-11-08 09:00", "09:00", or "Monday 09:00".`, text)
	}
	if at.Before(m.clock.Now()) {
		return time.Time{}, sdk.Userf("That time is in the past. Choose a future time.")
	}
	return at, nil
}

// relativeDelay converts <num> <unit> to a duration. Singular and plural units
// are accepted; the count must be positive.
func relativeDelay(num int, unit string) (time.Duration, error) {
	if num <= 0 {
		return 0, sdk.Userf("Reminder count must be a positive number.")
	}

	var base time.Duration
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "second", "seconds", "sec", "secs", "s":
		base = time.Second
	case "minute", "minutes", "min", "mins", "m":
		base = time.Minute
	case "hour", "hours", "hr", "hrs", "h":
		base = time.Hour
	case "day", "days", "d":
		base = time.Hour * 24
	default:
		return 0, sdk.Userf("Unknown reminder unit %q. Use seconds, minutes, hours, or days (s, min, h, d).", unit)
	}

	if int64(num) > math.MaxInt64/int64(base) {
		return 0, sdk.Userf("That reminder is too far in the future.")
	}
	return time.Duration(num) * base, nil
}

// send emits one /message response to the originating sender.
func (m *Manager) send(t sdk.Transmission, text string) error {
	if m == nil || m.client == nil {
		return errors.New("testsat: outbound client is not configured")
	}

	satellite := strings.TrimSpace(t.Satellite)
	if satellite == "" {
		satellite = SatelliteName
	}

	return m.client.Send(context.Background(), sdk.Response{
		Recipients: []string{t.ReplyTo()},
		Text:       text,
		Satellite:  satellite,
		ThreadID:   t.ThreadID,
	})
}

func requireSender(t sdk.Transmission) error {
	if strings.TrimSpace(t.Sender) == "" {
		return errors.New("testsat: sender is required")
	}
	return nil
}

func messageOr(msg string) string {
	if msg = strings.TrimSpace(msg); msg == "" {
		return defaultReminderText
	}
	return msg
}

// stripTrigger removes a leading trigger or canonical command name so both the
// raw text and a router-normalized envelope parse the same way.
func stripTrigger(text, trigger string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return text
	}
	switch strings.ToLower(fields[0]) {
	case "remind", "reminder", strings.ToLower(strings.TrimSpace(trigger)):
		return strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	}
	return text
}

func stripMe(text string) string {
	if rest, ok := strings.CutPrefix(text, "me "); ok {
		return strings.TrimSpace(rest)
	}
	return text
}

// normalizeWhitespace collapses newlines and runs of spaces into single
// spaces. It makes a schedule that starts on its own line parse like one that
// follows the message on the same line.
func normalizeWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func argString(args map[string]any, name string) string {
	if args == nil {
		return ""
	}
	value, ok := args[name]
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func argInt(args map[string]any, name string) (int, bool) {
	if args == nil {
		return 0, false
	}
	value, ok := args[name]
	if !ok || value == nil {
		return 0, false
	}

	switch n := value.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if math.Trunc(n) != n {
			return 0, false
		}
		return int(n), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		parsed, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(n)))
		if err != nil {
			return 0, false
		}
		return parsed, true
	}
}
