package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"proxy/internal/domain"
	"proxy/internal/mocks"

	"github.com/stretchr/testify/mock"
)

func gzipBytes(t *testing.T, raw string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(raw)); err != nil {
		t.Fatalf("failed to write gzip payload: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return b.Bytes()
}

func TestDecompressBody_GzipAndPlain(t *testing.T) {
	raw := `{"ok":true}`
	zipped := gzipBytes(t, raw)
	if got := decompressBody(zipped, "application/json"); got != raw {
		t.Fatalf("expected decompressed payload %q, got %q", raw, got)
	}

	plain := "not-gzip"
	if got := decompressBody([]byte(plain), "text/plain"); got != plain {
		t.Fatalf("expected plain payload %q, got %q", plain, got)
	}
}

func TestFormatJSON_ValidAndInvalid(t *testing.T) {
	valid := `{"a":1,"b":{"c":2}}`
	formatted := formatJSON(valid)
	if formatted == valid {
		t.Fatal("expected valid JSON to be pretty-printed")
	}
	if formatted == "" || formatted[0] != '{' {
		t.Fatalf("unexpected formatted JSON: %q", formatted)
	}

	invalid := "{this is not json"
	if got := formatJSON(invalid); got != invalid {
		t.Fatalf("expected invalid JSON to remain unchanged, got %q", got)
	}
}

func TestStartTracking_CapturesRequestAndRestoresBody(t *testing.T) {
	repo := mocks.NewMockRequestPublisher(t)
	tracker := NewRequestTracker(repo)

	req := httptest.NewRequest("POST", "http://example.com/v1/chat", bytes.NewBufferString(`{"x":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "test-agent")
	req.Header.Set("Authorization", "secret")
	req.Header.Set("X-Forwarded-For", "203.0.113.8")
	req.Header.Set("X-Custom", "value")

	rc := tracker.StartTracking(req, "backend-1", "gpt-test", nil, "")

	if rc.RequestInfo.ID == "" {
		t.Fatal("expected generated request ID")
	}
	if rc.RequestInfo.BackendID != "backend-1" {
		t.Fatalf("expected backend backend-1, got %s", rc.RequestInfo.BackendID)
	}
	if rc.RequestInfo.Model != "gpt-test" {
		t.Fatalf("expected model gpt-test, got %s", rc.RequestInfo.Model)
	}
	if rc.RequestInfo.ClientIP != "203.0.113.8" {
		t.Fatalf("expected forwarded client IP, got %s", rc.RequestInfo.ClientIP)
	}
	if _, ok := rc.RequestInfo.Headers["Authorization"]; ok {
		t.Fatal("authorization header should not be stored")
	}
	if rc.RequestInfo.Headers["X-Custom"] != "value" {
		t.Fatalf("expected custom header to be captured, got %q", rc.RequestInfo.Headers["X-Custom"])
	}
	if rc.RequestInfo.RequestBody == `{"x":1}` {
		t.Fatalf("expected JSON request body to be formatted, got %q", rc.RequestInfo.RequestBody)
	}

	bodyAfter, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read restored request body: %v", err)
	}
	if string(bodyAfter) != `{"x":1}` {
		t.Fatalf("expected original body restored, got %q", string(bodyAfter))
	}
}

func TestGetClientIP_PriorityFallbacks(t *testing.T) {
	tracker := NewRequestTracker(mocks.NewMockRequestPublisher(t))

	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	req.Header.Set("X-Real-IP", "198.51.100.2")
	req.RemoteAddr = "198.51.100.3:1234"
	if got := tracker.getClientIP(req); got != "198.51.100.1" {
		t.Fatalf("expected X-Forwarded-For first, got %s", got)
	}

	req = httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("X-Real-IP", "198.51.100.2")
	req.RemoteAddr = "198.51.100.3:1234"
	if got := tracker.getClientIP(req); got != "198.51.100.2" {
		t.Fatalf("expected X-Real-IP fallback, got %s", got)
	}

	req = httptest.NewRequest("GET", "http://example.com/", nil)
	req.RemoteAddr = "198.51.100.3:1234"
	if got := tracker.getClientIP(req); got != "198.51.100.3" {
		t.Fatalf("expected parsed host from RemoteAddr, got %s", got)
	}

	req = httptest.NewRequest("GET", "http://example.com/", nil)
	req.RemoteAddr = ""
	if got := tracker.getClientIP(req); got != "unknown" {
		t.Fatalf("expected unknown when no IP source exists, got %s", got)
	}
}

func TestRecordAttempt_CapturesResponseAndRestoresBody(t *testing.T) {
	repo := mocks.NewMockRequestPublisher(t)
	tracker := NewRequestTracker(repo)
	req := httptest.NewRequest("GET", "http://example.com/test", nil)
	rc := tracker.StartTracking(req, "backend-1", "gpt-test", nil, "")

	respBody := gzipBytes(t, `{"answer":"ok"}`)
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}
	start := time.Now().Add(-20 * time.Millisecond)
	rc.RecordAttempt(start, resp, errors.New("transport fail"))

	if len(rc.attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(rc.attempts))
	}
	at := rc.attempts[0]
	if at.AttemptNumber != 1 {
		t.Fatalf("expected attempt number 1, got %d", at.AttemptNumber)
	}
	if at.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected status %d, got %d", http.StatusTooManyRequests, at.StatusCode)
	}
	if at.Error != "transport fail" {
		t.Fatalf("expected attempt error to be captured, got %q", at.Error)
	}
	if at.ResponseBody == `{"answer":"ok"}` {
		t.Fatalf("expected formatted JSON response body, got %q", at.ResponseBody)
	}

	restored, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed reading restored response body: %v", err)
	}
	if !bytes.Equal(restored, respBody) {
		t.Fatalf("expected original compressed response body restored, got %q", string(restored))
	}
}

func TestFinishTracking_PublishesWithAttemptsAndIsIdempotent(t *testing.T) {
	pub := mocks.NewMockRequestPublisher(t)
	tracker := NewRequestTracker(pub)
	req := httptest.NewRequest("GET", "http://example.com/final", nil)
	rc := tracker.StartTracking(req, "backend-x", "gpt-test", nil, "")
	rc.RecordAttempt(time.Now().Add(-10*time.Millisecond), nil, nil)
	rc.RecordAttempt(time.Now().Add(-5*time.Millisecond), nil, errors.New("retry err"))

	finalResp := &http.Response{
		StatusCode: http.StatusCreated,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Trace-Id":   []string{"trace-123"},
		},
		Body: io.NopCloser(bytes.NewBufferString(`{"result":"created"}`)),
	}

	pub.On("PublishRequest", mock.Anything, mock.MatchedBy(func(info domain.DetailedRequestInfo) bool {
		return info.BackendID == "backend-x" &&
			info.StatusCode == http.StatusCreated &&
			info.RetryCount == 1 &&
			info.ResponseHeaders["X-Trace-Id"] == "trace-123" &&
			info.Error == "final err" &&
			len(info.Attempts) == 2 && info.Attempts[1].Error == "retry err"
	})).Return(nil).Once()

	if err := rc.FinishTracking(finalResp, errors.New("final err")); err != nil {
		t.Fatalf("expected finish success, got %v", err)
	}
	// second call should be a no-op and must not publish twice.
	if err := rc.FinishTracking(finalResp, nil); err != nil {
		t.Fatalf("expected second finish to be no-op, got %v", err)
	}
}

func TestFinishTracking_PublisherErrorIsWrapped(t *testing.T) {
	pub := mocks.NewMockRequestPublisher(t)
	tracker := NewRequestTracker(pub)
	req := httptest.NewRequest("GET", "http://example.com/fail", nil)
	rc := tracker.StartTracking(req, "backend-fail", "gpt-test", nil, "")

	pub.On("PublishRequest", mock.Anything, mock.Anything).Return(errors.New("stream down")).Once()

	err := rc.FinishTracking(nil, nil)
	if err == nil {
		t.Fatal("expected finish to return wrapped publisher error")
	}
	if got := err.Error(); got == "stream down" || !bytes.Contains([]byte(got), []byte("failed to publish request info")) {
		t.Fatalf("expected wrapped error message, got %q", got)
	}
}

func TestFinishTracking_ClientTag(t *testing.T) {
	tests := []struct {
		name            string
		clientTagHeader string
		headerValue     string
		expectedTag     any // always a string: the tag, or untaggedClientTag
	}{
		{
			name:            "configured header present",
			clientTagHeader: "X-Client-Id",
			headerValue:     "acme",
			expectedTag:     "acme",
		},
		{
			name:            "configured header absent",
			clientTagHeader: "X-Client-Id",
			headerValue:     "",
			expectedTag:     untaggedClientTag,
		},
		{
			name:            "feature off",
			clientTagHeader: "",
			headerValue:     "acme",
			expectedTag:     untaggedClientTag,
		},
		{
			name:            "configured header blank",
			clientTagHeader: "X-Client-Id",
			headerValue:     "   ",
			expectedTag:     untaggedClientTag,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publisher := mocks.NewMockRequestPublisher(t)
			var published domain.DetailedRequestInfo
			publisher.On("PublishRequest", mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) {
					published = args.Get(1).(domain.DetailedRequestInfo)
				}).Return(nil)
			tracker := NewRequestTracker(publisher)
			logger, buf := captureLogger()
			tracker.logger = logger

			req := httptest.NewRequest("POST", "http://example.com/v1/chat", nil)
			if tt.headerValue != "" {
				req.Header.Set("X-Client-Id", tt.headerValue)
			}

			rc := tracker.StartTracking(req, "backend-1", "gpt-test", nil, tt.clientTagHeader)
			rc.RecordAttempt(time.Now(), &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(bytes.NewBufferString("upstream boom")),
				Header:     http.Header{},
			}, nil)
			if err := rc.FinishTracking(&http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(bytes.NewBufferString("upstream boom")),
				Header:     http.Header{},
			}, nil); err != nil {
				t.Fatalf("FinishTracking failed: %v", err)
			}

			lines := parseLogLines(t, buf.String())

			finished := findLogLine(lines, "request tracking finished")
			if finished == nil {
				t.Fatal("expected a request tracking finished line")
			}
			got, ok := finished["client_tag"]
			if !ok {
				t.Fatal("finished line must always carry client_tag")
			}
			if got != tt.expectedTag {
				t.Fatalf("finished line client_tag = %#v, want %#v", got, tt.expectedTag)
			}

			// The start line carries the same tag, so an unfinished request is
			// still attributable to its caller.
			started := findLogLine(lines, "request tracking started")
			if started == nil {
				t.Fatal("expected a request tracking started line")
			}
			startTag, ok := started["client_tag"]
			if !ok {
				t.Fatal("start line must always carry client_tag")
			}
			if startTag != tt.expectedTag {
				t.Fatalf("start line client_tag = %#v, want %#v", startTag, tt.expectedTag)
			}

			// client_tag must never leak onto per-attempt lines: they share
			// log_key=request_trace and status_code, so a tag there would make
			// the per-caller failure alert double-count each attempt.
			attempt := findLogLine(lines, "attempt response body captured")
			if attempt == nil {
				t.Fatal("expected an attempt response body captured line")
			}
			if _, ok := attempt["client_tag"]; ok {
				t.Fatal("attempt line must not carry client_tag")
			}

			// The payload sent to the dashboard over SSE must carry the same
			// value — an untagged request serializes the placeholder, never an
			// empty string and never a missing key.
			payload, err := json.Marshal(published.RequestInfo)
			if err != nil {
				t.Fatalf("failed to marshal published request info: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatalf("failed to decode published request info: %v", err)
			}
			publishedTag, ok := decoded["client_tag"]
			if !ok {
				t.Fatal("published payload must always define client_tag")
			}
			if publishedTag != tt.expectedTag {
				t.Fatalf("published client_tag = %#v, want %#v", publishedTag, tt.expectedTag)
			}
		})
	}
}

func TestStartTracking_LogsStartLine(t *testing.T) {
	repo := mocks.NewMockRequestPublisher(t)
	tracker := NewRequestTracker(repo)
	logger, buf := captureLogger()
	tracker.logger = logger

	req := httptest.NewRequest("POST", "http://example.com/v1/chat", bytes.NewBufferString(`{"x":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "test-agent")
	req.Header.Set("X-Forwarded-For", "203.0.113.8")
	req.Header.Set("X-Client-Tag", "squad-a")

	rc := tracker.StartTracking(req, "backend-1", "gpt-test", nil, "X-Client-Tag")

	rec := logRecordsByMsg(t, buf)["request tracking started"]
	if rec == nil {
		t.Fatal("expected a start line to be emitted")
	}
	if rec["log_key"] != logKeyRequestTrace {
		t.Fatalf("expected log_key %q, got %v", logKeyRequestTrace, rec["log_key"])
	}
	if rec["request_id"] != rc.RequestInfo.ID {
		t.Fatalf("expected request_id %q, got %v", rc.RequestInfo.ID, rec["request_id"])
	}
	if rec["backend_id"] != "backend-1" || rec["model"] != "gpt-test" {
		t.Fatalf("unexpected backend/model fields: %v", rec)
	}
	if rec["method"] != "POST" || rec["path"] != "/v1/chat" {
		t.Fatalf("unexpected method/path fields: %v", rec)
	}
	if rec["client_ip"] != "203.0.113.8" || rec["user_agent"] != "test-agent" {
		t.Fatalf("unexpected client fields: %v", rec)
	}
	if got := rec["request_body_size"].(float64); int(got) != len(rc.RequestInfo.RequestBody) {
		t.Fatalf("expected request_body_size %d, got %v", len(rc.RequestInfo.RequestBody), got)
	}
	if rec["client_tag"] != "squad-a" {
		t.Fatalf("expected client_tag squad-a, got %#v", rec["client_tag"])
	}
	// The bodies stay exclusive to the finish line.
	if _, ok := rec["request_body"]; ok {
		t.Fatal("the start line must not carry the request body")
	}
}
