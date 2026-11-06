package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/testutil"
	"github.com/charlesrobsampson/aether/internal/transport"
)

// recordingMessenger records deliveries and lets a test wait for one without
// sleeping.
type recordingMessenger struct {
	mu    sync.Mutex
	calls []transport.Call
	sent  chan transport.Call
}

func newRecordingMessenger() *recordingMessenger {
	return &recordingMessenger{sent: make(chan transport.Call, 32)}
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
		t.Fatal("timed out waiting for an outbound message")
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

// fakeWatcher is an injectable config.Watcher so a test controls reloads.
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

// stepClock exposes each timer registration so a test can advance the fake
// clock only after the watcher has armed its debounce timer.
type stepClock struct {
	*testutil.FakeClock
	armed chan struct{}
}

func newStepClock(start time.Time) *stepClock {
	return &stepClock{FakeClock: testutil.NewFakeClock(start), armed: make(chan struct{}, 8)}
}

func (c *stepClock) AfterFunc(d time.Duration, fn func()) func() {
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return c.FakeClock.AfterFunc(d, fn)
}

// testRoot copies the example config into a temp dir and rewrites system.json so
// the database is hermetic and no background schedule interferes.
func testRoot(t *testing.T, mutate func(*config.SystemConfig)) string {
	t.Helper()

	root := testutil.TempConfig(t)
	cfg, err := config.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.System.Server.DBPath = filepath.Join(t.TempDir(), "aether.db")
	cfg.System.Schedules = nil
	if mutate != nil {
		mutate(&cfg.System)
	}
	testutil.WriteJSON(t, filepath.Join(root, "system.json"), cfg.System)
	return root
}

func rewriteSystem(t *testing.T, root string, mutate func(*config.SystemConfig)) {
	t.Helper()

	cfg, err := config.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	mutate(&cfg.System)
	testutil.WriteJSON(t, filepath.Join(root, "system.json"), cfg.System)
}

// startApp runs Aether in the background and returns its base URL plus a stop
// function. It waits for Run to report readiness, so no test sleeps.
func startApp(t *testing.T, opts Options, messenger transport.Messenger) (string, func() error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	opts.Messenger = messenger
	opts.Addr = "127.0.0.1:0"
	opts.OnReady = func(addr string) { ready <- addr }

	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, opts) }()

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

func postJSON(t *testing.T, url, body string, headers map[string]string) int {
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
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestAppWiring proves Run serves /healthz, swaps the config snapshot on a
// reload, accepts an injected fake Messenger, and shuts down cleanly.
func TestAppWiring(t *testing.T) {
	root := testRoot(t, func(system *config.SystemConfig) {
		system.Auth.JWTSecret = "initial-secret"
	})

	clock := newStepClock(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC))
	watcher := newFakeWatcher()
	defer watcher.close()
	messenger := newRecordingMessenger()

	reloaded := make(chan *config.Config, 4)
	baseURL, stop := startApp(t, Options{
		ConfigDir: root,
		Clock:     clock,
		Watcher:   watcher,
		OnReload:  func(cfg *config.Config) { reloaded <- cfg },
	}, messenger)

	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Fatalf("/healthz = %d %q, want 200 with ok", resp.StatusCode, body)
	}

	// A reload must swap the whole snapshot atomically, including the JWT
	// secret the /message handler reads per request.
	rewriteSystem(t, root, func(system *config.SystemConfig) {
		system.Auth.JWTSecret = "updated-secret"
	})
	watcher.trigger()
	<-clock.armed
	clock.Advance(100 * time.Millisecond)

	select {
	case cfg := <-reloaded:
		if cfg.System.Auth.JWTSecret != "updated-secret" {
			t.Fatalf("reloaded secret = %q, want updated-secret", cfg.System.Auth.JWTSecret)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("config reload did not complete")
	}

	oldToken, err := auth.Issue("initial-secret", "+15555550101", auth.Admin, time.Minute)
	if err != nil {
		t.Fatalf("issue old token: %v", err)
	}
	if got := postJSON(t, baseURL+"/message", `{"recipients":["+15555550102"],"text":"stale"}`, map[string]string{
		"Authorization": "Bearer " + oldToken,
	}); got != http.StatusUnauthorized {
		t.Fatalf("old secret: /message status = %d, want 401", got)
	} else {
		t.Logf("old-secret /message status = %d", got)
	}

	newToken, err := auth.Issue("updated-secret", "+15555550101", auth.Admin, time.Minute)
	if err != nil {
		t.Fatalf("issue new token: %v", err)
	}
	if got := postJSON(t, baseURL+"/message", `{"recipients":["+15555550102"],"text":"hello"}`, map[string]string{
		"Authorization": "Bearer " + newToken,
	}); got != http.StatusAccepted {
		t.Fatalf("new secret: /message status = %d, want 202", got)
	}

	call := messenger.wait(t)
	if call.Recipient != "+15555550102" || call.Text != "hello" {
		t.Fatalf("messenger call = %+v, want hello to +15555550102", call)
	}

	// An unknown sender requests approval once, not once per message. The
	// maintainer rotation counter advances only for the first pending request.
	for i := 0; i < 2; i++ {
		if got := postJSON(t, baseURL+"/receive", `{"sender":"+15555550777","text":"!test"}`, nil); got != http.StatusAccepted {
			t.Fatalf("unknown sender /receive status = %d, want 202", got)
		}
	}
	maintainerRaw, err := os.ReadFile(filepath.Join(root, "maintainer.json"))
	if err != nil {
		t.Fatalf("read maintainer.json: %v", err)
	}
	var maintainer config.MaintainerConfig
	if err := json.Unmarshal(maintainerRaw, &maintainer); err != nil {
		t.Fatalf("parse maintainer.json: %v", err)
	}
	if maintainer.LastAssigned != 1 {
		t.Fatalf("maintainer last_assigned = %d, want 1 after two unknown messages", maintainer.LastAssigned)
	}
	t.Logf("maintainer last_assigned = %d after two unknown messages", maintainer.LastAssigned)

	if err := stop(); err != nil {
		t.Fatalf("Run returned %v, want clean shutdown", err)
	}
}

