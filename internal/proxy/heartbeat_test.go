package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a goroutine-safe log sink for tests that read captured lines
// while a logging goroutine is still running.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSyncLogger is captureLogger with a goroutine-safe buffer.
func captureSyncLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// parseLogLines decodes every captured JSON log line into a generic map.
func parseLogLines(t *testing.T, logged string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logged), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		out = append(out, rec)
	}
	return out
}

// findLogLine returns the first captured line with the given msg, or nil.
func findLogLine(lines []map[string]any, msg string) map[string]any {
	for _, rec := range lines {
		if rec["msg"] == msg {
			return rec
		}
	}
	return nil
}

// heartbeatQueueManager builds a QueueManager with worker-less queues so queue
// lengths stay deterministic (GetQueue would start a consuming worker).
func heartbeatQueueManager(queueSizes map[string]int, capacity int) *QueueManager {
	qm := NewQueueManager()
	for backendID, size := range queueSizes {
		rq := &RequestQueue{
			backendID: backendID,
			queue:     make(chan *QueuedRequest, capacity),
		}
		for range size {
			rq.queue <- &QueuedRequest{}
		}
		qm.queues[backendID] = rq
	}
	return qm
}

func TestNewHeartbeat_DefaultsNonPositiveInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		expected time.Duration
	}{
		{name: "zero interval", interval: 0, expected: defaultHeartbeatInterval},
		{name: "negative interval", interval: -time.Second, expected: defaultHeartbeatInterval},
		{name: "explicit interval", interval: 5 * time.Second, expected: 5 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hb := NewHeartbeat(NewQueueManager(), tt.interval)
			assert.Equal(t, tt.expected, hb.interval)
		})
	}
}

func TestHeartbeat_EmitsSummaryLine(t *testing.T) {
	qm := heartbeatQueueManager(map[string]int{"backend-a": 3, "backend-b": 1}, 8)
	hb := NewHeartbeat(qm, time.Hour)
	logger, buf := captureLogger()
	hb.logger = logger

	hb.emit()

	summary := findLogLine(parseLogLines(t, buf.String()), "heartbeat")
	require.NotNil(t, summary, "expected a summary heartbeat line")
	assert.Equal(t, logKeyHeartbeat, summary["log_key"])
	assert.Equal(t, float64(2), summary["queues"])
	assert.Equal(t, float64(4), summary["queue_length_total"])
	assert.Equal(t, float64(3), summary["queue_length_max"])
	assert.Equal(t, float64(8), summary["queue_capacity"])
	assert.Greater(t, summary["goroutines"], float64(0))
}

func TestHeartbeat_EmitsPerBackendLines(t *testing.T) {
	qm := heartbeatQueueManager(map[string]int{"backend-a": 2, "backend-b": 0}, 8)
	hb := NewHeartbeat(qm, time.Hour)
	logger, buf := captureLogger()
	hb.logger = logger

	hb.emit()

	perBackend := map[string]float64{}
	for _, rec := range parseLogLines(t, buf.String()) {
		if rec["msg"] != "queue heartbeat" {
			continue
		}
		assert.Equal(t, logKeyHeartbeat, rec["log_key"])
		assert.Equal(t, float64(8), rec["queue_capacity"])
		perBackend[rec["backend_id"].(string)] = rec["queue_length"].(float64)
	}
	assert.Equal(t, map[string]float64{"backend-a": 2, "backend-b": 0}, perBackend)
}

func TestHeartbeat_EmptyManagerEmitsOnlySummary(t *testing.T) {
	hb := NewHeartbeat(NewQueueManager(), time.Hour)
	logger, buf := captureLogger()
	hb.logger = logger

	hb.emit()

	lines := parseLogLines(t, buf.String())
	require.Len(t, lines, 1)
	assert.Equal(t, "heartbeat", lines[0]["msg"])
	assert.Equal(t, float64(0), lines[0]["queues"])
}

func TestHeartbeat_RunEmitsImmediatelyAndStopsOnCancel(t *testing.T) {
	hb := NewHeartbeat(NewQueueManager(), time.Hour)
	logger, buf := captureSyncLogger()
	hb.logger = logger

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hb.Run(ctx)
		close(done)
	}()

	// The first heartbeat is emitted before the first tick.
	assert.Eventually(t, func() bool {
		return findLogLine(parseLogLines(t, buf.String()), "heartbeat") != nil
	}, time.Second, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestHeartbeat_RunTicksOnInterval(t *testing.T) {
	hb := NewHeartbeat(NewQueueManager(), 10*time.Millisecond)
	logger, buf := captureSyncLogger()
	hb.logger = logger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.Run(ctx)

	assert.Eventually(t, func() bool {
		count := 0
		for _, rec := range parseLogLines(t, buf.String()) {
			if rec["msg"] == "heartbeat" {
				count++
			}
		}
		return count >= 3
	}, time.Second, 10*time.Millisecond)
}
