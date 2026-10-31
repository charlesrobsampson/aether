// Package metrics provides Aether's process-local Prometheus counters.
//
// The exposition format is written by hand so Aether does not depend on
// prometheus/client_golang. Counters are atomic and safe for concurrent use.
// Most callers use the package-level helpers; tests that need isolation create a
// Registry with New.
package metrics

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// Metric names exposed on /metrics.
const (
	BulkMessageRequestsName = "aether_bulk_message_requests_total"
	OutboundJobsName        = "aether_outbound_jobs_total"
	InboundMessagesName     = "aether_inbound_messages_total"
	DroppedName             = "aether_dropped_total"
)

// Snapshot is a consistent point-in-time copy of every counter.
type Snapshot struct {
	BulkMessageRequests uint64
	OutboundJobs        uint64
	InboundMessages     uint64
	Dropped             uint64
}

// Registry holds Aether's counters.
type Registry struct {
	bulkMessageRequests atomic.Uint64
	outboundJobs        atomic.Uint64
	inboundMessages     atomic.Uint64
	dropped             atomic.Uint64
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{}
}

// Default is the process-wide registry used by the package-level helpers.
var Default = New()

// IncBulkMessageRequests counts one request that fanned out to more than one
// recipient.
func (r *Registry) IncBulkMessageRequests() {
	r.bulkMessageRequests.Add(1)
}

// AddBulkMessageRequests counts n bulk requests.
func (r *Registry) AddBulkMessageRequests(n uint64) {
	r.bulkMessageRequests.Add(n)
}

// IncOutboundJobs counts one outbound delivery job.
func (r *Registry) IncOutboundJobs() {
	r.outboundJobs.Add(1)
}

// AddOutboundJobs counts n outbound delivery jobs.
func (r *Registry) AddOutboundJobs(n uint64) {
	r.outboundJobs.Add(n)
}

// IncInboundMessages counts one accepted inbound message.
func (r *Registry) IncInboundMessages() {
	r.inboundMessages.Add(1)
}

// AddInboundMessages counts n accepted inbound messages.
func (r *Registry) AddInboundMessages(n uint64) {
	r.inboundMessages.Add(n)
}

// IncDropped counts one outbound job dropped without delivery.
func (r *Registry) IncDropped() {
	r.dropped.Add(1)
}

// AddDropped counts n outbound jobs dropped without delivery.
func (r *Registry) AddDropped(n uint64) {
	r.dropped.Add(n)
}

// Snapshot returns the current counter values.
func (r *Registry) Snapshot() Snapshot {
	return Snapshot{
		BulkMessageRequests: r.bulkMessageRequests.Load(),
		OutboundJobs:        r.outboundJobs.Load(),
		InboundMessages:     r.inboundMessages.Load(),
		Dropped:             r.dropped.Load(),
	}
}

// Handler serves the registry in Prometheus text exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
