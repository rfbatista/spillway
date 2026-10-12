package proxy

import (
	"context"
	"log/slog"
	"runtime"
	"time"
)

// defaultHeartbeatInterval is used when NewHeartbeat receives a
// non-positive interval.
const defaultHeartbeatInterval = 30 * time.Second

// Heartbeat periodically emits a liveness log line so Loki-based absence
// alerts can tell a dead proxy (or broken log shipping) apart from an idle
// one, and so queue depth is observable even with zero traffic.
type Heartbeat struct {
	qm       *QueueManager
	interval time.Duration
	logger   *slog.Logger
}

// NewHeartbeat creates a Heartbeat over the given queue manager. A
// non-positive interval falls back to defaultHeartbeatInterval.
func NewHeartbeat(qm *QueueManager, interval time.Duration) *Heartbeat {
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	return &Heartbeat{
		qm:       qm,
		interval: interval,
		logger:   slog.With(slog.String("component", "Heartbeat")),
	}
}

// Run emits one heartbeat immediately and then one per interval until ctx is
// cancelled. It blocks; run it in its own goroutine.
func (h *Heartbeat) Run(ctx context.Context) {
	h.emit()

	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.emit()
		}
	}
}

// emit logs one summary line (always) plus one line per backend queue. The
// summary line is the absence-alert signal; the per-backend lines feed the
// queue-depth gauge.
func (h *Heartbeat) emit() {
	stats := h.qm.QueueStats()

	total, max, capacity := 0, 0, 0
	for _, s := range stats {
		total += s.Length
		if s.Length > max {
			max = s.Length
		}
		if s.Capacity > capacity {
			capacity = s.Capacity
		}
	}

	h.logger.Info("heartbeat",
		logKeyAttr(logKeyHeartbeat),
		slog.Int("goroutines", runtime.NumGoroutine()),
		slog.Int("queues", len(stats)),
		slog.Int("queue_length_total", total),
		slog.Int("queue_length_max", max),
		slog.Int("queue_capacity", capacity),
	)

	for _, s := range stats {
		h.logger.Info("queue heartbeat",
			logKeyAttr(logKeyHeartbeat),
			slog.String("backend_id", s.BackendID),
			slog.Int("queue_length", s.Length),
			slog.Int("queue_capacity", s.Capacity),
		)
	}
}
