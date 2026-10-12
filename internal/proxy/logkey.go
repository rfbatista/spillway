package proxy

import "log/slog"

// log_key tags a log line with a stable, machine-filterable event type. Filter
// in Loki/Grafana with `log_key="<value>"` and pivot to a single request by
// combining it with `request_id`. The msg field stays human-readable — log_key
// carries the machine identity, so dashboards and alerts keep working when the
// message text is reworded.
const (
	// logKeyRequestTrace tags the per-request lifecycle trace and the attempt
	// body capture (see RequestTracker).
	logKeyRequestTrace = "request_trace"

	// logKeyRateLimitSnapshot tags rate-limit/quota snapshots parsed from the
	// upstream X-RateLimit-* response headers.
	logKeyRateLimitSnapshot = "rate_limit_snapshot"

	// logKeyUpstreamResponse tags per-attempt upstream HTTP outcomes (a status
	// line or a nil response).
	logKeyUpstreamResponse = "upstream_response"

	// logKeyRetryQueue tags retry-queue lifecycle events: enqueue, dequeue,
	// re-enqueue, queue-full rejection, and the retry decision that feeds it.
	logKeyRetryQueue = "retry_queue"

	// logKeyHeartbeat tags the periodic liveness lines emitted by Heartbeat.
	// Their presence proves the proxy and its log pipeline are alive (absence
	// alerts key on this), and they carry queue depth as a continuous gauge.
	logKeyHeartbeat = "heartbeat"
)

// logKeyAttr returns the slog attribute that tags a log line with a stable
// event type. It satisfies both the variadic slog logging methods (...any) and
// the LogAttrs API ([]slog.Attr).
func logKeyAttr(key string) slog.Attr {
	return slog.String("log_key", key)
}
