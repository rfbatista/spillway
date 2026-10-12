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

func TestAnthropicMapper_MapRequest_ExtractsSystemAndWrapsContent(t *testing.T) {
	m := NewAnthropicMapper("claude-opus-4-5")
	req := newJSONRequest(t, `{
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hello"}
		],
		"temperature":0.5,
		"stream":true
	}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))

	assert.Equal(t, "claude-opus-4-5", got["model"])
	assert.Equal(t, "be brief", got["system"])
	assert.Equal(t, 0.5, got["temperature"])
	assert.Equal(t, true, got["stream"])

	msgs := got["messages"].([]any)
	require.Len(t, msgs, 1) // system removed from messages
	user := msgs[0].(map[string]any)
	assert.Equal(t, "user", user["role"])
	content := user["content"].([]any)
	block := content[0].(map[string]any)
	assert.Equal(t, "text", block["type"])
	assert.Equal(t, "hello", block["text"])
}

func TestAnthropicMapper_MapRequest_DefaultsMaxTokens(t *testing.T) {
	m := NewAnthropicMapper("claude-opus-4-5")
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	assert.EqualValues(t, 4096, got["max_tokens"])
}

func TestAnthropicMapper_MapRequest_PreservesProvidedMaxTokens(t *testing.T) {
	m := NewAnthropicMapper("claude-opus-4-5")
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	assert.EqualValues(t, 256, got["max_tokens"])
}

func TestNewMapper_ThreadsConfiguredMaxTokens(t *testing.T) {
	m, err := NewMapper(BackendConfig{Mapper: "anthropic", Model: "claude", MaxTokens: 8000})
	require.NoError(t, err)
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	assert.EqualValues(t, 8000, got["max_tokens"], "cfg.MaxTokens must reach the mapper")
}

func TestAnthropicMapper_MapRequest_ConfiguredMaxTokensFallback(t *testing.T) {
	m := NewAnthropicMapper("claude-opus-4-5", WithMaxTokens(8000))
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	assert.EqualValues(t, 8000, got["max_tokens"], "config max_tokens replaces the 4096 fallback")
}

func TestAnthropicMapper_MapRequest_ClientMaxTokensWinsOverConfig(t *testing.T) {
	m := NewAnthropicMapper("claude-opus-4-5", WithMaxTokens(8000))
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	assert.EqualValues(t, 256, got["max_tokens"], "an explicit client max_tokens still wins")
}

func anthropicResponse(t *testing.T, body string) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestAnthropicMapper_MapResponse_NonStreaming(t *testing.T) {
	m := NewAnthropicMapper("claude-opus-4-5")
	resp := anthropicResponse(t, `{
		"id":"msg_123",
		"model":"claude-opus-4-5",
		"stop_reason":"end_turn",
		"content":[{"type":"text","text":"hello there"}],
		"usage":{"input_tokens":11,"output_tokens":7}
	}`)

	out, err := m.MapResponse(resp)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.NewDecoder(out.Body).Decode(&got))

	assert.Equal(t, "chatcmpl-msg_123", got["id"])
	assert.Equal(t, "chat.completion", got["object"])
	choices := got["choices"].([]any)
	choice := choices[0].(map[string]any)
	assert.Equal(t, "stop", choice["finish_reason"])
	msg := choice["message"].(map[string]any)
	assert.Equal(t, "assistant", msg["role"])
	assert.Equal(t, "hello there", msg["content"])
	usage := got["usage"].(map[string]any)
	assert.EqualValues(t, 11, usage["prompt_tokens"])
	assert.EqualValues(t, 7, usage["completion_tokens"])
	assert.EqualValues(t, 18, usage["total_tokens"])
}

func TestAnthropicMapper_MapResponse_StopReasonPassthrough(t *testing.T) {
	m := NewAnthropicMapper("claude-opus-4-5")
	resp := anthropicResponse(t, `{
		"id":"msg_9","model":"claude-opus-4-5","stop_reason":"max_tokens",
		"content":[{"type":"text","text":"x"}],
		"usage":{"input_tokens":1,"output_tokens":1}
	}`)

	out, err := m.MapResponse(resp)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.NewDecoder(out.Body).Decode(&got))
	choice := got["choices"].([]any)[0].(map[string]any)
	assert.Equal(t, "max_tokens", choice["finish_reason"]) // only end_turn is remapped
}

func TestAnthropicMapper_MapResponse_Streaming(t *testing.T) {
	m := NewAnthropicMapper("claude-opus-4-5")
	sse := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: [DONE]\n\n"
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}

	out, err := m.MapResponse(resp)
	require.NoError(t, err)

	data, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	text := string(data)

	assert.Contains(t, text, `"delta":{"content":"Hel"}`)
	assert.Contains(t, text, `"delta":{"content":"lo"}`)
	assert.Contains(t, text, `"finish_reason":"stop"`)
	assert.Contains(t, text, "data: [DONE]")
}

// With thinking enabled the stream opens with thinking_delta events; they must
// come back as reasoning_content chunks — mirroring the non-streaming path —
// not be silently dropped. signature_delta events carry neither text nor
// thinking and are skipped.
func TestAnthropicMapper_MapResponse_StreamingThinkingDeltas(t *testing.T) {
	m := NewAnthropicMapper("claude-opus-4-5")
	sse := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"pondering\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"signature_delta\",\"signature\":\"abc\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: [DONE]\n\n"
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}

	out, err := m.MapResponse(resp)
	require.NoError(t, err)

	data, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	text := string(data)

	assert.Contains(t, text, `"delta":{"reasoning_content":"pondering"}`)
	assert.Contains(t, text, `"delta":{"content":"Hello"}`)
	assert.NotContains(t, text, "signature", "signature deltas must be skipped, not forwarded")
	assert.Contains(t, text, `"finish_reason":"stop"`)
}
