package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"proxy/internal/domain"
	"proxy/internal/ports"
)

// truncateBody caps body to maxLen bytes. It works on the raw bytes so the hot
// path never copies a large body into a string just to cut it back down.
func truncateBody(body []byte, maxLen int) string {
	if len(body) <= maxLen {
		return string(body)
	}
	return string(body[:maxLen]) + truncationMarker
}

type RetryTransport struct {
	ID    string
	Model string
	// ClientTagHeader names the client request header captured as the client_tag
	// log field (from BackendConfig.ClientTagHeader); empty disables it.
	ClientTagHeader string
	Base            http.RoundTripper
	MaxRetries      int
	saveusage       ports.SaveUsageService
	getusage        ports.GetUsageService
	logger          *slog.Logger
	queue           *RequestQueue
	tracker         *RequestTracker
	summarizer      *logBodySummarizer
}

// RetryTransportOption configures optional transport behaviour.
type RetryTransportOption func(*RetryTransport)

// WithLogBodySummarizer sets the summarizer that shapes request and response
// bodies on their way to the logs. A nil summarizer keeps the plain byte cut.
func WithLogBodySummarizer(s *logBodySummarizer) RetryTransportOption {
	return func(t *RetryTransport) { t.summarizer = s }
}

func NewRetryTransport(
	id string,
	model string,
	saveusage ports.SaveUsageService,
	getusage ports.GetUsageService,
	queueManager *QueueManager,
	tracker *RequestTracker,
	opts ...RetryTransportOption,
) (*RetryTransport, error) {
	if saveusage == nil {
		return nil, errors.New("missing SaveModelQuote service")
	}
	if getusage == nil {
		return nil, errors.New("missing GetUsage service")
	}
	if queueManager == nil {
		return nil, errors.New("missing QueueManager")
	}
	if tracker == nil {
		return nil, errors.New("missing RequestTracker")
	}
	logger := slog.With(slog.String("component", "RetryTransport"), slog.String("id", id))

	// Get or create queue for this backend
	queue := queueManager.GetQueue(id, http.DefaultTransport, getusage, saveusage)

	t := &RetryTransport{
		ID:         id,
		Model:      model,
		logger:     logger,
		MaxRetries: 3,
		saveusage:  saveusage,
		getusage:   getusage,
		queue:      queue,
		tracker:    tracker,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t, nil
}

func (t *RetryTransport) SaveUsage(resp *http.Response, requestID string) {
	if resp == nil {
		t.logger.Warn("attempted to save usage for nil response",
			slog.String("request_id", requestID),
			slog.String("backend_id", t.ID),
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

	usage := domain.NewModelUsageQuota(t.ID,
		domain.WithLimitRequests(limitRPM),
		domain.WithLimitTokens(limitTPM),
		domain.WithRemainingRequests(rpm),
		domain.WithRemainingTokens(tpm),
		domain.WithResetRequests(requestResetTime),
		domain.WithResetTokens(tokenResetTime),
	)

	if err := t.saveusage.Execute(context.Background(), usage); err != nil {
		t.logger.Error("failed to persist usage from response",
			slog.String("request_id", requestID),
			slog.String("backend_id", t.ID),
			slog.Int64("limit_requests", usage.LimitRequests),
			slog.Int64("limit_tokens", usage.LimitTokens),
			slog.Int64("remaining_requests", usage.RemainingRequests),
			slog.Int64("remaining_tokens", usage.RemainingTokens),
			slog.Time("reset_at", usage.ResetAt),
			slog.String("upstream_request_id", upstreamRequestID),
			slog.Any("error", err),
		)
		return
	}

	satReq := saturationPct(usage.LimitRequests, usage.RemainingRequests)
	satTok := saturationPct(usage.LimitTokens, usage.RemainingTokens)

	t.logger.Info("rate_limit_snapshot",
		logKeyAttr(logKeyRateLimitSnapshot),
		slog.String("request_id", requestID),
		slog.String("backend_id", t.ID),
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

func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Base == nil {
		t.Base = http.DefaultTransport
	}

	requestContext := t.tracker.StartTracking(req, t.ID, t.Model, t.summarizer, t.ClientTagHeader)

	var origBodyBytes []byte
	if req.Body != nil {
		var readErr error
		origBodyBytes, readErr = io.ReadAll(req.Body)
		if readErr != nil {
			t.logger.Error("failed to read request body for logging",
				slog.String("backend_id", t.ID),
				slog.String("request_id", requestContext.RequestInfo.ID),
				slog.String("method", req.Method),
				slog.String("path", req.URL.Path),
				slog.Any("error", readErr),
			)
		}
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(origBodyBytes))
	}

	// Only pay for the string conversion when a summarizer will actually reshape
	// the body; otherwise cut the raw bytes.
	bodyPreview := truncateBody(origBodyBytes, maxBodyLogBytes)
	if t.summarizer.active() {
		bodyPreview = truncateString(t.summarizer.RequestBody(string(origBodyBytes)), maxBodyLogBytes)
	}

	t.logger.Info("request started",
		slog.String("request_id", requestContext.RequestInfo.ID),
		slog.String("model", t.Model),
		slog.String("method", req.Method),
		slog.String("url", req.URL.String()),
		slog.String("host", req.Host),
		slog.String("path", req.URL.Path),
		slog.String("scheme", req.URL.Scheme),
		slog.String("backend_id", t.ID),
		// request_headers is logged here (Authorization excluded at capture time)
		// as well as on "request tracking finished", so the headers are still
		// captured for requests that fail or hang before tracking finishes.
		slog.Any("request_headers", requestContext.RequestInfo.Headers),
		slog.Int("body_size", len(origBodyBytes)),
		slog.String("content_type", req.Header.Get("Content-Type")),
		slog.String("body_preview", bodyPreview),
	)

	// A request whose mapping failed never goes upstream: the Director left the
	// error in the context and the synthetic answer is produced here, inside
	// tracking, so the failure still gets a "request tracking finished" line, a
	// client_tag, and a place on the dashboard — the per-client failure alerts
	// key on exactly those.
	if mapErr := mapErrFromContext(req.Context()); mapErr != nil {
		resp := syntheticMapError(req, mapErr)
		requestContext.RecordAttempt(time.Now(), resp, nil)
		requestContext.FinishTracking(resp, nil)
		t.logger.Info("request mapping failed, returning synthetic response",
			slog.String("request_id", requestContext.RequestInfo.ID),
			slog.String("backend_id", t.ID),
			slog.Int("status_code", resp.StatusCode),
			slog.String("error", mapErr.Error()),
		)
		return resp, nil
	}

	roundTripStart := time.Now()
	var finalResp *http.Response
	var finalErr error

	for attempt := 0; attempt <= t.MaxRetries; attempt++ {
		attemptStartTime := time.Now()

		resp, err := t.Base.RoundTrip(req)

		attemptDuration := time.Since(attemptStartTime)
		requestContext.RecordAttempt(attemptStartTime, resp, err)

		attemptAttrs := []slog.Attr{
			slog.String("request_id", requestContext.RequestInfo.ID),
			slog.Int("attempt", attempt+1),
			slog.Duration("duration", attemptDuration),
		}
		if resp != nil {
			attemptAttrs = append(attemptAttrs, slog.Int("status_code", resp.StatusCode))
			attemptAttrs = append(attemptAttrs, slog.Int64("content_length", resp.ContentLength))
		}
		if err != nil {
			attemptAttrs = append(attemptAttrs, slog.String("error", err.Error()))
		}
		t.logger.LogAttrs(context.Background(), slog.LevelInfo, "attempt completed", attemptAttrs...)

		if resp == nil {
			nilRespAttrs := []slog.Attr{
				logKeyAttr(logKeyUpstreamResponse),
				slog.String("request_id", requestContext.RequestInfo.ID),
				slog.String("backend_id", t.ID),
				slog.String("method", req.Method),
				slog.String("path", req.URL.Path),
				slog.Int("attempt", attempt+1),
			}
			if err != nil {
				nilRespAttrs = append(nilRespAttrs, slog.String("error", err.Error()))
			}
			t.logger.LogAttrs(context.Background(), slog.LevelError, "upstream request returned nil response", nilRespAttrs...)
		}

		// Emit explicit status telemetry for every upstream response.
		if resp != nil {
			statusAttrs := []slog.Attr{
				logKeyAttr(logKeyUpstreamResponse),
				slog.String("request_id", requestContext.RequestInfo.ID),
				slog.String("backend_id", t.ID),
				slog.String("method", req.Method),
				slog.String("path", req.URL.Path),
				slog.Int("attempt", attempt+1),
				slog.Int("status_code", resp.StatusCode),
			}
			if resp.ContentLength >= 0 {
				statusAttrs = append(statusAttrs, slog.Int64("content_length", resp.ContentLength))
			}

			statusLevel := slog.LevelInfo
			// Preserve operational error semantics for critical statuses.
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				statusLevel = slog.LevelError
			} else if resp.StatusCode >= 400 {
				statusLevel = slog.LevelWarn
			}

			t.logger.LogAttrs(context.Background(), statusLevel, "upstream request returned status", statusAttrs...)
		}

		if err == nil || (resp != nil && resp.StatusCode == http.StatusTooManyRequests) {
			if resp != nil {
				t.SaveUsage(resp, requestContext.RequestInfo.ID)
			}
		}

		if origBodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(origBodyBytes))
		}

		// Success - return immediately
		if err == nil && resp != nil && resp.StatusCode < 400 {
			finalResp = resp
			break
		}

		// If this is the last attempt, return the error
		if attempt == t.MaxRetries {
			finalResp = resp
			finalErr = err
			break
		}

		// Non-retryable client errors - return immediately
		if resp != nil && resp.StatusCode >= 400 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			finalResp = resp
			break
		}

		// Check if we hit a retryable error (rate limit or server error)
		if resp != nil && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) {
			// Get usage information to determine retry time
			usage, usageErr := t.getusage.Execute(context.Background(), t.ID)
			if usageErr != nil {
				t.logger.Error("failed to get usage for retry decision",
					slog.String("request_id", requestContext.RequestInfo.ID),
					slog.String("backend_id", t.ID),
					slog.Int("attempt", attempt+1),
					slog.Int("status_code", resp.StatusCode),
					slog.Any("error", usageErr),
				)
				finalResp = resp
				finalErr = err
				break
			}

			// Calculate retry time
			var retryAfter time.Time
			if !usage.ResetAt.IsZero() {
				retryAfter = usage.ResetAt.Add(time.Second) // Add 1 second buffer
			} else {
				retryAfter = time.Now().Add(1 * time.Second)
			}

			t.logger.Info("error found, enqueuing for retry",
				logKeyAttr(logKeyRetryQueue),
				slog.String("request_id", requestContext.RequestInfo.ID),
				slog.String("backend_id", t.ID),
				slog.String("method", req.Method),
				slog.String("path", req.URL.Path),
				slog.Int("attempt", attempt),
				slog.Int("retry_count", attempt+1),
				slog.Time("retry_after", retryAfter),
				slog.Duration("retry_after_in", time.Until(retryAfter)),
				slog.Int("status_code", resp.StatusCode),
				slog.Int64("remaining_requests", usage.RemainingRequests),
				slog.Int64("remaining_tokens", usage.RemainingTokens),
			)

			responseChan := t.queue.EnqueueRequest(req, attempt+1, retryAfter, requestContext.RequestInfo.ID)

			qr := <-responseChan
			if qr != nil && qr.Error != nil {
				t.logger.Error("queued retry failed",
					logKeyAttr(logKeyRetryQueue),
					slog.String("request_id", requestContext.RequestInfo.ID),
					slog.String("backend_id", t.ID),
					slog.String("method", req.Method),
					slog.String("path", req.URL.Path),
					slog.Int("attempt", attempt+1),
					slog.Any("error", qr.Error),
				)
				finalResp = resp
				finalErr = fmt.Errorf("queued retry: %w", qr.Error)
				break
			}
		}
	}
	requestContext.FinishTracking(finalResp, finalErr)

	summaryAttrs := []slog.Attr{
		slog.String("request_id", requestContext.RequestInfo.ID),
		slog.String("backend_id", t.ID),
		slog.Duration("total_duration", time.Since(roundTripStart)),
		slog.Int("retry_count", requestContext.RequestInfo.RetryCount),
	}
	if finalResp != nil {
		summaryAttrs = append(summaryAttrs, slog.Int("final_status_code", finalResp.StatusCode))
	}
	if finalErr != nil {
		summaryAttrs = append(summaryAttrs, slog.String("final_error", finalErr.Error()))
	}
	t.logger.LogAttrs(context.Background(), slog.LevelInfo, "request completed", summaryAttrs...)

	return finalResp, finalErr
}

// saturationPct returns the percentage of quota consumed ((limit-remaining)/limit*100).
// Returns 0 when limit is unknown (zero) to avoid division by zero.
func saturationPct(limit, remaining int64) float64 {
	if limit <= 0 {
		return 0
	}
	used := limit - remaining
	if used < 0 {
		used = 0
	}
	return float64(used) / float64(limit) * 100
}
