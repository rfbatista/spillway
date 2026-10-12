package proxy

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// converseRequestBody is a Bedrock Converse request in the shape the tracker
// captures it: a long prompt buried in a block list, surrounded by the small
// fields that matter during triage.
const converseRequestBody = `{
	"messages": [
		{"role":"user","content":[{"text":"` + longPrompt + `"},{"text":"segundo bloco"}]},
		{"role":"assistant","content":[{"text":"ok"}]}
	],
	"system": [{"text":"` + longPrompt + `"}],
	"inferenceConfig": {"maxTokens":4096,"temperature":0},
	"additionalModelRequestFields": {"reasoning_config":{"type":"enabled","budget_tokens":2048}}
}`

// longPrompt stands in for a real prompt: far longer than any crop limit used
// in these tests.
const longPrompt = "Voce e um analista de matriculas imobiliarias e deve extrair proprietario, area e onus do documento a seguir, respeitando a ordem das averbacoes e citando o numero de cada registro encontrado no corpo da matricula"

func TestLogBodySummarizer_NilIsNoop(t *testing.T) {
	var s *logBodySummarizer

	assert.False(t, s.active(), "a nil summarizer must report itself inactive")
	assert.Equal(t, converseRequestBody, s.RequestBody(converseRequestBody), "a nil summarizer must return the request body unchanged")
	assert.Equal(t, converseRequestBody, s.ResponseBody(converseRequestBody), "a nil summarizer must return the response body unchanged")
}

func TestNewLogBodySummarizer_NoFieldsReturnsNil(t *testing.T) {
	tests := []struct {
		name string
		cfg  BackendConfig
	}{
		{name: "nothing configured", cfg: BackendConfig{Model: "m"}},
		{name: "only a crop limit", cfg: BackendConfig{Model: "m", LogCropLimit: 100}},
		{name: "only blank paths", cfg: BackendConfig{Model: "m", LogCropRequestFields: []string{"", "  "}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Nil(t, newLogBodySummarizer(tt.cfg), "without usable paths the summarizer must be nil so the common path pays nothing")
		})
	}
}

func TestNewLogBodySummarizer_DefaultsCropLimit(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
	}{
		{name: "unset falls back to the default", configured: 0, want: defaultLogCropLimit},
		{name: "negative falls back to the default", configured: -10, want: defaultLogCropLimit},
		{name: "explicit value is kept", configured: 40, want: 40},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newLogBodySummarizer(BackendConfig{
				LogCropRequestFields: []string{"messages.content.text"},
				LogCropLimit:         tt.configured,
			})
			require.NotNil(t, s, "configured crop paths must produce a summarizer")
			assert.Equal(t, tt.want, s.cropLimit, "crop limit must match")
		})
	}
}

func TestLogBodySummarizer_DropsConfiguredRequestFields(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{LogDropRequestFields: []string{"system"}})
	require.NotNil(t, s)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(s.RequestBody(converseRequestBody)), &got))

	assert.NotContains(t, got, "system", "the configured field must be dropped")
	assert.Contains(t, got, "messages", "unrelated fields must survive")
	assert.Contains(t, got, "inferenceConfig", "the small fields the filter exists to surface must survive")
	assert.Contains(t, got, "additionalModelRequestFields", "the small fields the filter exists to surface must survive")
}

func TestLogBodySummarizer_CropsStringsAcrossArrays(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{
		LogCropRequestFields: []string{"messages.content.text"},
		LogCropLimit:         20,
	})
	require.NotNil(t, s)

	out := s.RequestBody(converseRequestBody)
	assert.Less(t, len(out), len(converseRequestBody), "cropping must shrink the body")

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	messages := got["messages"].([]any)
	first := messages[0].(map[string]any)["content"].([]any)
	assert.Equal(t, longPrompt[:20]+cropMarker, first[0].(map[string]any)["text"], "the long block must be cropped at the limit")
	assert.Equal(t, "segundo bloco", first[1].(map[string]any)["text"], "a block under the limit must be untouched")

	second := messages[1].(map[string]any)["content"].([]any)
	assert.Equal(t, "ok", second[0].(map[string]any)["text"], "every message must be traversed, and short text left alone")

	assert.Contains(t, got, "inferenceConfig", "crop must not disturb sibling fields")
	system := got["system"].([]any)
	assert.Equal(t, longPrompt, system[0].(map[string]any)["text"], "a path that was not configured must be left alone")
}

func TestLogBodySummarizer_CropsArrayOfStrings(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{
		LogCropRequestFields: []string{"system"},
		LogCropLimit:         10,
	})
	require.NotNil(t, s)

	var got map[string]any
	body := `{"system":["` + longPrompt + `","curto"],"keep":1}`
	require.NoError(t, json.Unmarshal([]byte(s.RequestBody(body)), &got))

	system := got["system"].([]any)
	assert.Equal(t, longPrompt[:10]+cropMarker, system[0], "a long element must be cropped")
	assert.Equal(t, "curto", system[1], "a short element must be untouched")
	assert.EqualValues(t, 1, got["keep"], "unrelated fields must survive")
}

