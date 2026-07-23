package logstore

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Metrics is a tiny dependency-free counter registry for the log tier. It is
// rendered in Prometheus text exposition format at GET /api/v1/metrics — no
// external client library is pulled in for this.
type Metrics struct {
	BytesIngested    atomic.Int64 // total masked log bytes accepted at ingest
	Truncations      atomic.Int64 // jobs whose logs hit MAX_JOB_LOG_BYTES
	ArchiveOK        atomic.Int64 // successful blob-archive flushes
	ArchiveErrors    atomic.Int64 // failed blob-archive flushes
	ArchiveMillis    atomic.Int64 // cumulative archive-flush latency (ms)
	RedisUnavailable atomic.Int64 // Redis errors / fallbacks
	SSESubscribers   atomic.Int64 // currently-connected live-tail clients (gauge)
}

func NewMetrics() *Metrics { return &Metrics{} }

// SSEOpen/SSEClose maintain the live subscriber gauge.
func (m *Metrics) SSEOpen()  { m.SSESubscribers.Add(1) }
func (m *Metrics) SSEClose() { m.SSESubscribers.Add(-1) }

// Render returns the metrics in Prometheus text exposition format.
func (m *Metrics) Render(backend string) string {
	var b strings.Builder
	metric := func(name, typ, help string, val int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, typ, name, val)
	}
	metric("forge_log_bytes_ingested_total", "counter",
		"Total masked log bytes accepted at ingest.", m.BytesIngested.Load())
	metric("forge_log_truncations_total", "counter",
		"Jobs whose logs exceeded MAX_JOB_LOG_BYTES.", m.Truncations.Load())
	metric("forge_log_archive_flush_total", "counter",
		"Successful log archive flushes to the blob store.", m.ArchiveOK.Load())
	metric("forge_log_archive_flush_errors_total", "counter",
		"Failed log archive flushes.", m.ArchiveErrors.Load())
	metric("forge_log_archive_flush_millis_total", "counter",
		"Cumulative log archive flush latency in milliseconds.", m.ArchiveMillis.Load())
	metric("forge_log_redis_unavailable_total", "counter",
		"Redis errors or fallbacks in the log tier.", m.RedisUnavailable.Load())
	metric("forge_log_sse_subscribers", "gauge",
		"Currently-connected live-tail (SSE) clients.", m.SSESubscribers.Load())
	fmt.Fprintf(&b, "# HELP forge_log_backend_info Active log backend (label).\n")
	fmt.Fprintf(&b, "# TYPE forge_log_backend_info gauge\n")
	fmt.Fprintf(&b, "forge_log_backend_info{backend=%q} 1\n", backend)
	return b.String()
}
