package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/rfbatista/spillway/internal/apperr"
)

// BedrockMapper translates OpenAI Chat Completions <-> AWS Bedrock Converse API.
// The model ID is encoded in the URL path, not the body, and every request must
// be SigV4-signed.
type BedrockMapper struct {
	pathPrefix string // e.g. "/model"
	model      string
	region     string
	service    string
	signer     Signer
	maxTokens  int
}

// NewBedrockMapper builds a BedrockMapper. service defaults to "bedrock". Options
// tune the fallback max_tokens (see WithMaxTokens); by default it is
// defaultMaxTokens.
func NewBedrockMapper(pathPrefix, model, region, service string, signer Signer, opts ...MapperOption) *BedrockMapper {
	if service == "" {
		service = "bedrock"
	}
	return &BedrockMapper{
		pathPrefix: pathPrefix,
		model:      model,
		region:     region,
		service:    service,
		signer:     signer,
		maxTokens:  newMapperOptions(opts).maxTokens,
	}
}

type bedrockTextBlock struct {
	Text string `json:"text"`
}

type bedrockMessage struct {
	Role    string             `json:"role"`
	Content []bedrockTextBlock `json:"content"`
}

type bedrockInferenceConfig struct {
	MaxTokens   int      `json:"maxTokens"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
}

type bedrockRequest struct {
	Messages        []bedrockMessage       `json:"messages"`
	System          []bedrockTextBlock     `json:"system,omitempty"`
	InferenceConfig bedrockInferenceConfig `json:"inferenceConfig"`
	// AdditionalModelRequestFields carries model-specific parameters Converse
	// does not model itself, such as Anthropic's reasoning_config.
	AdditionalModelRequestFields map[string]any `json:"additionalModelRequestFields,omitempty"`
}

type openAIBedrockInput struct {
	Messages        []openAIMessage `json:"messages"`
	MaxTokens       *int            `json:"max_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Stream          *bool           `json:"stream"`
	ReasoningEffort string          `json:"reasoning_effort"`
	// AdditionalModelRequestFields lets a caller reach model-specific Converse
	// parameters directly; it is forwarded as-is. A reasoning_config inside it
	// takes precedence over reasoning_effort; unrelated fields coexist with
	// the reasoning_effort translation.
	AdditionalModelRequestFields map[string]any `json:"additionalModelRequestFields"`
}

// converseCtxKey marks a request that already arrived in Converse shape, so
// MapResponse knows to hand the provider's answer back unchanged.
type converseCtxKey struct{}

