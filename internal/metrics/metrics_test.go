package metrics

import (
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sampleNameRE is the Prometheus metric-name grammar.
var sampleNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// labelPairRE matches one name="value" pair inside a label set.
var labelPairRE = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"$`)

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
}

// TestReliabilityExposition proves every plan §3.19 family is exposed with the
// documented type and labels, summaries carry _sum/_count, and no label is a
// phone number or message ID.
func TestReliabilityExposition(t *testing.T) {
	r := New()
	r.IncBulkMessageRequests()
	r.IncOutboundJobs()
	r.IncInboundMessages()
	r.IncDropped()
	r.IncReceiveRequest(ReceiveAccepted)
	r.IncReceiveRequest(ReceiveDuplicate)
	r.IncReceiveRequest(ReceiveQueueFull)
	r.IncReceiveRequest(ReceiveBadRequest)
	r.IncReceiveRequest(ReceiveUnauthorized)
	r.SetInboundQueueDepth(3)
	r.ObserveDispatchDuration(DispatchStatusDispatched, 25*time.Millisecond)
	r.IncDispatchFailure(DispatchReasonTimeout)
	r.IncDedupeHits()
	r.IncQueueFull(QueueInbound)
	r.IncQueueFull(QueueOutbound)
	r.SetOutboundQueueDepth(7)
	r.ObserveOutboundSendDuration("signal", SendResultOK, 10*time.Millisecond)
	r.IncOutboundSendFailure("signal", SendFailure5xx)
	r.IncOutboundRetry("signal", SendFailure5xx)
	r.IncDeadLetter("signal")

	body := scrape(t, r.Handler())
	samples, types := parseExposition(t, body)

	wantTypes := map[string]string{
		BulkMessageRequestsName:  "counter",
		OutboundJobsName:         "counter",
		InboundMessagesName:      "counter",
		DroppedName:              "counter",
		ReceiveRequestsName:      "counter",
		InboundQueueDepthName:    "gauge",
		DispatchDurationName:     "summary",
		DispatchFailuresName:     "counter",
		DedupeHitsName:           "counter",
		QueueFullName:            "counter",
		OutboundQueueDepthName:   "gauge",
		OutboundSendDurationName: "summary",
		OutboundSendFailuresName: "counter",
		OutboundRetriesName:      "counter",
		DeadLetterName:           "counter",
	}
	if len(types) != len(wantTypes) {
		t.Errorf("exposition has %d metric families, want %d: %v", len(types), len(wantTypes), types)
	}
	for name, want := range wantTypes {
		got, ok := types[name]
		if !ok {
			t.Errorf("exposition is missing # TYPE %s", name)
			continue
		}
		if got != want {
			t.Errorf("%s type = %q, want %q", name, got, want)
		}
	}

	wantSamples := []struct {
		key  string
		want float64
	}{
		{BulkMessageRequestsName, 1},
		{OutboundJobsName, 1},
		{InboundMessagesName, 1},
		{DroppedName, 1},
		{ReceiveRequestsName + `{result="accepted"}`, 1},
		{ReceiveRequestsName + `{result="duplicate"}`, 1},
		{ReceiveRequestsName + `{result="queue_full"}`, 1},
		{ReceiveRequestsName + `{result="bad_request"}`, 1},
		{ReceiveRequestsName + `{result="unauthorized"}`, 1},
		{InboundQueueDepthName, 3},
		{DispatchDurationName + `{status="dispatched"}_sum`, 0.025},
		{DispatchDurationName + `{status="dispatched"}_count`, 1},
		{DispatchFailuresName + `{reason="timeout"}`, 1},
		{DedupeHitsName, 1},
		{QueueFullName + `{queue="inbound"}`, 1},
		{QueueFullName + `{queue="outbound"}`, 1},
		{OutboundQueueDepthName, 7},
		{OutboundSendDurationName + `{provider="signal",result="ok"}_sum`, 0.010},
		{OutboundSendDurationName + `{provider="signal",result="ok"}_count`, 1},
		{OutboundSendFailuresName + `{provider="signal",reason="5xx"}`, 1},
		{OutboundRetriesName + `{provider="signal",reason="5xx"}`, 1},
		{DeadLetterName + `{provider="signal"}`, 1},
	}
	for _, want := range wantSamples {
		got, ok := samples[want.key]
		if !ok {
			t.Errorf("exposition is missing sample %s", want.key)
			continue
		}
		if math.Abs(got-want.want) > 1e-9 {
			t.Errorf("%s = %v, want %v", want.key, got, want.want)
		}
	}

	// Labels are bounded enums and configured names, never phone numbers or
	// message IDs.
	allowedLabels := map[string]bool{
		"result":   true,
		"status":   true,
		"reason":   true,
		"queue":    true,
		"provider": true,
	}
	for raw := range samples {
		_, labels := parseSampleName(t, raw)
		for name, value := range labels {
			if !allowedLabels[name] {
				t.Errorf("sample %s has undocumented label %q", raw, name)
			}
			if strings.HasPrefix(value, "+") || strings.Contains(value, "@") {
				t.Errorf("sample %s has a phone-like label value %q", raw, value)
			}
		}
	}
	for _, forbidden := range []string{"message_id=", "sender=", "+1555"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("exposition leaked %q:\n%s", forbidden, body)
		}
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
// malformed names, labels, and values. It returns samples keyed by their full
// name (including any label set or summary suffix) and # TYPE lines by family.
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
			if len(fields) >= 4 && fields[1] == "TYPE" {
				types[fields[2]] = fields[3]
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("line %d: sample %q has %d fields, want 2", i+1, line, len(fields))
		}
		parseSampleName(t, fields[0])
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("line %d: invalid value %q: %v", i+1, fields[1], err)
		}
		samples[fields[0]] = value
	}
	return samples, types
}

// parseSampleName validates a sample's name and label set and returns its
// family name and labels. A trailing _sum/_count is accepted for summaries.
func parseSampleName(t *testing.T, raw string) (string, map[string]string) {
	t.Helper()

	name := raw
	labelPart := ""
	if idx := strings.IndexByte(raw, '{'); idx >= 0 {
		name = raw[:idx]
		end := strings.IndexByte(raw[idx:], '}')
		if end < 0 {
			t.Fatalf("sample %q has an unterminated label set", raw)
		}
		end += idx
		labelPart = raw[idx+1 : end]
		if rest := raw[end+1:]; rest != "" && rest != "_sum" && rest != "_count" {
			t.Fatalf("sample %q has an unexpected suffix %q", raw, rest)
		}
	}

	family := strings.TrimSuffix(strings.TrimSuffix(name, "_sum"), "_count")
	if !sampleNameRE.MatchString(family) {
		t.Fatalf("invalid metric name %q", family)
	}

	labels := make(map[string]string)
	if labelPart == "" {
		return family, labels
	}
	for _, pair := range strings.Split(labelPart, ",") {
		match := labelPairRE.FindStringSubmatch(pair)
		if match == nil {
			t.Fatalf("sample %q has an invalid label pair %q", raw, pair)
		}
		if _, duplicate := labels[match[1]]; duplicate {
			t.Fatalf("sample %q repeats label %q", raw, match[1])
		}
		labels[match[1]] = unescapeLabel(match[2])
	}
	return family, labels
}

func unescapeLabel(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			i++
		}
		b.WriteByte(value[i])
	}
	return b.String()
}
