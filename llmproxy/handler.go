package llmproxy

import (
	"bytes"
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

// errClientGone marks a failure caused by the client going away.
var errClientGone = errors.New("client disconnected")

type handler struct {
	upstream          *url.URL
	attributionHeader string
	sink              UsageSink
	client            *http.Client
	log               *slog.Logger
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := UsageRecord{
		ID:          newID(),
		Attribution: r.Header.Get(h.attributionHeader),
		Method:      r.Method,
		Path:        r.URL.Path,
		StartedAt:   time.Now(),
	}
	var head []byte
	usage := &usageTee{}
	defer func() {
		rec.EndedAt = time.Now()
		// Usage is read and recorded after the handler returns, so neither
		// delays the end of the response.
		go h.record(rec, usage, head)
	}()

	head, body, err := readHead(r)
	if err != nil {
		rec.Error = h.fail(w, r, http.StatusBadRequest, "request body", fmt.Errorf("%w: %w", errClientGone, err))
		return
	}
	out, err := h.outgoing(r, body)
	if err != nil {
		rec.Error = h.fail(w, r, http.StatusBadGateway, "build upstream request", err)
		return
	}
	resp, err := h.client.Do(out)
	if err != nil {
		rec.Error = h.fail(w, r, http.StatusBadGateway, "upstream request", err)
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

	if err := copyBody(w, resp.Body, usage); err != nil {
		rec.Error = h.describe(r, "response body", err)
	}
}

// outgoing builds the upstream request: same method, path, query, headers and
// body, minus the attribution header and the hop-by-hop headers.
func (h *handler) outgoing(r *http.Request, body io.Reader) (*http.Request, error) {
	target := *h.upstream
	target.Path = strings.TrimSuffix(h.upstream.Path, "/") + r.URL.Path
	if r.URL.RawPath != "" {
		target.RawPath = strings.TrimSuffix(h.upstream.EscapedPath(), "/") + r.URL.RawPath
	} else {
		target.RawPath = ""
	}
	target.RawQuery = r.URL.RawQuery

	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		return nil, err
	}
	out.ContentLength = r.ContentLength
	out.Header = r.Header.Clone()
	out.Header.Del(h.attributionHeader)
	removeHopHeaders(out.Header)
	return out, nil
}

// readHead reads the first maxRequestHead bytes of the request body, where
// its "model" is, and returns them with a reader for the whole body.
func readHead(r *http.Request) ([]byte, io.Reader, error) {
	if r.ContentLength == 0 || r.Body == nil {
		return nil, http.NoBody, nil
	}
	head := make([]byte, maxRequestHead)
	n, err := io.ReadFull(r.Body, head)
	head = head[:n]
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return head, bytes.NewReader(head), nil
	}
	if err != nil {
		return head, nil, err
	}
	return head, io.MultiReader(bytes.NewReader(head), r.Body), nil
}

// copyBody streams the upstream body to the client, flushing after every
// read so server-sent events reach the client as they arrive. Each chunk is
// teed to usage once the client has it.
func copyBody(w http.ResponseWriter, body io.Reader, usage *usageTee) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return fmt.Errorf("%w: %w", errClientGone, err)
			}
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return fmt.Errorf("%w: %w", errClientGone, err)
			}
			usage.Write(buf[:n])
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// fail answers a request the proxy could not complete with status, and
// describes the failure for its usage record.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, status int, stage string, err error) string {
	w.WriteHeader(status)
	return h.describe(r, stage, err)
}

// describe words a failed request for its usage record and logs it, without
// bodies or credentials.
func (h *handler) describe(r *http.Request, stage string, err error) string {
	msg := fmt.Sprintf("%s: %v", stage, err)
	if r.Context().Err() != nil && !errors.Is(err, errClientGone) {
		msg = fmt.Sprintf("%v: %s", errClientGone, msg)
	}
	h.log.Warn("llm proxy request failed",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("error", msg),
	)
	return msg
}

// record reads the usage out of what the tee saw, completes rec and hands it
// to the usage sink. It runs off the response path; a panicking sink is
// logged and contained.
func (h *handler) record(rec UsageRecord, usage *usageTee, requestHead []byte) {
	defer func() {
		if p := recover(); p != nil {
			h.log.Error("usage sink panicked", slog.String("id", rec.ID), slog.Any("panic", p))
		}
	}()
	usage.fill(&rec)
	if rec.Model == "" {
		rec.Model = requestModel(requestHead)
	}
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
