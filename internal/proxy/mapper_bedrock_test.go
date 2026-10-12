package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type spySigner struct{ called bool }

func (s *spySigner) Sign(req *http.Request, body []byte, service, region string) error {
	s.called = true
	return nil
}

func TestBedrockMapper_MapRequest_RewritesPathAndBody(t *testing.T) {
	signer := &spySigner{}
	m := NewBedrockMapper("/model", "anthropic.claude-3-5-sonnet-20241022-v2:0", "us-east-1", "bedrock", signer)
	req := newJSONRequest(t, `{
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hi"}
		],
		"temperature":0.5,
		"top_p":0.9
	}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	assert.Equal(t, "/model/anthropic.claude-3-5-sonnet-20241022-v2%3A0/converse", out.URL.EscapedPath())
	assert.Equal(t, "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/converse", out.URL.Path)
	assert.True(t, signer.called, "signer must be invoked")

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))

	_, hasModel := got["model"]
	assert.False(t, hasModel, "model must be removed from body")

	sys := got["system"].([]any)
	assert.Equal(t, "be brief", sys[0].(map[string]any)["text"])

	msgs := got["messages"].([]any)
	require.Len(t, msgs, 1)
	user := msgs[0].(map[string]any)
	content := user["content"].([]any)
	assert.Equal(t, "hi", content[0].(map[string]any)["text"])

	cfg := got["inferenceConfig"].(map[string]any)
	assert.EqualValues(t, 4096, cfg["maxTokens"])
	assert.Equal(t, 0.5, cfg["temperature"])
	assert.Equal(t, 0.9, cfg["topP"])
}

func TestBearerSigner_SetsAuthorizationHeader(t *testing.T) {
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`)

	out, err := NewBedrockMapper("/model", "my-model", "", "bedrock", NewBearerSigner("api-key-123")).MapRequest(req)
	require.NoError(t, err)
	assert.Equal(t, "Bearer api-key-123", out.Header.Get("Authorization"))
}

func TestBedrockMapper_MapRequest_StreamSwitchesPath(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}],"stream":true}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)
	assert.Equal(t, "/model/my-model/converse-stream", out.URL.Path)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	_, hasStream := got["stream"]
	assert.False(t, hasStream, "stream must be removed from body")
}

func TestBedrockMapper_MapRequest_ConfiguredMaxTokensFallback(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{}, WithMaxTokens(8000))
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	cfg := got["inferenceConfig"].(map[string]any)
	assert.EqualValues(t, 8000, cfg["maxTokens"], "config max_tokens replaces the 4096 fallback")
}

func TestBedrockMapper_MapRequest_ClientMaxTokensWinsOverConfig(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{}, WithMaxTokens(8000))
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	cfg := got["inferenceConfig"].(map[string]any)
	assert.EqualValues(t, 256, cfg["maxTokens"], "an explicit client max_tokens still wins")
}

func bedrockResponse(t *testing.T, body string) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestBedrockMapper_MapResponse(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	resp := bedrockResponse(t, `{
		"output":{"message":{"role":"assistant","content":[{"text":"hi back"}]}},
		"stopReason":"end_turn",
		"usage":{"inputTokens":12,"outputTokens":4}
	}`)

	out, err := m.MapResponse(resp)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.NewDecoder(out.Body).Decode(&got))

	assert.Equal(t, "chat.completion", got["object"])
	choice := got["choices"].([]any)[0].(map[string]any)
	assert.Equal(t, "stop", choice["finish_reason"])
	msg := choice["message"].(map[string]any)
	assert.Equal(t, "assistant", msg["role"])
	assert.Equal(t, "hi back", msg["content"])
	usage := got["usage"].(map[string]any)
	assert.EqualValues(t, 12, usage["prompt_tokens"])
	assert.EqualValues(t, 4, usage["completion_tokens"])
	assert.EqualValues(t, 16, usage["total_tokens"])
}

func TestBedrockMapper_MapResponse_MaxTokensPassthrough(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	resp := bedrockResponse(t, `{
		"output":{"message":{"role":"assistant","content":[{"text":"x"}]}},
		"stopReason":"max_tokens",
		"usage":{"inputTokens":1,"outputTokens":1}
	}`)
	out, err := m.MapResponse(resp)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.NewDecoder(out.Body).Decode(&got))
	choice := got["choices"].([]any)[0].(map[string]any)
	assert.Equal(t, "max_tokens", choice["finish_reason"])
}
