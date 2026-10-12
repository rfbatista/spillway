package configsource

import (
	"context"
	"errors"
	"fmt"
	"os"

	"proxy/internal/proxy"
)

// EnvConfigSource loads backend configuration from the PROXY_CONFIG environment variable.
type EnvConfigSource struct{}

func NewEnvConfigSource() *EnvConfigSource {
	return &EnvConfigSource{}
}

func (s *EnvConfigSource) Load(_ context.Context) ([]proxy.BackendConfig, error) {
	raw := os.Getenv("PROXY_CONFIG")
	if raw == "" {
		return nil, errors.New("PROXY_CONFIG env var not set")
	}
	cfgs, err := parseBackendConfigs([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid PROXY_CONFIG: %w", err)
	}
	return cfgs, nil
}
