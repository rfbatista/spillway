package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/rfbatista/spillway/internal/domain"
)

type DummyBackendConfigHeaders struct {
	Key   string
	Value string
}

type dummyBackendConfig struct {
	Headers []DummyBackendConfigHeaders
	url     url.URL
}

func (d dummyBackendConfig) GetURL() url.URL {
	return d.url
}

type fakeGetUsage struct{}

func (f fakeGetUsage) Execute(ctx context.Context, id string) (*domain.ModelUsageQuota, error) {
	return &domain.ModelUsageQuota{
		RemainingTokens:   100,
		RemainingRequests: 100,
	}, nil
}

func convertHeaders(dummies []DummyBackendConfigHeaders) []BackendConfigHeaders {
	headers := make([]BackendConfigHeaders, len(dummies))
	for i, d := range dummies {
		headers[i] = BackendConfigHeaders{Key: d.Key, Value: d.Value}
	}
	return headers
}

func TestInjectHeaders(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com", nil)
	req.Header.Set("Host", "example.com")

	headers := []DummyBackendConfigHeaders{
		{Key: "X-Custom-1", Value: "value1"},
		{Key: "X-Custom-2", Value: "value2"},
	}

	injectHeaders(req, convertHeaders(headers))

	if got := req.Header.Get("X-Forwarded-Host"); got != "example.com" {
		t.Errorf("expected X-Forwarded-Host to be 'example.com', got %q", got)
	}
	if got := req.Header.Get("X-Custom-1"); got != "value1" {
		t.Errorf("expected X-Custom-1 to be 'value1', got %q", got)
	}
	if got := req.Header.Get("X-Custom-2"); got != "value2" {
		t.Errorf("expected X-Custom-2 to be 'value2', got %q", got)
	}
}

type dummyHandler struct {
	captured *http.Request
}

func (d *dummyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.captured = r
	w.WriteHeader(200)
}

func convertConfig(d dummyBackendConfig) BackendConfig {
	return BackendConfig{
		Headers: convertHeaders(d.Headers),
	}
}

func TestGetRemoteIP(t *testing.T) {
	tests := []struct {
		name      string
		remote    string
		want      string
		wantError bool
	}{
		{"ipv4", "127.0.0.1:8080", "127.0.0.1", false},
		{"ipv6 bracketed", "[::1]:8080", "::1", false},
		{"ipv6 bracketed full", "[2001:db8::1]:443", "2001:db8::1", false},
		{"empty", "", "", true},
		{"invalid", "not-an-addr", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getRemoteIP(tt.remote)
			if tt.wantError {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("getRemoteIP: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBackend_ServeHTTP(t *testing.T) {
	cfg := dummyBackendConfig{
		Headers: []DummyBackendConfigHeaders{
			{Key: "X-Test", Value: "TestValue"},
		},
		url: url.URL{Scheme: "http", Host: "backend-host"},
	}

	dummyProxy := &dummyHandler{}

	b := &Backend{
		ID:       "test-backend",
		proxy:    dummyProxy,
		URL:      cfg.GetURL(),
		cfg:      convertConfig(cfg),
		getusage: fakeGetUsage{},
		logger:   slog.Default(),
	}

	req := httptest.NewRequest("GET", "http://example.com/path", nil)
	req.Header.Set("Host", "example.com")
	req.RemoteAddr = "127.0.0.1:8080"
	rr := httptest.NewRecorder()

	b.ServeHTTP(rr, req)

	captured := dummyProxy.captured
	if captured == nil {
		t.Fatal("Request was not forwarded to the downstream handler")
	}
	if got := captured.Header.Get("X-Forwarded-Host"); got != "example.com" {
		t.Errorf("expected X-Forwarded-Host to be 'example.com', got %q", got)
	}
	if got := captured.Header.Get("X-Test"); got != "TestValue" {
		t.Errorf("expected X-Test header to be 'TestValue', got %q", got)
	}
	if captured.Host != "backend-host" {
		t.Errorf("expected request Host to be 'backend-host', got %q", captured.Host)
	}
}
