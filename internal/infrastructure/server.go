package infrastructure

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"log/slog"

	"proxy/internal/handlers"
	"proxy/internal/proxy"
)

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.statusCode = code
	sr.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying ResponseWriter so that streaming responses
// (Server-Sent Events) keep working through the logging middleware.
func (sr *statusRecorder) Flush() {
	if flusher, ok := sr.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		requestID := getOrCreateRequestID(r)
		w.Header().Set("X-Request-Id", requestID)

		clientIP := r.Header.Get("X-Forwarded-For")
		if clientIP == "" {
			clientIP = r.Header.Get("X-Real-IP")
		}
		if clientIP == "" {
			clientIP = r.RemoteAddr
		}
		clientIP = strings.TrimSpace(strings.Split(clientIP, ",")[0])

		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)

		slog.Info("request completed",
			slog.String("request_id", requestID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("client_ip", clientIP),
			slog.Int("status_code", rec.statusCode),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

func getOrCreateRequestID(r *http.Request) string {
	if requestID := strings.TrimSpace(r.Header.Get("X-Request-Id")); requestID != "" {
		return requestID
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func NewServer(cfg Config, brd *broadcaster, reqBrd *RequestBroadcaster, handler *proxy.ProxyHandler) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/quotes", handlers.QuotaPage)
	mux.HandleFunc("/status", handlers.QuotaStatusPage(handler.GetPool))
	mux.HandleFunc("/events", SSE(brd.Stream))

	mux.HandleFunc("/dashboard", handlers.DashboardPage)
	mux.HandleFunc("/request-events", SSE(reqBrd.Stream))
	mux.Handle("/", handler)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%s", cfg.Port),
		Handler: loggingMiddleware(mux),
	}
	server.ReadHeaderTimeout = 15 * time.Minute
	return server
}
