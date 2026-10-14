package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rfbatista/spillway/internal/domain"
	"github.com/rfbatista/spillway/internal/mocks"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newFakeBackend creates a fake Backend instance given a proxy handler.
// It sets getusage to always return a live quota.
func newFakeBackend(t *testing.T, proxyHandler http.Handler) *Backend {
	mockGetUsage := mocks.NewMockGetUsageService(t)
	mockGetUsage.
		EXPECT().
		Execute(mock.Anything, "test-backend").
		Return(&domain.ModelUsageQuota{
			RemainingTokens:   100,
			RemainingRequests: 100,
			Requested:         false,
		}, nil).
		Maybe()
	return &Backend{
		ID:       "test-backend",
		URL:      url.URL{Scheme: "http", Host: "dummy"},
		proxy:    proxyHandler,
		Base:     http.DefaultTransport,
		getusage: mockGetUsage,
		logger:   slog.New(slog.NewTextHandler(nil, nil)),
		cfg:      BackendConfig{},
	}
}

func TestServeHTTP_MissingPool(t *testing.T) {
	handler, err := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	// Pool is nil by default — no SetPool call.

	req := httptest.NewRequest("GET", "http://example.com", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status %d, got %d", http.StatusServiceUnavailable, rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "missing backend pool") {
		t.Errorf("Expected error message containing 'missing backend pool', got %s", rr.Body.String())
	}
}

func TestServeHTTP_NoAvailableBackend(t *testing.T) {
	pool := &BackendPool{backend: []*Backend{}}
	handler, err := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	handler.SetPool(pool)

	req := httptest.NewRequest("GET", "http://example.com", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status %d, got %d", http.StatusServiceUnavailable, rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "missing backend") {
		t.Errorf("Expected error message containing 'missing backend', got %s", rr.Body.String())
	}
}

func TestServeHTTP_Success(t *testing.T) {
	fakeProxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend success"))
	})
	fakeBackend := newFakeBackend(t, fakeProxy)
	pool := &BackendPool{backend: []*Backend{fakeBackend}}
	handler, err := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	handler.SetPool(pool)

	req := httptest.NewRequest("GET", "http://example.com", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}
	if body := rr.Body.String(); body != "backend success" {
		t.Errorf("Expected body 'backend success', got '%s'", body)
	}
}

func TestServeHTTP_PanicRecovery(t *testing.T) {
	panicProxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("forced panic")
	})
	fakeBackend := newFakeBackend(t, panicProxy)
	pool := &BackendPool{backend: []*Backend{fakeBackend}}
	testLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := NewHandler(testLogger)
	require.NoError(t, err)
	handler.SetPool(pool)

	req := httptest.NewRequest("GET", "http://example.com", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status %d due to panic recovery, got %d", http.StatusServiceUnavailable, rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "service unavailable") {
		t.Errorf("Expected error message containing 'service unavailable', got %s", rr.Body.String())
	}
}
