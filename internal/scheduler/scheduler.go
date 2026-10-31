// Package scheduler schedules one-shot and repeating work against an injectable
// clock.
//
// robfig/cron is used only to compute the next activation of a five-field
// calendar specification; firing is driven by Clock.AfterFunc so tests advance a
// fake clock instead of sleeping. Relative and absolute one-shot timers use the
// same mechanism.
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Clock supplies the current time and schedules delayed callbacks. The returned
// function cancels the timer and is safe to call more than once.
// *testutil.FakeClock satisfies Clock, which keeps scheduler tests hermetic.
type Clock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) func()
}

// ID identifies a scheduled activation. IDs are unique for the lifetime of a
// Scheduler.
type ID uint64

// entry is one scheduled activation. A nil schedule means the entry is a
// one-shot timer; a non-nil schedule is re-armed after every activation.
type entry struct {
	id     ID
	fn     func()
	sched  cron.Schedule
	cancel func()
}

// Scheduler owns the timers registered against one Clock. All methods are safe
// for concurrent use, and callbacks may call back into the Scheduler.
type Scheduler struct {
	clock Clock

	mu      sync.Mutex
	nextID  ID
	entries map[ID]*entry
}

// New returns a Scheduler driven by clock.
func New(clock Clock) *Scheduler {
	if clock == nil {
		panic("scheduler: nil Clock")
	}
	return &Scheduler{clock: clock, entries: make(map[ID]*entry)}
}

// After runs fn once, d from now. A negative duration fires at the next clock
// tick.
func (s *Scheduler) After(d time.Duration, fn func()) ID {
	if d < 0 {
		d = 0
	}
	return s.addOneShot(d, fn)
}

// At runs fn once at the absolute instant at. An instant that has already
// passed is clamped to now and fires at the next clock tick.
func (s *Scheduler) At(at time.Time, fn func()) ID {
	return s.After(at.Sub(s.clock.Now()), fn)
}

// Every runs fn at each activation of the five-field cron spec, starting from
// the next activation after the current clock time. Six-field (seconds) and
// descriptor specs are rejected.
func (s *Scheduler) Every(spec string, fn func()) (ID, error) {
	sched, err := parseSpec(spec)
	if err != nil {
		return 0, err
	}
	if sched.Next(s.clock.Now()).IsZero() {
		return 0, fmt.Errorf("scheduler: cron spec %q has no future activation", spec)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	e := &entry{id: s.nextID, fn: fn, sched: sched}
	s.entries[e.id] = e
	s.armLocked(e)
	return e.id, nil
}

// Cancel stops a scheduled activation. It reports whether the ID was live;
// cancelling an unknown or already-fired ID is a no-op returning false.
func (s *Scheduler) Cancel(id ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[id]
	if !ok {
		return false
	}
	delete(s.entries, id)
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	return true
}

// Next reports the first activation of the five-field cron spec after now.
// specs with a different field count and invalid specs are rejected.
func Next(spec string, now time.Time) (time.Time, error) {
	sched, err := parseSpec(spec)
	if err != nil {
		return time.Time{}, err
	}
	next := sched.Next(now)
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("scheduler: cron spec %q has no future activation", spec)
	}
	return next, nil
}

// addOneShot registers a non-repeating timer.
func (s *Scheduler) addOneShot(d time.Duration, fn func()) ID {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	e := &entry{id: s.nextID, fn: fn}
	s.entries[e.id] = e
	e.cancel = s.clock.AfterFunc(d, func() { s.fireOneShot(e.id) })
	return e.id
}

// armLocked schedules the entry's next cron activation. The caller must hold
// s.mu.
func (s *Scheduler) armLocked(e *entry) {
	now := s.clock.Now()
	next := e.sched.Next(now)
	if next.IsZero() {
		e.cancel = nil
		return
	}
	e.cancel = s.clock.AfterFunc(next.Sub(now), func() { s.fireRecurring(e.id) })
}

// fireOneShot removes and runs a one-shot entry. Removing it before the callback
// means Cancel observes the timer as already fired.
func (s *Scheduler) fireOneShot(id ID) {
	s.mu.Lock()
	e, ok := s.entries[id]
	if ok {
		delete(s.entries, id)
		e.cancel = nil
	}
	s.mu.Unlock()

	if ok && e.fn != nil {
		e.fn()
	}
}

// fireRecurring runs a recurring entry and re-arms it from the clock's current
// time. The entry is re-armed only if the callback did not cancel it.
func (s *Scheduler) fireRecurring(id ID) {
	s.mu.Lock()
	e, ok := s.entries[id]
	if ok {
		e.cancel = nil
	}
	s.mu.Unlock()

	if !ok {
		return
	}
	if e.fn != nil {
		e.fn()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; ok {
		s.armLocked(e)
	}
}

// parseSpec accepts exactly five whitespace-separated cron fields and returns
// robfig's standard schedule for them.
func parseSpec(spec string) (cron.Schedule, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("scheduler: empty cron spec")
	}
	if fields := len(strings.Fields(spec)); fields != 5 {
		return nil, fmt.Errorf("scheduler: cron spec %q has %s fields, want 5",
			spec, strconv.Itoa(fields))
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, fmt.Errorf("scheduler: invalid cron spec %q: %w", spec, err)
	}
	return sched, nil
}
