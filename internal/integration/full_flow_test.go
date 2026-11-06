// Package integration exercises Aether's subsystems together over its real
// HTTP surface. Tests here start the application in-process with a temp config
// tree, SQLite, a fake clock, and a fake messenger: no external services, no
// $HOME, and no sleeps.
package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/app"
	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/metrics"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/internal/testutil"
	"github.com/charlesrobsampson/aether/internal/transport"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

// TestFullFlow walks the acceptance path from plan §5: an admin mints a JWT,
// registers a satellite, sends it a typed inbound command, locks a session, lets
// the user answer inside that session, and closes it. Every outbound message
// goes through the real queue, and the bulk/outbound counters are checked.
func TestFullFlow(t *testing.T) {
	const (
		secret     = "integration-secret"
		adminPhone = "+15555550101"
		userPhone  = "+15555550102"
	)

	root := testRoot(t)

	recorder := newSatelliteRecorder()
	satellite := httptest.NewServer(recorder)
	defer satellite.Close()

	clock := newStepClock(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC))
	watcher := newFakeWatcher()
	defer watcher.close()
	messenger := newRecordingMessenger()
	reloaded := make(chan struct{}, 1)

	baseURL, stop := startApp(t, app.Options{
		ConfigDir: root,
		Clock:     clock,
		Watcher:   watcher,
		OnReload:  func(*config.Config) { reloaded <- struct{}{} },
	}, messenger)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("Run returned %v, want clean shutdown", err)
		}
	}()

	// The satellite itself authenticates as an admin, exactly like an operator.
	token, err := auth.Issue(secret, adminPhone, auth.Admin, time.Minute)
	if err != nil {
		t.Fatalf("issue JWT: %v", err)
	}

	// Register the satellite through the protected endpoint.
	document := config.SatelliteConfig{
		Name:     "watchtv",
		Trigger:  "watch",
		Endpoint: satellite.URL,
		Commands: []config.CommandConfig{
			{
				Name:     "watch",
				Path:     "/watch",
				Patterns: []string{"watch <genre>"},
				Args:     []config.ArgDef{{Name: "genre", Type: "string"}},
			},
			{
				// The reply pattern has no trigger: a session intercepts the
				// message before trigger parsing (plan §3.7).
				Name:     "choose",
				Path:     "/choose",
				Patterns: []string{"<choice>"},
				Args:     []config.ArgDef{{Name: "choice", Type: "string"}},
			},
		},
	}
	rawDocument, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal satellite: %v", err)
	}
	if status, body := postJSON(t, baseURL+"/register-satellite", string(rawDocument), map[string]string{
		"Authorization": "Bearer " + token,
	}); status != http.StatusCreated {
		t.Fatalf("register status = %d (%s), want 201", status, body)
	}

	// The running app sees a registered satellite after the config watcher
	// reloads, which is what happens on a real host via fsnotify.
	watcher.trigger()
	<-clock.armed
	clock.Advance(100 * time.Millisecond)
	select {
	case <-reloaded:
	case <-time.After(2 * time.Second):
		t.Fatal("config did not reload after satellite registration")
	}

	before := metrics.Default.Snapshot()

	// Inbound command: !watch sci-fi becomes a typed watch command.
	if status, body := postJSON(t, baseURL+"/receive", `{"sender":"`+userPhone+`","text":"!watch sci-fi"}`, nil); status != http.StatusAccepted {
		t.Fatalf("/receive status = %d (%s), want 202", status, body)
	}

	watch := recorder.wait(t)
	if watch.Path != "/watch" {
		t.Fatalf("satellite path = %q, want /watch", watch.Path)
	}
	if watch.Transmission.Command != "watch" || watch.Transmission.Trigger != "watch" {
		t.Fatalf("command/trigger = %q/%q, want watch/watch", watch.Transmission.Command, watch.Transmission.Trigger)
	}
	if watch.Transmission.Satellite != "watchtv" || watch.Transmission.Sender != userPhone {
		t.Fatalf("metadata = satellite %q sender %q, want watchtv/%s", watch.Transmission.Satellite, watch.Transmission.Sender, userPhone)
	}
	if got, _ := watch.Transmission.Args["genre"]; got != "sci-fi" {
		t.Fatalf("typed args = %v, want genre string sci-fi", watch.Transmission.Args)
	}

	// The satellite answers through the SDK and locks a session.
	satelliteClient := sdk.NewClient(baseURL, token)
	session := sdk.NewSessionManager([]string{userPhone}, "watchtv", "")
	lock := session.Lock(5 * time.Minute)
	lock.Text = "Pick one: 1 drama, 2 sci-fi"
	if err := satelliteClient.Send(context.Background(), lock); err != nil {
		t.Fatalf("send session lock: %v", err)
	}

	// The lock reply is one queued delivery.
	first := messenger.wait(t)
	if first.Recipient != userPhone || first.Text != "Pick one: 1 drama, 2 sci-fi" {
		t.Fatalf("first delivery = %+v, want the session prompt", first)
	}

	// A bare "2" is intercepted by the active session and routed to the reply
	// pattern rather than being parsed as a new trigger.
	if status, body := postJSON(t, baseURL+"/receive", `{"sender":"`+userPhone+`","text":"2"}`, nil); status != http.StatusAccepted {
		t.Fatalf("session reply status = %d (%s), want 202", status, body)
	}

	choice := recorder.wait(t)
	if choice.Path != "/choose" {
		t.Fatalf("session path = %q, want /choose", choice.Path)
	}
	if choice.Transmission.Command != "choose" || choice.Transmission.Trigger != "watch" {
		t.Fatalf("session command/trigger = %q/%q, want choose/watch", choice.Transmission.Command, choice.Transmission.Trigger)
	}
	if choice.Transmission.ThreadID != "watchtv" {
		t.Fatalf("session thread = %q, want watchtv", choice.Transmission.ThreadID)
	}
	if got, _ := choice.Transmission.Args["choice"]; got != "2" {
		t.Fatalf("session args = %v, want choice string 2", choice.Transmission.Args)
	}

	// The satellite closes the session and reports completion.
	closing := session.Close()
	closing.Text = "Closed"
	if err := satelliteClient.Send(context.Background(), closing); err != nil {
		t.Fatalf("send session close: %v", err)
	}

	second := messenger.wait(t)
	if second.Recipient != userPhone || second.Text != "Closed" {
		t.Fatalf("second delivery = %+v, want the close message", second)
	}

	// The close removed the session, so the same bare "2" is now an unknown
	// trigger and must not reach the satellite. The dispatch worker may still
	// be running; shutdown drains it before the test returns.
	if status, body := postJSON(t, baseURL+"/receive", `{"sender":"`+userPhone+`","text":"2"}`, nil); status != http.StatusAccepted {
		t.Fatalf("post-close status = %d (%s), want 202", status, body)
	}
	afterClose := recorder.count()
	if afterClose != 2 {
		t.Fatalf("satellite calls after close = %d, want 2 (session must be closed)", afterClose)
	}
	t.Logf("satellite calls after close = %d", afterClose)

	// Both replies were delivered through the queue; no bulk request occurred.
	if calls := messenger.snapshot(); len(calls) != 2 {
		t.Fatalf("messenger calls = %d, want exactly 2", len(calls))
	}

	after := metrics.Default.Snapshot()
	if after.BulkMessageRequests != before.BulkMessageRequests {
		t.Fatalf("bulk counter = %d, want unchanged at %d", after.BulkMessageRequests, before.BulkMessageRequests)
	}
	if after.OutboundJobs != before.OutboundJobs+2 {
		t.Fatalf("outbound counter = %d, want %d", after.OutboundJobs, before.OutboundJobs+2)
	}
	t.Logf("bulk counter unchanged at %d", after.BulkMessageRequests)
	t.Logf("outbound counter increased %d -> %d", before.OutboundJobs, after.OutboundJobs)
}

