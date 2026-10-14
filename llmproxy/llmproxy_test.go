package llmproxy_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rfbatista/spillway/llmproxy"
)

// recordingSink collects every usage record the LLM proxy hands it.
type recordingSink struct {
	records chan llmproxy.UsageRecord
}

func newRecordingSink() *recordingSink {
	return &recordingSink{records: make(chan llmproxy.UsageRecord, 16)}
}

func (s *recordingSink) Record(_ context.Context, r llmproxy.UsageRecord) {
	s.records <- r
}

// next waits for the next usage record.
func (s *recordingSink) next(t *testing.T) llmproxy.UsageRecord {
	t.Helper()
	select {
	case r := <-s.records:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("no usage record reached the sink")
		return llmproxy.UsageRecord{}
	}
}

// none asserts no further usage record arrives.
func (s *recordingSink) none(t *testing.T) {
	t.Helper()
	select {
	case r := <-s.records:
		t.Fatalf("unexpected extra usage record: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}

// seenRequest is what the fake Anthropic upstream received.
type seenRequest struct {
	method string
	uri    string
	header http.Header
	body   []byte
}

// fakeUpstream starts a fake Anthropic API that records each request it gets
// and answers with respond.
func fakeUpstream(t *testing.T, respond http.HandlerFunc) (*httptest.Server, <-chan seenRequest) {
	t.Helper()
	seen := make(chan seenRequest, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- seenRequest{method: r.Method, uri: r.RequestURI, header: r.Header.Clone(), body: body}
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// mountProxy mounts the LLM proxy under /llm on a host server, the way the
// harness does, and returns the host's base URL for it.
func mountProxy(t *testing.T, cfg llmproxy.Config) string {
	t.Helper()
	h, err := llmproxy.New(cfg)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("/llm/", http.StripPrefix("/llm", h))
	host := httptest.NewServer(mux)
	t.Cleanup(host.Close)
	return host.URL + "/llm"
}

const messageJSON = `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929",` +
	`"content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":12,"output_tokens":34,"cache_creation_input_tokens":56,"cache_read_input_tokens":78}}`

func TestPassThroughForwardsTheRequestUntouchedAndRecordsJSONUsage(t *testing.T) {
	upstream, seen := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_abc")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "49")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, messageJSON)
	})
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	reqBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"Hello"}]}`)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages?beta=true", bytes.NewReader(reqBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-ant-oat-test")
	req.Header.Set("X-Api-Key", "sk-ant-api-test")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("X-Llm-Attribution", "session-42")

	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	gotBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	got := <-seen
	assert.Equal(t, http.MethodPost, got.method)
	assert.Equal(t, "/v1/messages?beta=true", got.uri)
	assert.Equal(t, reqBody, got.body)
	assert.Equal(t, "Bearer sk-ant-oat-test", got.header.Get("Authorization"))
	assert.Equal(t, "sk-ant-api-test", got.header.Get("X-Api-Key"))
	assert.Equal(t, "2023-06-01", got.header.Get("Anthropic-Version"))
	assert.Equal(t, "oauth-2025-04-20", got.header.Get("Anthropic-Beta"))
	assert.Equal(t, "application/json", got.header.Get("Content-Type"))
	assert.NotContains(t, got.header, "X-Llm-Attribution")

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, messageJSON, string(gotBody))
	assert.Equal(t, "req_abc", resp.Header.Get("Request-Id"))
	assert.Equal(t, "49", resp.Header.Get("Anthropic-Ratelimit-Requests-Remaining"))

	rec := sink.next(t)
	assert.NotEmpty(t, rec.ID)
	assert.Equal(t, "session-42", rec.Attribution)
	assert.Equal(t, http.MethodPost, rec.Method)
	assert.Equal(t, "/v1/messages", rec.Path)
	assert.Equal(t, "claude-sonnet-4-5-20250929", rec.Model)
	assert.Equal(t, http.StatusOK, rec.Status)
	assert.False(t, rec.Streamed)
	assert.Equal(t, int64(12), rec.InputTokens)
	assert.Equal(t, int64(34), rec.OutputTokens)
	assert.Equal(t, int64(56), rec.CacheCreationInputTokens)
	assert.Equal(t, int64(78), rec.CacheReadInputTokens)
	assert.Empty(t, rec.Error)
	assert.False(t, rec.StartedAt.Before(started.Truncate(time.Second)))
	assert.False(t, rec.EndedAt.Before(rec.StartedAt))
	sink.none(t)
}

func post(t *testing.T, url, body string, header map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(got)
}

func TestCountTokensIsRecordedWithZeroTokensAndTheRequestModel(t *testing.T) {
	upstream, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":2095}`)
	})
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	resp, body := post(t, base+"/v1/messages/count_tokens",
		`{"messages":[{"role":"user","content":"x"}],"model":"claude-opus-4-1","system":"s"}`, nil)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `{"input_tokens":2095}`, body)
	rec := sink.next(t)
	assert.Equal(t, "/v1/messages/count_tokens", rec.Path)
	assert.Equal(t, "claude-opus-4-1", rec.Model)
	assert.Empty(t, rec.Attribution)
	assert.Zero(t, rec.InputTokens)
	assert.Zero(t, rec.OutputTokens)
	assert.Zero(t, rec.CacheCreationInputTokens)
	assert.Zero(t, rec.CacheReadInputTokens)
	assert.Empty(t, rec.Error)
}

