package llmproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// hopHeaders are connection-scoped and never forwarded (RFC 9110 §7.6.1).
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

type handler struct {
	upstream    *url.URL
	attribution string
	sink        UsageSink
	client      *http.Client
	log         *slog.Logger
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := UsageRecord{
		ID:          newID(),
		Attribution: r.Header.Get(h.attribution),
		Method:      r.Method,
		Path:        r.URL.Path,
		StartedAt:   time.Now(),
	}
	reqModel := &requestModel{}
	usage := &usageReader{}
	defer func() {
		rec.EndedAt = time.Now()
		usage.fill(&rec)
		if rec.Model == "" {
			rec.Model = reqModel.model()
		}
		go h.record(rec)
	}()

	out, err := h.outgoing(r, reqModel)
	if err != nil {
		rec.Error = err.Error()
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	resp, err := h.client.Do(out)
	if err != nil {
		rec.Error = h.failure(r, "upstream request", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	rec.Status = resp.StatusCode
	rec.Streamed = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	usage.start(rec.Streamed, resp.Header.Get("Content-Encoding"))

	dst := w.Header()
	for k, vv := range resp.Header {
		dst[k] = append([]string(nil), vv...)
	}
	removeHopHeaders(dst)
	w.WriteHeader(resp.StatusCode)

	if err := h.copyBody(w, resp.Body, usage); err != nil {
		rec.Error = h.failure(r, "response body", err)
	}
}

// outgoing builds the upstream request: same method, path, query, headers and
// body, minus the attribution header and the hop-by-hop headers.
func (h *handler) outgoing(r *http.Request, model *requestModel) (*http.Request, error) {
	target := *h.upstream
	target.Path = strings.TrimSuffix(h.upstream.Path, "/") + r.URL.Path
	if r.URL.RawPath != "" {
		target.RawPath = strings.TrimSuffix(h.upstream.EscapedPath(), "/") + r.URL.RawPath
	} else {
		target.RawPath = ""
	}
	target.RawQuery = r.URL.RawQuery

	var body io.Reader = http.NoBody
	if r.ContentLength != 0 && r.Body != nil {
		body = io.TeeReader(r.Body, model)
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	out.ContentLength = r.ContentLength
	out.Header = r.Header.Clone()
	out.Header.Del(h.attribution)
	removeHopHeaders(out.Header)
	return out, nil
}

// copyBody streams the upstream body to the client, flushing after every
// read so server-sent events reach the client as they arrive, and tees each
// chunk to the usage reader.
func (h *handler) copyBody(w http.ResponseWriter, body io.Reader, usage *usageReader) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			usage.Write(buf[:n])
			if _, err := w.Write(buf[:n]); err != nil {
				return fmt.Errorf("client disconnected: %w", err)
			}
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return fmt.Errorf("client disconnected: %w", err)
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// failure describes a failed request for its usage record and logs it,
// without bodies or credentials.
func (h *handler) failure(r *http.Request, stage string, err error) string {
	msg := fmt.Sprintf("%s: %v", stage, err)
	if r.Context().Err() != nil && !strings.Contains(msg, "client disconnected") {
		msg = "client disconnected: " + msg
	}
	h.log.Warn("llm proxy request failed",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("error", msg),
	)
	return msg
}

// record hands rec to the usage sink, off the response path. A panicking sink
// is logged and contained.
func (h *handler) record(rec UsageRecord) {
	defer func() {
		if p := recover(); p != nil {
			h.log.Error("usage sink panicked", slog.String("id", rec.ID), slog.Any("panic", p))
		}
	}()
	h.sink.Record(context.Background(), rec)
}

func removeHopHeaders(hdr http.Header) {
	for _, f := range hdr.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				hdr.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		hdr.Del(name)
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
