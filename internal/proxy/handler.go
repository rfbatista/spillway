package proxy

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"
)

// RequestModelHeader lets a client target a backend by its configured alias.
// When absent or unmatched, the pool falls back to round-robin selection.
const RequestModelHeader = "X-Request-Model"

type responseWriterWrapper struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *responseWriterWrapper) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

type ProxyHandler struct {
	log  *slog.Logger
	pool atomic.Pointer[BackendPool]
}

func NewHandler(log *slog.Logger) (*ProxyHandler, error) {
	if log == nil {
		return nil, errors.New("missing logger")
	}
	return &ProxyHandler{log: log}, nil
}

// SetPool atomically replaces the active BackendPool.
// In-flight requests on the previous pool complete naturally.
func (p *ProxyHandler) SetPool(pool *BackendPool) {
	p.pool.Store(pool)
}

// GetPool returns the current BackendPool. Returns nil if SetPool has not been called.
func (p *ProxyHandler) GetPool() *BackendPool {
	return p.pool.Load()
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w = &responseWriterWrapper{ResponseWriter: w}
	defer func() {
		if rec := recover(); rec != nil {
			stackTrace := string(debug.Stack())
			p.log.Error("panic in ServeHTTP",
				slog.Any("error", rec),
				slog.String("stack", stackTrace),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("query", r.URL.RawQuery),
				slog.String("host", r.Host),
				slog.String("remote_addr", r.RemoteAddr),
			)
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
	}()
	pool := p.pool.Load()
	if pool == nil {
		http.Error(w, "service unavailable, missing backend pool", http.StatusServiceUnavailable)
		return
	}
	back, err := pool.GetNextBackendForModel(r.Header.Get(RequestModelHeader))
	if err != nil {
		http.Error(w, "service unavailable, missing backend", http.StatusServiceUnavailable)
		return
	}
	back.ServeHTTP(w, r)
}
