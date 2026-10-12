package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMapper_PassthroughAndGrok(t *testing.T) {
	tests := []struct {
		name   string
		mapper string
	}{
		{name: "empty defaults to passthrough", mapper: ""},
		{name: "explicit passthrough", mapper: "passthrough"},
		{name: "grok", mapper: "grok"},
		{name: "openai", mapper: "openai"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewMapper(BackendConfig{Model: "m", Mapper: tt.mapper})
			require.NoError(t, err)
			require.NotNil(t, m)
			_, ok := m.(*PassthroughMapper)
			assert.True(t, ok, "expected *PassthroughMapper")
		})
	}
}

func TestNewMapper_UnknownIsRejected(t *testing.T) {
	_, err := NewMapper(BackendConfig{Mapper: "gemini"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown mapper "gemini"`)
}

func TestNewMapper_BedrockRequiresRegion(t *testing.T) {
	_, err := NewMapper(BackendConfig{Mapper: "bedrock", Model: "m"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bedrock mapper requires aws_region")
}

func TestNewMapper_BedrockAPIKeyUsesBearerSigner(t *testing.T) {
	m, err := NewMapper(BackendConfig{Mapper: "bedrock", Model: "m", Path: "/model", AWSAPIKey: "secret"})
	require.NoError(t, err)
	require.NotNil(t, m)

	bm, ok := m.(*BedrockMapper)
	require.True(t, ok, "expected *BedrockMapper")
	_, ok = bm.signer.(BearerSigner)
	assert.True(t, ok, "aws_api_key must select BearerSigner, not SigV4")
}
