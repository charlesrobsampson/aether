// Package session provides Aether's in-memory multi-step conversation state.
//
// A session is keyed by (phone, satellite, thread). Sessions expire lazily: the
// store never starts a goroutine or a wall-clock timer, so a test can advance an
// injected Clock instead of sleeping. Expiry is evaluated on every lookup, and
// expired entries are removed as they are encountered.
//
// All methods are safe for concurrent use.
package session

import (
	"sort"
	"sync"
	"time"
)

// DefaultTTL is how long a session lives without being extended.
const DefaultTTL = 5 * time.Minute

// Clock supplies the current time. *testutil.FakeClock satisfies it, which keeps
// session tests hermetic.
type Clock interface {
	Now() time.Time
}

// Session is one active conversation thread. ThreadID may be empty when the
// caller does not use named threads.
type Session struct {
	Phone      string
	Satellite  string
	ThreadID   string
	StartedAt  time.Time
	LastActive time.Time
	ExpiresAt  time.Time
}

// Key identifies a session. Thread is the empty string for untagged sessions.
type Key struct {
	Phone     string
	Satellite string
	Thread    string
}

// entry adds store-private ordering so ties on LastActive still resolve to the
// most recent activity deterministically.
type entry struct {
	session Session
	seq     uint64
}

// Store is an in-memory session table.
type Store struct {
	mu       sync.Mutex
	clock    Clock
	ttl      time.Duration
	seq      uint64
	sessions map[Key]*entry
}

// New returns a Store using the default five-minute TTL.
func New(clock Clock) *Store {
	return NewWithTTL(clock, DefaultTTL)
}

// NewWithTTL returns a Store whose new sessions live for ttl. A non-positive ttl
// falls back to DefaultTTL.
func NewWithTTL(clock Clock, ttl time.Duration) *Store {
	if clock == nil {
		panic("session: nil Clock")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Store{
		clock:    clock,
		ttl:      ttl,
		sessions: make(map[Key]*entry),
	}
}

// Start creates or restarts the session for (phone, satellite, thread) using the
// store's default TTL. Starting an existing key replaces it.
func (s *Store) Start(phone, satellite, thread string) Session {
	return s.StartWithTTL(phone, satellite, thread, s.ttl)
}

// StartWithTTL is Start with an explicit lifetime, used when a satellite
// configures its own TTL. A non-positive ttl falls back to the store default.
func (s *Store) StartWithTTL(phone, satellite, thread string, ttl time.Duration) Session {
	if ttl <= 0 {
		ttl = s.ttl
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	s.pruneLocked(now)

	s.seq++
	sess := Session{
		Phone:      phone,
		Satellite:  satellite,
		ThreadID:   thread,
		StartedAt:  now,
		LastActive: now,
		ExpiresAt:  now.Add(ttl),
	}
	s.sessions[Key{Phone: phone, Satellite: satellite, Thread: thread}] = &entry{
		session: sess,
		seq:     s.seq,
	}
	return sess
}

// Get returns a live session and marks it as the phone's most recently active
// thread. An expired session is removed and reported as absent.
func (s *Store) Get(phone, satellite, thread string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	key := Key{Phone: phone, Satellite: satellite, Thread: thread}
	e, ok := s.sessions[key]
	if !ok {
		return Session{}, false
	}
	if !now.Before(e.session.ExpiresAt) {
		delete(s.sessions, key)
		return Session{}, false
	}

	s.seq++
	e.seq = s.seq
	e.session.LastActive = now
	return e.session, true
}

// Extend resets a live session's deadline to now+ttl and marks it active. It
// reports false when the session does not exist or has already expired.
func (s *Store) Extend(phone, satellite, thread string, ttl time.Duration) bool {
	if ttl <= 0 {
		ttl = s.ttl
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	key := Key{Phone: phone, Satellite: satellite, Thread: thread}
	e, ok := s.sessions[key]
	if !ok {
		return false
	}
	if !now.Before(e.session.ExpiresAt) {
		delete(s.sessions, key)
		return false
	}

	s.seq++
	e.seq = s.seq
	e.session.LastActive = now
	e.session.ExpiresAt = now.Add(ttl)
	return true
}

// Close removes the session. It reports whether a live session was removed;
// closing an unknown or already-expired session is a no-op returning false.
func (s *Store) Close(phone, satellite, thread string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	key := Key{Phone: phone, Satellite: satellite, Thread: thread}
	e, ok := s.sessions[key]
	if !ok {
		return false
	}
	delete(s.sessions, key)
	return now.Before(e.session.ExpiresAt)
}

// MostRecent returns the live session for phone with the most recent activity.
// Ties (for example two sessions started at the same instant) are broken by
// activity order. Finding the session counts as activity, so a lookup for
// #thread-id makes that thread the default until another session is used.
func (s *Store) MostRecent(phone string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	var best *entry
	for key, e := range s.sessions {
		if key.Phone != phone {
			continue
		}
		if !now.Before(e.session.ExpiresAt) {
			delete(s.sessions, key)
			continue
		}
		if best == nil || e.seq > best.seq {
			best = e
		}
	}
	if best == nil {
		return Session{}, false
	}

	s.seq++
	best.seq = s.seq
	best.session.LastActive = now
	return best.session, true
}

// List returns the live sessions for phone, most recently active first.
// Expired sessions are removed as they are encountered.
func (s *Store) List(phone string) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	type pair struct {
		session Session
		seq     uint64
	}
	var live []pair
	for key, e := range s.sessions {
		if key.Phone != phone {
			continue
		}
		if !now.Before(e.session.ExpiresAt) {
			delete(s.sessions, key)
			continue
		}
		live = append(live, pair{session: e.session, seq: e.seq})
	}
	sort.Slice(live, func(i, j int) bool {
		return live[i].seq > live[j].seq
	})

	out := make([]Session, len(live))
	for i, item := range live {
		out[i] = item.session
	}
	return out
}

// pruneLocked removes every expired session. The caller must hold s.mu.
func (s *Store) pruneLocked(now time.Time) {
	for key, e := range s.sessions {
		if !now.Before(e.session.ExpiresAt) {
			delete(s.sessions, key)
		}
	}
}
