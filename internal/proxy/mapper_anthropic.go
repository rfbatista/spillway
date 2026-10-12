package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"proxy/internal/apperr"
)

const defaultMaxTokens = 4096

// AnthropicMapper translates OpenAI Chat Completions <-> Anthropic Messages API.
type AnthropicMapper struct {
	model     string
	maxTokens int
}

// NewAnthropicMapper returns an AnthropicMapper that injects model. Options tune
// the fallback max_tokens (see WithMaxTokens); by default it is defaultMaxTokens.
func NewAnthropicMapper(model string, opts ...MapperOption) *AnthropicMapper {
	return &AnthropicMapper{model: model, maxTokens: newMapperOptions(opts).maxTokens}
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// Thinking carries the model's reasoning on a "thinking" block, where Text
	// is empty.
	Thinking string `json:"thinking,omitempty"`
}

type anthropicMessage struct {
	Role    string               `json:"role"`
	Content []anthropicTextBlock `json:"content"`
}

// anthropicThinking is the Messages API extended-thinking config. BudgetTokens
// is omitted when zero so a caller-supplied {"type": "disabled"} round-trips
// without gaining a budget_tokens field the disabled variant does not accept.
type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type openAIRequest struct {
	Messages        []openAIMessage `json:"messages"`
	MaxTokens       *int            `json:"max_tokens"`
	Temperature     *float64        `json:"temperature"`
	Stream          *bool           `json:"stream"`
	ReasoningEffort string          `json:"reasoning_effort"`
	// Thinking lets a caller that already speaks the Messages API configure
	// extended thinking directly; it is forwarded untouched and takes
	// precedence over reasoning_effort.
	Thinking *anthropicThinking `json:"thinking"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float64           `json:"temperature,omitempty"`
	Stream      *bool              `json:"stream,omitempty"`
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
}

// MapRequest converts an OpenAI request body into an Anthropic Messages body.
func (m *AnthropicMapper) MapRequest(req *http.Request) (*http.Request, error) {
	if req.Body == nil {
		return req, nil
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}

	var in openAIRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("%w: parsing request body: %v", apperr.ErrInvalidRequest, err)
	}

	out := anthropicRequest{
		Model:       m.model,
		MaxTokens:   m.maxTokens,
		Temperature: in.Temperature,
		Stream:      in.Stream,
	}
	if in.MaxTokens != nil {
		out.MaxTokens = *in.MaxTokens
	}

	switch {
	case in.Thinking != nil:
		out.Thinking = in.Thinking
	default:
		budget, thinking, err := reasoningBudget(in.ReasoningEffort, out.MaxTokens)
		if err != nil {
			return nil, err
		}
		if thinking {
			out.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
		}
	}
	if out.Thinking != nil && out.Thinking.Type == "enabled" {
		// Anthropic rejects sampling parameters while thinking is enabled; a
		// disabled (or unrecognised) thinking config leaves them alone.
		out.Temperature = nil
	}

	for _, msg := range in.Messages {
		if msg.Role == "system" {
			if out.System != "" {
				out.System += "\n"
			}
			out.System += msg.Content
			continue
		}
		out.Messages = append(out.Messages, anthropicMessage{
			Role:    msg.Role,
			Content: []anthropicTextBlock{{Type: "text", Text: msg.Content}},
		})
	}

	newBody, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	setJSONBody(req, newBody)
	return req, nil
}

type anthropicResponseBody struct {
	ID         string               `json:"id"`
	Model      string               `json:"model"`
	StopReason string               `json:"stop_reason"`
	Content    []anthropicTextBlock `json:"content"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// MapResponse converts an Anthropic Messages response into OpenAI shape.
// Streaming responses (Content-Type text/event-stream) are handled by
// mapStreamingResponse.
func (m *AnthropicMapper) MapResponse(resp *http.Response) (*http.Response, error) {
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return m.mapStreamingResponse(resp)
	}
	if resp.Body == nil {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}

	var in anthropicResponseBody
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}

	// With thinking enabled the answer's text block is preceded by a thinking
	// block, so the blocks are collected by kind rather than by position.
	var text, reasoning strings.Builder
	for _, block := range in.Content {
		if block.Type == "thinking" {
			reasoning.WriteString(block.Thinking)
			continue
		}
		text.WriteString(block.Text)
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	out := map[string]any{
		"id":     "chatcmpl-" + in.ID,
		"object": "chat.completion",
		"model":  in.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": mapAnthropicStopReason(in.StopReason),
		}},
		"usage": map[string]any{
			"prompt_tokens":     in.Usage.InputTokens,
			"completion_tokens": in.Usage.OutputTokens,
			"total_tokens":      in.Usage.InputTokens + in.Usage.OutputTokens,
		},
	}

	newBody, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	setResponseJSONBody(resp, newBody)
	return resp, nil
}