// TestReceiveReturnsBeforeSlowSatellite proves /receive only enqueues: a
// satellite blocked in its handler cannot delay the 202, and the route log is
// written by the dispatch worker afterwards.
func TestReceiveReturnsBeforeSlowSatellite(t *testing.T) {
	const userPhone = "+15555550102"

	root, dbPath := testRootWithDB(t)

	slow := newGatedSatellite()
	satellite := httptest.NewServer(slow)
	defer satellite.Close()

	testutil.WriteJSON(t, filepath.Join(root, "satellites", "slow.json"), config.SatelliteConfig{
		Name:     "slow",
		Trigger:  "slow",
		Endpoint: satellite.URL,
		Commands: []config.CommandConfig{
			{Name: "slow", Path: "/slow", Patterns: []string{"slow"}},
		},
	})

	clock := newStepClock(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC))
	watcher := newFakeWatcher()
	defer watcher.close()

	baseURL, stop := startApp(t, app.Options{
		ConfigDir: root,
		Clock:     clock,
		Watcher:   watcher,
	}, newRecordingMessenger())
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("Run returned %v, want clean shutdown", err)
		}
	}()

	start := time.Now()
	status, body := postJSON(t, baseURL+"/receive", `{"sender":"`+userPhone+`","text":"!slow"}`, nil)
	elapsed := time.Since(start)
	if status != http.StatusAccepted {
		t.Fatalf("/receive status = %d (%s), want 202", status, body)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("/receive took %s, want it to return before the satellite responds", elapsed)
	}

	// The dispatch worker is now blocked inside the satellite; release it.
	select {
	case <-slow.started:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never reached the slow satellite")
	}
	close(slow.release)

	select {
	case call := <-slow.got:
		if call.Path != "/slow" {
			t.Fatalf("satellite path = %q, want /slow", call.Path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow satellite never completed the dispatch")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store for audit: %v", err)
	}
	defer st.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int
		err := st.DB().QueryRow(
			`SELECT COUNT(*) FROM route_log WHERE direction = 'inbound' AND satellite = 'slow'`,
		).Scan(&count)
		if err == nil && count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("route_log inbound/slow rows = %d (err %v), want 1", count, err)
		}
		<-time.After(10 * time.Millisecond)
	}
}