// isConverseRequest reports whether body is already an AWS Converse payload
// rather than an OpenAI Chat Completions one. Converse keeps its sampling
// parameters under inferenceConfig and its message content as a list of
// type-less blocks ({"text": ...}). OpenAI multimodal content is a list too,
// but every part carries a "type" discriminator ({"type": "text", ...}), so
// the block shape — not the mere presence of a list — is the signal.
// additionalModelRequestFields alone is not a signal either — an OpenAI caller
// may set it to reach a model-specific parameter and still expect an OpenAI
// answer back.
func isConverseRequest(body []byte) bool {
	var probe struct {
		InferenceConfig json.RawMessage `json:"inferenceConfig"`
		Messages        []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	if len(probe.InferenceConfig) > 0 {
		return true
	}
	for _, msg := range probe.Messages {
		if len(msg.Content) == 0 || msg.Content[0] != '[' {
			continue
		}
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(msg.Content, &blocks); err != nil || len(blocks) == 0 {
			return false
		}
		_, typed := blocks[0]["type"]
		return !typed
	}
	return false
}

// converseAction picks the Converse action from the path the caller used, so a
// client asking to stream keeps streaming. The OpenAI path reads the intent from
// the body's "stream" field instead, which a Converse body does not carry.
func converseAction(path string) string {
	if strings.HasSuffix(path, "/converse-stream") {
		return "converse-stream"
	}
	return "converse"
}

// setModelPath points the request at the backend's own model. Bedrock model IDs
// contain ':' (e.g. "...-v2:0"), which url.PathEscape leaves raw since it is a
// legal path-segment char. Bedrock and the SigV4 canonical URI expect it
// percent-encoded, so force ':' -> '%3A'. Path holds the decoded form and RawPath
// the encoded form so EscapedPath() (used for signing and forwarding) stays
// consistent without double-encoding.
func (m *BedrockMapper) setModelPath(req *http.Request, action string) {
	escapedModel := strings.ReplaceAll(url.PathEscape(m.model), ":", "%3A")
	req.URL.Path = fmt.Sprintf("%s/%s/%s", m.pathPrefix, m.model, action)
	req.URL.RawPath = fmt.Sprintf("%s/%s/%s", m.pathPrefix, escapedModel, action)
}

// forwardConverse sends an already-Converse body upstream untouched. The caller
// speaks the provider's own API, so there is nothing to translate and anything
// this mapper did to the body could only take fields away. Only the path, which
// carries the backend's model ID, and the signature are ours to set.
func (m *BedrockMapper) forwardConverse(req *http.Request, body []byte) (*http.Request, error) {
	*req = *req.WithContext(context.WithValue(req.Context(), converseCtxKey{}, true))
	m.setModelPath(req, converseAction(req.URL.Path))
	setJSONBody(req, body)
	if err := m.signer.Sign(req, body, m.service, m.region); err != nil {
		return nil, fmt.Errorf("bedrock sigv4 signing: %w", err)
	}
	return req, nil
}

// MapRequest rewrites the path with the model ID, transforms the body into the
// Converse shape, and signs the request. A body that is already in Converse
// shape is forwarded as-is.
func (m *BedrockMapper) MapRequest(req *http.Request) (*http.Request, error) {
	if req.Body == nil {
		return req, nil
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}

	if isConverseRequest(body) {
		return m.forwardConverse(req, body)
	}

	var in openAIBedrockInput
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("%w: parsing request body: %v", apperr.ErrInvalidRequest, err)
	}

	out := bedrockRequest{
		InferenceConfig: bedrockInferenceConfig{
			MaxTokens:   m.maxTokens,
			Temperature: in.Temperature,
			TopP:        in.TopP,
		},
	}
	if in.MaxTokens != nil {
		out.InferenceConfig.MaxTokens = *in.MaxTokens
	}

	out.AdditionalModelRequestFields = in.AdditionalModelRequestFields
	thinking := reasoningConfigEnabled(in.AdditionalModelRequestFields)
	if _, native := in.AdditionalModelRequestFields["reasoning_config"]; !native {
		budget, enabled, err := reasoningBudget(in.ReasoningEffort, out.InferenceConfig.MaxTokens)
		if err != nil {
			return nil, err
		}
		if enabled {
			if out.AdditionalModelRequestFields == nil {
				out.AdditionalModelRequestFields = map[string]any{}
			}
			out.AdditionalModelRequestFields["reasoning_config"] = map[string]any{
				"type": "enabled", "budget_tokens": budget,
			}
			thinking = true
		}
	}
	if thinking {
		// Anthropic rejects sampling parameters while thinking is enabled.
		out.InferenceConfig.Temperature = nil
		out.InferenceConfig.TopP = nil
	}

	for _, msg := range in.Messages {
		if msg.Role == "system" {
			out.System = append(out.System, bedrockTextBlock{Text: msg.Content})
			continue
		}
		out.Messages = append(out.Messages, bedrockMessage{
			Role:    msg.Role,
			Content: []bedrockTextBlock{{Text: msg.Content}},
		})
	}

	stream := in.Stream != nil && *in.Stream
	action := "converse"
	if stream {
		action = "converse-stream"
	}
	m.setModelPath(req, action)

	newBody, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	setJSONBody(req, newBody)

	if err := m.signer.Sign(req, newBody, m.service, m.region); err != nil {
		return nil, fmt.Errorf("bedrock sigv4 signing: %w", err)
	}
	return req, nil
}

// reasoningConfigEnabled reports whether a caller-supplied
// additionalModelRequestFields carries a reasoning_config that turns thinking
// on. An absent, non-object, or non-"enabled" config counts as off; the
// provider validates the field itself either way.
func reasoningConfigEnabled(fields map[string]any) bool {
	rc, ok := fields["reasoning_config"].(map[string]any)
	if !ok {
		return false
	}
	kind, _ := rc["type"].(string)
	return kind == "enabled"
}

