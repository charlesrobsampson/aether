package session

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/testutil"
)

func TestSessionLifecycle(t *testing.T) {
	start := time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC)
	clock := testutil.NewFakeClock(start)
	store := New(clock)

	const (
		phone     = "+15555550102"
		satellite = "jellyfin"
		thread    = "main"
	)

	if got, ok := store.Get(phone, satellite, thread); ok {
		t.Fatalf("Get on an empty store returned %+v", got)
	}
	if got, ok := store.MostRecent(phone); ok {
		t.Fatalf("MostRecent on an empty store returned %+v", got)
	}
	if got := store.List(phone); len(got) != 0 {
		t.Fatalf("List on an empty store returned %+v", got)
	}
	if store.Extend(phone, satellite, thread, time.Minute) {
		t.Fatal("Extend succeeded for a session that does not exist")
	}

	started := store.Start(phone, satellite, thread)
	if started.Phone != phone || started.Satellite != satellite || started.ThreadID != thread {
		t.Fatalf("Start returned wrong key: %+v", started)
	}
	if !started.StartedAt.Equal(start) || !started.LastActive.Equal(start) {
		t.Fatalf("Start timestamps = (%v, %v), want both %v",
			started.StartedAt, started.LastActive, start)
	}
	if want := start.Add(DefaultTTL); !started.ExpiresAt.Equal(want) {
		t.Fatalf("Start ExpiresAt = %v, want %v (default TTL)", started.ExpiresAt, want)
	}

	got, ok := store.Get(phone, satellite, thread)
	if !ok {
		t.Fatal("Get did not find the session that was just started")
	}
	if got != started {
		t.Fatalf("Get = %+v, want %+v", got, started)
	}

	// Extend resets the deadline from the current clock time.
	clock.Advance(4 * time.Minute)
	if !store.Extend(phone, satellite, thread, 2*time.Minute) {
		t.Fatal("Extend did not find a live session")
	}
	got, ok = store.Get(phone, satellite, thread)
	if !ok {
		t.Fatal("Get did not find the extended session")
	}
	if want := start.Add(6 * time.Minute); !got.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt after Extend = %v, want %v", got.ExpiresAt, want)
	}

	// One minute before the new deadline the session is still live; exactly at
	// the deadline it is gone.
	clock.Advance(time.Minute)
	if _, ok := store.Get(phone, satellite, thread); !ok {
		t.Fatal("session expired before its deadline")
	}
	clock.Advance(time.Minute)
	if _, ok := store.Get(phone, satellite, thread); ok {
		t.Fatal("session is still live at its deadline")
	}

	// A fresh session can be closed exactly once.
	clock.Advance(time.Minute)
	store.Start(phone, satellite, thread)
	if !store.Close(phone, satellite, thread) {
		t.Fatal("Close did not report the live session")
	}
	if _, ok := store.Get(phone, satellite, thread); ok {
		t.Fatal("Get found a session after Close")
	}
	if store.Close(phone, satellite, thread) {
		t.Fatal("second Close reported a live session")
	}

	// StartWithTTL honours an explicit lifetime independent of the default.
	short := store.StartWithTTL(phone, satellite, "short", 30*time.Second)
	if want := clock.Now().Add(30 * time.Second); !short.ExpiresAt.Equal(want) {
		t.Fatalf("StartWithTTL ExpiresAt = %v, want %v", short.ExpiresAt, want)
	}
	if got := store.List(phone); len(got) != 1 || got[0].ThreadID != "short" {
		t.Fatalf("List = %+v, want only the short-lived session", got)
	}

	t.Run("concurrent", func(t *testing.T) {
		concurrentClock := testutil.NewFakeClock(start)
		concurrent := New(concurrentClock)

		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()

				phone := fmt.Sprintf("+1555555%04d", i%8)
				satellite := fmt.Sprintf("sat-%d", i%3)
				thread := fmt.Sprintf("thread-%d", i%2)

				concurrent.Start(phone, satellite, thread)
				concurrent.Get(phone, satellite, thread)
				concurrent.Extend(phone, satellite, thread, time.Minute)
				concurrent.MostRecent(phone)
				concurrent.List(phone)
				if i%4 == 0 {
					concurrent.Close(phone, satellite, thread)
				}
			}(i)
		}
		wg.Wait()
	})
}

func TestThreadTargeting(t *testing.T) {
	start := time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC)
	clock := testutil.NewFakeClock(start)
	store := New(clock)

	const (
		phone     = "+15555550102"
		other     = "+15555550103"
		satellite = "jellyfin"
	)

	store.Start(phone, satellite, "jellyfin")
	clock.Advance(time.Minute)
	store.Start(phone, satellite, "music")

	// Without a tag the most recently active session wins.
	got, ok := store.MostRecent(phone)
	if !ok {
		t.Fatal("MostRecent did not find any session")
	}
	if got.ThreadID != "music" {
		t.Fatalf("MostRecent = %q, want %q", got.ThreadID, "music")
	}

	// A #thread-id in the message selects that thread once the router strips
	// the leading '#'. Looking it up also makes it the most recent thread.
	target := strings.TrimPrefix("#jellyfin", "#")
	got, ok = store.Get(phone, satellite, target)
	if !ok {
		t.Fatalf("Get(#%s) did not find the session", target)
	}
	if got.ThreadID != "jellyfin" {
		t.Fatalf("Get(#%s).ThreadID = %q, want %q", target, got.ThreadID, "jellyfin")
	}
	got, ok = store.MostRecent(phone)
	if !ok || got.ThreadID != "jellyfin" {
		t.Fatalf("MostRecent after targeting #jellyfin = (%+v, %v), want jellyfin", got, ok)
	}

	// Threads from different phones do not leak into each other.
	if _, ok := store.MostRecent(other); ok {
		t.Fatal("MostRecent found a session for a phone that never sent one")
	}
	store.Start(other, satellite, "news")
	if got := store.List(other); len(got) != 1 || got[0].ThreadID != "news" {
		t.Fatalf("List(other) = %+v, want only the news thread", got)
	}

	// List returns the phone's most recently active session first.
	list := store.List(phone)
	if len(list) != 2 {
		t.Fatalf("List(phone) returned %d sessions, want 2: %+v", len(list), list)
	}
	if list[0].ThreadID != "jellyfin" {
		t.Fatalf("List(phone)[0] = %q, want jellyfin (most recent)", list[0].ThreadID)
	}

	// Unknown threads and expired sessions are treated as absent.
	if _, ok := store.Get(phone, satellite, "unknown"); ok {
		t.Fatal("Get found a thread that was never started")
	}
	clock.Advance(DefaultTTL)
	if got, ok := store.MostRecent(phone); ok {
		t.Fatalf("MostRecent returned the expired session %+v", got)
	}
	if got := store.List(phone); len(got) != 0 {
		t.Fatalf("List returned expired sessions: %+v", got)
	}
}
