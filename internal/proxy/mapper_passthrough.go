package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// PassthroughMapper preserves OpenAI-compatible behaviour: it only injects the
// configured model into the JSON request body and forwards the response
// unchanged. Used for OpenRouter, Azure OpenAI, and any OpenAI-compatible host.
type PassthroughMapper struct {
	model string
}

// NewPassthroughMapper returns a PassthroughMapper that injects model.
func NewPassthroughMapper(model string) *PassthroughMapper {
	return &PassthroughMapper{model: model}
}

// NewGrokMapper returns the mapper for xAI's Grok API. Grok is fully
// OpenAI-compatible, so this is an explicit alias for PassthroughMapper; naming
// it keeps config intent self-documenting and allows future divergence.
func NewGrokMapper(model string) *PassthroughMapper {
	return NewPassthroughMapper(model)
}

// NewOpenAIMapper returns the mapper for an OpenAI Chat Completions backend.
// OpenAI is the canonical wire format, so this is an explicit alias for
// PassthroughMapper; naming it keeps config intent self-documenting and allows
// future divergence.
func NewOpenAIMapper(model string) *PassthroughMapper {
	return NewPassthroughMapper(model)
}

// MapRequest injects the configured model into a JSON body, leaving everything
// else untouched. Non-JSON or empty bodies pass through unmodified.
func (m *PassthroughMapper) MapRequest(req *http.Request) (*http.Request, error) {
	if m.model == "" || req.Body == nil || req.Header.Get("Content-Type") != "application/json" {
		return req, nil
	}

	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		// Not a JSON object we can rewrite; forward the original bytes.
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		return req, nil
	}

	payload["model"] = m.model
	newBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	setJSONBody(req, newBody)
	return req, nil
}

// MapResponse forwards the response unchanged.
func (m *PassthroughMapper) MapResponse(resp *http.Response) (*http.Response, error) {
	return resp, nil
}

// setJSONBody replaces a request body with newBody and fixes Content-Length.
func setJSONBody(req *http.Request, newBody []byte) {
	req.Body = io.NopCloser(bytes.NewReader(newBody))
	req.ContentLength = int64(len(newBody))
	req.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
}
