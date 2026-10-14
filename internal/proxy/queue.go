package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rfbatista/spillway/internal/apperr"
	"github.com/rfbatista/spillway/internal/domain"
	"github.com/rfbatista/spillway/internal/ports"
)

// queue_event values attached to retry_queue log lines. Alerts and dashboards
// key on log_key="retry_queue" plus queue_event, so the human-readable msg text
// can be reworded freely.
const (
	queueEventEnqueued     = "enqueued"
	queueEventDequeued     = "dequeued"
	queueEventReEnqueued   = "re_enqueued"
	queueEventRejectedFull = "rejected_full"
)

// QueuedRequest represents a request waiting in the retry queue
type QueuedRequest struct {
	Request    *http.Request
	Response   chan *QueuedResponse
	Attempt    int
	RequestID  string
	EnqueuedAt time.Time
	RetryAfter time.Time
}

// QueuedResponse represents the response from a queued request
type QueuedResponse struct {
	Response *http.Response
	Error    error
}

// QueueManager manages retry queues for each backend
type QueueManager struct {
	queues map[string]*RequestQueue
	mutex  sync.RWMutex
	logger *slog.Logger
}

// RequestQueue manages requests for a specific backend
type RequestQueue struct {
	backendID string
	queue     chan *QueuedRequest
	worker    *QueueWorker
	mutex     sync.RWMutex
	logger    *slog.Logger
}

// QueueWorker processes requests from the queue
type QueueWorker struct {
	backendID     string
	requestQueue  *RequestQueue
	baseTransport http.RoundTripper
	getusage      ports.GetUsageService
	saveusage     ports.SaveUsageService
	logger        *slog.Logger
	stopChan      chan struct{}
	stopOnce      sync.Once
}

func buildRequestLogAttrs(req *http.Request) []any {
	if req == nil || req.URL == nil {
		return nil
	}

	return []any{
		slog.String("method", req.Method),
		slog.String("path", req.URL.Path),
		slog.String("host", req.URL.Host),
		slog.String("query", req.URL.RawQuery),
	}
}

// NewQueueManager creates a new queue manager
func NewQueueManager() *QueueManager {
	return &QueueManager{
		queues: make(map[string]*RequestQueue),
		logger: slog.With(slog.String("component", "QueueManager")),
	}
}

// StopAll stops every queue worker this manager owns. It is test-only today —
// nothing in production calls it, and it is NOT part of the pool-swap path,
// because pools share a single QueueManager and a worker outlives any one pool
// generation.
//
// Do NOT wire this into process shutdown as-is. RetryTransport.RoundTrip
// (retry.go) blocks on a bare `qr := <-responseChan` with no context, no
// timeout, and no default case. If a worker is stopped while requests are
// still sitting in its queue, every caller waiting on that worker's responses
// hangs forever — a worse failure mode than the leak this type fixes. The same
// hazard applies to the ungated re-enqueue goroutine started in
// processRequest. Draining each queue (and unblocking any waiting callers) is
// a prerequisite for ever using StopAll on a shutdown path.
//
// Safe to call more than once.
func (qm *QueueManager) StopAll() {
	qm.mutex.Lock()
	defer qm.mutex.Unlock()
	for id, queue := range qm.queues {
		queue.worker.Stop()
		delete(qm.queues, id)
	}
}

// Prune drops every queue whose backend ID is not in active, so a config
// reload that removes or renames backends stops reporting phantom queues via
// QueueStats and the heartbeat, and a later reload that re-adds the ID starts
// from a fresh queue. NewBackendPool calls it after a successful build.
//
// The pruned queue's worker is deliberately left running: in-flight requests
// from the previous pool generation may still hold a pointer to the queue and
// block on its responses, and stopping the worker would strand them forever
// (see StopAll). Left alone, the worker drains those naturally and then idles.
func (qm *QueueManager) Prune(active map[string]struct{}) {
	qm.mutex.Lock()
	defer qm.mutex.Unlock()
	for id := range qm.queues {
		if _, ok := active[id]; !ok {
			delete(qm.queues, id)
		}
	}
}

// QueueStats is a point-in-time snapshot of one backend's retry queue.
type QueueStats struct {
	BackendID string
	Length    int
	Capacity  int
}

