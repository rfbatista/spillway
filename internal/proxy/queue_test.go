package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rfbatista/spillway/internal/apperr"
	"github.com/rfbatista/spillway/internal/domain"
	"github.com/rfbatista/spillway/internal/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

var errGetUsage = errors.New("get usage failed")

func testQueueWorker(t *testing.T, backendID string, get *mocks.MockGetUsageService, save *mocks.MockSaveUsageService) *QueueWorker {
	t.Helper()
	rq := &RequestQueue{
		backendID: backendID,
		queue:     make(chan *QueuedRequest, 32),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if save == nil {
		save = mocks.NewMockSaveUsageService(t)
	}
	w := &QueueWorker{
		backendID:     backendID,
		requestQueue:  rq,
		baseTransport: http.DefaultTransport,
		getusage:      get,
		saveusage:     save,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		stopChan:      make(chan struct{}),
	}
	return w
}

func TestEnqueueRequest_QueueFull(t *testing.T) {
	rq := &RequestQueue{
		queue:  make(chan *QueuedRequest, 1),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	_ = rq.EnqueueRequest(req, 1, time.Now(), "first")
	ch2 := rq.EnqueueRequest(req, 1, time.Now(), "second")
	qr := <-ch2
	if qr == nil || qr.Error == nil {
		t.Fatal("expected error on second enqueue when queue is full")
	}
	if !errors.Is(qr.Error, apperr.ErrRetryQueueFull) {
		t.Fatalf("expected ErrRetryQueueFull, got %v", qr.Error)
	}
}

func TestEnqueueRequest_BufferedResponseOnFullQueue(t *testing.T) {
	rq := &RequestQueue{
		queue:  make(chan *QueuedRequest, 0),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	ch := rq.EnqueueRequest(req, 1, time.Now(), "blocked")
	select {
	case qr := <-ch:
		if qr == nil || !errors.Is(qr.Error, apperr.ErrRetryQueueFull) {
			t.Fatalf("expected ErrRetryQueueFull when channel buffer is 0 and send blocks, got %#v", qr)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("expected immediate ErrRetryQueueFull on full queue (default branch)")
	}
}

func TestQueueManager_GetQueue_SameBackendID(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	get.On("Execute", mock.Anything, mock.Anything).Maybe().Return(&domain.ModelUsageQuota{}, nil)
	save.On("Execute", mock.Anything, mock.Anything).Maybe().Return(nil)

	qm := NewQueueManager()
	a := qm.GetQueue("same", http.DefaultTransport, get, save)
	b := qm.GetQueue("same", http.DefaultTransport, get, save)
	if a != b {
		t.Fatal("GetQueue should return the same RequestQueue for the same backend ID")
	}
}

// TestQueueWorker_Stop_IdempotentWhenCalledTwiceOnSameWorker is the load-bearing
// regression test: it calls Stop() twice on the SAME *QueueWorker instance,
// which is what actually exercises the sync.Once guard around close(stopChan).
// Without the guard, the second Stop() would close an already-closed channel
// and panic.
func TestQueueWorker_Stop_IdempotentWhenCalledTwiceOnSameWorker(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	get.On("Execute", mock.Anything, mock.Anything).Maybe().Return(&domain.ModelUsageQuota{}, nil)
	save.On("Execute", mock.Anything, mock.Anything).Maybe().Return(nil)

	qm := NewQueueManager()
	rq := qm.GetQueue("backend-a", http.DefaultTransport, get, save)
	worker := rq.worker

	assert.NotPanics(t, func() {
		worker.Stop()
		worker.Stop()
	})
}

// TestQueueManager_StopAll_SafeWhenCalledTwice covers StopAll's own claim of
// being safe to call more than once. Because StopAll deletes each entry from
// qm.queues as it stops it, the second call iterates an empty map and never
// re-invokes Stop() on any worker — so this test alone does NOT exercise the
// sync.Once guard; see TestQueueWorker_Stop_IdempotentWhenCalledTwiceOnSameWorker
// for that.
func TestQueueManager_StopAll_SafeWhenCalledTwice(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	get.On("Execute", mock.Anything, mock.Anything).Maybe().Return(&domain.ModelUsageQuota{}, nil)
	save.On("Execute", mock.Anything, mock.Anything).Maybe().Return(nil)

	qm := NewQueueManager()
	qm.GetQueue("backend-a", http.DefaultTransport, get, save)
	qm.GetQueue("backend-b", http.DefaultTransport, get, save)

	assert.NotPanics(t, func() {
		qm.StopAll()
		qm.StopAll()
	})
}

func TestQueueManager_GetQueue_DifferentBackendID(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	get.On("Execute", mock.Anything, mock.Anything).Maybe().Return(&domain.ModelUsageQuota{}, nil)
	save.On("Execute", mock.Anything, mock.Anything).Maybe().Return(nil)

	qm := NewQueueManager()
	a := qm.GetQueue("backend-a", http.DefaultTransport, get, save)
	b := qm.GetQueue("backend-b", http.DefaultTransport, get, save)
	if a == b {
		t.Fatal("GetQueue should return distinct RequestQueue values for different backend IDs")
	}
}

func TestEnqueueRequest_ProcessedByWorker(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	get.On("Execute", mock.Anything, "live-backend").Return(&domain.ModelUsageQuota{ID: "live-backend"}, nil).Once()
	save.On("Execute", mock.Anything, mock.Anything).Maybe().Return(nil)

	qm := NewQueueManager()
	rq := qm.GetQueue("live-backend", http.DefaultTransport, get, save)
	req := httptest.NewRequest("GET", "http://example.com/foo", nil)
	ch := rq.EnqueueRequest(req, 2, time.Time{}, "req-1")

	select {
	case qr := <-ch:
		if qr.Error != nil {
			t.Fatalf("expected nil error, got %v", qr.Error)
		}
		if qr.Response != nil {
			t.Fatalf("expected nil Response on success path, got %#v", qr.Response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for queued response")
	}
	get.AssertExpectations(t)
}

func TestProcessRequest_GetUsageFails(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	get.On("Execute", mock.Anything, "b-get-fail").Return(nil, errGetUsage).Once()

	w := testQueueWorker(t, "b-get-fail", get, save)
	ch := make(chan *QueuedResponse, 1)
	req := httptest.NewRequest("GET", "http://example.com/r", nil)
	w.processRequest(&QueuedRequest{
		Request:    req,
		Response:   ch,
		Attempt:    1,
		RequestID:  "rid",
		EnqueuedAt: time.Now(),
		RetryAfter: time.Time{},
	})

	qr := <-ch
	if qr == nil || !errors.Is(qr.Error, errGetUsage) {
		t.Fatalf("expected errGetUsage, got %#v", qr)
	}
	get.AssertExpectations(t)
}

func TestProcessRequest_AllowsWhenResetAtZero(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	get.On("Execute", mock.Anything, "b-clear").Return(&domain.ModelUsageQuota{
		ID:      "b-clear",
		ResetAt: time.Time{},
	}, nil).Once()

	w := testQueueWorker(t, "b-clear", get, save)
	ch := make(chan *QueuedResponse, 1)
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	w.processRequest(&QueuedRequest{
		Request:    req,
		Response:   ch,
		Attempt:    1,
		RequestID:  "x",
		EnqueuedAt: time.Now(),
		RetryAfter: time.Time{},
	})

	qr := <-ch
	if qr.Error != nil {
		t.Fatalf("expected nil error, got %v", qr.Error)
	}
	get.AssertExpectations(t)
}

func TestProcessRequest_AllowsWhenResetAtInPast(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	get.On("Execute", mock.Anything, "b-past").Return(&domain.ModelUsageQuota{
		ID:      "b-past",
		ResetAt: time.Now().Add(-time.Hour),
	}, nil).Once()

	w := testQueueWorker(t, "b-past", get, save)
	ch := make(chan *QueuedResponse, 1)
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	w.processRequest(&QueuedRequest{
		Request:    req,
		Response:   ch,
		Attempt:    1,
		RequestID:  "x",
		EnqueuedAt: time.Now(),
		RetryAfter: time.Time{},
	})

	qr := <-ch
	if qr.Error != nil {
		t.Fatalf("expected nil error, got %v", qr.Error)
	}
	get.AssertExpectations(t)
}

func TestProcessRequest_RetryAfterDelaysBeforeGetUsage(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	delay := 40 * time.Millisecond
	get.On("Execute", mock.Anything, "b-delay").Return(&domain.ModelUsageQuota{ID: "b-delay"}, nil).Once()

	w := testQueueWorker(t, "b-delay", get, save)
	ch := make(chan *QueuedResponse, 1)
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	start := time.Now()
	w.processRequest(&QueuedRequest{
		Request:    req,
		Response:   ch,
		Attempt:    1,
		RequestID:  "x",
		EnqueuedAt: time.Now(),
		RetryAfter: start.Add(delay),
	})

	<-ch
	elapsed := time.Since(start)
	if elapsed < delay {
		t.Fatalf("expected at least %v before getusage, got %v", delay, elapsed)
	}
	get.AssertExpectations(t)
}

func TestQueueWorker_saveUsage_PassesParsedQuotaToSave(t *testing.T) {
	save := mocks.NewMockSaveUsageService(t)
	save.On("Execute", mock.Anything, mock.MatchedBy(func(u domain.ModelUsageQuota) bool {
		return u.ID == "save-backend" &&
			u.RemainingTokens == 7 &&
			u.RemainingRequests == 3
	})).Return(nil).Once()

	get := mocks.NewMockGetUsageService(t)
	w := testQueueWorker(t, "save-backend", get, save)

	resp := &http.Response{
		Header: http.Header{
			"X-Ratelimit-Remaining-Tokens":   {"7"},
			"X-Ratelimit-Remaining-Requests": {"3"},
			"X-Ratelimit-Reset-Tokens":       {"30s"},
			"X-Ratelimit-Reset-Requests":     {"1m"},
		},
	}
	w.saveUsage(resp, "test-request-id")
	save.AssertExpectations(t)
}

func TestQueueWorker_saveUsage_IgnoresSaveError(t *testing.T) {
	save := mocks.NewMockSaveUsageService(t)
	save.On("Execute", mock.Anything, mock.Anything).Return(errors.New("persist failed")).Once()

	get := mocks.NewMockGetUsageService(t)
	w := testQueueWorker(t, "save-err", get, save)

	resp := &http.Response{Header: http.Header{
		"X-Ratelimit-Remaining-Tokens": {"1"},
	}}
	w.saveUsage(resp, "test-request-id")
	save.AssertExpectations(t)
}

func TestQueueWorker_Stop_UnblocksStart(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	w := testQueueWorker(t, "stop-me", get, save)

	done := make(chan struct{})
	go func() {
		w.Start()
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	w.Stop()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

func TestProcessRequest_ReEnqueueFailsWhenQueueFull(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps until retryAfter (~1s) from queue.go re-enqueue path")
	}
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	// Still rate-limited: ResetAt in the near future so the re-enqueue path runs.
	resetAt := time.Now().Add(50 * time.Millisecond)
	get.On("Execute", mock.Anything, "b-refill").Return(&domain.ModelUsageQuota{
		ID:      "b-refill",
		ResetAt: resetAt,
	}, nil).Once()

	rq := &RequestQueue{
		backendID: "b-refill",
		queue:     make(chan *QueuedRequest, 0),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	workerLogger, workerBuf := captureSyncLogger()
	w := &QueueWorker{
		backendID:     "b-refill",
		requestQueue:  rq,
		baseTransport: http.DefaultTransport,
		getusage:      get,
		saveusage:     save,
		logger:        workerLogger,
		stopChan:      make(chan struct{}),
	}

	ch := make(chan *QueuedResponse, 1)
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	queued := &QueuedRequest{
		Request:    req,
		Response:   ch,
		Attempt:    1,
		RequestID:  "requeue-full",
		EnqueuedAt: time.Now(),
		RetryAfter: time.Time{},
	}

	w.processRequest(queued)

	retryAfter := resetAt.Add(time.Second)
	wait := time.Until(retryAfter) + 100*time.Millisecond
	if wait < 0 {
		wait = 100 * time.Millisecond
	}

	select {
	case qr := <-ch:
		if !errors.Is(qr.Error, apperr.ErrRetryQueueFull) {
			t.Fatalf("expected ErrRetryQueueFull when re-enqueue cannot send, got %#v", qr)
		}
	case <-time.After(wait):
		t.Fatal("timeout waiting for re-enqueue failure response")
	}
	get.AssertExpectations(t)

	rejected := findLogLine(parseLogLines(t, workerBuf.String()), "re-enqueue failed: queue full")
	if assert.NotNil(t, rejected, "expected a re-enqueue rejection log line") {
		assert.Equal(t, queueEventRejectedFull, rejected["queue_event"])
		assert.Equal(t, logKeyRetryQueue, rejected["log_key"])
	}
}

func TestQueueManager_QueueStats(t *testing.T) {
	t.Run("empty manager", func(t *testing.T) {
		assert.Empty(t, NewQueueManager().QueueStats())
	})

	t.Run("reports length and capacity per backend", func(t *testing.T) {
		qm := heartbeatQueueManager(map[string]int{"backend-a": 3, "backend-b": 0}, 16)

		assert.ElementsMatch(t, []QueueStats{
			{BackendID: "backend-a", Length: 3, Capacity: 16},
			{BackendID: "backend-b", Length: 0, Capacity: 16},
		}, qm.QueueStats())
	})
}

func TestEnqueueRequest_QueueEventAttrs(t *testing.T) {
	logger, buf := captureLogger()
	rq := &RequestQueue{
		queue:  make(chan *QueuedRequest, 1),
		logger: logger,
	}
	req := httptest.NewRequest("GET", "http://example.com/", nil)

	_ = rq.EnqueueRequest(req, 1, time.Now(), "fits")
	_ = rq.EnqueueRequest(req, 1, time.Now(), "rejected")

	lines := parseLogLines(t, buf.String())

	enqueued := findLogLine(lines, "request enqueued for retry")
	if assert.NotNil(t, enqueued, "expected an enqueue log line") {
		assert.Equal(t, queueEventEnqueued, enqueued["queue_event"])
		assert.Equal(t, logKeyRetryQueue, enqueued["log_key"])
	}

	rejected := findLogLine(lines, "queue is full, rejecting request")
	if assert.NotNil(t, rejected, "expected a rejection log line") {
		assert.Equal(t, queueEventRejectedFull, rejected["queue_event"])
		assert.Equal(t, logKeyRetryQueue, rejected["log_key"])
	}
}

// A config reload that removes a backend must stop reporting its queue: Prune
// drops the stale entry (its worker keeps draining on its own — see Prune) and
// a later reload that re-adds the ID starts from a fresh queue.
func TestQueueManager_Prune_DropsStaleQueues(t *testing.T) {
	get := mocks.NewMockGetUsageService(t)
	save := mocks.NewMockSaveUsageService(t)
	get.On("Execute", mock.Anything, mock.Anything).Maybe().Return(&domain.ModelUsageQuota{}, nil)
	save.On("Execute", mock.Anything, mock.Anything).Maybe().Return(nil)

	qm := NewQueueManager()
	kept := qm.GetQueue("backend-kept", http.DefaultTransport, get, save)
	stale := qm.GetQueue("backend-stale", http.DefaultTransport, get, save)

	qm.Prune(map[string]struct{}{"backend-kept": {}})

	stats := qm.QueueStats()
	assert.Len(t, stats, 1)
	assert.Equal(t, "backend-kept", stats[0].BackendID)

	assert.Same(t, kept, qm.GetQueue("backend-kept", http.DefaultTransport, get, save),
		"an active queue must survive the prune")
	assert.NotSame(t, stale, qm.GetQueue("backend-stale", http.DefaultTransport, get, save),
		"a pruned ID must start from a fresh queue when it comes back")
}
