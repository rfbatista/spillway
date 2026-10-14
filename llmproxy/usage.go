package llmproxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

const (
	// maxJSONCapture bounds how much of a non-streamed response body is kept
	// to read its usage. A Messages response is far smaller; past the bound the
	// response is still forwarded, only its usage goes unread.
	maxJSONCapture = 8 << 20
	// maxRequestHead bounds how much of a request body is kept to find its
	// "model", which comes first in practice.
	maxRequestHead = 64 << 10
)

// tokenUsage is Anthropic's usage object. Pointers tell a count that was
// reported apart from one that was not.
type tokenUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
}

// merge overwrites the counts in u with those reported in later.
func (u *tokenUsage) merge(later *tokenUsage) {
	if later == nil {
		return
	}
	if later.InputTokens != nil {
		u.InputTokens = later.InputTokens
	}
	if later.OutputTokens != nil {
		u.OutputTokens = later.OutputTokens
	}
	if later.CacheCreationInputTokens != nil {
		u.CacheCreationInputTokens = later.CacheCreationInputTokens
	}
	if later.CacheReadInputTokens != nil {
		u.CacheReadInputTokens = later.CacheReadInputTokens
	}
}

// usageReader reads the model and token usage out of a response body as it is
// copied to the client. A gzip or deflate body is decoded on its own goroutine
// for reading only; the client still gets the encoded bytes.
type usageReader struct {
	body     bytes.Buffer
	overflow bool

	encoded *io.PipeWriter
	decoded chan struct{}
}

func (u *usageReader) start(streamed bool, contentEncoding string) {
	var decode func(io.Reader) (io.ReadCloser, error)
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "gzip", "x-gzip":
		decode = func(r io.Reader) (io.ReadCloser, error) { return gzip.NewReader(r) }
	case "deflate":
		decode = zlib.NewReader
	default:
		return
	}
	pr, pw := io.Pipe()
	u.encoded = pw
	u.decoded = make(chan struct{})
	go func() {
		defer close(u.decoded)
		// Drain whatever is left so Write never blocks on a bad stream.
		defer io.Copy(io.Discard, pr)
		zr, err := decode(pr)
		if err != nil {
			return
		}
		defer zr.Close()
		io.Copy(writerFunc(u.capture), zr)
	}()
}

// Write takes the next chunk of the body as the client receives it.
func (u *usageReader) Write(p []byte) (int, error) {
	if u.encoded != nil {
		u.encoded.Write(p)
		return len(p), nil
	}
	return u.capture(p)
}

func (u *usageReader) capture(p []byte) (int, error) {
	if u.overflow || u.body.Len()+len(p) > maxJSONCapture {
		u.overflow = true
		u.body.Reset()
		return len(p), nil
	}
	return u.body.Write(p)
}

// finish waits for the decoder, if any, to read the whole body.
func (u *usageReader) finish() {
	if u.encoded != nil {
		u.encoded.Close()
		<-u.decoded
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// fill copies what was read into rec.
func (u *usageReader) fill(rec *UsageRecord) {
	u.finish()
	if u.overflow || u.body.Len() == 0 {
		return
	}
	var msg struct {
		Model string      `json:"model"`
		Usage *tokenUsage `json:"usage"`
	}
	if json.Unmarshal(u.body.Bytes(), &msg) != nil {
		return
	}
	rec.Model = msg.Model
	var usage tokenUsage
	usage.merge(msg.Usage)
	rec.InputTokens = deref(usage.InputTokens)
	rec.OutputTokens = deref(usage.OutputTokens)
	rec.CacheCreationInputTokens = deref(usage.CacheCreationInputTokens)
	rec.CacheReadInputTokens = deref(usage.CacheReadInputTokens)
}

func deref(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

// requestModel keeps the head of a request body as the transport reads it,
// to find the request's top-level "model".
type requestModel struct {
	mu   sync.Mutex
	head bytes.Buffer
}

func (m *requestModel) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if room := maxRequestHead - m.head.Len(); room > 0 {
		if len(p) > room {
			m.head.Write(p[:room])
		} else {
			m.head.Write(p)
		}
	}
	return len(p), nil
}

// model walks the top-level keys of the captured head until "model". A head
// cut short before the key gives "".
func (m *requestModel) model() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	dec := json.NewDecoder(bytes.NewReader(m.head.Bytes()))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return ""
		}
		if key == "model" {
			var model string
			if dec.Decode(&model) != nil {
				return ""
			}
			return model
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return ""
		}
	}
	return ""
}
