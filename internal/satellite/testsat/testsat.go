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
	"time"

	"github.com/charlesrobsampson/aether/internal/scheduler"
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

// Options configures a Manager.
type Options struct {
	// Client posts responses to Aether. A nil client makes every command fail.
	Client Sender
	// Scheduler owns the reminder timers. A nil scheduler rejects reminders.
	Scheduler *scheduler.Scheduler
	// Clock is the reference time for datetimes that omit a date. Defaults to
	// the system clock.
	Clock Clock
	// Timezone resolves the sender's IANA zone for absolute reminders. A nil
	// resolver uses UTC.
	Timezone func(sender string) string
	// OnError observes a delivery failure from a fired reminder. The scheduler
	// callback cannot return an error, so it is reported here instead. Nil
	// discards it.
	OnError func(error)
}

// Manager handles the built-in test and reminder commands. All fields are
// immutable after New, so it is safe for concurrent use.
type Manager struct {
	client    Sender
	scheduler *scheduler.Scheduler
	clock     Clock
	timezone  func(string) string
	onError   func(error)
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
	return &Manager{
		client:    options.Client,
		scheduler: options.Scheduler,
		clock:     clock,
		timezone:  timezone,
		onError:   options.OnError,
	}
}

// HandleTest replies "test received" to the originating sender (plan §3.16).
func (m *Manager) HandleTest(t sdk.Transmission) error {
	if err := requireSender(t); err != nil {
		return err
	}
	return m.send(t, testReply)
}

// HandleRemind schedules one reminder back to the originating sender. It
// accepts the typed form produced by a router matcher (num/unit or at plus msg)
// and the canonical text form:
//
//	remind me <msg> in <num> <unit>
//	remind me <msg> at <datetime>
//	in <num> <unit>
//	at <datetime>
//
// A message omitted by the short form defaults to "Reminder". Relative units
// are seconds, minutes, hours, and days.
func (m *Manager) HandleRemind(t sdk.Transmission) error {
	if err := requireSender(t); err != nil {
		return err
	}
	if m == nil || m.scheduler == nil {
		return errors.New("testsat: reminder scheduler is not configured")
	}

	reminder, err := m.parseReminder(t)
	if err != nil {
		return err
	}

	fire := func() {
		if err := m.send(t, reminder.text); err != nil && m.onError != nil {
			m.onError(err)
		}
	}
	if reminder.absolute {
		m.scheduler.At(reminder.at, fire)
	} else {
		m.scheduler.After(reminder.delay, fire)
	}
	return nil
}

// reminder is a parsed reminder ready to hand to the scheduler.
type reminder struct {
	text     string
	delay    time.Duration
	at       time.Time
	absolute bool
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
		at, err := m.absoluteTime(raw, t.Sender)
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
		return reminder{}, true, errors.New("testsat: reminder unit is required")
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
		return reminder{}, errors.New(`testsat: expected "remind me <msg> in <num> <unit>" or "remind me <msg> at <datetime>"`)
	}

	text = stripTrigger(text, t.Trigger)
	text = stripMe(text)

	if index := strings.LastIndex(text, " in "); index >= 0 {
		return m.relativeReminder(strings.TrimSpace(text[:index]), strings.TrimSpace(text[index+len(" in "):]))
	}
	if rest, ok := strings.CutPrefix(text, "in "); ok {
		return m.relativeReminder("", strings.TrimSpace(rest))
	}

	if index := strings.LastIndex(text, " at "); index >= 0 {
		return m.absoluteReminder(strings.TrimSpace(text[:index]), strings.TrimSpace(text[index+len(" at "):]), t.Sender)
	}
	if rest, ok := strings.CutPrefix(text, "at "); ok {
		return m.absoluteReminder("", strings.TrimSpace(rest), t.Sender)
	}

	return reminder{}, fmt.Errorf("testsat: %q is not a reminder", t.Text)
}

func (m *Manager) relativeReminder(msg, spec string) (reminder, error) {
	fields := strings.Fields(spec)
	if len(fields) != 2 {
		return reminder{}, fmt.Errorf("testsat: invalid relative reminder %q", spec)
	}
	num, err := strconv.Atoi(fields[0])
	if err != nil {
		return reminder{}, fmt.Errorf("testsat: invalid reminder count %q", fields[0])
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
			return time.Time{}, errors.New("testsat: reminder time is empty")
		}
		return v.UTC(), nil
	case string:
		text = strings.TrimSpace(v)
	default:
		text = strings.TrimSpace(fmt.Sprint(v))
	}
	if text == "" {
		return time.Time{}, errors.New("testsat: reminder time is empty")
	}

	timezone := strings.TrimSpace(m.timezone(sender))
	if timezone == "" {
		timezone = "UTC"
	}
	at, err := sdk.ToUTC(text, timezone, m.clock.Now())
	if err != nil {
		return time.Time{}, fmt.Errorf("testsat: %w", err)
	}
	return at, nil
}

// relativeDelay converts <num> <unit> to a duration. Singular and plural units
// are accepted; the count must be positive.
func relativeDelay(num int, unit string) (time.Duration, error) {
	if num <= 0 {
		return 0, fmt.Errorf("testsat: reminder count must be positive, got %d", num)
	}

	var base time.Duration
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "second", "seconds":
		base = time.Second
	case "minute", "minutes":
		base = time.Minute
	case "hour", "hours":
		base = time.Hour
	case "day", "days":
		base = time.Hour * 24
	default:
		return 0, fmt.Errorf("testsat: unknown reminder unit %q", unit)
	}

	if int64(num) > math.MaxInt64/int64(base) {
		return 0, errors.New("testsat: reminder duration is too large")
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
		Recipients: []string{t.Sender},
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
	switch fields[0] {
	case "remind", "reminder", strings.TrimSpace(trigger):
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
