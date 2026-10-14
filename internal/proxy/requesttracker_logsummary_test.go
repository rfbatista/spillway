package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rfbatista/spillway/internal/domain"
	"github.com/rfbatista/spillway/internal/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// logRecordsByMsg parses captured JSON log lines into a msg -> record map, so a
// test can assert on any field of a specific line.
func logRecordsByMsg(t *testing.T, buf *bytes.Buffer) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "every log line must be valid JSON")
		msg, _ := rec["msg"].(string)
		out[msg] = rec
	}
	return out
}

// jsonResponse builds a response the tracker will capture and format.
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// converseResponseBody is a Converse answer with the long text the crop targets and
// the small fields that must survive it.
const converseResponseBody = `{
	"metrics": {"latencyMs": 18432},
	"output": {"message": {"role":"assistant","content":[{"text":"` + longPrompt + `"}]}},
	"stopReason": "end_turn",
	"usage": {"prompt_tokens": 9214, "completion_tokens": 1877, "total_tokens": 11091}
}`

func TestFinishTracking_LogsSummarizedBodyButPublishesFull(t *testing.T) {
	summarizer := newLogBodySummarizer(BackendConfig{
		LogDropRequestFields:  []string{"system"},
		LogCropRequestFields:  []string{"messages.content.text"},
		LogCropResponseFields: []string{"output.message.content.text"},
		LogCropLimit:          20,
	})
	require.NotNil(t, summarizer)

	pub := mocks.NewMockRequestPublisher(t)
	tracker := NewRequestTracker(pub)
	logger, buf := captureLogger()
	tracker.logger = logger

	req := httptest.NewRequest("POST", "http://example.com/v1/chat", strings.NewReader(converseRequestBody))
	req.Header.Set("Content-Type", "application/json")
	rc := tracker.StartTracking(req, "backend-1", "claude-opus", summarizer, "")

	// The published payload must still carry both bodies in full — that is what
	// the dashboard renders.
	pub.On("PublishRequest", mock.Anything, mock.MatchedBy(func(info domain.DetailedRequestInfo) bool {
		return strings.Contains(info.RequestBody, longPrompt) &&
			strings.Contains(info.RequestBody, `"system"`) &&
			strings.Contains(info.ResponseBody, longPrompt)
	})).Return(nil).Once()

	capturedRequestBody := rc.RequestInfo.RequestBody
	require.NoError(t, rc.FinishTracking(jsonResponse(http.StatusOK, converseResponseBody), nil))

	rec := logRecordsByMsg(t, buf)["request tracking finished"]
	require.NotNil(t, rec, "the final trace line must be emitted")

	loggedRequest := rec["request_body"].(string)
	assert.NotContains(t, loggedRequest, `"system"`, "the dropped field must not reach the log")
	assert.Contains(t, loggedRequest, cropMarker, "the long prompt must be cropped in the log")
	assert.Contains(t, loggedRequest, "inferenceConfig", "the small fields must now fit in the log line")
	assert.Contains(t, loggedRequest, "additionalModelRequestFields", "the small fields must now fit in the log line")

	loggedResponse := rec["response_body"].(string)
	assert.Contains(t, loggedResponse, cropMarker, "the long answer must be cropped in the log")
	assert.Contains(t, loggedResponse, "stopReason", "the tail fields must now fit in the log line")
	assert.Contains(t, loggedResponse, "11091", "usage must now fit in the log line")

	assert.Equal(t, capturedRequestBody, rc.RequestInfo.RequestBody, "summarizing must not mutate the captured body")
	assert.Contains(t, rc.RequestInfo.ResponseBody, longPrompt, "the captured response body must stay complete")
}

