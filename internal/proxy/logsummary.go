package proxy

import (
	"encoding/json"
	"strings"
)

// defaultLogCropLimit is the crop length used when a backend configures crop
// paths but leaves log_crop_limit unset.
const defaultLogCropLimit = 256

// cropMarker is appended to a value cropped for logging. It mirrors
// truncationMarker so a reader can tell a per-field crop from a whole-body cut.
const cropMarker = "... (cropped)"

// logBodySummarizer reduces a body to its readable core before it is written to
// a log line: configured JSON paths are dropped, and long strings at configured
// paths are cropped. Without it a single huge prompt fills the whole byte
// ceiling and hides every small field around it — inferenceConfig, stopReason,
// usage — which are exactly the fields worth reading during triage.
//
// It shapes logged bodies only. The body forwarded upstream is untouched, and
// the dashboard still receives the full body over SSE.
//
// A nil *logBodySummarizer is valid and returns every body unchanged, so a
// backend that configures nothing keeps the previous behaviour byte for byte.
// A summarizer is immutable after construction and safe for concurrent use, and
// must stay that way: one instance serves every request to a backend.
type logBodySummarizer struct {
	dropRequest  [][]string
	cropRequest  [][]string
	dropResponse [][]string
	cropResponse [][]string
	cropLimit    int
}

// newLogBodySummarizer builds the summarizer for a backend, returning nil when
// nothing is configured so the common path pays nothing. It never fails: a blank
// path is skipped rather than rejected, so a typo cannot break a config reload.
func newLogBodySummarizer(cfg BackendConfig) *logBodySummarizer {
	s := &logBodySummarizer{
		dropRequest:  compileJSONPaths(cfg.LogDropRequestFields),
		cropRequest:  compileJSONPaths(cfg.LogCropRequestFields),
		dropResponse: compileJSONPaths(cfg.LogDropResponseFields),
		cropResponse: compileJSONPaths(cfg.LogCropResponseFields),
		cropLimit:    cfg.LogCropLimit,
	}
	if len(s.dropRequest)+len(s.cropRequest)+len(s.dropResponse)+len(s.cropResponse) == 0 {
		return nil
	}
	if s.cropLimit <= 0 {
		s.cropLimit = defaultLogCropLimit
	}
	return s
}

// compileJSONPaths splits the configured paths once at build time, skipping
// blank entries.
func compileJSONPaths(paths []string) [][]string {
	var out [][]string
	for _, p := range paths {
		if segments := splitJSONPath(p); len(segments) > 0 {
			out = append(out, segments)
		}
	}
	return out
}

// active reports whether the summarizer would change anything. It is nil-safe,
// so a caller can skip converting a large body to a string for nothing.
func (s *logBodySummarizer) active() bool { return s != nil }

// RequestBody returns the log-facing form of a request body.
func (s *logBodySummarizer) RequestBody(body string) string {
	if s == nil {
		return body
	}
	return s.summarize(body, s.dropRequest, s.cropRequest)
}

// ResponseBody returns the log-facing form of a response body.
func (s *logBodySummarizer) ResponseBody(body string) string {
	if s == nil {
		return body
	}
	return s.summarize(body, s.dropResponse, s.cropResponse)
}

// summarize decodes body, applies drops and then crops, and re-encodes it
// compactly. body is returned untouched when it is empty, is not JSON (a
// streamed answer, say), or when no configured path matched anything — so a
// stale path can never degrade a log line, and the no-match case stays
// byte-identical.
//
// The output is compact on purpose: the value lands in a JSON log field where
// indentation renders as escaped newlines, buying no readability while spending
// the byte budget this filter exists to protect.
func (s *logBodySummarizer) summarize(body string, drops, crops [][]string) string {
	if body == "" || len(drops)+len(crops) == 0 {
		return body
	}

	dec := json.NewDecoder(strings.NewReader(body))
	// Keep numbers verbatim: decoding into float64 would round an integer past
	// 2^53 and re-render a large one in exponent form.
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return body
	}

	changed := false
	// Drops run first, so a path listed in both lists is dropped rather than
	// cropped: by the time the crop runs, the key is gone.
	for _, segments := range drops {
		applyJSONPath(payload, segments, func(any) (any, bool) {
			changed = true
			return nil, true
		})
	}
	for _, segments := range crops {
		applyJSONPath(payload, segments, func(v any) (any, bool) {
			cropped, didCrop := cropValue(v, s.cropLimit)
			changed = changed || didCrop
			return cropped, false
		})
	}
	if !changed {
		return body
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return string(out)
}

// cropValue crops v when it is a string, and every string it holds when it is an
// array — arrays are transparent to a value here as they are to a path in
// applyJSONPath. Any other value is returned unchanged, so a path aimed at the
// wrong kind of node is a visible no-op rather than a mangled body.
func cropValue(v any, limit int) (any, bool) {
	switch t := v.(type) {
	case string:
		return cropString(t, limit)
	case []any:
		changed := false
		for i, elem := range t {
			cropped, didCrop := cropValue(elem, limit)
			if didCrop {
				t[i] = cropped
				changed = true
			}
		}
		return t, changed
	default:
		return v, false
	}
}

// cropString returns s cut to limit runes with cropMarker appended, reporting
// whether it actually cut. The cut lands on a rune boundary, so a multi-byte
// character is never split into invalid UTF-8.
func cropString(s string, limit int) (string, bool) {
	if limit <= 0 {
		return s, false
	}
	runes := 0
	for i := range s {
		if runes == limit {
			return s[:i] + cropMarker, true
		}
		runes++
	}
	return s, false
}