// TestTokenCommand proves RunToken prints a JWT that verifies against the
// configured secret.
func TestTokenCommand(t *testing.T) {
	root := testutil.TempConfig(t)
	cfg, err := config.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldStdout := os.Stdout
	os.Stdout = write
	runErr := RunToken([]string{"--sub", "+15555550101", "--role", "admin"})
	os.Stdout = oldStdout
	write.Close()
	output, _ := io.ReadAll(read)
	read.Close()
	if runErr != nil {
		t.Fatalf("RunToken: %v", runErr)
	}

	token := strings.TrimSpace(string(output))
	claims, err := auth.Verify(cfg.System.Auth.JWTSecret, token)
	if err != nil {
		t.Fatalf("printed token does not verify: %v (output %q)", err, token)
	}
	if claims.Sub != "+15555550101" || claims.Role != auth.Admin {
		t.Fatalf("claims = %+v, want sub +15555550101 role admin", claims)
	}

	if err := RunToken([]string{"--sub", "+15555550101", "--role", "wizard"}); err == nil {
		t.Fatal("RunToken accepted an unknown role")
	}
}

// TestBuiltinRoutes proves !config, !admin, !remind, and !test reach the
// internal handlers without any HTTP satellite.
func TestBuiltinRoutes(t *testing.T) {
	var (
		mu        sync.Mutex
		satellite int
	)
	httpSatellite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		satellite++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer httpSatellite.Close()

	root := testRoot(t, nil)
	for _, name := range []string{"reminder", "wind-predictor"} {
		path := filepath.Join(root, "satellites", name+".json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc config.SatelliteConfig
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		doc.Endpoint = httpSatellite.URL
		testutil.WriteJSON(t, path, doc)
	}

	clock := testutil.NewFakeClock(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC))
	watcher := newFakeWatcher()
	defer watcher.close()
	messenger := newRecordingMessenger()

	baseURL, stop := startApp(t, Options{
		ConfigDir: root,
		Clock:     clock,
		Watcher:   watcher,
	}, messenger)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("Run returned %v, want clean shutdown", err)
		}
	}()

	// !config set is handled by configmgr and edits the sender's own file.
	if got := postJSON(t, baseURL+"/receive", `{"sender":"+15555550102","text":"!config set timezone America/New_York"}`, nil); got != http.StatusAccepted {
		t.Fatalf("!config /receive status = %d, want 202", got)
	}

	// !admin add is handled by adminmgr.
	if got := postJSON(t, baseURL+"/receive", `{"sender":"+15555550101","text":"!admin add +15555550108"}`, nil); got != http.StatusAccepted {
		t.Fatalf("!admin /receive status = %d, want 202", got)
	}

	// !test is handled by testsat and replies through the queue. The dispatch
	// worker is single-threaded, so seeing this reply proves the two commands
	// above have finished their file writes.
	if got := postJSON(t, baseURL+"/receive", `{"sender":"+15555550102","text":"!test"}`, nil); got != http.StatusAccepted {
		t.Fatalf("!test /receive status = %d, want 202", got)
	}
	if call := messenger.wait(t); call.Text != "test received" {
		t.Fatalf("!test reply = %q, want test received", call.Text)
	}

	userRaw, err := os.ReadFile(filepath.Join(root, "users", "+15555550102.json"))
	if err != nil {
		t.Fatalf("read user file: %v", err)
	}
	var user config.UserConfig
	if err := json.Unmarshal(userRaw, &user); err != nil {
		t.Fatalf("parse user file: %v", err)
	}
	if user.Timezone != "America/New_York" {
		t.Fatalf("user timezone = %q, want America/New_York", user.Timezone)
	}

	adminRaw, err := os.ReadFile(filepath.Join(root, "admin.json"))
	if err != nil {
		t.Fatalf("read admin.json: %v", err)
	}
	var admin config.AdminConfig
	if err := json.Unmarshal(adminRaw, &admin); err != nil {
		t.Fatalf("parse admin.json: %v", err)
	}
	if !contains(admin.Admins, "+15555550108") {
		t.Fatalf("admin.json admins = %v, want +15555550108", admin.Admins)
	}

	// !remind is handled by testsat and fires on the injected clock. A second
	// !test reply proves the reminder timer was registered before we advance.
	if got := postJSON(t, baseURL+"/receive", `{"sender":"+15555550102","text":"!remind me stretch in 1 minute"}`, nil); got != http.StatusAccepted {
		t.Fatalf("!remind /receive status = %d, want 202", got)
	}
	if got := postJSON(t, baseURL+"/receive", `{"sender":"+15555550102","text":"!test"}`, nil); got != http.StatusAccepted {
		t.Fatalf("!test /receive status = %d, want 202", got)
	}
	if call := messenger.wait(t); call.Text != "test received" {
		t.Fatalf("second !test reply = %q, want test received", call.Text)
	}
	clock.Advance(time.Minute)
	if call := messenger.wait(t); call.Text != "stretch" {
		t.Fatalf("!remind reply = %q, want stretch", call.Text)
	}

	mu.Lock()
	gotSatelliteCalls := satellite
	mu.Unlock()
	if gotSatelliteCalls != 0 {
		t.Fatalf("HTTP satellite calls = %d, want 0 for built-in commands", gotSatelliteCalls)
	}
	t.Logf("HTTP satellite calls = %d for built-in commands", gotSatelliteCalls)
}

