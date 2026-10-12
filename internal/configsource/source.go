package configsource

import (
	"context"
	"errors"

	"proxy/internal/proxy"
)

// ErrNoChange is returned by ConfigSource.Load when configuration has not changed since the last call.
var ErrNoChange = errors.New("config unchanged")

// ConfigSource loads backend configurations from a source.
type ConfigSource interface {
	Load(ctx context.Context) ([]proxy.BackendConfig, error)
}