// QueueStats snapshots the length and capacity of every queue this manager
// owns. Safe for concurrent use; lengths are instantaneous and may drift the
// moment the lock is released.
func (qm *QueueManager) QueueStats() []QueueStats {
	qm.mutex.RLock()
	defer qm.mutex.RUnlock()

	stats := make([]QueueStats, 0, len(qm.queues))
	for id, queue := range qm.queues {
		stats = append(stats, QueueStats{
			BackendID: id,
			Length:    len(queue.queue),
			Capacity:  cap(queue.queue),
		})
	}
	return stats
}

// GetQueue returns or creates a queue for the given backend ID.
//
// On a cache hit, the passed baseTransport, getusage, and saveusage arguments
// are ignored: the returned queue's worker keeps whatever was supplied on the
// first call for that backend ID. baseTransport is currently unused by
// QueueWorker — it is stored on the struct but never read — so its pinning has
// no observable effect either way. getusage/saveusage pinning is real: this is
// safe today because cmd/server/main.go passes the same process-wide
// singleton services on every call, including across pool rebuilds. If a
// future caller ever needs getusage/saveusage to vary per backend after the
// first build, this pinning will silently prevent that from taking effect.
func (qm *QueueManager) GetQueue(backendID string, baseTransport http.RoundTripper, getusage ports.GetUsageService, saveusage ports.SaveUsageService) *RequestQueue {
	qm.mutex.Lock()
	defer qm.mutex.Unlock()

	if queue, exists := qm.queues[backendID]; exists {
		return queue
	}

	queue := &RequestQueue{
		backendID: backendID,
		queue:     make(chan *QueuedRequest, 1000), // Buffer for up to 1000 queued requests
		logger:    slog.With(slog.String("component", "RequestQueue"), slog.String("backend", backendID)),
	}

	worker := &QueueWorker{
		backendID:     backendID,
		requestQueue:  queue,
		baseTransport: baseTransport,
		getusage:      getusage,
		saveusage:     saveusage,
		logger:        slog.With(slog.String("component", "QueueWorker"), slog.String("backend", backendID)),
		stopChan:      make(chan struct{}),
	}

	queue.worker = worker
	qm.queues[backendID] = queue

	// Start the worker
	go worker.Start()

	return queue
}

// EnqueueRequest adds a request to the queue for retry
func (rq *RequestQueue) EnqueueRequest(req *http.Request, attempt int, retryAfter time.Time, requestID string) <-chan *QueuedResponse {
	responseChan := make(chan *QueuedResponse, 1)

	queuedReq := &QueuedRequest{
		Request:    req,
		Response:   responseChan,
		Attempt:    attempt,
		RequestID:  requestID,
		EnqueuedAt: time.Now(),
		RetryAfter: retryAfter,
	}

	select {
	case rq.queue <- queuedReq:
		attrs := append([]any{
			logKeyAttr(logKeyRetryQueue),
			slog.String("queue_event", queueEventEnqueued),
			slog.String("request_id", requestID),
			slog.Int("attempt", attempt),
			slog.Time("retry_after", retryAfter),
			slog.Duration("retry_after_in", time.Until(retryAfter)),
			slog.Int("queue_length", len(rq.queue)),
			slog.Int("queue_capacity", cap(rq.queue)),
		}, buildRequestLogAttrs(req)...)
		rq.logger.Info("request enqueued for retry", attrs...)
	default:
		attrs := append([]any{
			logKeyAttr(logKeyRetryQueue),
			slog.String("queue_event", queueEventRejectedFull),
			slog.String("request_id", requestID),
			slog.Int("attempt", attempt),
			slog.Int("queue_length", len(rq.queue)),
			slog.Int("queue_capacity", cap(rq.queue)),
		}, buildRequestLogAttrs(req)...)
		rq.logger.Error("queue is full, rejecting request", attrs...)
		responseChan <- &QueuedResponse{
			Error: apperr.ErrRetryQueueFull,
		}
	}

	return responseChan
}

