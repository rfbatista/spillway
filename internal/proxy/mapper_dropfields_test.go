package proxy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDropFieldsMapper_StripsConfiguredFields(t *testing.T) {
	m := NewDropFieldsMapper([]string{"temperature", "top_p"}, NewPassthroughMapper("gpt-4o"))
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}],"temperature":0,"top_p":0.9}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	assert.NotContains(t, got, "temperature", "temperature must be stripped")
	assert.NotContains(t, got, "top_p", "top_p must be stripped")
	assert.Contains(t, got, "messages", "unrelated fields must be preserved")
	assert.Equal(t, "gpt-4o", got["model"], "inner passthrough mapper must still run")
}

func TestDropFieldsMapper_StripsNestedPath(t *testing.T) {
	m := NewDropFieldsMapper([]string{"inferenceConfig.temperature"}, NewPassthroughMapper("gpt-4o"))
	req := newJSONRequest(t, `{"inferenceConfig":{"temperature":0.7,"maxTokens":4096},"messages":[]}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	cfg := got["inferenceConfig"].(map[string]any)
	assert.NotContains(t, cfg, "temperature", "nested temperature must be stripped")
	assert.Contains(t, cfg, "maxTokens", "sibling fields must be preserved")
}

func TestDropFieldsMapper_NestedPathMissingIsNoop(t *testing.T) {
	m := NewDropFieldsMapper([]string{"inferenceConfig.temperature", "a.b.c"}, NewPassthroughMapper("gpt-4o"))
	req := newJSONRequest(t, `{"messages":[],"a":"scalar"}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	assert.Equal(t, "scalar", got["a"], "a scalar on the path must be left untouched")
	assert.Contains(t, got, "messages", "unrelated fields must be preserved")
}

func TestDropFieldsMapper_DropsAcrossArrays(t *testing.T) {
	m := NewDropFieldsMapper([]string{"messages.cachePoint"}, NewPassthroughMapper("gpt-4o"))
	req := newJSONRequest(t, `{"messages":[
		{"role":"user","cachePoint":{"type":"default"},"content":"hi"},
		{"role":"assistant","cachePoint":{"type":"default"},"content":"yo"}
	]}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	messages := got["messages"].([]any)
	require.Len(t, messages, 2, "both messages must survive")
	for i, msg := range messages {
		assert.NotContains(t, msg, "cachePoint", "message %d must have cachePoint stripped", i)
		assert.Contains(t, msg, "content", "message %d must keep its content", i)
	}
}

func TestNewMapper_WrapsWithDropFieldsWhenConfigured(t *testing.T) {
	m, err := NewMapper(BackendConfig{Mapper: "openai", Model: "m", DropRequestFields: []string{"temperature"}})
	require.NoError(t, err)
	_, ok := m.(*DropFieldsMapper)
	assert.True(t, ok, "drop_request_fields must wrap the base mapper")
}

func TestNewMapper_NoDropFieldsReturnsBaseMapper(t *testing.T) {
	m, err := NewMapper(BackendConfig{Mapper: "openai", Model: "m"})
	require.NoError(t, err)
	_, ok := m.(*PassthroughMapper)
	assert.True(t, ok, "without drop_request_fields the base mapper is returned directly")
}

// TestDropFields_BedrockOmitsTemperature exercises the full chain: the decorator
// strips temperature from the OpenAI body, so the Bedrock mapper never emits it
// into inferenceConfig.
func TestDropFields_BedrockOmitsTemperature(t *testing.T) {
	m, err := NewMapper(BackendConfig{
		Mapper:            "bedrock",
		Model:             "my-model",
		Path:              "/model",
		AWSAPIKey:         "key",
		DropRequestFields: []string{"temperature", "top_p"},
	})
	require.NoError(t, err)

	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}],"temperature":0,"top_p":0.9}`)
	out, err := m.MapRequest(req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	cfg := got["inferenceConfig"].(map[string]any)
	assert.NotContains(t, cfg, "temperature", "temperature must not reach Bedrock inferenceConfig")
	assert.NotContains(t, cfg, "topP", "topP must not reach Bedrock inferenceConfig")
	assert.EqualValues(t, 4096, cfg["maxTokens"], "maxTokens must still be set")
}