// TestSchedulesWired proves a system.json schedule fires on the injected clock
// and dispatches to the configured satellite.
func TestSchedulesWired(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []scheduleCall
	)
	satellite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope scheduleCall
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Errorf("satellite received invalid JSON: %v", err)
		}
		envelope.Path = r.URL.Path
		mu.Lock()
		calls = append(calls, envelope)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer satellite.Close()

	root := testRoot(t, func(system *config.SystemConfig) {
		system.Schedules = []config.ScheduleConfig{{
			Name:       "tick",
			Cron:       "* * * * *",
			Satellite:  "wind-predictor",
			Command:    "fly",
			Args:       map[string]any{"station": "FPS"},
			Recipients: []string{"+15555550102"},
		}}
	})

	path := filepath.Join(root, "satellites", "wind-predictor.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read satellite: %v", err)
	}
	var satelliteDoc config.SatelliteConfig
	if err := json.Unmarshal(raw, &satelliteDoc); err != nil {
		t.Fatalf("parse satellite: %v", err)
	}
	satelliteDoc.Endpoint = satellite.URL
	testutil.WriteJSON(t, path, satelliteDoc)

	clock := testutil.NewFakeClock(time.Date(2026, 1, 5, 6, 59, 30, 0, time.UTC))
	watcher := newFakeWatcher()
	defer watcher.close()
	messenger := newRecordingMessenger()

	_, stop := startApp(t, Options{
		ConfigDir: root,
		Clock:     clock,
		Watcher:   watcher,
	}, messenger)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("Run returned %v, want clean shutdown", err)
		}
	}()

	clock.Advance(30 * time.Second)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := append([]scheduleCall(nil), calls...)
		mu.Unlock()
		if len(got) == 1 {
			call := got[0]
			if call.Path != "/api/v1/predict/text" {
				t.Fatalf("schedule path = %q, want /api/v1/predict/text", call.Path)
			}
			if call.Command != "fly" || call.Trigger != "fly" || call.Satellite != "wind-predictor" {
				t.Fatalf("schedule envelope = %+v, want fly to wind-predictor", call)
			}
			if gotArgs, _ := json.Marshal(call.Args); string(gotArgs) != `{"station":"FPS"}` {
				t.Fatalf("schedule args = %s, want {\"station\":\"FPS\"}", gotArgs)
			}
			return
		}
		if len(got) > 1 {
			t.Fatalf("schedule fired %d times, want exactly once", len(got))
		}
		select {
		case <-deadline:
			t.Fatal("schedule did not dispatch")
		default:
		}
	}
}

type scheduleCall struct {
	Command   string         `json:"command"`
	Trigger   string         `json:"trigger"`
	Args      map[string]any `json:"args"`
	Text      string         `json:"text"`
	Sender    string         `json:"sender"`
	Satellite string         `json:"satellite"`
	ThreadID  string         `json:"thread_id"`
	Path      string         `json:"-"`
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
