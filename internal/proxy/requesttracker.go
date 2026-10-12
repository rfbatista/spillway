package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"proxy/internal/domain"
	"proxy/internal/ports"
)

// maxBodyLogBytes caps the per-attempt response body preview.
const maxBodyLogBytes = 4096

// maxLoggedBodyBytes caps the response body in the final request trace, so a
// single large payload (e.g. a big LLM response) can't bloat a log line. Token
// usage is extracted from the full body before truncation, and the untruncated
// length is still reported via response_body_size.
const maxLoggedBodyBytes = 4 * 1024

// maxLoggedRequestBodyBytes caps the request body in the final request trace.
// It is kept smaller than the response cap because the request body only needs
// to identify the call (model, path, start of the prompt), not carry the full
// payload. The untruncated length is still reported via request_body_size.
const maxLoggedRequestBodyBytes = 2 * 1024

// untaggedClientTag is the value written to the client_tag field when there is
// no tag to report: the backend configures no client_tag_header, or the client
// sent that header empty or not at all. A literal placeholder beats an empty
// string here — it reads unambiguously on a log line and gives Loki a real
// bucket to group by. Alert queries that count *per caller* must exclude it (see
// scripts/build_ndocs_alerts.py), otherwise all untagged traffic collapses into
// a single synthetic caller.
const untaggedClientTag = "untagged"

// truncationMarker is appended to a body cut short by a byte ceiling.
const truncationMarker = "... (truncated)"

// truncateString returns body capped to limit bytes, appending a marker when
// the body was truncated.
func truncateString(body string, limit int) string {
	if len(body) <= limit {
		return body
	}
	return body[:limit] + truncationMarker
}

type completionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	Details          struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	PromptDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func extractUsage(body string) (completionUsage, bool) {
	var envelope struct {
		Usage completionUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return completionUsage{}, false
	}
	// A zero-value usage (all fields zero) means the field was absent.
	u := envelope.Usage
	if u.TotalTokens == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return completionUsage{}, false
	}
	return u, true
}

// RequestTracker gerencia o rastreamento de requests
type RequestTracker struct {
	publisher ports.RequestPublisher
	logger    *slog.Logger
}

// decompressBody decompresses gzipped response body if needed
func decompressBody(bodyBytes []byte, contentType string) string {
	// Check if content is gzipped
	if len(bodyBytes) >= 2 && bodyBytes[0] == 0x1f && bodyBytes[1] == 0x8b {
		reader, err := gzip.NewReader(bytes.NewReader(bodyBytes))
		if err != nil {
			return string(bodyBytes) // Return original if decompression fails
		}
		defer reader.Close()

		decompressed, err := io.ReadAll(reader)
		if err != nil {
			return string(bodyBytes) // Return original if decompression fails
		}
		return string(decompressed)
	}
	return string(bodyBytes)
}

// formatJSON formats JSON content for better readability
func formatJSON(content string) string {
	if content == "" {
		return content
	}

	// Try to parse and format as JSON
	var jsonData interface{}
	if err := json.Unmarshal([]byte(content), &jsonData); err != nil {
		return content // Return original if not valid JSON
	}

	// Format with indentation
	formatted, err := json.MarshalIndent(jsonData, "", "  ")
	if err != nil {
		return content // Return original if formatting fails
	}

	return string(formatted)
}

// NewRequestTracker cria uma nova instância do RequestTracker
func NewRequestTracker(publisher ports.RequestPublisher) *RequestTracker {
	return &RequestTracker{
		publisher: publisher,
		logger:    slog.With(slog.String("component", "RequestTracker")),
	}
}

