package proxy

import "net/url"

type BackendConfigHeaders struct {
	Key   string `yaml:"key,omitempty" json:"key,omitempty"`
	Value string `yaml:"value,omitempty" json:"value,omitempty"`
}

type BackendConfigQueries struct {
	Key   string `yaml:"key,omitempty" json:"key,omitempty"`
	Value string `yaml:"value,omitempty" json:"value,omitempty"`
}

type BackendConfig struct {
	Name       string `yaml:"name,omitempty" json:"name,omitempty"` // optional human-readable identifier; when set it becomes the backend ID (logs, quota, queues), replacing the derived model@host form
	Host       string `yaml:"host,omitempty" json:"host,omitempty"`
	Path       string `yaml:"path,omitempty" json:"path,omitempty"`
	Scheme     string `yaml:"scheme,omitempty" json:"scheme,omitempty"`
	Model      string `yaml:"model,omitempty" json:"model,omitempty"`
	Alias      string `yaml:"alias,omitempty" json:"alias,omitempty"`             // routing alias matched against the X-Request-Model header
	Mapper     string `yaml:"mapper,omitempty" json:"mapper,omitempty"`           // "" | "passthrough" | "grok" | "openai" | "anthropic" | "bedrock"
	AWSRegion  string `yaml:"aws_region,omitempty" json:"aws_region,omitempty"`   // required for "bedrock" SigV4 auth
	AWSService string `yaml:"aws_service,omitempty" json:"aws_service,omitempty"` // defaults to "bedrock"
	AWSAPIKey  string `yaml:"aws_api_key,omitempty" json:"aws_api_key,omitempty"` // Bedrock API key; when set, bearer auth is used instead of SigV4

	// MaxTokens sets the max_tokens injected when the client omits it, replacing
	// the built-in 4096 fallback. Only the "anthropic" and "bedrock" mappers
	// inject max_tokens, so it has no effect on the others. An explicit client
	// max_tokens always wins.
	MaxTokens int `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`

	// DropRequestFields lists JSON fields stripped from the request body before
	// mapping. Each entry is a dot-separated path: "temperature" for a top-level
	// key, "inferenceConfig.temperature" for a nested one (e.g. a native Converse
	// body). Applies to any mapper; lets config adapt a backend to a model that
	// rejects a parameter without client changes.
	DropRequestFields []string `yaml:"drop_request_fields,omitempty" json:"drop_request_fields,omitempty"`

	// LogDropRequestFields and LogCropRequestFields shape the request body as it
	// is written to the logs, and only there: the body forwarded upstream is
	// untouched, and the dashboard still receives the full body over SSE.
	// Dropped paths are removed outright; cropped paths keep their first
	// LogCropLimit runes. Each entry is a dot-separated path that traverses
	// arrays implicitly, so "messages.content.text" reaches every
	// messages[].content[].text. Paths are in the upstream shape, because the
	// body is captured inside the transport, after mapping.
	LogDropRequestFields []string `yaml:"log_drop_request_fields,omitempty" json:"log_drop_request_fields,omitempty"`
	LogCropRequestFields []string `yaml:"log_crop_request_fields,omitempty" json:"log_crop_request_fields,omitempty"`

	// LogDropResponseFields and LogCropResponseFields do the same for the
	// response body. Their paths are in the provider's shape, because the body is
	// captured before the mapper translates it back to OpenAI form. Dropping a
	// field never hides token usage: usage is extracted from the full body.
	LogDropResponseFields []string `yaml:"log_drop_response_fields,omitempty" json:"log_drop_response_fields,omitempty"`
	LogCropResponseFields []string `yaml:"log_crop_response_fields,omitempty" json:"log_crop_response_fields,omitempty"`

	// LogCropLimit is the number of runes kept from a cropped string. It defaults
	// to 256 when crop paths are configured without it.
	LogCropLimit int `yaml:"log_crop_limit,omitempty" json:"log_crop_limit,omitempty"`

	// ClientTagHeader names a client request header whose value is logged as the
	// client_tag field on the request trace, so retries and failures can be
	// grouped per caller in Loki/Grafana. Point it at a bounded-cardinality
	// identifier (caller/tenant/document id) — never a per-request UUID, or every
	// value becomes its own alert instance. Empty disables the field.
	ClientTagHeader string `yaml:"client_tag_header,omitempty" json:"client_tag_header,omitempty"`

	Headers []BackendConfigHeaders `yaml:"headers,omitempty" json:"headers,omitempty"`
	Queries []BackendConfigQueries `yaml:"queries,omitempty" json:"queries,omitempty"`
}

func (b BackendConfig) GetURL() url.URL {
	q := url.Values{}
	for _, d := range b.Queries {
		q.Add(d.Key, d.Value)
	}
	targetURL := url.URL{
		Scheme:   b.GetScheme(),
		Host:     b.Host,
		Path:     b.Path,
		RawQuery: q.Encode(),
	}
	return targetURL
}

func (b BackendConfig) GetScheme() string {
	if b.Scheme == "" {
		return "https"
	}
	return b.Scheme
}