// gatedSatellite blocks every dispatch until the test releases it, so the test
// can prove the HTTP ACK happened while the satellite was still working.
type gatedSatellite struct {
	started chan struct{}
	release chan struct{}
	got     chan recordedCall
	once    sync.Once
}

func newGatedSatellite() *gatedSatellite {
	return &gatedSatellite{
		started: make(chan struct{}),
		release: make(chan struct{}),
		got:     make(chan recordedCall, 1),
	}
}

func (s *gatedSatellite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	transmission, err := sdk.ParseWebhook(r)
	if err != nil {
		http.Error(w, "bad envelope", http.StatusBadRequest)
		return
	}
	s.once.Do(func() { close(s.started) })
	<-s.release
	select {
	case s.got <- recordedCall{Transmission: transmission, Path: r.URL.Path}:
	default:
	}
	w.WriteHeader(http.StatusOK)
}

// testRoot copies the example tree into a temp dir, points SQLite at a temp
// file, disables background schedules, and pins the JWT secret.
func testRoot(t *testing.T) string {
	t.Helper()
	root, _ := testRootWithDB(t)
	return root
}

// testRootWithDB is testRoot plus the SQLite path, for tests that inspect the
// audit tables while the app is running.
func testRootWithDB(t *testing.T) (string, string) {
	t.Helper()

	root := testutil.TempConfig(t)
	cfg, err := config.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "aether.db")
	cfg.System.Server.DBPath = dbPath
	cfg.System.Schedules = nil
	cfg.System.Auth.JWTSecret = "integration-secret"
	testutil.WriteJSON(t, filepath.Join(root, "system.json"), cfg.System)
	return root, dbPath
}