// generateRequestID gera um ID único para a request
func (rt *RequestTracker) generateRequestID() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// StartTracking inicia o rastreamento de uma request. summarizer shapes the
// bodies written to the logs and may be nil, in which case they are only capped
// by the byte ceilings. clientTagHeader, when non-empty, names the request
// header whose value is captured as ClientTag.
func (rt *RequestTracker) StartTracking(req *http.Request, backendID, model string, summarizer *logBodySummarizer, clientTagHeader string) *RequestContext {
	requestID := strings.TrimSpace(req.Header.Get("X-Request-Id"))
	if requestID == "" {
		requestID = rt.generateRequestID()
		req.Header.Set("X-Request-Id", requestID)
	}

	// Captura informações básicas da request
	requestInfo := domain.RequestInfo{
		ID:        requestID,
		BackendID: backendID,
		Model:     model,
		Method:    req.Method,
		URL:       req.URL.String(),
		Path:      req.URL.Path,
		Headers:   make(map[string]string),
		StartTime: time.Now(),
		ClientIP:  rt.getClientIP(req),
		UserAgent: req.Header.Get("User-Agent"),
	}
	// Normalized once, at capture: every consumer — the two log lines and the
	// payload published to the dashboard — reads the same field, so none of them
	// has to re-derive what "no tag" looks like.
	requestInfo.ClientTag = untaggedClientTag
	if clientTagHeader != "" {
		if tag := strings.TrimSpace(req.Header.Get(clientTagHeader)); tag != "" {
			requestInfo.ClientTag = tag
		}
	}

	// Captura headers (excluindo Authorization por segurança)
	for name, values := range req.Header {
		if len(values) > 0 && name != "Authorization" {
			requestInfo.Headers[name] = values[0]
		}
	}

	// Captura body da request
	if req.Body != nil {
		bodyBytes, err := io.ReadAll(req.Body)
		if err == nil {
			bodyStr := string(bodyBytes)
			// Formata JSON se for conteúdo JSON
			if req.Header.Get("Content-Type") == "application/json" {
				requestInfo.RequestBody = formatJSON(bodyStr)
			} else {
				requestInfo.RequestBody = bodyStr
			}
			// Restaura o body para uso posterior
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
	}

	// Emitted before the first upstream attempt so a request that never reaches
	// "request tracking finished" (crash, hang, client disconnect) is still
	// visible, pivotable by request_id and attributable to a caller. It stays
	// lean on purpose: no bodies. client_tag is always present, carrying
	// untaggedClientTag when the backend configures no client_tag_header or the
	// client sent none — a caller with no tag is a fact worth reading off the
	// line, not an absence.
	// Because this line carries a client_tag but no status_code, per-caller
	// alert queries must keep counting only lines that report an outcome (see
	// the status_code guard in scripts/build_ndocs_alerts.py).
	rt.logger.Info("request tracking started",
		logKeyAttr(logKeyRequestTrace),
		slog.String("request_id", requestInfo.ID),
		slog.String("backend_id", requestInfo.BackendID),
		slog.String("client_tag", requestInfo.ClientTag),
		slog.String("model", requestInfo.Model),
		slog.String("method", requestInfo.Method),
		slog.String("path", requestInfo.Path),
		slog.String("client_ip", requestInfo.ClientIP),
		slog.String("user_agent", requestInfo.UserAgent),
		slog.Int("request_body_size", len(requestInfo.RequestBody)),
	)

	return &RequestContext{
		RequestInfo: requestInfo,
		tracker:     rt,
		attempts:    make([]domain.RequestAttempt, 0),
		summarizer:  summarizer,
	}
}

// RequestContext mantém o contexto de uma request sendo rastreada
type RequestContext struct {
	RequestInfo domain.RequestInfo
	tracker     *RequestTracker
	attempts    []domain.RequestAttempt
	finished    bool // Flag para prevenir múltiplas chamadas de FinishTracking
	// summarizer shapes bodies for the log lines only. RequestInfo keeps the
	// full bodies, which is what the dashboard receives over SSE.
	summarizer *logBodySummarizer
}

// RecordAttempt registra uma tentativa de request
func (rc *RequestContext) RecordAttempt(startTime time.Time, resp *http.Response, err error) {
	defer func() {
		if r := recover(); r != nil {
			rc.tracker.logger.Error("panic in RecordAttempt",
				slog.Any("panic", r),
				slog.String("request_id", rc.RequestInfo.ID),
				slog.String("backend_id", rc.RequestInfo.BackendID),
			)
		}
	}()
	endTime := time.Now()
	duration := endTime.Sub(startTime)

	attempt := domain.RequestAttempt{
		AttemptNumber: len(rc.attempts) + 1,
		StartTime:     startTime,
		EndTime:       endTime,
		Duration:      duration,
	}
	attempt.DurationFormatted = attempt.FormattedDuration()

	if resp != nil {
		attempt.StatusCode = resp.StatusCode

		// Captura response body
		if resp.Body != nil {
			bodyBytes, readErr := io.ReadAll(resp.Body)
			if readErr == nil {
				contentType := resp.Header.Get("Content-Type")
				decompressed := decompressBody(bodyBytes, contentType)
				// Formata JSON se for conteúdo JSON
				if contentType == "application/json" {
					attempt.ResponseBody = formatJSON(decompressed)
				} else {
					attempt.ResponseBody = decompressed
				}
				// Restaura o body para uso posterior
				resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
		}
	}

	if err != nil {
		attempt.Error = err.Error()
	}

	bodyPreview := truncateString(rc.summarizer.ResponseBody(attempt.ResponseBody), maxBodyLogBytes)
	rc.tracker.logger.Info("attempt response body captured",
		logKeyAttr(logKeyRequestTrace),
		slog.String("request_id", rc.RequestInfo.ID),
		slog.String("backend_id", rc.RequestInfo.BackendID),
		slog.String("method", rc.RequestInfo.Method),
		slog.String("path", rc.RequestInfo.Path),
		slog.Int("attempt", attempt.AttemptNumber),
		slog.Int("status_code", attempt.StatusCode),
		slog.Int("body_size", len(attempt.ResponseBody)),
		slog.Duration("attempt_duration", attempt.Duration),
		slog.String("body_preview", bodyPreview),
	)

	rc.attempts = append(rc.attempts, attempt)
}

// FinishTracking finaliza o rastreamento, registra a request no log e a
// publica para os clientes do dashboard ao vivo via SSE.
func (rc *RequestContext) FinishTracking(resp *http.Response, err error) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			rc.tracker.logger.Error("panic in FinishTracking",
				slog.Any("panic", r),
				slog.String("request_id", rc.RequestInfo.ID),
				slog.String("backend_id", rc.RequestInfo.BackendID),
			)
			retErr = fmt.Errorf("panic in FinishTracking: %v", r)
		}
	}()
	// Verifica se já foi finalizado para prevenir múltiplas chamadas
	if rc.finished {
		return nil
	}
	rc.finished = true

	rc.RequestInfo.EndTime = time.Now()
	rc.RequestInfo.Duration = rc.RequestInfo.EndTime.Sub(rc.RequestInfo.StartTime)
	rc.RequestInfo.DurationFormatted = rc.RequestInfo.FormattedDuration()
	rc.RequestInfo.RetryCount = len(rc.attempts) - 1 // -1 porque a primeira tentativa não é retry

	if resp != nil {
		rc.RequestInfo.StatusCode = resp.StatusCode

		// Captura response headers
		rc.RequestInfo.ResponseHeaders = make(map[string]string)
		for name, values := range resp.Header {
			if len(values) > 0 {
				rc.RequestInfo.ResponseHeaders[name] = values[0]
			}
		}

		// Captura response body final
		if resp.Body != nil {
			bodyBytes, readErr := io.ReadAll(resp.Body)
			if readErr == nil {
				contentType := resp.Header.Get("Content-Type")
				decompressed := decompressBody(bodyBytes, contentType)
				// Formata JSON se for conteúdo JSON
				if contentType == "application/json" {
					rc.RequestInfo.ResponseBody = formatJSON(decompressed)
				} else {
					rc.RequestInfo.ResponseBody = decompressed
				}
				// Restaura o body para uso posterior
				resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
		}
	}

	if err != nil {
		rc.RequestInfo.Error = err.Error()
	}

	logAttrs := []any{
		logKeyAttr(logKeyRequestTrace),
		slog.String("request_id", rc.RequestInfo.ID),
		slog.String("backend_id", rc.RequestInfo.BackendID),
		slog.String("model", rc.RequestInfo.Model),
		slog.String("method", rc.RequestInfo.Method),
		slog.String("path", rc.RequestInfo.Path),
		slog.String("client_ip", rc.RequestInfo.ClientIP),
		slog.String("user_agent", rc.RequestInfo.UserAgent),
		slog.Int("status_code", rc.RequestInfo.StatusCode),
		slog.Int("retry_count", rc.RequestInfo.RetryCount),
		slog.String("duration", rc.RequestInfo.DurationFormatted),
		slog.Duration("duration_raw", rc.RequestInfo.Duration),
		// Full request payload (Authorization header is excluded at capture time).
		// The *_body_size fields measure the captured body, so they keep
		// reporting the true length even when the logged body is summarized.
		slog.Any("request_headers", rc.RequestInfo.Headers),
		slog.Int("request_body_size", len(rc.RequestInfo.RequestBody)),
		slog.String("request_body", truncateString(rc.summarizer.RequestBody(rc.RequestInfo.RequestBody), maxLoggedRequestBodyBytes)),
		// Full response payload.
		slog.Any("response_headers", rc.RequestInfo.ResponseHeaders),
		slog.Int("response_body_size", len(rc.RequestInfo.ResponseBody)),
		slog.String("response_body", truncateString(rc.summarizer.ResponseBody(rc.RequestInfo.ResponseBody), maxLoggedBodyBytes)),
	}
	if rc.RequestInfo.Error != "" {
		logAttrs = append(logAttrs, slog.String("error", rc.RequestInfo.Error))
	}
	// client_tag is always present — untaggedClientTag when the caller sent no
	// tag — so an operator reading a single line never has to guess whether the
	// field was dropped. It stays off the per-attempt lines, which share
	// log_key and status_code with this one: a tag there would make the
	// per-caller failure alert count every attempt instead of every request.
	logAttrs = append(logAttrs, slog.String("client_tag", rc.RequestInfo.ClientTag))
	if rc.RequestInfo.StatusCode == http.StatusOK {
		// Reads the full body on purpose: log_drop_response_fields must never be
		// able to hide token usage.
		if usage, ok := extractUsage(rc.RequestInfo.ResponseBody); ok {
			logAttrs = append(logAttrs,
				slog.Int("usage_prompt_tokens", usage.PromptTokens),
				slog.Int("usage_completion_tokens", usage.CompletionTokens),
				slog.Int("usage_total_tokens", usage.TotalTokens),
				slog.Int("usage_reasoning_tokens", usage.Details.ReasoningTokens),
				slog.Int("usage_cached_tokens", usage.PromptDetails.CachedTokens),
			)
		}
	}
	rc.tracker.logger.Info("request tracking finished", logAttrs...)

	// Publica a request (com tentativas) para os clientes do dashboard ao vivo.
	// Nada é mantido em memória: clientes só recebem requests concluídas após
	// se conectarem ao stream SSE.
	detailed := domain.DetailedRequestInfo{
		RequestInfo: rc.RequestInfo,
		Attempts:    rc.attempts,
	}
	if err := rc.tracker.publisher.PublishRequest(context.Background(), detailed); err != nil {
		rc.tracker.logger.Error("failed to publish tracked request",
			slog.String("request_id", rc.RequestInfo.ID),
			slog.String("backend_id", rc.RequestInfo.BackendID),
			slog.Any("error", err),
		)
		return fmt.Errorf("failed to publish request info: %w", err)
	}

	return nil
}

// getClientIP extrai o IP do cliente da request
func (rt *RequestTracker) getClientIP(req *http.Request) string {
	// Tenta obter IP de headers de proxy
	if ip := req.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	if ip := req.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}

	// Fallback para RemoteAddr
	if req.RemoteAddr != "" {
		// Remove porta se presente
		if colonIndex := bytes.IndexByte([]byte(req.RemoteAddr), ':'); colonIndex != -1 {
			return req.RemoteAddr[:colonIndex]
		}
		return req.RemoteAddr
	}

	return "unknown"
}
