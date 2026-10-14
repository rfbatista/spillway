package configsource

import (
	"encoding/json"
	"fmt"

	"github.com/rfbatista/spillway/internal/proxy"

	"gopkg.in/yaml.v3"
)

type AppConfig struct {
	Backends []proxy.BackendConfig `yaml:"backends,omitempty" json:"backends"`
}

// parseBackendConfigs decodes a backend configuration document that may be
// either JSON or YAML. The document is an AppConfig object whose "backends" key
// holds the backend list. JSON is attempted first so JSON input yields precise
// JSON errors; on failure the input is parsed as YAML. If neither succeeds the
// returned error wraps both failures. Each attempt decodes into a fresh
// AppConfig so a failed JSON decode cannot leak partial state into the YAML
// result.
func parseBackendConfigs(raw []byte) ([]proxy.BackendConfig, error) {
	var fromJSON AppConfig
	jsonErr := json.Unmarshal(raw, &fromJSON)
	if jsonErr == nil {
		return fromJSON.Backends, nil
	}

	var fromYAML AppConfig
	yamlErr := yaml.Unmarshal(raw, &fromYAML)
	if yamlErr == nil {
		return fromYAML.Backends, nil
	}

	return nil, fmt.Errorf("config is neither valid JSON (%v) nor YAML (%w)", jsonErr, yamlErr)
}