func TestErrorStatusPassesThroughAndIsRecorded(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, 529} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			errBody := `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
			upstream, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				io.WriteString(w, errBody)
			})
			sink := newRecordingSink()
			base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

			resp, body := post(t, base+"/v1/messages", `{"model":"claude-haiku-4-5","max_tokens":1}`,
				map[string]string{"X-Llm-Attribution": "harness:system"})

			assert.Equal(t, status, resp.StatusCode)
			assert.Equal(t, "7", resp.Header.Get("Retry-After"))
			assert.Equal(t, errBody, body)
			rec := sink.next(t)
			assert.Equal(t, status, rec.Status)
			assert.Equal(t, "harness:system", rec.Attribution)
			assert.Equal(t, "claude-haiku-4-5", rec.Model)
			assert.Zero(t, rec.InputTokens+rec.OutputTokens)
			assert.Empty(t, rec.Error)
		})
	}
}

type panickingSink struct{ calls chan struct{} }

func (s panickingSink) Record(context.Context, llmproxy.UsageRecord) {
	s.calls <- struct{}{}
	panic("sink exploded")
}

func TestAPanickingSinkNeverBreaksTheResponse(t *testing.T) {
	upstream, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, messageJSON)
	})
	sink := panickingSink{calls: make(chan struct{}, 4)}
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	for i := 0; i < 2; i++ {
		resp, body := post(t, base+"/v1/messages", `{"model":"m"}`, nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, messageJSON, body)
		select {
		case <-sink.calls:
		case <-time.After(2 * time.Second):
			t.Fatal("sink was not called")
		}
	}
}

type blockingSink struct{ release chan struct{} }

func (s blockingSink) Record(context.Context, llmproxy.UsageRecord) { <-s.release }

func TestABlockingSinkNeverDelaysTheResponse(t *testing.T) {
	upstream, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, messageJSON)
	})
	sink := blockingSink{release: make(chan struct{})}
	defer close(sink.release)
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	done := make(chan string)
	go func() {
		// No require here: FailNow must run on the test goroutine.
		resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			done <- err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		done <- string(body)
	}()
	select {
	case body := <-done:
		assert.Equal(t, messageJSON, body)
	case <-time.After(2 * time.Second):
		t.Fatal("response waited on the usage sink")
	}
}

func TestNewRequiresASink(t *testing.T) {
	_, err := llmproxy.New(llmproxy.Config{Upstream: "http://127.0.0.1:1"})
	assert.Error(t, err)
}

func TestNewRejectsARelativeUpstream(t *testing.T) {
	_, err := llmproxy.New(llmproxy.Config{Upstream: "api.anthropic.com", Sink: newRecordingSink()})
	assert.Error(t, err)
}

func TestACustomAttributionHeaderIsReadAndStripped(t *testing.T) {
	upstream, seen := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, messageJSON)
	})
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink, AttributionHeader: "x-who"})

	post(t, base+"/v1/messages", `{"model":"m"}`, map[string]string{
		"X-Who":             "session-7",
		"X-Llm-Attribution": "kept-as-any-other-header",
	})

	got := <-seen
	assert.NotContains(t, got.header, "X-Who")
	assert.Equal(t, "kept-as-any-other-header", got.header.Get("X-Llm-Attribution"))
	assert.Equal(t, "session-7", sink.next(t).Attribution)
}

func TestEachRequestGetsItsOwnRecordID(t *testing.T) {
	upstream, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, messageJSON)
	})
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	post(t, base+"/v1/messages", `{"model":"m"}`, nil)
	post(t, base+"/v1/messages", `{"model":"m"}`, nil)

	assert.NotEqual(t, sink.next(t).ID, sink.next(t).ID)
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := io.WriteString(zw, s)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestAGzippedResponsePassesThroughCompressedAndIsCounted(t *testing.T) {
	compressed := gzipped(t, messageJSON)
	upstream, seen := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed)
	})
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(`{"model":"m"}`))
	require.NoError(t, err)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, "gzip, deflate, br", (<-seen).header.Get("Accept-Encoding"))
	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, compressed, body)
	rec := sink.next(t)
	assert.Equal(t, "claude-sonnet-4-5-20250929", rec.Model)
	assert.Equal(t, int64(12), rec.InputTokens)
	assert.Equal(t, int64(34), rec.OutputTokens)
	assert.Equal(t, int64(56), rec.CacheCreationInputTokens)
	assert.Equal(t, int64(78), rec.CacheReadInputTokens)
}

// sseEvents is a streamed Messages response the way Anthropic sends it: usage
// in message_start, the running output count in each message_delta.
var sseEvents = []string{
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_02\",\"type\":\"message\",\"role\":\"assistant\"," +
		"\"model\":\"claude-opus-4-1-20250805\",\"content\":[],\"stop_reason\":null," +
		"\"usage\":{\"input_tokens\":25,\"cache_creation_input_tokens\":100,\"cache_read_input_tokens\":200,\"output_tokens\":1}}}\n\n",
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
	"event: ping\ndata: {\"type\": \"ping\"}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null},\"usage\":{\"output_tokens\":7}}\n\n",
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null}," +
		"\"usage\":{\"input_tokens\":25,\"cache_creation_input_tokens\":100,\"cache_read_input_tokens\":200,\"output_tokens\":15}}\n\n",
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
}

// sseUpstream answers with the given chunks, flushing after each one.
func sseUpstream(t *testing.T, chunks []string) (*httptest.Server, <-chan seenRequest) {
	return fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			io.WriteString(w, c)
			w.(http.Flusher).Flush()
		}
	})
}

func assertStreamUsage(t *testing.T, rec llmproxy.UsageRecord) {
	t.Helper()
	assert.True(t, rec.Streamed)
	assert.Equal(t, "claude-opus-4-1-20250805", rec.Model)
	assert.Equal(t, int64(25), rec.InputTokens)
	assert.Equal(t, int64(15), rec.OutputTokens)
	assert.Equal(t, int64(100), rec.CacheCreationInputTokens)
	assert.Equal(t, int64(200), rec.CacheReadInputTokens)
	assert.Empty(t, rec.Error)
}

func TestAStreamedResponsePassesThroughAndIsCounted(t *testing.T) {
	upstream, _ := sseUpstream(t, sseEvents)
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	resp, body := post(t, base+"/v1/messages", `{"model":"claude-opus-4-1","stream":true}`,
		map[string]string{"X-Llm-Attribution": "session-9"})

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, strings.Join(sseEvents, ""), body)
	rec := sink.next(t)
	assert.Equal(t, "session-9", rec.Attribution)
	assertStreamUsage(t, rec)
	sink.none(t)
}

func TestAStreamedEventSplitAcrossWritesIsStillCounted(t *testing.T) {
	whole := strings.Join(sseEvents, "")
	var chunks []string
	for i := 0; i < len(whole); i += 7 {
		chunks = append(chunks, whole[i:min(i+7, len(whole))])
	}
	upstream, _ := sseUpstream(t, chunks)
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	_, body := post(t, base+"/v1/messages", `{"model":"claude-opus-4-1","stream":true}`, nil)

	assert.Equal(t, whole, body)
	assertStreamUsage(t, sink.next(t))
}

func TestEachStreamedEventReachesTheClientBeforeTheNextIsSent(t *testing.T) {
	received := make(chan struct{})
	upstream, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range sseEvents {
			io.WriteString(w, ev)
			w.(http.Flusher).Flush()
			select {
			case <-received:
			case <-time.After(2 * time.Second):
				return // the client never saw this event: the test fails below
			}
		}
	})
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(`{"stream":true}`))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	for i, want := range sseEvents {
		got := ""
		for !strings.HasSuffix(got, "\n\n") {
			line, err := r.ReadString('\n')
			require.NoError(t, err, "event %d never arrived", i)
			got += line
		}
		assert.Equal(t, want, got)
		received <- struct{}{}
	}
	assertStreamUsage(t, sink.next(t))
}

func TestAnUnreachableUpstreamGivesABadGatewayAndARecord(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: gone.URL, Sink: sink})

	resp, _ := post(t, base+"/v1/messages", `{"model":"claude-haiku-4-5"}`,
		map[string]string{"X-Llm-Attribution": "session-1"})

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	rec := sink.next(t)
	assert.Zero(t, rec.Status)
	assert.NotEmpty(t, rec.Error)
	assert.Equal(t, "session-1", rec.Attribution)
	assert.Equal(t, "claude-haiku-4-5", rec.Model)
	sink.none(t)
}

func TestAClientThatGoesAwayMidStreamIsRecordedWithTheUsageSoFar(t *testing.T) {
	cancelled := make(chan struct{})
	upstream, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseEvents[0])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
	})
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", strings.NewReader(`{"stream":true}`))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, err = bufio.NewReader(resp.Body).ReadString('}')
	require.NoError(t, err)
	cancel()
	resp.Body.Close()

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the upstream request was not cancelled")
	}
	rec := sink.next(t)
	assert.True(t, rec.Streamed)
	assert.Equal(t, http.StatusOK, rec.Status)
	assert.Equal(t, "claude-opus-4-1-20250805", rec.Model)
	assert.Equal(t, int64(25), rec.InputTokens)
	assert.Equal(t, int64(1), rec.OutputTokens)
	assert.Contains(t, rec.Error, "client disconnected")
	sink.none(t)
}

func TestAnUpstreamThatDropsMidStreamIsRecordedWithAnError(t *testing.T) {
	upstream, _ := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n",
			len(sseEvents[0]), sseEvents[0])
		buf.Flush()
	})
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})

	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(`{"stream":true}`))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	io.ReadAll(resp.Body)
	resp.Body.Close()

	rec := sink.next(t)
	assert.True(t, rec.Streamed)
	assert.Equal(t, int64(25), rec.InputTokens)
	assert.NotEmpty(t, rec.Error)
	assert.NotContains(t, rec.Error, "client disconnected")
	sink.none(t)
}

func TestALargeRequestBodyArrivesWhole(t *testing.T) {
	upstream, seen := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":1}`)
	})
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{Upstream: upstream.URL, Sink: sink})
	big := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"` + strings.Repeat("ab", 300_000) + `"}]}`

	post(t, base+"/v1/messages/count_tokens", big, nil)

	assert.Equal(t, big, string((<-seen).body))
	assert.Equal(t, "claude-sonnet-4-5", sink.next(t).Model)
}

func TestAHostClientStillPassesTheEncodedBodyThrough(t *testing.T) {
	compressed := gzipped(t, messageJSON)
	upstream, seen := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed)
	})
	sink := newRecordingSink()
	base := mountProxy(t, llmproxy.Config{
		Upstream: upstream.URL,
		Sink:     sink,
		Client:   &http.Client{Transport: &http.Transport{}},
	})

	conn, err := net.Dial("tcp", strings.TrimPrefix(strings.TrimSuffix(base, "/llm"), "http://"))
	require.NoError(t, err)
	defer conn.Close()
	// A raw request: Go's client would add Accept-Encoding on its own.
	fmt.Fprint(conn, "POST /llm/v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Empty(t, (<-seen).header.Get("Accept-Encoding"))
	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, compressed, body)
	assert.Equal(t, int64(34), sink.next(t).OutputTokens)
}
