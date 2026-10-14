package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/rfbatista/spillway/internal/apperr"
)

// mapErrCtxKey carries a MapRequest error from the Director to the transport,
// which answers with a synthetic error (400 for invalid requests, 503
// otherwise) instead of calling upstream.
type mapErrCtxKey struct{}

// NewProxy builds a reverse proxy for a single backend. The mapper translates
// the request on the way out and the response on the way back; transport is the
// rate-limit-aware RetryTransport, which also answers Director-stage mapping
// failures with syntheticMapError inside request tracking, so those failures
// still get logs, a client_tag, and a place on the dashboard.
func NewProxy(targetURL url.URL, backendCfg BackendConfig, mapper RequestMapper, transport http.RoundTripper) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(&targetURL)
	proxy.Director = buildProxyDirector(targetURL, mapper)
	proxy.Transport = &mapErrorTransport{next: transport}
	// FlushInterval -1 streams bytes to the client as they arrive (required for
	// SSE streaming responses; harmless for buffered JSON).
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Only a successful provider response is in the shape the mapper expects.
		// The proxy's own synthetic errors are already client-shaped, and an
		// upstream error body carries the reason the caller needs; mapping either
		// one would rewrite it into an empty completion and hide the failure.
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil
		}
		if _, err := mapper.MapResponse(resp); err != nil {
			slog.Error("map response failed",
				slog.String("backend_host", targetURL.Host),
				slog.Any("error", err),
			)
			return err
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("reverse proxy error",
			slog.String("backend_host", targetURL.Host),
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		w.WriteHeader(http.StatusBadGateway)
	}
	return proxy
}

func buildProxyDirector(targetURL url.URL, mapper RequestMapper) func(req *http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.URL.Path = targetURL.Path + req.URL.Path
		req.URL.RawQuery = targetURL.RawQuery
		req.Host = targetURL.Host

		if _, err := mapper.MapRequest(req); err != nil {
			slog.Error("map request failed",
				slog.String("backend_host", targetURL.Host),
				slog.String("path", req.URL.Path),
				slog.Any("error", err),
			)
			ctx := contextWithMapErr(req.Context(), err)
			*req = *req.WithContext(ctx)
		}
	}
}

// mapErrorTransport guards transports that do not answer Director-stage
// mapping failures themselves: an unmapped request must never reach the
// upstream. When the inner transport is the RetryTransport it steps aside —
// RetryTransport produces the same synthetic answer itself, inside request
// tracking, so the failure gets logs, a client_tag, and a dashboard entry.
type mapErrorTransport struct {
	next http.RoundTripper
}

func (t *mapErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := mapErrFromContext(req.Context()); err != nil {
		if _, tracked := t.next.(*RetryTransport); !tracked {
			return syntheticMapError(req, err), nil
		}
	}
	return t.next.RoundTrip(req)
}

// syntheticMapError answers a request whose Director-stage mapping failed,
// without contacting the upstream. A request we cannot map is the caller's to
// fix, so it gets a 400 and the reason; anything else is ours, and stays an
// opaque 503.
func syntheticMapError(req *http.Request, err error) *http.Response {
	if errors.Is(err, apperr.ErrInvalidRequest) {
		return syntheticError(req, http.StatusBadRequest, err.Error(), "invalid_request_error")
	}
	return syntheticError(req, http.StatusServiceUnavailable, "request mapping failed", "proxy_error")
}

func contextWithMapErr(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, mapErrCtxKey{}, err)
}

func mapErrFromContext(ctx context.Context) error {
	if e, ok := ctx.Value(mapErrCtxKey{}).(error); ok {
		return e
	}
	return nil
}

// syntheticError builds an OpenAI-shaped error response the proxy answers with
// itself, without contacting the upstream.
func syntheticError(req *http.Request, code int, message, errType string) *http.Response {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": errType, "code": code},
	})
	if err != nil {
		body = []byte(`{"error":{"message":"request mapping failed","type":"proxy_error"}}`)
	}
	return &http.Response{
		StatusCode:    code,
		Status:        fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
