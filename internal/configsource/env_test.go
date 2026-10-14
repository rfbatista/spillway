package configsource_test

import (
	"context"
	"testing"

	"github.com/rfbatista/spillway/internal/configsource"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvConfigSource_Load_Success(t *testing.T) {
	t.Setenv("PROXY_CONFIG", `{"backends":[{"host":"api.openai.com","path":"/v1","scheme":"https","model":"gpt-4o"}]}`)
	src := configsource.NewEnvConfigSource()
	cfgs, err := src.Load(context.Background())
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	assert.Equal(t, "api.openai.com", cfgs[0].Host)
	assert.Equal(t, "gpt-4o", cfgs[0].Model)
}

func TestEnvConfigSource_Load_Missing(t *testing.T) {
	t.Setenv("PROXY_CONFIG", "")
	src := configsource.NewEnvConfigSource()
	_, err := src.Load(context.Background())
	assert.ErrorContains(t, err, "PROXY_CONFIG")
}

func TestEnvConfigSource_Load_Invalid(t *testing.T) {
	t.Setenv("PROXY_CONFIG", "not-json")
	src := configsource.NewEnvConfigSource()
	_, err := src.Load(context.Background())
	assert.ErrorContains(t, err, "invalid PROXY_CONFIG")
}

func TestEnvConfigSource_Load_YAML(t *testing.T) {
	t.Setenv("PROXY_CONFIG", "backends:\n  - host: api.openai.com\n    model: gpt-4o\n    mapper: passthrough\n")
	src := configsource.NewEnvConfigSource()
	cfgs, err := src.Load(context.Background())
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	assert.Equal(t, "api.openai.com", cfgs[0].Host)
	assert.Equal(t, "passthrough", cfgs[0].Mapper)
}
