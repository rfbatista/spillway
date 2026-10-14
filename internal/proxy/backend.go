package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/rfbatista/spillway/internal/ports"
)

type Backend struct {
	ID       string
	URL      url.URL
	proxy    http.Handler
	Base     http.RoundTripper
	getusage ports.GetUsageService
	logger   *slog.Logger
	cfg      BackendConfig
}

func NewBackend(
	id string,
	get ports.GetUsageService,
	cfg BackendConfig,
	proxy http.Handler,
) (*Backend, error) {
	targetURL := cfg.GetURL()
	logger := slog.With(slog.String("component", "Backend"), slog.String("id", id))
	return &Backend{
		logger:   logger,
		proxy:    proxy,
		ID:       id,
		URL:      targetURL,
		cfg:      cfg,
		getusage: get,
	}, nil
}

// Alias returns the routing alias configured for this backend, or "" when none
// is set. Backends without an alias are reachable only via round-robin.
func (t *Backend) Alias() string {
	return t.cfg.Alias
}

func (t *Backend) IsAlive() bool {
	m, err := t.getusage.Execute(context.Background(), t.ID)
	if err != nil {
		return false
	}
	// Backend has capacity: serve it regardless of the reset window.
	if m.RemainingRequests > 0 && m.RemainingTokens > 0 {
		return true
	}
	// No usage recorded yet: assume alive until proven otherwise.
	if m.ResetAt.IsZero() {
		return true
	}
	// Out of capacity but the reset window has elapsed.
	return time.Now().After(m.ResetAt)
}

func (p *Backend) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ip, err := getRemoteIP(req.RemoteAddr)
	if err != nil {
		slog.Error("failed to parse client address",
			slog.String("backend_id", p.ID),
			slog.String("remote_addr", req.RemoteAddr),
			slog.String("method", req.Method),
			slog.String("path", req.URL.Path),
			slog.Any("error", err),
		)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("request received for backend",
		slog.String("backend_id", p.ID),
		slog.String("client_ip", ip),
		slog.String("remote_addr", req.RemoteAddr),
		slog.String("method", req.Method),
		slog.String("path", req.URL.Path),
		slog.String("query", req.URL.RawQuery),
		slog.String("target_host", p.URL.Host),
	)

	slog.Info("proxying request",
		slog.String("backend_id", p.ID),
		slog.String("full_url", req.URL.String()),
		slog.String("host", req.Host),
		slog.String("path", req.URL.Path),
		slog.String("scheme", req.URL.Scheme),
		slog.String("method", req.Method),
		slog.String("query", req.URL.RawQuery),
	)

	injectHeaders(req, p.cfg.Headers)
	req.Host = p.URL.Host

	// Request tracking is handled by the RetryTransport
	p.proxy.ServeHTTP(w, req)
}

func injectHeaders(req *http.Request, cfgHeaders []BackendConfigHeaders) {
	// net/http promotes the incoming Host header into req.Host and strips it
	// from req.Header, so read it from req.Host.
	req.Header.Set("X-Forwarded-Host", req.Host)
	// Inject all custom headers from the configuration
	for _, h := range cfgHeaders {
		req.Header.Set(h.Key, h.Value)
	}
}

func getRemoteIP(remoteAddr string) (string, error) {
	if remoteAddr == "" {
		return "", fmt.Errorf("empty remote address")
	}
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().String(), nil
	}
	if addr, err := netip.ParseAddr(remoteAddr); err == nil {
		return addr.String(), nil
	}
	return "", fmt.Errorf("invalid remote address %q", remoteAddr)
}
