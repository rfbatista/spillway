package configsource

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"proxy/internal/proxy"
)

// AppConfigAgentSource loads backend configuration from the AWS AppConfig Agent
// HTTP API at localhost:2772.
//
// The agent returns HTTP 200 with the full configuration document on every poll
// — it does not signal "unchanged" via 204. The 204 branch below is kept because
// it is correct if it ever fires, but it must not be relied on: detecting an
// unchanged configuration is the Watcher's job (see Watcher.poll).
type AppConfigAgentSource struct {
	url    string
	client *http.Client
	logger *slog.Logger
}

func NewAppConfigAgentSource(appID, envID, profileID string) *AppConfigAgentSource {
	return NewAppConfigAgentSourceWithURL(
		fmt.Sprintf("http://localhost:2772/applications/%s/environments/%s/configurations/%s", appID, envID, profileID),
	)
}

// NewAppConfigAgentSourceWithURL is the test constructor — allows injecting a custom URL.
func NewAppConfigAgentSourceWithURL(url string) *AppConfigAgentSource {
	return &AppConfigAgentSource{
		url:    url,
		client: &http.Client{Timeout: 5 * time.Second},
		logger: slog.With(slog.String("component", "AppConfigAgentSource")),
	}
}

func (s *AppConfigAgentSource) Load(ctx context.Context) ([]proxy.BackendConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling AppConfig agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, ErrNoChange
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AppConfig agent returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	cfgs, err := parseBackendConfigs(body)
	if err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return cfgs, nil
}
