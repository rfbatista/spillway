package configsource

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBackendConfigs(t *testing.T) {
	t.Run("valid JSON", func(t *testing.T) {
		cfgs, err := parseBackendConfigs([]byte(`{"backends":[{"host":"api.openai.com","model":"gpt-4o","mapper":"passthrough"}]}`))
		require.NoError(t, err)
		require.Len(t, cfgs, 1)
		assert.Equal(t, "api.openai.com", cfgs[0].Host)
		assert.Equal(t, "passthrough", cfgs[0].Mapper)
	})

	t.Run("valid YAML with mapper fields", func(t *testing.T) {
		raw := `
backends:
  - host: bedrock-runtime.us-east-1.amazonaws.com
    path: /model
    model: anthropic.claude-3-5-sonnet-20241022-v2:0
    mapper: bedrock
    aws_region: us-east-1
    aws_service: bedrock
`
		cfgs, err := parseBackendConfigs([]byte(raw))
		require.NoError(t, err)
		require.Len(t, cfgs, 1)
		assert.Equal(t, "bedrock", cfgs[0].Mapper)
		assert.Equal(t, "us-east-1", cfgs[0].AWSRegion)
		assert.Equal(t, "bedrock", cfgs[0].AWSService)
	})

	t.Run("JSON is also valid YAML, parsed by JSON path", func(t *testing.T) {
		cfgs, err := parseBackendConfigs([]byte(`{"backends":[{"host":"h","model":"m"}]}`))
		require.NoError(t, err)
		require.Len(t, cfgs, 1)
		assert.Equal(t, "h", cfgs[0].Host)
	})

	t.Run("empty backends list", func(t *testing.T) {
		cfgs, err := parseBackendConfigs([]byte(`{"backends":[]}`))
		require.NoError(t, err)
		assert.Empty(t, cfgs)
	})

	t.Run("neither valid JSON nor YAML", func(t *testing.T) {
		// "not-json": JSON rejects it; YAML parses it as a scalar string which
		// cannot unmarshal into a slice, so the YAML attempt also errors.
		_, err := parseBackendConfigs([]byte("not-json"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "JSON")
		assert.Contains(t, err.Error(), "YAML")
	})
}