// mapAnthropicStopReason maps Anthropic stop_reason to OpenAI finish_reason.
// Only "end_turn" is remapped to "stop"; other values pass through.
func mapAnthropicStopReason(reason string) string {
	if reason == "end_turn" {
		return "stop"
	}
	return reason
}

type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// Thinking carries the reasoning text of a thinking_delta, where Text
		// is empty — the streaming twin of anthropicTextBlock.Thinking.
		Thinking   string `json:"thinking"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
}

// mapStreamingResponse wraps the upstream SSE body with a reader that rewrites
// each Anthropic event into an OpenAI chat.completion.chunk event. The output
// is streamed lazily via io.Pipe so the proxy's FlushInterval=-1 forwards chunks
// to the client as they arrive.
func (m *AnthropicMapper) mapStreamingResponse(resp *http.Response) (*http.Response, error) {
	pr, pw := io.Pipe()
	upstream := resp.Body
	model := m.model

	go func() {
		defer pw.Close()
		defer upstream.Close()

		scanner := bufio.NewScanner(upstream)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue // skip "event:" lines and blanks; we re-emit our own framing
			}
			if data == "[DONE]" {
				_, _ = io.WriteString(pw, "data: [DONE]\n\n")
				continue
			}

			var ev anthropicStreamEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue // ignore unparseable events
			}

			var chunk map[string]any
			switch ev.Type {
			case "content_block_delta":
				// With thinking enabled the answer's text_delta events are
				// preceded by thinking_delta events, forwarded as
				// reasoning_content just like the non-streaming path.
				switch {
				case ev.Delta.Thinking != "":
					chunk = openAIChunk(model, map[string]any{"reasoning_content": ev.Delta.Thinking}, nil)
				case ev.Delta.Text != "":
					chunk = openAIChunk(model, map[string]any{"content": ev.Delta.Text}, nil)
				default:
					continue // signature_delta and other non-text deltas
				}
			case "message_delta":
				if ev.Delta.StopReason == "" {
					continue
				}
				fr := mapAnthropicStopReason(ev.Delta.StopReason)
				chunk = openAIChunk(model, map[string]any{}, &fr)
			default:
				continue // message_start, content_block_start/stop, ping, etc.
			}

			payload, err := json.Marshal(chunk)
			if err != nil {
				continue
			}
			if _, err := io.WriteString(pw, "data: "+string(payload)+"\n\n"); err != nil {
				return
			}
		}
	}()

	resp.Body = pr
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	resp.Header.Set("Content-Type", "text/event-stream")
	return resp, nil
}

// openAIChunk builds one chat.completion.chunk object. finishReason is nil for
// content deltas and set for the terminal stop event.
func openAIChunk(model string, delta map[string]any, finishReason *string) map[string]any {
	choice := map[string]any{"index": 0, "delta": delta}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	} else {
		choice["finish_reason"] = nil
	}
	return map[string]any{
		"object":  "chat.completion.chunk",
		"model":   model,
		"choices": []any{choice},
	}
}
