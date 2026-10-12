package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"proxy/internal/domain"
	"proxy/internal/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// upstreamErrorTransport stands in for a provider that rejected the request.
type upstreamErrorTransport struct{}

func (upstreamErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	const body = `{"message":"model not enabled in this region"}`
	return &http.Response{
		StatusCode:    http.StatusBadRequest,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// serveProxy drives a request through the production transport stack: the
// mapper runs in the Director and upstream (the fake transport) sits behind a
// real RetryTransport, which is where mapping failures are answered.
// publisher captures what the tracker publishes for the dashboard.
func serveProxy(t *testing.T, mapper RequestMapper, upstream http.RoundTripper, body string) (*http.Response, *mocks.MockRequestPublisher) {
	t.Helper()
	target, err := url.Parse("http://upstream.invalid")
	require.NoError(t, err)

	saveUsage := mocks.NewMockSaveUsageService(t)
	saveUsage.On("Execute", mock.Anything, mock.Anything).Maybe().Return(nil)
	getUsage := mocks.NewMockGetUsageService(t)
	getUsage.On("Execute", mock.Anything, mock.Anything).Maybe().Return(&domain.ModelUsageQuota{}, nil)
	publisher := mocks.NewMockRequestPublisher(t)
	publisher.On("PublishRequest", mock.Anything, mock.Anything).Maybe().Return(nil)

	qm := NewQueueManager()
	t.Cleanup(qm.StopAll)
	retry, err := NewRetryTransport("test", "test-model", saveUsage, getUsage, qm, NewRequestTracker(publisher))
	require.NoError(t, err)
	retry.Base = upstream

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	NewProxy(*target, BackendConfig{}, mapper, retry).ServeHTTP(rec, req)
	return rec.Result(), publisher
}

// A request the mapper refuses must reach the caller as a 400 explaining why —
// not as a 200 with an empty completion, and not as an opaque 503.
func TestProxy_InvalidRequest_Returns400WithReason(t *testing.T) {
	mapper := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	resp, _ := serveProxy(t, mapper, upstreamErrorTransport{},
		`{"messages":[{"role":"user","content":"hi"}],"max_tokens":4096,"reasoning_effort":"high"}`)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var got map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	errObj := got["error"].(map[string]any)
	assert.Equal(t, "invalid_request_error", errObj["type"])
	assert.Contains(t, errObj["message"], "max_tokens")
}

// A synthetic mapping-failure response must still be tracked: it is published
// to the dashboard with its real status code, so the per-client failure alerts
// and the live view see the request at all.
func TestProxy_InvalidRequest_IsTracked(t *testing.T) {
	mapper := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	_, publisher := serveProxy(t, mapper, upstreamErrorTransport{},
		`{"messages":[{"role":"user","content":"hi"}],"max_tokens":4096,"reasoning_effort":"high"}`)

	published := false
	for _, call := range publisher.Calls {
		info, ok := call.Arguments.Get(1).(domain.DetailedRequestInfo)
		if ok && info.RequestInfo.StatusCode == http.StatusBadRequest {
			published = true
		}
	}
	assert.True(t, published, "expected the synthetic 400 to be published with its status code")
}

// An upstream rejection must not be run through the response mapper, which would
// turn the provider's explanation into an empty chat completion.
func TestProxy_UpstreamError_IsForwardedUnmapped(t *testing.T) {
	mapper := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	resp, _ := serveProxy(t, mapper, upstreamErrorTransport{},
		`{"messages":[{"role":"user","content":"hi"}],"max_tokens":4096}`)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "model not enabled in this region")
}