func TestLogBodySummarizer_CropIgnoresNonStringValues(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{
		LogCropRequestFields: []string{"count", "flag", "nested"},
		LogCropLimit:         2,
	})
	require.NotNil(t, s)

	body := `{"count":12345,"flag":true,"nested":{"text":"uma frase bem longa"}}`
	out := s.RequestBody(body)

	assert.Equal(t, body, out, "a crop path aimed at a non-string must leave the body byte-identical")
	assert.True(t, json.Valid([]byte(out)), "the output must stay valid JSON")
}

func TestLogBodySummarizer_ShortStringIsReturnedByteIdentical(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{
		LogCropRequestFields: []string{"prompt"},
		LogCropLimit:         500,
	})
	require.NotNil(t, s)

	body := `{  "prompt" : "curto"  }`
	assert.Equal(t, body, s.RequestBody(body), "when nothing is cropped the original string must be returned untouched, spacing included")
}

func TestLogBodySummarizer_CropIsRuneSafe(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{
		LogCropRequestFields: []string{"prompt"},
		LogCropLimit:         5,
	})
	require.NotNil(t, s)

	for _, prompt := range []string{strings.Repeat("é", 50), strings.Repeat("🙂", 50), strings.Repeat("矩", 50)} {
		body, err := json.Marshal(map[string]string{"prompt": prompt})
		require.NoError(t, err)

		out := s.RequestBody(string(body))
		assert.True(t, utf8.ValidString(out), "cropping must never split a multi-byte rune")

		var got map[string]string
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		kept := strings.TrimSuffix(got["prompt"], cropMarker)
		assert.Equal(t, 5, utf8.RuneCountInString(kept), "exactly the configured number of runes must be kept")
	}
}

func TestLogBodySummarizer_DropWinsWhenPathIsInBothLists(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{
		LogDropRequestFields: []string{"system"},
		LogCropRequestFields: []string{"system"},
		LogCropLimit:         5,
	})
	require.NotNil(t, s)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(s.RequestBody(converseRequestBody)), &got))
	assert.NotContains(t, got, "system", "a path in both lists must be dropped, not cropped")
}

func TestLogBodySummarizer_NonJSONBodyUnchanged(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{
		LogDropResponseFields: []string{"usage"},
		LogCropResponseFields: []string{"output.message.content.text"},
	})
	require.NotNil(t, s)

	tests := []struct {
		name string
		body string
	}{
		{name: "plain text", body: "not json at all"},
		{name: "streamed chunk", body: "data: {\"delta\":\"hi\"}\n\n"},
		{name: "truncated json", body: `{"output":{"message":`},
		{name: "empty", body: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.body, s.ResponseBody(tt.body), "a body that is not a JSON document must pass through untouched")
		})
	}
}

func TestLogBodySummarizer_RootArrayAndScalarBodies(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{
		LogCropResponseFields: []string{"text"},
		LogCropLimit:          4,
	})
	require.NotNil(t, s)

	assert.NotPanics(t, func() {
		out := s.ResponseBody(`[{"text":"` + longPrompt + `"}]`)
		assert.Contains(t, out, cropMarker, "a root array must still be traversed")

		assert.Equal(t, `42`, s.ResponseBody(`42`), "a scalar root matches nothing and must pass through")
		assert.Equal(t, `null`, s.ResponseBody(`null`), "a null root matches nothing and must pass through")
	}, "an unusual root must never panic")
}

func TestLogBodySummarizer_PreservesLargeNumbers(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{LogDropResponseFields: []string{"drop"}})
	require.NotNil(t, s)

	out := s.ResponseBody(`{"seed":12345678901234567890,"ratio":0.30,"drop":1}`)

	assert.Contains(t, out, "12345678901234567890", "a large integer must survive the decode/encode round trip verbatim")
	assert.NotContains(t, out, "drop", "the configured field must still be dropped")
}

func TestLogBodySummarizer_RequestAndResponseListsAreSeparate(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{
		LogDropRequestFields:  []string{"onlyInRequest"},
		LogDropResponseFields: []string{"onlyInResponse"},
	})
	require.NotNil(t, s)

	body := `{"onlyInRequest":1,"onlyInResponse":2}`

	var fromRequest map[string]any
	require.NoError(t, json.Unmarshal([]byte(s.RequestBody(body)), &fromRequest))
	assert.NotContains(t, fromRequest, "onlyInRequest", "the request list must apply to the request body")
	assert.Contains(t, fromRequest, "onlyInResponse", "the response list must not leak into the request body")

	var fromResponse map[string]any
	require.NoError(t, json.Unmarshal([]byte(s.ResponseBody(body)), &fromResponse))
	assert.NotContains(t, fromResponse, "onlyInResponse", "the response list must apply to the response body")
	assert.Contains(t, fromResponse, "onlyInRequest", "the request list must not leak into the response body")
}

func TestLogBodySummarizer_OutputIsCompact(t *testing.T) {
	s := newLogBodySummarizer(BackendConfig{
		LogCropRequestFields: []string{"messages.content.text"},
		LogCropLimit:         20,
	})
	require.NotNil(t, s)

	out := s.RequestBody(converseRequestBody)
	assert.NotContains(t, out, "\n", "the logged body must be compact so indentation does not spend the byte budget")
	assert.NotContains(t, out, "\t", "the logged body must be compact so indentation does not spend the byte budget")
}