// bedrockContentBlock is one block of a Converse assistant message. With
// reasoning enabled the model emits its thinking as a reasoningContent block
// ahead of the answer's text block, so a block carries one or the other.
type bedrockContentBlock struct {
	Text             string `json:"text"`
	ReasoningContent *struct {
		ReasoningText struct {
			Text string `json:"text"`
		} `json:"reasoningText"`
	} `json:"reasoningContent"`
}

type bedrockResponseBody struct {
	Output struct {
		Message struct {
			Role    string                `json:"role"`
			Content []bedrockContentBlock `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
	} `json:"usage"`
}

// MapResponse converts a Bedrock Converse response into OpenAI shape. A caller
// that spoke Converse on the way in expects Converse back, so its answer is left
// alone. Streaming (converse-stream) is out of scope this iteration; such
// responses are not transformed here.
func (m *BedrockMapper) MapResponse(resp *http.Response) (*http.Response, error) {
	if isConverseCaller(resp.Request) {
		return resp, nil
	}
	if resp.Body == nil {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}

	var in bedrockResponseBody
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}

	var text, reasoning strings.Builder
	for _, block := range in.Output.Message.Content {
		if block.ReasoningContent != nil {
			reasoning.WriteString(block.ReasoningContent.ReasoningText.Text)
			continue
		}
		text.WriteString(block.Text)
	}
	role := in.Output.Message.Role
	if role == "" {
		role = "assistant"
	}
	message := map[string]any{"role": role, "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	out := map[string]any{
		"object": "chat.completion",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": mapBedrockStopReason(in.StopReason),
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

// isConverseCaller reports whether req is the outbound request MapRequest
// forwarded verbatim. req is the request the transport sent, which net/http
// attaches to the response; a nil one means we cannot tell, so the answer is
// mapped as it always was.
func isConverseCaller(req *http.Request) bool {
	if req == nil {
		return false
	}
	native, _ := req.Context().Value(converseCtxKey{}).(bool)
	return native
}

// mapBedrockStopReason maps Bedrock stopReason to OpenAI finish_reason.
// "end_turn" -> "stop"; "max_tokens" and others pass through unchanged.
func mapBedrockStopReason(reason string) string {
	if reason == "end_turn" {
		return "stop"
	}
	return reason
}

// NoopSigner satisfies Signer without signing; used in tests.
type NoopSigner struct{}

// Sign is a no-op.
func (NoopSigner) Sign(req *http.Request, body []byte, service, region string) error {
	return nil
}

// BearerSigner authenticates Bedrock requests with a long-term Bedrock API key
// via the "Authorization: Bearer" header instead of SigV4. service and region
// are ignored.
type BearerSigner struct {
	key string
}

// NewBearerSigner returns a Signer that sets a static bearer token.
func NewBearerSigner(key string) BearerSigner {
	return BearerSigner{key: key}
}

// Sign sets the Authorization header to the bearer API key, replacing any value
// injected earlier in the request pipeline.
func (s BearerSigner) Sign(req *http.Request, body []byte, service, region string) error {
	req.Header.Set("Authorization", "Bearer "+s.key)
	return nil
}

// AWSSigner signs requests with AWS Signature V4 using credentials from the
// standard SDK chain (env vars -> ~/.aws/credentials -> instance role).
type AWSSigner struct {
	creds  aws.CredentialsProvider
	signer *v4.Signer
}

// NewAWSSigner loads the default AWS config and returns a SigV4 signer.
func NewAWSSigner(ctx context.Context) (*AWSSigner, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &AWSSigner{creds: cfg.Credentials, signer: v4.NewSigner()}, nil
}

// Sign computes the SigV4 signature over body and sets the auth headers on req.
func (s *AWSSigner) Sign(req *http.Request, body []byte, service, region string) error {
	creds, err := s.creds.Retrieve(req.Context())
	if err != nil {
		return fmt.Errorf("retrieve aws credentials: %w", err)
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	return s.signer.SignHTTP(req.Context(), creds, req, payloadHash, service, region, time.Now())
}
