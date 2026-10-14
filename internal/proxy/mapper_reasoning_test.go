package proxy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rfbatista/spillway/internal/apperr"
)

// Clients (LangChain ChatOpenAI) ask for reasoning with the OpenAI
// reasoning_effort field; Bedrock expects reasoning_config under
// additionalModelRequestFields.
func TestBedrockMapper_MapRequest_TranslatesReasoningEffort(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	req := newJSONRequest(t, `{
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":8192,
		"reasoning_effort":"high"
	}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))

	_, hasEffort := got["reasoning_effort"]
	assert.False(t, hasEffort, "reasoning_effort is not a Converse field")

	fields := got["additionalModelRequestFields"].(map[string]any)
	cfg := fields["reasoning_config"].(map[string]any)
	assert.Equal(t, "enabled", cfg["type"])
	assert.EqualValues(t, 4096, cfg["budget_tokens"])
	assert.EqualValues(t, 8192, got["inferenceConfig"].(map[string]any)["maxTokens"])
}

// Thinking and sampling parameters are mutually exclusive for Anthropic models.
func TestBedrockMapper_MapRequest_ReasoningDropsSamplingParams(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	req := newJSONRequest(t, `{
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":8192,
		"temperature":0.5,
		"top_p":0.9,
		"reasoning_effort":"low"
	}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))

	cfg := got["inferenceConfig"].(map[string]any)
	assert.NotContains(t, cfg, "temperature")
	assert.NotContains(t, cfg, "topP")
}

// A caller that already speaks Converse can send the native field; the proxy
// must forward it untouched instead of overwriting it.
func TestBedrockMapper_MapRequest_ForwardsAdditionalModelRequestFields(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	req := newJSONRequest(t, `{
		"messages":[{"role":"user","content":"hi"}],
		"additionalModelRequestFields":{"reasoning_config":{"type":"enabled","budget_tokens":10000},"anthropic_beta":["x"]}
	}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))

	fields := got["additionalModelRequestFields"].(map[string]any)
	cfg := fields["reasoning_config"].(map[string]any)
	assert.EqualValues(t, 10000, cfg["budget_tokens"])
	assert.Equal(t, []any{"x"}, fields["anthropic_beta"])
}

func TestBedrockMapper_MapRequest_RejectsBudgetLargerThanMaxTokens(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	req := newJSONRequest(t, `{
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":4096,
		"reasoning_effort":"high"
	}`)

	_, err := m.MapRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, apperr.ErrInvalidRequest)
}

// With reasoning on, Converse puts a reasoningContent block before the text
// block: reading content[0] blindly returns an empty completion.
func TestBedrockMapper_MapResponse_ReadsTextPastReasoningBlock(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	resp := bedrockResponse(t, `{
		"output":{"message":{"role":"assistant","content":[
			{"reasoningContent":{"reasoningText":{"text":"let me think","signature":"sig"}}},
			{"text":"hi back"}
		]}},
		"stopReason":"end_turn",
		"usage":{"inputTokens":12,"outputTokens":4}
	}`)

	out, err := m.MapResponse(resp)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.NewDecoder(out.Body).Decode(&got))

	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	assert.Equal(t, "hi back", msg["content"])
	assert.Equal(t, "let me think", msg["reasoning_content"])
}

func TestAnthropicMapper_MapRequest_TranslatesReasoningEffort(t *testing.T) {
	m := NewAnthropicMapper("claude")
	req := newJSONRequest(t, `{
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":8192,
		"temperature":0.5,
		"reasoning_effort":"medium"
	}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))

	thinking := got["thinking"].(map[string]any)
	assert.Equal(t, "enabled", thinking["type"])
	assert.EqualValues(t, 2048, thinking["budget_tokens"])
	assert.NotContains(t, got, "temperature", "temperature is rejected when thinking is enabled")
}

func TestAnthropicMapper_MapRequest_RejectsBudgetLargerThanMaxTokens(t *testing.T) {
	req := newJSONRequest(t, `{
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":1024,
		"reasoning_effort":"low"
	}`)

	_, err := NewAnthropicMapper("claude").MapRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, apperr.ErrInvalidRequest)
}

func TestAnthropicMapper_MapResponse_ReadsTextPastThinkingBlock(t *testing.T) {
	m := NewAnthropicMapper("claude")
	resp := bedrockResponse(t, `{
		"id":"msg_1","model":"claude","stop_reason":"end_turn",
		"content":[
			{"type":"thinking","thinking":"let me think","signature":"sig"},
			{"type":"text","text":"hi back"}
		],
		"usage":{"input_tokens":2,"output_tokens":3}
	}`)

	out, err := m.MapResponse(resp)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.NewDecoder(out.Body).Decode(&got))

	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	assert.Equal(t, "hi back", msg["content"])
	assert.Equal(t, "let me think", msg["reasoning_content"])
}

// additionalModelRequestFields carrying an unrelated knob (anthropic_beta) must
// not silently disable the reasoning_effort translation: the reasoning_config
// is merged in next to the caller's fields and sampling is stripped.
func TestBedrockMapper_MapRequest_EffortMergesWithUnrelatedAdditionalFields(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	req := newJSONRequest(t, `{
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":8192,
		"temperature":0.5,
		"reasoning_effort":"high",
		"additionalModelRequestFields":{"anthropic_beta":["x"]}
	}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))

	fields := got["additionalModelRequestFields"].(map[string]any)
	assert.Equal(t, []any{"x"}, fields["anthropic_beta"], "caller's fields must survive")
	cfg := fields["reasoning_config"].(map[string]any)
	assert.Equal(t, "enabled", cfg["type"])
	assert.EqualValues(t, 4096, cfg["budget_tokens"])
	assert.NotContains(t, got["inferenceConfig"].(map[string]any), "temperature")
}

// A caller enabling thinking through the native reasoning_config gets the same
// sampling-parameter strip the reasoning_effort translation applies — Anthropic
// rejects temperature with thinking enabled either way.
func TestBedrockMapper_MapRequest_NativeReasoningConfigDropsSamplingParams(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	req := newJSONRequest(t, `{
		"messages":[{"role":"user","content":"hi"}],
		"temperature":0.5,
		"top_p":0.9,
		"additionalModelRequestFields":{"reasoning_config":{"type":"enabled","budget_tokens":2048}}
	}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))

	cfg := got["inferenceConfig"].(map[string]any)
	assert.NotContains(t, cfg, "temperature")
	assert.NotContains(t, cfg, "topP")
	native := got["additionalModelRequestFields"].(map[string]any)["reasoning_config"].(map[string]any)
	assert.EqualValues(t, 2048, native["budget_tokens"], "the native config must win untouched")
}

// A native thinking:{"type":"disabled"} must round-trip without gaining a
// budget_tokens field (the disabled variant does not accept one) and without
// losing temperature, which is only invalid while thinking is enabled.
func TestAnthropicMapper_MapRequest_ThinkingDisabledForwardedClean(t *testing.T) {
	m := NewAnthropicMapper("claude-model")
	req := newJSONRequest(t, `{
		"messages":[{"role":"user","content":"hi"}],
		"temperature":0.7,
		"thinking":{"type":"disabled"}
	}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))

	thinking := got["thinking"].(map[string]any)
	assert.Equal(t, "disabled", thinking["type"])
	assert.NotContains(t, thinking, "budget_tokens")
	assert.EqualValues(t, 0.7, got["temperature"])
}
