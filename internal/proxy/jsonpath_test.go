package proxy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeJSON is a test helper that decodes a JSON document into the generic
// tree applyJSONPath walks.
func decodeJSON(t *testing.T, body string) any {
	t.Helper()
	var payload any
	require.NoError(t, json.Unmarshal([]byte(body), &payload), "fixture must be valid JSON")
	return payload
}

// dropVisitor removes every value the path matches.
func dropVisitor() jsonPathVisitor {
	return func(any) (any, bool) { return nil, true }
}

// markVisitor replaces every matched value with a sentinel and counts the hits.
func markVisitor(hits *int) jsonPathVisitor {
	return func(any) (any, bool) {
		*hits++
		return "MARKED", false
	}
}

func TestSplitJSONPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want []string
	}{
		{name: "empty is no segments", path: "", want: nil},
		{name: "blank is no segments", path: "   ", want: nil},
		{name: "single segment", path: "temperature", want: []string{"temperature"}},
		{name: "nested segments", path: "inferenceConfig.temperature", want: []string{"inferenceConfig", "temperature"}},
		{name: "surrounding space is trimmed", path: "  a.b  ", want: []string{"a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, splitJSONPath(tt.path), "segments must match")
		})
	}
}

func TestApplyJSONPath_DropsTopLevelKey(t *testing.T) {
	payload := decodeJSON(t, `{"temperature":0.7,"messages":[]}`)

	applyJSONPath(payload, splitJSONPath("temperature"), dropVisitor())

	got := payload.(map[string]any)
	assert.NotContains(t, got, "temperature", "top-level key must be dropped")
	assert.Contains(t, got, "messages", "unrelated key must be preserved")
}

func TestApplyJSONPath_DropsNestedKey(t *testing.T) {
	payload := decodeJSON(t, `{"inferenceConfig":{"temperature":0.7,"maxTokens":4096}}`)

	applyJSONPath(payload, splitJSONPath("inferenceConfig.temperature"), dropVisitor())

	cfg := payload.(map[string]any)["inferenceConfig"].(map[string]any)
	assert.NotContains(t, cfg, "temperature", "nested key must be dropped")
	assert.Contains(t, cfg, "maxTokens", "sibling must be preserved")
}

func TestApplyJSONPath_TraversesArraysImplicitly(t *testing.T) {
	payload := decodeJSON(t, `{"messages":[
		{"role":"user","content":[{"text":"a"},{"text":"b"}]},
		{"role":"assistant","content":[{"text":"c"}]}
	]}`)

	hits := 0
	applyJSONPath(payload, splitJSONPath("messages.content.text"), markVisitor(&hits))

	assert.Equal(t, 3, hits, "every text across every message and block must be visited")
	messages := payload.(map[string]any)["messages"].([]any)
	first := messages[0].(map[string]any)["content"].([]any)
	assert.Equal(t, "MARKED", first[0].(map[string]any)["text"], "first block must be replaced")
	assert.Equal(t, "user", messages[0].(map[string]any)["role"], "sibling fields must be preserved")
}

func TestApplyJSONPath_TraversesNestedArrays(t *testing.T) {
	payload := decodeJSON(t, `{"rows":[[{"v":1},{"v":2}],[{"v":3}]]}`)

	hits := 0
	applyJSONPath(payload, splitJSONPath("rows.v"), markVisitor(&hits))

	assert.Equal(t, 3, hits, "an array of arrays must be traversed to the leaves")
}

func TestApplyJSONPath_RootArrayAppliesToEachElement(t *testing.T) {
	payload := decodeJSON(t, `[{"text":"a"},{"text":"b"},{"other":"c"}]`)

	hits := 0
	applyJSONPath(payload, splitJSONPath("text"), markVisitor(&hits))

	assert.Equal(t, 2, hits, "only the elements carrying the key must be visited")
}

func TestApplyJSONPath_LeafOnArrayPassesArrayToVisitor(t *testing.T) {
	payload := decodeJSON(t, `{"system":["one","two"],"keep":1}`)

	var seen any
	applyJSONPath(payload, splitJSONPath("system"), func(v any) (any, bool) {
		seen = v
		return v, false
	})
	assert.IsType(t, []any{}, seen, "the visitor must receive the whole array")

	applyJSONPath(payload, splitJSONPath("system"), dropVisitor())
	got := payload.(map[string]any)
	assert.NotContains(t, got, "system", "dropping a leaf array must remove its key")
	assert.Contains(t, got, "keep", "unrelated key must be preserved")
}

func TestApplyJSONPath_MissingSegmentIsNoop(t *testing.T) {
	payload := decodeJSON(t, `{"messages":[],"a":{"b":1}}`)

	hits := 0
	applyJSONPath(payload, splitJSONPath("a.missing.deep"), markVisitor(&hits))
	applyJSONPath(payload, splitJSONPath("nothing"), markVisitor(&hits))

	assert.Zero(t, hits, "a missing segment must not visit anything")
	assert.Equal(t, float64(1), payload.(map[string]any)["a"].(map[string]any)["b"], "the tree must be untouched")
}

func TestApplyJSONPath_ScalarOnPathIsNoop(t *testing.T) {
	payload := decodeJSON(t, `{"a":"scalar","messages":[]}`)

	hits := 0
	applyJSONPath(payload, splitJSONPath("a.b.c"), markVisitor(&hits))

	assert.Zero(t, hits, "a scalar on the path must not be descended into")
	assert.Equal(t, "scalar", payload.(map[string]any)["a"], "the scalar must be left untouched")
}

func TestApplyJSONPath_RootScalarIsNoop(t *testing.T) {
	hits := 0
	assert.NotPanics(t, func() {
		applyJSONPath(decodeJSON(t, `"just a string"`), splitJSONPath("a"), markVisitor(&hits))
		applyJSONPath(decodeJSON(t, `42`), splitJSONPath("a"), markVisitor(&hits))
		applyJSONPath(nil, splitJSONPath("a"), markVisitor(&hits))
	}, "a scalar or nil root must never panic")
	assert.Zero(t, hits, "nothing can be visited on a scalar root")
}

func TestApplyJSONPath_EmptyPathAndNilVisitorAreNoop(t *testing.T) {
	payload := decodeJSON(t, `{"a":1}`)

	assert.NotPanics(t, func() {
		applyJSONPath(payload, splitJSONPath(""), dropVisitor())
		applyJSONPath(payload, nil, dropVisitor())
		applyJSONPath(payload, splitJSONPath("a"), nil)
	}, "an empty path or nil visitor must never panic")

	assert.Contains(t, payload.(map[string]any), "a", "nothing must be dropped")
}