// Start begins processing requests from the queue
func (qw *QueueWorker) Start() {
	qw.logger.Info("queue worker started",
		logKeyAttr(logKeyRetryQueue),
		slog.String("backend_id", qw.backendID),
		slog.Int("queue_length", len(qw.requestQueue.queue)),
		slog.Int("queue_capacity", cap(qw.requestQueue.queue)),
	)
	defer qw.logger.Info("queue worker stopped",
		logKeyAttr(logKeyRetryQueue),
		slog.String("backend_id", qw.backendID),
		slog.Int("queue_length", len(qw.requestQueue.queue)),
		slog.Int("queue_capacity", cap(qw.requestQueue.queue)),
	)

	for {
		select {
		case <-qw.stopChan:
			qw.logger.Info("stopping queue worker",
				logKeyAttr(logKeyRetryQueue),
				slog.String("backend_id", qw.backendID),
				slog.Int("queue_length", len(qw.requestQueue.queue)),
			)
			return
		case queuedReq := <-qw.requestQueue.queue:
			qw.processRequest(queuedReq)
		}
	}
}

// Stop stops the queue worker. Safe to call more than once.
func (qw *QueueWorker) Stop() {
	qw.stopOnce.Do(func() { close(qw.stopChan) })
}

// processRequest processes a single queued request
func (qw *QueueWorker) processRequest(queuedReq *QueuedRequest) {
	attrs := append([]any{
		logKeyAttr(logKeyRetryQueue),
		slog.String("queue_event", queueEventDequeued),
		slog.String("request_id", queuedReq.RequestID),
		slog.Int("attempt", queuedReq.Attempt),
		slog.Duration("time_in_queue", time.Since(queuedReq.EnqueuedAt)),
		slog.Time("enqueued_at", queuedReq.EnqueuedAt),
		slog.Time("retry_after", queuedReq.RetryAfter),
		slog.Int("queue_length", len(qw.requestQueue.queue)),
		slog.Int("queue_capacity", cap(qw.requestQueue.queue)),
	}, buildRequestLogAttrs(queuedReq.Request)...)
	qw.logger.Info("dequeued request for processing", attrs...)

	now := time.Now()
	if queuedReq.RetryAfter.After(now) {
		waitTime := queuedReq.RetryAfter.Sub(now)
		attrs := append([]any{
			logKeyAttr(logKeyRetryQueue),
			slog.String("request_id", queuedReq.RequestID),
			slog.Duration("wait_time", waitTime),
			slog.Int("attempt", queuedReq.Attempt),
			slog.Time("retry_after", queuedReq.RetryAfter),
		}, buildRequestLogAttrs(queuedReq.Request)...)
		qw.logger.Info("waiting before retry", attrs...)
		time.Sleep(waitTime)
	}

	usage, err := qw.getusage.Execute(context.Background(), qw.backendID)
	if err != nil {
		attrs := append([]any{
			logKeyAttr(logKeyRetryQueue),
			slog.String("request_id", queuedReq.RequestID),
			slog.Int("attempt", queuedReq.Attempt),
			slog.Int("queue_length", len(qw.requestQueue.queue)),
			slog.Any("error", err),
		}, buildRequestLogAttrs(queuedReq.Request)...)
		qw.logger.Error("failed to get usage for retry decision", attrs...)
		queuedReq.Response <- &QueuedResponse{Error: err}
		return
	}

	if !usage.ResetAt.IsZero() && time.Now().Before(usage.ResetAt) {
		retryAfter := usage.ResetAt.Add(time.Second)
		attrs := append([]any{
			logKeyAttr(logKeyRetryQueue),
			slog.String("request_id", queuedReq.RequestID),
			slog.Time("reset_at", usage.ResetAt),
			slog.Time("retry_after", retryAfter),
			slog.Duration("retry_after_in", time.Until(retryAfter)),
			slog.Int("attempt", queuedReq.Attempt),
			slog.Int64("remaining_requests", usage.RemainingRequests),
			slog.Int64("remaining_tokens", usage.RemainingTokens),
			slog.Int("queue_length", len(qw.requestQueue.queue)),
			slog.Int("queue_capacity", cap(qw.requestQueue.queue)),
		}, buildRequestLogAttrs(queuedReq.Request)...)
		qw.logger.Info("still rate limited, re-enqueuing", attrs...)

		go func() {
			time.Sleep(time.Until(retryAfter))
			select {
			case qw.requestQueue.queue <- queuedReq:
				attrs := append([]any{
					logKeyAttr(logKeyRetryQueue),
					slog.String("queue_event", queueEventReEnqueued),
					slog.String("request_id", queuedReq.RequestID),
					slog.Int("attempt", queuedReq.Attempt),
					slog.Time("retry_after", retryAfter),
					slog.Duration("total_time_in_queue", time.Since(queuedReq.EnqueuedAt)),
					slog.Int("queue_length", len(qw.requestQueue.queue)),
					slog.Int("queue_capacity", cap(qw.requestQueue.queue)),
				}, buildRequestLogAttrs(queuedReq.Request)...)
				qw.logger.Info("request re-enqueued for another retry cycle", attrs...)
			default:
				attrs := append([]any{
					logKeyAttr(logKeyRetryQueue),
					slog.String("queue_event", queueEventRejectedFull),
					slog.String("request_id", queuedReq.RequestID),
					slog.Int("attempt", queuedReq.Attempt),
					slog.Int("queue_length", len(qw.requestQueue.queue)),
					slog.Int("queue_capacity", cap(qw.requestQueue.queue)),
				}, buildRequestLogAttrs(queuedReq.Request)...)
				qw.logger.Error("re-enqueue failed: queue full", attrs...)
				queuedReq.Response <- &QueuedResponse{Error: apperr.ErrRetryQueueFull}
			}
		}()
		return
	}

	attrs = append([]any{
		logKeyAttr(logKeyRetryQueue),
		slog.String("request_id", queuedReq.RequestID),
		slog.Int("attempt", queuedReq.Attempt),
		slog.Duration("total_time_in_queue", time.Since(queuedReq.EnqueuedAt)),
		slog.Int("queue_length", len(qw.requestQueue.queue)),
	}, buildRequestLogAttrs(queuedReq.Request)...)
	qw.logger.Info("re-executing queued request", attrs...)
	queuedReq.Response <- &QueuedResponse{}
}

