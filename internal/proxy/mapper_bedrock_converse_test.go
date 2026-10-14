package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rfbatista/spillway/internal/apperr"
)

// converseBody is a request in AWS Converse shape, as an AWS SDK client sends
// it: sampling parameters under inferenceConfig, message content as a block
// list, and model-specific parameters under additionalModelRequestFields.
const converseBody = `{
	"additionalModelRequestFields":{
		"output_config":{"effort":"xhigh"},
		"thinking":{"type":"adaptive"}
	},
	"inferenceConfig":{"maxTokens":128000,"temperature":1},
	"system":[{"text":"be brief"}],
	"messages":[{"role":"user","content":[{"text":"hi"}]}]
}`

func newConverseRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://example.com"+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestBedrockMapper_MapRequest_ConverseBodyForwardedVerbatim(t *testing.T) {
	signer := &spySigner{}
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", signer)
	req := newConverseRequest(t, "/model/caller-model/converse", converseBody)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	assert.Equal(t, "/model/my-model/converse", out.URL.Path, "path must carry the backend's model")
	assert.True(t, signer.called, "signer must be invoked")

	var want, got map[string]any
	require.NoError(t, json.Unmarshal([]byte(converseBody), &want))
	require.NoError(t, json.Unmarshal([]byte(readBody(t, out)), &got))
	assert.Equal(t, want, got, "a Converse body must reach the backend untouched")
}

func TestBedrockMapper_MapRequest_ConverseStreamKeepsStreamingAction(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	req := newConverseRequest(t, "/model/caller-model/converse-stream", converseBody)

	out, err := m.MapRequest(req)
	require.NoError(t, err)
	assert.Equal(t, "/model/my-model/converse-stream", out.URL.Path)
}

func TestBedrockMapper_MapResponse_ConverseAnswerLeftUntouched(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	sent, err := m.MapRequest(newConverseRequest(t, "/model/caller-model/converse", converseBody))
	require.NoError(t, err)

	const answer = `{"output":{"message":{"role":"assistant","content":[{"text":"hi back"}]}},"stopReason":"end_turn"}`
	resp := bedrockResponse(t, answer)
	resp.Request = sent

	out, err := m.MapResponse(resp)
	require.NoError(t, err)

	body, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	assert.JSONEq(t, answer, string(body), "a Converse caller must get the provider's answer back unchanged")
}

func TestBedrockMapper_MapResponse_OpenAICallerStillMapped(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	sent, err := m.MapRequest(newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)

	resp := bedrockResponse(t, `{"output":{"message":{"role":"assistant","content":[{"text":"hi back"}]}},"stopReason":"end_turn"}`)
	resp.Request = sent

	out, err := m.MapResponse(resp)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.NewDecoder(out.Body).Decode(&got))
	assert.Equal(t, "chat.completion", got["object"])
}

// OpenAI multimodal content is a list too, but its parts carry a "type"
// discriminator — the mere presence of a list must not classify the body as
// native Converse, or the OpenAI body would be forwarded to Bedrock verbatim.
func TestIsConverseRequest_BlockShapeDecides(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "converse block content",
			body: `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`,
			want: true,
		},
		{
			name: "inferenceConfig alone",
			body: `{"messages":[{"role":"user","content":"hi"}],"inferenceConfig":{"maxTokens":100}}`,
			want: true,
		},
		{
			name: "openai string content",
			body: `{"messages":[{"role":"user","content":"hi"}],"max_tokens":100}`,
			want: false,
		},
		{
			name: "openai typed content parts",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":100}`,
			want: false,
		},
		{
			name: "empty content list",
			body: `{"messages":[{"role":"user","content":[]}]}`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isConverseRequest([]byte(tt.body)))
		})
	}
}

// An OpenAI multimodal body a bedrock backend cannot translate is the caller's
// to fix: it must be rejected as an invalid request, not forwarded to Bedrock
// in a shape the provider will bounce with an unmapped error.
func TestBedrockMapper_MapRequest_OpenAIContentPartsRejected(t *testing.T) {
	m := NewBedrockMapper("/model", "my-model", "us-east-1", "bedrock", &NoopSigner{})
	req := newConverseRequest(t, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":100}`)

	_, err := m.MapRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, apperr.ErrInvalidRequest)
}
