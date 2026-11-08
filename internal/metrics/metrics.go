// Package metrics provides Aether's process-local Prometheus metrics.
//
// The exposition format is written by hand so Aether does not depend on
// prometheus/client_golang. Counters, gauges, and summaries are safe for
// concurrent use. Most callers receive a Registry through their options; tests
// that need isolation create one with New. Label values are always bounded
// enums or configured provider/profile names: phone numbers and message IDs are
// never used as labels (plan §3.19).
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metric names exposed on /metrics.
const (
	BulkMessageRequestsName = "aether_bulk_message_requests_total"
	OutboundJobsName        = "aether_outbound_jobs_total"
	InboundMessagesName     = "aether_inbound_messages_total"
	DroppedName             = "aether_dropped_total"

	ReceiveRequestsName      = "aether_receive_requests_total"
	InboundQueueDepthName    = "aether_inbound_queue_depth"
	DispatchDurationName     = "aether_dispatch_duration_seconds"
	DispatchFailuresName     = "aether_dispatch_failures_total"
	DedupeHitsName           = "aether_dedupe_hits_total"
	QueueFullName            = "aether_queue_full_total"
	OutboundQueueDepthName   = "aether_outbound_queue_depth"
	OutboundSendDurationName = "aether_outbound_send_duration_seconds"
	OutboundSendFailuresName = "aether_outbound_send_failures_total"
	OutboundRetriesName      = "aether_outbound_retries_total"
	DeadLetterName           = "aether_dead_letter_total"
)

// Bounded label values. Keeping these as constants prevents accidental
// high-cardinality labels such as phone numbers or message IDs.
const (
	ReceiveAccepted     = "accepted"
	ReceiveDuplicate    = "duplicate"
	ReceiveQueueFull    = "queue_full"
	ReceiveBadRequest   = "bad_request"
	ReceiveUnauthorized = "unauthorized"

	QueueInbound  = "inbound"
	QueueOutbound = "outbound"

	DispatchStatusDispatched = "dispatched"
	DispatchStatusFailed     = "failed"
	DispatchStatusDropped    = "dropped"

	DispatchReasonParse   = "parse"
	DispatchReasonPost    = "post"
	DispatchReasonTimeout = "timeout"
	DispatchReasonBuiltin = "builtin"
	DispatchReasonConfig  = "config"

	SendResultOK        = "ok"
	SendResultRetryable = "retryable"
	SendResultTerminal  = "terminal"

	SendFailureTimeout = "timeout"
	SendFailureNetwork = "network"
	SendFailure429     = "429"
	SendFailure5xx     = "5xx"
	SendFailure4xx     = "4xx"
	SendFailureOther   = "other"
)

// Snapshot is a consistent point-in-time copy of every unlabeled counter.
type Snapshot struct {
	BulkMessageRequests uint64
	OutboundJobs        uint64
	InboundMessages     uint64
	Dropped             uint64
	DedupeHits          uint64
}

// counterSample is one labeled counter value.
type counterSample struct {
	labels []string
	value  uint64
}

// counterVec is a counter with a fixed set of label names and a bounded set of
// values observed at runtime.
type counterVec struct {
	name   string
	help   string
	labels []string

	mu      sync.Mutex
	samples map[string]counterSample
}

func newCounterVec(name, help string, labels []string) *counterVec {
	return &counterVec{
		name:    name,
		help:    help,
		labels:  labels,
		samples: make(map[string]counterSample),
	}
}

func labelKey(values []string) string { return strings.Join(values, "\x1f") }

func (v *counterVec) inc(values ...string) {
	if v == nil {
		return
	}
	key := labelKey(values)
	v.mu.Lock()
	sample := v.samples[key]
	sample.labels = append([]string(nil), values...)
	sample.value++
	v.samples[key] = sample
	v.mu.Unlock()
}

func (v *counterVec) seed(values ...string) {
	if v == nil {
		return
	}
	key := labelKey(values)
	v.mu.Lock()
	if _, ok := v.samples[key]; !ok {
		v.samples[key] = counterSample{labels: append([]string(nil), values...)}
	}
	v.mu.Unlock()
}