// recordingMessenger records deliveries and lets the test wait for one without
// polling or sleeping.
type recordingMessenger struct {
	mu    sync.Mutex
	calls []transport.Call
	sent  chan transport.Call
}

func newRecordingMessenger() *recordingMessenger {
	return &recordingMessenger{sent: make(chan transport.Call, 8)}
}

func (m *recordingMessenger) Send(_ context.Context, recipient, text string) error {
	call := transport.Call{Recipient: recipient, Text: text}
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	select {
	case m.sent <- call:
	default:
	}
	return nil
}

func (m *recordingMessenger) Name() string { return "recording" }

func (m *recordingMessenger) wait(t *testing.T) transport.Call {
	t.Helper()
	select {
	case call := <-m.sent:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an outbound delivery")
		return transport.Call{}
	}
}

func (m *recordingMessenger) snapshot() []transport.Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]transport.Call, len(m.calls))
	copy(out, m.calls)
	return out
}

// recordedCall is one dispatch envelope as the satellite saw it.
type recordedCall struct {
	Transmission *sdk.Transmission
	Path         string
}

// satelliteRecorder is the mock satellite. It parses the standard SDK webhook
// envelope and records it for assertions.
type satelliteRecorder struct {
	mu    sync.Mutex
	calls []recordedCall
	got   chan recordedCall
}

func newSatelliteRecorder() *satelliteRecorder {
	return &satelliteRecorder{got: make(chan recordedCall, 8)}
}

func (s *satelliteRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	transmission, err := sdk.ParseWebhook(r)
	if err != nil {
		http.Error(w, "bad envelope", http.StatusBadRequest)
		return
	}
	call := recordedCall{Transmission: transmission, Path: r.URL.Path}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	select {
	case s.got <- call:
	default:
	}
	w.WriteHeader(http.StatusOK)
}

func (s *satelliteRecorder) wait(t *testing.T) recordedCall {
	t.Helper()
	select {
	case call := <-s.got:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a satellite dispatch")
		return recordedCall{}
	}
}

func (s *satelliteRecorder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// fakeWatcher is an injectable config.Watcher so the test controls reloads.
type fakeWatcher struct {
	events chan struct{}
	once   sync.Once
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{events: make(chan struct{})}
}

func (w *fakeWatcher) Events() <-chan struct{} { return w.events }

func (w *fakeWatcher) trigger() { w.events <- struct{}{} }

func (w *fakeWatcher) close() { w.once.Do(func() { close(w.events) }) }

// stepClock exposes each timer registration so the test advances the fake clock
// only after the watcher has armed its debounce timer.
type stepClock struct {
	*testutil.FakeClock
	armed chan struct{}
}

func newStepClock(start time.Time) *stepClock {
	return &stepClock{FakeClock: testutil.NewFakeClock(start), armed: make(chan struct{}, 8)}
}

func (c *stepClock) AfterFunc(d time.Duration, fn func()) func() {
	cancel := c.FakeClock.AfterFunc(d, fn)
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return cancel
}

// startApp runs Aether in the background and returns its base URL plus a stop
// function. It waits for readiness, so the test never sleeps.
func startApp(t *testing.T, opts app.Options, messenger transport.Messenger) (string, func() error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	opts.Messenger = messenger
	opts.Addr = "127.0.0.1:0"
	opts.OnReady = func(addr string) { ready <- addr }

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx, opts) }()

	select {
	case addr := <-ready:
		return "http://" + addr, func() error {
			cancel()
			select {
			case err := <-errCh:
				return err
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return after cancellation")
				return nil
			}
		}
	case err := <-errCh:
		cancel()
		t.Fatalf("Run exited before becoming ready: %v", err)
		return "", nil
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Run did not become ready")
		return "", nil
	}
}

// postJSON sends one JSON body and returns the status plus a short body snippet.
func postJSON(t *testing.T, url, body string, headers map[string]string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	return resp.StatusCode, string(raw)
}
