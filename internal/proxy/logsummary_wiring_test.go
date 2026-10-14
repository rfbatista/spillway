package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"

	"github.com/rfbatista/spillway/internal/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestBackendConfig_ParsesLogBodyFieldsJSON(t *testing.T) {
	raw := `{
		"model": "claude-opus",
		"mapper": "bedrock",
		"log_drop_request_fields": ["system"],
		"log_crop_request_fields": ["messages.content.text"],
		"log_drop_response_fields": ["trace"],
		"log_crop_response_fields": ["output.message.content.text"],
		"log_crop_limit": 200
	}`

	var cfg BackendConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &cfg))

	assert.Equal(t, []string{"system"}, cfg.LogDropRequestFields)
	assert.Equal(t, []string{"messages.content.text"}, cfg.LogCropRequestFields)
	assert.Equal(t, []string{"trace"}, cfg.LogDropResponseFields)
	assert.Equal(t, []string{"output.message.content.text"}, cfg.LogCropResponseFields)
	assert.Equal(t, 200, cfg.LogCropLimit)
}

func TestBackendConfig_ParsesLogBodyFieldsYAML(t *testing.T) {
	raw := `
- model: claude-opus
  mapper: bedrock
  log_drop_request_fields:
    - system
  log_crop_response_fields:
    - output.message.content.text
  log_crop_limit: 200
`

	var cfgs []BackendConfig
	require.NoError(t, yaml.Unmarshal([]byte(raw), &cfgs))

	require.Len(t, cfgs, 1)
	assert.Equal(t, []string{"system"}, cfgs[0].LogDropRequestFields)
	assert.Equal(t, []string{"output.message.content.text"}, cfgs[0].LogCropResponseFields)
	assert.Equal(t, 200, cfgs[0].LogCropLimit)
}

func TestBackendConfig_OmitsUnsetLogBodyFields(t *testing.T) {
	// The watcher hashes the marshalled configs to decide whether to rebuild the
	// pool, so a config that does not use the feature must marshal unchanged.
	out, err := json.Marshal(BackendConfig{Host: "api.openai.com", Model: "gpt-4o"})
	require.NoError(t, err)

	assert.NotContains(t, string(out), "log_", "unset log fields must be omitted so config hashes stay stable")
}

func TestRoundTrip_RequestStartedPreviewIsSummarized(t *testing.T) {
	summarizer := newLogBodySummarizer(BackendConfig{
		LogDropRequestFields: []string{"system"},
		LogCropRequestFields: []string{"messages.content.text"},
		LogCropLimit:         20,
	})
	require.NotNil(t, summarizer)

	saveUsage := mocks.NewMockSaveUsageService(t)
	saveUsage.On("Execute", mock.Anything, mock.Anything).Return(nil).Maybe()
	getUsage := mocks.NewMockGetUsageService(t)
	publisher := mocks.NewMockRequestPublisher(t)
	publisher.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Maybe()

	rt, err := NewRetryTransport("test", "claude-opus", saveUsage, getUsage, NewQueueManager(),
		NewRequestTracker(publisher), WithLogBodySummarizer(summarizer))
	require.NoError(t, err)
	require.Same(t, summarizer, rt.summarizer, "the option must install the summarizer")

	logger, buf := captureLogger()
	rt.logger = logger
	rt.Base = &fakeRoundTripper{responses: []roundTripResult{{resp: dummyResponse(200, "ok")}}}

	req := httptest.NewRequest("POST", "http://example.com/v1/chat", strings.NewReader(converseRequestBody))
	req.Header.Set("Content-Type", "application/json")
	_, err = rt.RoundTrip(req)
	require.NoError(t, err)

	rec := logRecordsByMsg(t, buf)["request started"]
	require.NotNil(t, rec, "the request started line must be emitted")

	preview := rec["body_preview"].(string)
	assert.NotContains(t, preview, `"system"`, "the dropped field must not reach the preview")
	assert.Contains(t, preview, cropMarker, "the long prompt must be cropped in the preview")
	assert.Contains(t, preview, "inferenceConfig", "the small fields must fit in the preview")
	assert.EqualValues(t, len(converseRequestBody), rec["body_size"], "body_size must report the raw body length")
}

func TestRoundTrip_WithoutSummarizerPreviewIsRawBody(t *testing.T) {
	saveUsage := mocks.NewMockSaveUsageService(t)
	saveUsage.On("Execute", mock.Anything, mock.Anything).Return(nil).Maybe()
	getUsage := mocks.NewMockGetUsageService(t)
	publisher := mocks.NewMockRequestPublisher(t)
	publisher.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Maybe()

	rt, err := NewRetryTransport("test", "claude-opus", saveUsage, getUsage, NewQueueManager(),
		NewRequestTracker(publisher))
	require.NoError(t, err)
	require.Nil(t, rt.summarizer, "no option means no summarizer")

	logger, buf := captureLogger()
	rt.logger = logger
	rt.Base = &fakeRoundTripper{responses: []roundTripResult{{resp: dummyResponse(200, "ok")}}}

	req := httptest.NewRequest("POST", "http://example.com/v1/chat", strings.NewReader(converseRequestBody))
	req.Header.Set("Content-Type", "application/json")
	_, err = rt.RoundTrip(req)
	require.NoError(t, err)

	rec := logRecordsByMsg(t, buf)["request started"]
	require.NotNil(t, rec)
	assert.Equal(t, converseRequestBody, rec["body_preview"], "without a summarizer the preview must stay the raw body")
}

// retryTransportOf digs the RetryTransport out of a pooled backend, so a test
// can assert on how NewBackendPool wired it.
func retryTransportOf(t *testing.T, b *Backend) *RetryTransport {
	t.Helper()
	rp, ok := b.proxy.(*httputil.ReverseProxy)
	require.True(t, ok, "a pooled backend must hold a reverse proxy")
	mapErr, ok := rp.Transport.(*mapErrorTransport)
	require.True(t, ok, "the reverse proxy must wrap the transport for map errors")
	rt, ok := mapErr.next.(*RetryTransport)
	require.True(t, ok, "the mapping transport must wrap the retry transport")
	return rt
}

func TestNewBackendPool_WiresLogSummarizer(t *testing.T) {
	saveUsage := mocks.NewMockSaveUsageService(t)
	getUsage := mocks.NewMockGetUsageService(t)
	tracker := NewRequestTracker(mocks.NewMockRequestPublisher(t))

	pool, err := NewBackendPool(saveUsage, getUsage, []BackendConfig{
		{Host: "api.openai.com", Model: "gpt-4o"},
		{
			Host:                 "bedrock-runtime.us-east-1.amazonaws.com",
			Model:                "claude-opus",
			LogCropRequestFields: []string{"messages.content.text"},
			LogCropLimit:         200,
		},
	}, tracker, NewQueueManager())
	require.NoError(t, err)
	require.Len(t, pool.backend, 2)

	assert.Nil(t, retryTransportOf(t, pool.backend[0]).summarizer,
		"a backend without log field config must get no summarizer")

	configured := retryTransportOf(t, pool.backend[1]).summarizer
	require.NotNil(t, configured, "a backend with log field config must get a summarizer")
	assert.Equal(t, 200, configured.cropLimit, "the configured crop limit must be threaded through")
}
