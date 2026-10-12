package proxy

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestBackendConfig_ParsesYAMLMapperFields(t *testing.T) {
	raw := `
- host: bedrock-runtime.us-east-1.amazonaws.com
  path: /model
  model: anthropic.claude-3-5-sonnet-20241022-v2:0
  mapper: bedrock
  aws_region: us-east-1
  aws_service: bedrock
`

	var cfgs []BackendConfig
	require.NoError(t, yaml.Unmarshal([]byte(raw), &cfgs))

	require.Len(t, cfgs, 1)
	assert.Equal(t, "bedrock", cfgs[0].Mapper)
	assert.Equal(t, "us-east-1", cfgs[0].AWSRegion)
	assert.Equal(t, "bedrock", cfgs[0].AWSService)
	assert.Equal(t, "anthropic.claude-3-5-sonnet-20241022-v2:0", cfgs[0].Model)
}

func TestBackendConfig_ParsesMapperFields(t *testing.T) {
	raw := `{
		"host": "bedrock-runtime.us-east-1.amazonaws.com",
		"path": "/model",
		"model": "anthropic.claude-3-5-sonnet-20241022-v2:0",
		"mapper": "bedrock",
		"aws_region": "us-east-1",
		"client_tag_header": "X-Client-Id"
	}`

	var cfg BackendConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &cfg))

	require.Equal(t, "bedrock", cfg.Mapper)
	require.Equal(t, "us-east-1", cfg.AWSRegion)
	require.Equal(t, "", cfg.AWSService) // omitted → empty
	require.Equal(t, "X-Client-Id", cfg.ClientTagHeader)
}

func TestGetScheme_Default(t *testing.T) {
	cfg := BackendConfig{
		Host: "example.com",
		Path: "/api",
	}

	got := cfg.GetScheme()
	expected := "https"
	if got != expected {
		t.Errorf("expected scheme %q, got %q", expected, got)
	}
}

func TestGetScheme_Custom(t *testing.T) {
	cfg := BackendConfig{
		Scheme: "http",
		Host:   "example.com",
		Path:   "/",
	}

	got := cfg.GetScheme()
	expected := "http"
	if got != expected {
		t.Errorf("expected scheme %q, got %q", expected, got)
	}
}

func TestGetURL_WithQueries(t *testing.T) {
	cfg := BackendConfig{
		Scheme: "http",
		Host:   "example.com",
		Path:   "/api",
		Queries: []BackendConfigQueries{
			{Key: "a", Value: "1"},
			{Key: "b", Value: "2"},
		},
	}
	u := cfg.GetURL()

	// Check scheme, host, and path.
	if u.Scheme != "http" {
		t.Errorf("expected scheme %q, got %q", "http", u.Scheme)
	}
	if u.Host != "example.com" {
		t.Errorf("expected host %q, got %q", "example.com", u.Host)
	}
	if u.Path != "/api" {
		t.Errorf("expected path %q, got %q", "/api", u.Path)
	}

	// Parse RawQuery and verify query parameters.
	qs, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		t.Fatalf("failed to parse RawQuery: %v", err)
	}
	if qs.Get("a") != "1" {
		t.Errorf("expected query parameter a=1, got a=%q", qs.Get("a"))
	}
	if qs.Get("b") != "2" {
		t.Errorf("expected query parameter b=2, got b=%q", qs.Get("b"))
	}
}

func TestGetURL_WithoutQueries(t *testing.T) {
	cfg := BackendConfig{
		Scheme: "https",
		Host:   "example.com",
		Path:   "/",
	}
	u := cfg.GetURL()
	if u.RawQuery != "" {
		t.Errorf("expected empty RawQuery, got %q", u.RawQuery)
	}
}
