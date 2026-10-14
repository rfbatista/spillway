package llmproxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"strings"
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
	streamed bool
	body     bytes.Buffer // a JSON body, up to maxJSONCapture
	overflow bool
	events   sseReader

	encoded *io.PipeWriter
	decoded chan struct{}
}

func (u *usageReader) start(streamed bool, contentEncoding string) {
	u.streamed = streamed
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
	if u.streamed {
		u.events.feed(p)
		return len(p), nil
	}
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
	var model string
	var usage tokenUsage
	if u.streamed {
		u.events.endOfEvent()
		model, usage = u.events.model, u.events.usage
	} else {
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
		model = msg.Model
		usage.merge(msg.Usage)
	}
	rec.Model = model
	rec.InputTokens = deref(usage.InputTokens)
	rec.OutputTokens = deref(usage.OutputTokens)
	rec.CacheCreationInputTokens = deref(usage.CacheCreationInputTokens)
	rec.CacheReadInputTokens = deref(usage.CacheReadInputTokens)
}

// sseReader reads Anthropic's server-sent events as they pass: the model and
// usage from message_start, then the counts in each later message_delta, the
// last one winning.
type sseReader struct {
	model string
	usage tokenUsage

	line    []byte // the line being read, across writes
	skip    bool   // the line being read is overlong and ignored
	data    []byte // the data lines of the event being read
	hasData bool
}

// maxSSELine bounds one SSE line. Usage events are a few hundred bytes; a
// longer line is a content event and is skipped.
const maxSSELine = 1 << 20

func (s *sseReader) feed(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendLine(p)
			return
		}
		s.appendLine(p[:i])
		if !s.skip {
			s.handleLine(bytes.TrimSuffix(s.line, []byte("\r")))
		}
		s.line, s.skip = s.line[:0], false
		p = p[i+1:]
	}
}

func (s *sseReader) appendLine(p []byte) {
	if s.skip {
		return
	}
	if len(s.line)+len(p) > maxSSELine {
		s.line, s.skip = s.line[:0], true
		return
	}
	s.line = append(s.line, p...)
}

func (s *sseReader) handleLine(line []byte) {
	if len(line) == 0 {
		s.endOfEvent()
		return
	}
	if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
		if s.hasData {
			s.data = append(s.data, '\n')
		}
		s.data = append(s.data, bytes.TrimPrefix(data, []byte(" "))...)
		s.hasData = true
	}
}

// endOfEvent dispatches the event read so far, if any.
func (s *sseReader) endOfEvent() {
	defer func() { s.data, s.hasData = s.data[:0], false }()
	if !s.hasData || !bytes.Contains(s.data, []byte(`"message_`)) {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Model string      `json:"model"`
			Usage *tokenUsage `json:"usage"`
		} `json:"message"`
		Usage *tokenUsage `json:"usage"`
	}
	if json.Unmarshal(s.data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			s.model = ev.Message.Model
			s.usage.merge(ev.Message.Usage)
		}
	case "message_delta":
		s.usage.merge(ev.Usage)
	}
}

func deref(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

// requestModel walks the top-level keys of a request body's head until
// "model". A head cut short before the key gives "".
func requestModel(head []byte) string {
	dec := json.NewDecoder(bytes.NewReader(head))
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