func (v *counterVec) get(values ...string) uint64 {
	if v == nil {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.samples[labelKey(values)].value
}

func (v *counterVec) expose(b *strings.Builder) {
	if v == nil {
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n", v.name, v.help)
	fmt.Fprintf(b, "# TYPE %s counter\n", v.name)

	v.mu.Lock()
	keys := make([]string, 0, len(v.samples))
	for key := range v.samples {
		keys = append(keys, key)
	}
	snapshots := make(map[string]counterSample, len(v.samples))
	for key, sample := range v.samples {
		snapshots[key] = sample
	}
	v.mu.Unlock()

	sort.Strings(keys)
	for _, key := range keys {
		sample := snapshots[key]
		b.WriteString(formatSample(v.name, v.labels, sample.labels))
		b.WriteByte(' ')
		b.WriteString(strconv.FormatUint(sample.value, 10))
		b.WriteByte('\n')
	}
}

// summarySample is one labeled summary value. Only sum and count are exposed;
// quantiles stay a Prometheus-server concern (plan §3.19).
type summarySample struct {
	labels []string
	sum    float64
	count  uint64
}

// summaryVec is a summary with a fixed set of label names.
type summaryVec struct {
	name   string
	help   string
	labels []string

	mu      sync.Mutex
	samples map[string]*summarySample
}

func newSummaryVec(name, help string, labels []string) *summaryVec {
	return &summaryVec{
		name:    name,
		help:    help,
		labels:  labels,
		samples: make(map[string]*summarySample),
	}
}

func (v *summaryVec) observe(d time.Duration, values ...string) {
	if v == nil {
		return
	}
	seconds := d.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	key := labelKey(values)
	v.mu.Lock()
	sample := v.samples[key]
	if sample == nil {
		sample = &summarySample{labels: append([]string(nil), values...)}
		v.samples[key] = sample
	}
	sample.sum += seconds
	sample.count++
	v.mu.Unlock()
}

func (v *summaryVec) seed(values ...string) {
	if v == nil {
		return
	}
	key := labelKey(values)
	v.mu.Lock()
	if _, ok := v.samples[key]; !ok {
		v.samples[key] = &summarySample{labels: append([]string(nil), values...)}
	}
	v.mu.Unlock()
}

func (v *summaryVec) get(values ...string) (float64, uint64) {
	if v == nil {
		return 0, 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	sample := v.samples[labelKey(values)]
	if sample == nil {
		return 0, 0
	}
	return sample.sum, sample.count
}

func (v *summaryVec) expose(b *strings.Builder) {
	if v == nil {
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n", v.name, v.help)
	fmt.Fprintf(b, "# TYPE %s summary\n", v.name)

	v.mu.Lock()
	keys := make([]string, 0, len(v.samples))
	for key := range v.samples {
		keys = append(keys, key)
	}
	snapshots := make(map[string]summarySample, len(v.samples))
	for key, sample := range v.samples {
		snapshots[key] = *sample
	}
	v.mu.Unlock()

	sort.Strings(keys)
	for _, key := range keys {
		sample := snapshots[key]
		base := formatSample(v.name, v.labels, sample.labels)
		fmt.Fprintf(b, "%s_sum %s\n", base, formatFloat(sample.sum))
		fmt.Fprintf(b, "%s_count %d\n", base, sample.count)
	}
}

// Registry holds Aether's metrics.
type Registry struct {
	bulkMessageRequests atomic.Uint64
	outboundJobs        atomic.Uint64
	inboundMessages     atomic.Uint64
	dropped             atomic.Uint64
	dedupeHits          atomic.Uint64
	inboundQueueDepth   atomic.Int64
	outboundQueueDepth  atomic.Int64

	receiveRequests      *counterVec
	dispatchFailures     *counterVec
	queueFull            *counterVec
	outboundSendFailures *counterVec
	outboundRetries      *counterVec
	deadLetter           *counterVec
	dispatchDuration     *summaryVec
	outboundSendDuration *summaryVec
}

// New returns an empty Registry with every fixed label set seeded to zero so
// the exposition always documents the bounded enum values.
func New() *Registry {
	r := &Registry{
		receiveRequests: newCounterVec(ReceiveRequestsName,
			"Total /receive outcomes by result.",
			[]string{"result"}),
		dispatchFailures: newCounterVec(DispatchFailuresName,
			"Total failed satellite dispatches by reason.",
			[]string{"reason"}),
		queueFull: newCounterVec(QueueFullName,
			"Total work rejected because a bounded queue was full.",
			[]string{"queue"}),
		outboundSendFailures: newCounterVec(OutboundSendFailuresName,
			"Total failed outbound send attempts by provider and reason.",
			[]string{"provider", "reason"}),
		outboundRetries: newCounterVec(OutboundRetriesName,
			"Total outbound retries scheduled by provider and reason.",
			[]string{"provider", "reason"}),
		deadLetter: newCounterVec(DeadLetterName,
			"Total outbound jobs exhausted and dead-lettered.",
			[]string{"provider"}),
		dispatchDuration: newSummaryVec(DispatchDurationName,
			"Satellite dispatch latency in seconds by status.",
			[]string{"status"}),
		outboundSendDuration: newSummaryVec(OutboundSendDurationName,
			"One outbound send attempt latency in seconds by provider and result.",
			[]string{"provider", "result"}),
	}

	for _, result := range []string{
		ReceiveAccepted, ReceiveDuplicate, ReceiveQueueFull,
		ReceiveBadRequest, ReceiveUnauthorized,
	} {
		r.receiveRequests.seed(result)
	}
	for _, queue := range []string{QueueInbound, QueueOutbound} {
		r.queueFull.seed(queue)
	}
	for _, status := range []string{
		DispatchStatusDispatched, DispatchStatusFailed, DispatchStatusDropped,
	} {
		r.dispatchDuration.seed(status)
	}
	for _, reason := range []string{
		DispatchReasonParse, DispatchReasonPost, DispatchReasonTimeout,
		DispatchReasonBuiltin, DispatchReasonConfig,
	} {
		r.dispatchFailures.seed(reason)
	}
	return r
}

// Default is the process-wide registry used by components that were not given
// an explicit registry.
var Default = New()

// IncBulkMessageRequests counts one request that fanned out to more than one
// recipient.
func (r *Registry) IncBulkMessageRequests() { r.bulkMessageRequests.Add(1) }

// AddBulkMessageRequests counts n bulk requests.
func (r *Registry) AddBulkMessageRequests(n uint64) { r.bulkMessageRequests.Add(n) }

// IncOutboundJobs counts one outbound delivery job.
func (r *Registry) IncOutboundJobs() { r.outboundJobs.Add(1) }

// AddOutboundJobs counts n outbound delivery jobs.
func (r *Registry) AddOutboundJobs(n uint64) { r.outboundJobs.Add(n) }

// IncInboundMessages counts one accepted inbound message.
func (r *Registry) IncInboundMessages() { r.inboundMessages.Add(1) }

// AddInboundMessages counts n accepted inbound messages.
func (r *Registry) AddInboundMessages(n uint64) { r.inboundMessages.Add(n) }

// IncDropped counts one outbound job dropped without delivery.
func (r *Registry) IncDropped() { r.dropped.Add(1) }

// AddDropped counts n outbound jobs dropped without delivery.
func (r *Registry) AddDropped(n uint64) { r.dropped.Add(n) }

// IncDedupeHits counts one inbound duplicate suppressed by dedupe.
func (r *Registry) IncDedupeHits() { r.dedupeHits.Add(1) }

// DedupeHits reports inbound duplicates suppressed by dedupe.
func (r *Registry) DedupeHits() uint64 { return r.dedupeHits.Load() }

// IncReceiveRequest counts one /receive outcome. result must be a bounded enum
// such as ReceiveAccepted.
func (r *Registry) IncReceiveRequest(result string) { r.receiveRequests.inc(result) }

// ReceiveRequests reports the count for one /receive result.
func (r *Registry) ReceiveRequests(result string) uint64 { return r.receiveRequests.get(result) }

// SetInboundQueueDepth records the current inbound dispatch queue occupancy.
func (r *Registry) SetInboundQueueDepth(depth int) {
	if depth < 0 {
		depth = 0
	}
	r.inboundQueueDepth.Store(int64(depth))
}

// InboundQueueDepth reports the last recorded inbound queue occupancy.
func (r *Registry) InboundQueueDepth() int64 { return r.inboundQueueDepth.Load() }

// ObserveDispatchDuration records one router.Dispatch latency by status.
func (r *Registry) ObserveDispatchDuration(status string, d time.Duration) {
	r.dispatchDuration.observe(d, status)
}

// DispatchDuration reports the sum and count for one dispatch status.
func (r *Registry) DispatchDuration(status string) (float64, uint64) {
	return r.dispatchDuration.get(status)
}

// IncDispatchFailure counts one failed dispatch by reason.
func (r *Registry) IncDispatchFailure(reason string) { r.dispatchFailures.inc(reason) }

// DispatchFailures reports failed dispatches for one reason.
func (r *Registry) DispatchFailures(reason string) uint64 {
	return r.dispatchFailures.get(reason)
}

// IncQueueFull counts one rejected unit of work by queue name.
func (r *Registry) IncQueueFull(queue string) { r.queueFull.inc(queue) }

// QueueFull reports rejected work for one queue name.
func (r *Registry) QueueFull(queue string) uint64 { return r.queueFull.get(queue) }

// SetOutboundQueueDepth records the current outbound queue occupancy.
func (r *Registry) SetOutboundQueueDepth(depth int) {
	if depth < 0 {
		depth = 0
	}
	r.outboundQueueDepth.Store(int64(depth))
}

// OutboundQueueDepth reports the last recorded outbound queue occupancy.
func (r *Registry) OutboundQueueDepth() int64 { return r.outboundQueueDepth.Load() }

// ObserveOutboundSendDuration records one send attempt latency.
func (r *Registry) ObserveOutboundSendDuration(provider, result string, d time.Duration) {
	r.outboundSendDuration.observe(d, provider, result)
}

// OutboundSendDuration reports the sum and count for one provider/result pair.
func (r *Registry) OutboundSendDuration(provider, result string) (float64, uint64) {
	return r.outboundSendDuration.get(provider, result)
}

// IncOutboundSendFailure counts one failed send attempt.
func (r *Registry) IncOutboundSendFailure(provider, reason string) {
	r.outboundSendFailures.inc(provider, reason)
}

// OutboundSendFailures reports failed attempts for one provider/reason pair.
func (r *Registry) OutboundSendFailures(provider, reason string) uint64 {
	return r.outboundSendFailures.get(provider, reason)
}

// IncOutboundRetry counts one retry scheduled for an outbound job.
func (r *Registry) IncOutboundRetry(provider, reason string) {
	r.outboundRetries.inc(provider, reason)
}

// OutboundRetries reports scheduled retries for one provider/reason pair.
func (r *Registry) OutboundRetries(provider, reason string) uint64 {
	return r.outboundRetries.get(provider, reason)
}

// IncDeadLetter counts one outbound job that exhausted its attempts or failed
// terminally.
func (r *Registry) IncDeadLetter(provider string) { r.deadLetter.inc(provider) }

// DeadLetters reports dead-lettered jobs for one provider.
func (r *Registry) DeadLetters(provider string) uint64 { return r.deadLetter.get(provider) }

// Snapshot returns the current unlabeled counter values.
func (r *Registry) Snapshot() Snapshot {
	return Snapshot{
		BulkMessageRequests: r.bulkMessageRequests.Load(),
		OutboundJobs:        r.outboundJobs.Load(),
		InboundMessages:     r.inboundMessages.Load(),
		Dropped:             r.dropped.Load(),
		DedupeHits:          r.dedupeHits.Load(),
	}
}

// Handler serves the registry in Prometheus text exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if r == nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		snapshot := r.Snapshot()

		var b strings.Builder
		writeCounter(&b, BulkMessageRequestsName,
			"Total /message requests that fanned out to more than one recipient.",
			snapshot.BulkMessageRequests)
		writeCounter(&b, OutboundJobsName,
			"Total outbound delivery jobs enqueued.",
			snapshot.OutboundJobs)
		writeCounter(&b, InboundMessagesName,
			"Total inbound messages accepted from transports.",
			snapshot.InboundMessages)
		writeCounter(&b, DroppedName,
			"Total outbound jobs dropped without delivery.",
			snapshot.Dropped)

		r.receiveRequests.expose(&b)
		writeGauge(&b, InboundQueueDepthName,
			"Inbound dispatch queue occupancy.", r.inboundQueueDepth.Load())
		r.dispatchDuration.expose(&b)
		r.dispatchFailures.expose(&b)
		writeCounter(&b, DedupeHitsName,
			"Total inbound duplicates suppressed.", snapshot.DedupeHits)
		r.queueFull.expose(&b)
		writeGauge(&b, OutboundQueueDepthName,
			"Outbound delivery queue occupancy.", r.outboundQueueDepth.Load())
		r.outboundSendDuration.expose(&b)
		r.outboundSendFailures.expose(&b)
		r.outboundRetries.expose(&b)
		r.deadLetter.expose(&b)

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	})
}

// IncBulkMessageRequests counts a bulk request on the default registry.
func IncBulkMessageRequests() { Default.IncBulkMessageRequests() }

// AddBulkMessageRequests counts n bulk requests on the default registry.
func AddBulkMessageRequests(n uint64) { Default.AddBulkMessageRequests(n) }

// IncOutboundJobs counts one outbound job on the default registry.
func IncOutboundJobs() { Default.IncOutboundJobs() }

// AddOutboundJobs counts n outbound jobs on the default registry.
func AddOutboundJobs(n uint64) { Default.AddOutboundJobs(n) }

// IncInboundMessages counts one inbound message on the default registry.
func IncInboundMessages() { Default.IncInboundMessages() }

// AddInboundMessages counts n inbound messages on the default registry.
func AddInboundMessages(n uint64) { Default.AddInboundMessages(n) }

// IncDropped counts one dropped job on the default registry.
func IncDropped() { Default.IncDropped() }

// AddDropped counts n dropped jobs on the default registry.
func AddDropped(n uint64) { Default.AddDropped(n) }

// Handler serves the default registry in Prometheus text exposition format.
func Handler() http.Handler { return Default.Handler() }

// writeCounter appends one HELP/TYPE/sample triple in Prometheus text format.
func writeCounter(b *strings.Builder, name, help string, value uint64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(value, 10))
	b.WriteByte('\n')
}

// writeGauge appends one HELP/TYPE/sample triple for an unlabeled gauge.
func writeGauge(b *strings.Builder, name, help string, value int64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(strconv.FormatInt(value, 10))
	b.WriteByte('\n')
}

// formatSample renders name{label="value",...}. An empty label set is just the
// metric name.
func formatSample(name string, labelNames, labelValues []string) string {
	if len(labelNames) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, label := range labelNames {
		if i > 0 {
			b.WriteByte(',')
		}
		value := ""
		if i < len(labelValues) {
			value = labelValues[i]
		}
		b.WriteString(label)
		b.WriteString("=\"")
		b.WriteString(escapeLabel(value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabel escapes the three characters Prometheus requires in label
// values: backslash, double quote, and newline.
func escapeLabel(value string) string {
	if !strings.ContainsAny(value, "\\\"\n") {
		return value
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(value[i])
		}
	}
	return b.String()
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
