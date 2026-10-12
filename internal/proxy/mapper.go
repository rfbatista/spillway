package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// RequestMapper translates between the canonical OpenAI Chat Completions wire
// format and a provider-specific format.
//
// MapRequest is called inside the reverse proxy Director, before the request
// reaches the upstream. It receives the outbound request (already pointing at
// the backend URL) and mutates it in place, returning the same pointer.
//
// MapResponse is called inside ModifyResponse, before the upstream response is
// forwarded to the client. Returning an error causes the proxy to send 502.
//
// Implementations must be safe for concurrent use.
type RequestMapper interface {
	MapRequest(req *http.Request) (*http.Request, error)
	MapResponse(resp *http.Response) (*http.Response, error)
}

// mapperOptions holds the optional, config-driven knobs shared by the mappers
// that inject max_tokens (anthropic, bedrock).
type mapperOptions struct {
	maxTokens int
}

// MapperOption configures optional mapper behaviour.
type MapperOption func(*mapperOptions)

// WithMaxTokens sets the max_tokens value used when the client omits it,
// replacing the built-in defaultMaxTokens fallback. A non-positive value is
// ignored so an unset config keeps the default. An explicit client max_tokens
// always wins over this fallback.
func WithMaxTokens(n int) MapperOption {
	return func(o *mapperOptions) {
		if n > 0 {
			o.maxTokens = n
		}
	}
}

// newMapperOptions resolves the options, seeding maxTokens with the default.
func newMapperOptions(opts []MapperOption) mapperOptions {
	o := mapperOptions{maxTokens: defaultMaxTokens}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Signer signs an outbound request for a provider that requires request signing
// (currently AWS Bedrock via SigV4). body is the exact request body that will be
// sent upstream.
type Signer interface {
	Sign(req *http.Request, body []byte, service, region string) error
}

// NewMapper resolves the RequestMapper for a backend from its config. Unknown
// mapper names and a "bedrock" mapper without aws_region are rejected so the
// pool build (and config reload) fails fast. When drop_request_fields is set,
// the resolved mapper is wrapped so those fields are stripped from the request
// body before provider mapping.
func NewMapper(cfg BackendConfig) (RequestMapper, error) {
	base, err := newBaseMapper(cfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.DropRequestFields) > 0 {
		return NewDropFieldsMapper(cfg.DropRequestFields, base), nil
	}
	return base, nil
}

// newBaseMapper resolves the provider-specific mapper for a backend.
func newBaseMapper(cfg BackendConfig) (RequestMapper, error) {
	switch cfg.Mapper {
	case "", "passthrough":
		return NewPassthroughMapper(cfg.Model), nil
	case "grok":
		return NewGrokMapper(cfg.Model), nil
	case "openai":
		return NewOpenAIMapper(cfg.Model), nil
	case "anthropic":
		return NewAnthropicMapper(cfg.Model, WithMaxTokens(cfg.MaxTokens)), nil
	case "bedrock":
		// A Bedrock API key authenticates via a bearer token, so SigV4
		// credentials and a region are not needed.
		if cfg.AWSAPIKey != "" {
			return NewBedrockMapper(cfg.Path, cfg.Model, cfg.AWSRegion, cfg.AWSService, NewBearerSigner(cfg.AWSAPIKey), WithMaxTokens(cfg.MaxTokens)), nil
		}
		if cfg.AWSRegion == "" {
			return nil, fmt.Errorf("bedrock mapper requires aws_region or aws_api_key")
		}
		signer, err := NewAWSSigner(context.Background())
		if err != nil {
			return nil, fmt.Errorf("bedrock mapper: %w", err)
		}
		return NewBedrockMapper(cfg.Path, cfg.Model, cfg.AWSRegion, cfg.AWSService, signer, WithMaxTokens(cfg.MaxTokens)), nil
	default:
		return nil, fmt.Errorf("unknown mapper %q", cfg.Mapper)
	}
}

// setResponseJSONBody replaces a response body with newBody and fixes the
// Content-Length / Content-Type headers.
func setResponseJSONBody(resp *http.Response, newBody []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(newBody))
	resp.ContentLength = int64(len(newBody))
	resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Del("Content-Encoding")
}