// saveUsage saves the usage information from the response
func (qw *QueueWorker) saveUsage(resp *http.Response, requestID string) {
	if resp == nil {
		qw.logger.Warn("attempted to save usage for nil response",
			slog.String("request_id", requestID),
			slog.String("backend_id", qw.backendID),
		)
		return
	}
	var limitRPM, limitTPM, rpm, tpm int64
	var requestResetTime, tokenResetTime, upstreamRequestID string

	for key, values := range resp.Header {
		for _, v := range values {
			switch key {
			case "X-Ratelimit-Limit-Requests":
				limitRPM, _ = strconv.ParseInt(v, 10, 0)
			case "X-Ratelimit-Limit-Tokens":
				limitTPM, _ = strconv.ParseInt(v, 10, 0)
			case "X-Ratelimit-Remaining-Requests":
				rpm, _ = strconv.ParseInt(v, 10, 0)
			case "X-Ratelimit-Remaining-Tokens":
				tpm, _ = strconv.ParseInt(v, 10, 0)
			case "X-Ratelimit-Reset-Requests":
				requestResetTime = v
			case "X-Ratelimit-Reset-Tokens":
				tokenResetTime = v
			case "X-Request-Id":
				upstreamRequestID = v
			}
		}
	}

	usage := domain.NewModelUsageQuota(qw.backendID,
		domain.WithLimitRequests(limitRPM),
		domain.WithLimitTokens(limitTPM),
		domain.WithRemainingRequests(rpm),
		domain.WithRemainingTokens(tpm),
		domain.WithResetRequests(requestResetTime),
		domain.WithResetTokens(tokenResetTime),
	)

	if err := qw.saveusage.Execute(context.Background(), usage); err != nil {
		qw.logger.Error("failed to save usage",
			slog.String("request_id", requestID),
			slog.String("upstream_request_id", upstreamRequestID),
			slog.Any("error", err),
		)
		return
	}

	satReq := saturationPct(usage.LimitRequests, usage.RemainingRequests)
	satTok := saturationPct(usage.LimitTokens, usage.RemainingTokens)

	qw.logger.Info("rate_limit_snapshot",
		logKeyAttr(logKeyRateLimitSnapshot),
		slog.String("backend_id", qw.backendID),
		slog.String("request_id", requestID),
		slog.String("upstream_request_id", upstreamRequestID),
		slog.Int64("limit_requests", usage.LimitRequests),
		slog.Int64("limit_tokens", usage.LimitTokens),
		slog.Int64("remaining_requests", usage.RemainingRequests),
		slog.Int64("remaining_tokens", usage.RemainingTokens),
		slog.Float64("saturation_requests_pct", satReq),
		slog.Float64("saturation_tokens_pct", satTok),
		slog.Time("reset_at", usage.ResetAt),
	)
}