func TestFinishTracking_BodySizeReportsUntruncatedLength(t *testing.T) {
	summarizer := newLogBodySummarizer(BackendConfig{
		LogDropResponseFields: []string{"usage", "output"},
	})
	require.NotNil(t, summarizer)

	pub := mocks.NewMockRequestPublisher(t)
	pub.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Once()
	tracker := NewRequestTracker(pub)
	logger, buf := captureLogger()
	tracker.logger = logger

	req := httptest.NewRequest("POST", "http://example.com/v1/chat", nil)
	rc := tracker.StartTracking(req, "backend-1", "claude-opus", summarizer, "")
	require.NoError(t, rc.FinishTracking(jsonResponse(http.StatusOK, converseResponseBody), nil))

	rec := logRecordsByMsg(t, buf)["request tracking finished"]
	require.NotNil(t, rec)

	assert.NotContains(t, rec["response_body"].(string), "usage", "the dropped field must not reach the log")
	assert.EqualValues(t, len(rc.RequestInfo.ResponseBody), rec["response_body_size"],
		"response_body_size must keep measuring the full captured body, not the summarized one")
}

func TestFinishTracking_UsageExtractedFromFullBodyDespiteDrop(t *testing.T) {
	summarizer := newLogBodySummarizer(BackendConfig{
		LogDropResponseFields: []string{"usage"},
	})
	require.NotNil(t, summarizer)

	pub := mocks.NewMockRequestPublisher(t)
	pub.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Once()
	tracker := NewRequestTracker(pub)
	logger, buf := captureLogger()
	tracker.logger = logger

	req := httptest.NewRequest("POST", "http://example.com/v1/chat", nil)
	rc := tracker.StartTracking(req, "backend-1", "claude-opus", summarizer, "")
	require.NoError(t, rc.FinishTracking(jsonResponse(http.StatusOK, converseResponseBody), nil))

	rec := logRecordsByMsg(t, buf)["request tracking finished"]
	require.NotNil(t, rec)

	assert.NotContains(t, rec["response_body"].(string), "usage", "the field must be dropped from the logged body")
	assert.EqualValues(t, 11091, rec["usage_total_tokens"], "dropping usage from the log must never hide token usage")
	assert.EqualValues(t, 9214, rec["usage_prompt_tokens"], "dropping usage from the log must never hide token usage")
}

func TestRecordAttempt_BodyPreviewIsSummarized(t *testing.T) {
	summarizer := newLogBodySummarizer(BackendConfig{
		LogCropResponseFields: []string{"output.message.content.text"},
		LogCropLimit:          20,
	})
	require.NotNil(t, summarizer)

	tracker := NewRequestTracker(mocks.NewMockRequestPublisher(t))
	logger, buf := captureLogger()
	tracker.logger = logger

	req := httptest.NewRequest("POST", "http://example.com/v1/chat", nil)
	rc := tracker.StartTracking(req, "backend-1", "claude-opus", summarizer, "")
	rc.RecordAttempt(time.Now().Add(-10*time.Millisecond), jsonResponse(http.StatusOK, converseResponseBody), nil)

	rec := logRecordsByMsg(t, buf)["attempt response body captured"]
	require.NotNil(t, rec, "the per-attempt line must be emitted")

	preview := rec["body_preview"].(string)
	assert.Contains(t, preview, cropMarker, "the attempt preview must be cropped")
	assert.Contains(t, preview, "stopReason", "the attempt preview must surface the tail fields")
	assert.EqualValues(t, len(rc.attempts[0].ResponseBody), rec["body_size"],
		"body_size must keep measuring the full captured body")
	assert.Contains(t, rc.attempts[0].ResponseBody, longPrompt, "the captured attempt body must stay complete")
}

func TestStartTracking_NilSummarizerIsByteIdentical(t *testing.T) {
	tracker := NewRequestTracker(mocks.NewMockRequestPublisher(t))

	newReq := func() *http.Request {
		req := httptest.NewRequest("POST", "http://example.com/v1/chat", strings.NewReader(converseRequestBody))
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	withoutSummarizer := tracker.StartTracking(newReq(), "backend-1", "m", nil, "")
	withSummarizer := tracker.StartTracking(newReq(), "backend-1", "m", newLogBodySummarizer(BackendConfig{
		LogDropRequestFields: []string{"system"},
	}), "")

	assert.Equal(t, withoutSummarizer.RequestInfo.RequestBody, withSummarizer.RequestInfo.RequestBody,
		"capture must be identical either way: the summarizer only shapes the log line")
	assert.Contains(t, withSummarizer.RequestInfo.RequestBody, `"system"`,
		"a dropped field must still be captured for the dashboard")
}
