package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegration_AnthropicRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), `"system":"be brief"`)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_x","model":"claude","stop_reason":"end_turn","content":[{"type":"text","text":"hi back"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	h := NewProxy(*targetURL, BackendConfig{}, NewAnthropicMapper("claude-opus-4-5"), http.DefaultTransport)

	// Client carries the upstream path; the Director composes backend path
	// (empty here) + request path. AnthropicMapper does not rewrite the path.
	req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/messages", strings.NewReader(
		`{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	choice := got["choices"].([]any)[0].(map[string]any)
	assert.Equal(t, "hi back", choice["message"].(map[string]any)["content"])
	assert.Equal(t, "stop", choice["finish_reason"])
}

func TestIntegration_BedrockRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/model/")
		assert.Contains(t, r.URL.Path, "/converse")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"hi back"}]}},"stopReason":"end_turn","usage":{"inputTokens":2,"outputTokens":1}}`)
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	mapper := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	h := NewProxy(*targetURL, BackendConfig{}, mapper, http.DefaultTransport)

	req := httptest.NewRequest(http.MethodPost, "http://proxy/", strings.NewReader(
		`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	choice := got["choices"].([]any)[0].(map[string]any)
	assert.Equal(t, "hi back", choice["message"].(map[string]any)["content"])
}

func TestIntegration_BedrockConverseRoundTrip(t *testing.T) {
	const answer = `{"output":{"message":{"role":"assistant","content":[{"text":"hi back"}]}},"stopReason":"end_turn","usage":{"inputTokens":2,"outputTokens":1}}`
	var received string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/model/my-model/converse", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, answer)
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	mapper := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	h := NewProxy(*targetURL, BackendConfig{}, mapper, http.DefaultTransport)

	req := httptest.NewRequest(http.MethodPost, "http://proxy/model/caller-model/converse", strings.NewReader(converseBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.JSONEq(t, converseBody, received, "the backend must receive the Converse body untouched")
	assert.JSONEq(t, answer, rr.Body.String(), "the Converse caller must get the provider's answer back unchanged")
}
