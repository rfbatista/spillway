package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newJSONRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func readBody(t *testing.T, req *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	return string(b)
}

func TestPassthroughMapper_InjectsModel(t *testing.T) {
	m := NewPassthroughMapper("gpt-4o")
	req := newJSONRequest(t, `{"messages":[{"role":"user","content":"hi"}]}`)

	out, err := m.MapRequest(req)
	require.NoError(t, err)

	body := readBody(t, out)
	assert.Contains(t, body, `"model":"gpt-4o"`)
	assert.Contains(t, body, `"messages"`)
	assert.Equal(t, int64(len(body)), out.ContentLength)
}

func TestPassthroughMapper_NonJSONPassesThrough(t *testing.T) {
	m := NewPassthroughMapper("gpt-4o")
	req, err := http.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", strings.NewReader("not json"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "text/plain")

	out, err := m.MapRequest(req)
	require.NoError(t, err)
	assert.Equal(t, "not json", readBody(t, out))
}

func TestPassthroughMapper_ResponseUnchanged(t *testing.T) {
	m := NewPassthroughMapper("gpt-4o")
	resp := &http.Response{StatusCode: http.StatusOK}
	out, err := m.MapResponse(resp)
	require.NoError(t, err)
	assert.Same(t, resp, out)
}

func TestGrokMapper_IsPassthroughAlias(t *testing.T) {
	m := NewGrokMapper("grok-3")
	req := newJSONRequest(t, `{"messages":[]}`)
	out, err := m.MapRequest(req)
	require.NoError(t, err)
	assert.Contains(t, readBody(t, out), `"model":"grok-3"`)
}

func TestOpenAIMapper_IsPassthroughAlias(t *testing.T) {
	m := NewOpenAIMapper("gpt-4o")
	req := newJSONRequest(t, `{"messages":[]}`)
	out, err := m.MapRequest(req)
	require.NoError(t, err)
	assert.Contains(t, readBody(t, out), `"model":"gpt-4o"`)
}
