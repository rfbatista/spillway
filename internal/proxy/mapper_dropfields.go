package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// DropFieldsMapper strips configured fields from a JSON request body before
// delegating to the wrapped mapper. A field is a dot-separated path
// ("temperature" for a top-level key, "inferenceConfig.temperature" for a nested
// one), letting configuration adapt a backend to a model that rejects certain
// parameters (e.g. Bedrock Converse "inferenceConfig.temperature") without any
// client change. Responses pass through to the inner mapper unchanged.
type DropFieldsMapper struct {
	fields []string
	inner  RequestMapper
}

// NewDropFieldsMapper wraps inner so the named JSON request fields are removed
// before mapping.
func NewDropFieldsMapper(fields []string, inner RequestMapper) *DropFieldsMapper {
	return &DropFieldsMapper{fields: fields, inner: inner}
}

// MapRequest removes the configured fields from a JSON object body, then
// delegates. Non-JSON or empty bodies are forwarded to the inner mapper as-is.
func (m *DropFieldsMapper) MapRequest(req *http.Request) (*http.Request, error) {
	if req.Body == nil || req.Header.Get("Content-Type") != "application/json" {
		return m.inner.MapRequest(req)
	}

	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		// Not a JSON object we can edit; forward the original bytes.
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		return m.inner.MapRequest(req)
	}

	for _, f := range m.fields {
		deleteJSONPath(payload, f)
	}
	newBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	setJSONBody(req, newBody)
	return m.inner.MapRequest(req)
}

// MapResponse forwards to the inner mapper unchanged.
func (m *DropFieldsMapper) MapResponse(resp *http.Response) (*http.Response, error) {
	return m.inner.MapResponse(resp)
}

// deleteJSONPath removes the value at a dot-separated path from a decoded JSON
// object. A single segment deletes a top-level key; deeper segments descend
// through nested objects, and through arrays implicitly, so
// "messages.content" drops content from every message. The path is a no-op when
// a segment is missing or lands on a scalar, so a stale or mistyped path never
// panics or mangles unrelated fields.
func deleteJSONPath(payload map[string]any, path string) {
	applyJSONPath(payload, splitJSONPath(path), func(any) (any, bool) {
		return nil, true
	})
}
