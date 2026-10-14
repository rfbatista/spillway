package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rfbatista/spillway/internal/domain"
	"github.com/rfbatista/spillway/internal/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// captureLogger returns a JSON logger writing into the returned buffer so tests
// can assert on the structured fields of emitted log lines.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// logKeysByMsg parses captured JSON log lines into a msg -> log_key map. Lines
// without a log_key field are skipped.
func logKeysByMsg(t *testing.T, buf *bytes.Buffer) map[string]string {
	t.Helper()
	out := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		if lk, ok := rec["log_key"].(string); ok {
			msg, _ := rec["msg"].(string)
			out[msg] = lk
		}
	}
	return out
}

func TestLogKey_RateLimitSnapshot(t *testing.T) {
	saveUsage := mocks.NewMockSaveUsageService(t)
	getUsage := mocks.NewMockGetUsageService(t)
	saveUsage.On("Execute", mock.Anything, mock.Anything).Return(nil)

	requestPublisher := mocks.NewMockRequestPublisher(t)
	requestPublisher.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Maybe()
	tracker := NewRequestTracker(requestPublisher)

	rt, err := NewRetryTransport("test", "test-model", saveUsage, getUsage, NewQueueManager(), tracker)
	require.NoError(t, err)
	logger, buf := captureLogger()
	rt.logger = logger

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header: http.Header{
			"X-Ratelimit-Limit-Requests":     []string{"100"},
			"X-Ratelimit-Remaining-Requests": []string{"40"},
		},
	}
	rt.SaveUsage(resp, "req-1")

	keys := logKeysByMsg(t, buf)
	assert.Equal(t, logKeyRateLimitSnapshot, keys["rate_limit_snapshot"])
}

func TestLogKey_UpstreamResponse(t *testing.T) {
	fakeRT := &fakeRoundTripper{
		responses: []roundTripResult{
			{resp: dummyResponse(http.StatusNotFound, "not found"), err: nil},
		},
	}

	saveUsage := mocks.NewMockSaveUsageService(t)
	getUsage := mocks.NewMockGetUsageService(t)
	saveUsage.On("Execute", mock.Anything, mock.Anything).Return(nil).Maybe()

	requestPublisher := mocks.NewMockRequestPublisher(t)
	requestPublisher.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Maybe()
	tracker := NewRequestTracker(requestPublisher)

	rt, err := NewRetryTransport("test", "test-model", saveUsage, getUsage, NewQueueManager(), tracker)
	require.NoError(t, err)
	rt.Base = fakeRT
	logger, buf := captureLogger()
	rt.logger = logger

	_, err = rt.RoundTrip(httptest.NewRequest("GET", "http://example.com", nil))
	require.NoError(t, err)

	keys := logKeysByMsg(t, buf)
	assert.Equal(t, logKeyUpstreamResponse, keys["upstream request returned status"])
}

func TestLogKey_RetryQueue(t *testing.T) {
	// A 500 then success drives the retryable path that enqueues for retry.
	fakeRT := &fakeRoundTripper{
		responses: []roundTripResult{
			{resp: dummyResponse(http.StatusInternalServerError, "boom"), err: nil},
			{resp: dummyResponse(http.StatusOK, "ok"), err: nil},
		},
	}

	saveUsage := mocks.NewMockSaveUsageService(t)
	getUsage := mocks.NewMockGetUsageService(t)
	saveUsage.On("Execute", mock.Anything, mock.Anything).Return(nil).Maybe()
	getUsage.On("Execute", mock.Anything, mock.Anything).
		Return(&domain.ModelUsageQuota{}, nil)

	requestPublisher := mocks.NewMockRequestPublisher(t)
	requestPublisher.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Maybe()
	tracker := NewRequestTracker(requestPublisher)

	rt, err := NewRetryTransport("test", "test-model", saveUsage, getUsage, NewQueueManager(), tracker)
	require.NoError(t, err)
	rt.Base = fakeRT
	logger, buf := captureLogger()
	rt.logger = logger

	resp, err := rt.RoundTrip(httptest.NewRequest("POST", "http://example.com", bytes.NewBufferString("body")))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	keys := logKeysByMsg(t, buf)
	assert.Equal(t, logKeyRetryQueue, keys["error found, enqueuing for retry"])
}
