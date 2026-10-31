package metrics

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sampleNameRE is the Prometheus metric-name grammar.
var sampleNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

func TestCounters(t *testing.T) {
	r := New()

	if got := r.Snapshot(); got != (Snapshot{}) {
		t.Fatalf("new registry snapshot = %+v, want all zero", got)
	}

	r.IncBulkMessageRequests()
	r.AddOutboundJobs(3)
	r.IncInboundMessages()
	r.AddDropped(2)

	want := Snapshot{
		BulkMessageRequests: 1,
		OutboundJobs:        3,
		InboundMessages:     1,
		Dropped:             2,
	}
	if got := r.Snapshot(); got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}

	// Counters accumulate rather than reset.
	r.IncOutboundJobs()
	r.AddOutboundJobs(2)
	r.AddBulkMessageRequests(4)
	if got, want := r.Snapshot().OutboundJobs, uint64(6); got != want {
		t.Fatalf("outbound jobs = %d, want %d", got, want)
	}
	if got, want := r.Snapshot().BulkMessageRequests, uint64(5); got != want {
		t.Fatalf("bulk message requests = %d, want %d", got, want)
	}

	samples, _ := parseExposition(t, scrape(t, r.Handler()))
	wantSamples := map[string]uint64{
		"aether_bulk_message_requests_total": 5,
		"aether_outbound_jobs_total":         6,
		"aether_inbound_messages_total":      1,
		"aether_dropped_total":               2,
	}
	for name, wantValue := range wantSamples {
		got, ok := samples[name]
		if !ok {
			t.Errorf("exposition is missing %s", name)
			continue
		}
		if got != float64(wantValue) {
			t.Errorf("%s = %v, want %d", name, got, wantValue)
		}
	}
	if len(samples) != len(wantSamples) {
		t.Errorf("exposition has %d samples, want %d: %v", len(samples), len(wantSamples), samples)
	}
}

func TestMetricsHandler(t *testing.T) {
	r := New()
	r.IncInboundMessages()
	r.IncOutboundJobs()

	body := scrape(t, r.Handler())
	if !strings.Contains(body, "# TYPE") {
		t.Fatalf("/metrics body has no # TYPE line:\n%s", body)
	}

	samples, types := parseExposition(t, body)
	for _, name := range []string{
		"aether_bulk_message_requests_total",
		"aether_outbound_jobs_total",
		"aether_inbound_messages_total",
		"aether_dropped_total",
	} {
		if got, ok := types[name]; !ok {
			t.Errorf("%s has no # TYPE line", name)
		} else if got != "counter" {
			t.Errorf("%s type = %q, want counter", name, got)
		}
		if _, ok := samples[name]; !ok {
			t.Errorf("%s has no sample line", name)
		}
	}

	// The package-level default registry exposes the same handler.
	defaultBody := scrape(t, Handler())
	if _, defaultTypes := parseExposition(t, defaultBody); len(defaultTypes) != len(types) {
		t.Fatalf("package-level Handler exposed %d metric types, want %d", len(defaultTypes), len(types))
	}
}

// scrape serves h at /metrics and returns the response body. It fails the test
// on any status other than 200 or a non-text Content-Type.
func scrape(t *testing.T, h http.Handler) string {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("/metrics Content-Type = %q, want text/plain", got)
	}
	return rec.Body.String()
}

// parseExposition parses the Prometheus text format strictly enough to catch
// malformed names and values. It returns samples by name and # TYPE lines by
// metric name.
func parseExposition(t *testing.T, body string) (map[string]float64, map[string]string) {
	t.Helper()

	samples := make(map[string]float64)
	types := make(map[string]string)
	for i, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if line == "" {
			t.Fatalf("line %d: blank line in exposition", i+1)
		}
		if strings.HasPrefix(line, "#") {
			fields := strings.Fields(line)
			if len(fields) == 4 && fields[1] == "TYPE" {
				types[fields[2]] = fields[3]
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("line %d: sample %q has %d fields, want 2", i+1, line, len(fields))
		}
		if !sampleNameRE.MatchString(fields[0]) {
			t.Fatalf("line %d: invalid metric name %q", i+1, fields[0])
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("line %d: invalid value %q: %v", i+1, fields[1], err)
		}
		samples[fields[0]] = value
	}
	return samples, types
}
