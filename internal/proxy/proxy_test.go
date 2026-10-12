package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type errMapper struct{ reqErr, respErr error }

func (m errMapper) MapRequest(req *http.Request) (*http.Request, error) {
	if m.reqErr != nil {
		return nil, m.reqErr
	}
	return req, nil
}

func (m errMapper) MapResponse(resp *http.Response) (*http.Response, error) {
	if m.respErr != nil {
		return nil, m.respErr
	}
	return resp, nil
}

func TestNewProxy_MapRequestErrorReturns503(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be called when MapRequest fails")
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	require.NoError(t, err)

	h := NewProxy(*targetURL, BackendConfig{}, errMapper{reqErr: errors.New("boom")}, http.DefaultTransport)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", strings.NewReader(`{}`)))

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

func TestNewProxy_MapResponseErrorReturns502(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	require.NoError(t, err)

	h := NewProxy(*targetURL, BackendConfig{}, errMapper{respErr: errors.New("bad upstream body")}, http.DefaultTransport)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil))

	assert.Equal(t, http.StatusBadGateway, rr.Code)
}

func TestBuildProxyDirector(t *testing.T) {
	targetURL, err := url.Parse("http://localhost:8081/api")
	if err != nil {
		t.Fatalf("Failed to parse target URL: %v", err)
	}
	director := buildProxyDirector(*targetURL, NewPassthroughMapper(""))
	req := &http.Request{
		URL: &url.URL{
			Path:     "/test",
			RawQuery: "foo=bar",
		},
		Host: "example.com",
	}
	director(req)
	if req.URL.Scheme != "http" {
		t.Errorf("Expected scheme 'http', got %s", req.URL.Scheme)
	}
	if req.URL.Host != "localhost:8081" {
		t.Errorf("Expected host 'localhost:8081', got %s", req.URL.Host)
	}
	expectedPath := "/api/test"
	if req.URL.Path != expectedPath {
		t.Errorf("Expected path %s, got %s", expectedPath, req.URL.Path)
	}
	if req.URL.RawQuery != targetURL.RawQuery {
		t.Errorf("Expected raw query %q, got %q", targetURL.RawQuery, req.URL.RawQuery)
	}
	if req.Host != "localhost:8081" {
		t.Errorf("Expected Host field to be set to 'localhost:8081', got %s", req.Host)
	}
}

func TestNewProxy(t *testing.T) {
	// Create a target server that returns a known response.
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "proxied response")
	}))
	defer targetServer.Close()

	targetURL, err := url.Parse(targetServer.URL)
	if err != nil {
		t.Fatalf("Failed to parse target server URL: %v", err)
	}

	backendCfg := BackendConfig{
		Host:   targetURL.Host,
		Path:   targetURL.Path,
		Scheme: targetURL.Scheme,
	}
	proxyHandler := NewProxy(*targetURL, backendCfg, NewPassthroughMapper(""), http.DefaultTransport)

	req := httptest.NewRequest("GET", "http://example.com/somepath", nil)
	rr := httptest.NewRecorder()
	proxyHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "proxied response") {
		t.Errorf("Expected body to contain 'proxied response', got %s", body)
	}
}
