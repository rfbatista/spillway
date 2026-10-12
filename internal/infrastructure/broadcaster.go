package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"proxy/internal/domain"
	"proxy/internal/ports"
	"sync"
)

// Stream fans out values of type T to every subscribed SSE client. Sends are
// non-blocking, so a slow client is skipped rather than allowed to stall the
// whole stream.
type Stream[T any] struct {
	mu      sync.Mutex
	clients map[chan T]struct{}
}

// NewStream creates an empty Stream ready to accept subscribers.
func NewStream[T any]() *Stream[T] {
	return &Stream[T]{clients: make(map[chan T]struct{})}
}

// subscribe registers a new buffered client channel and returns it.
func (s *Stream[T]) subscribe() chan T {
	ch := make(chan T, 16)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[ch] = struct{}{}
	slog.Info("sse client subscribed", slog.Int("clients", len(s.clients)))
	return ch
}

// unsubscribe removes a client channel previously returned by subscribe.
func (s *Stream[T]) unsubscribe(ch chan T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, ch)
	slog.Info("sse client unsubscribed", slog.Int("clients", len(s.clients)))
}

// Broadcast delivers v to every subscriber without blocking on slow clients.
func (s *Stream[T]) Broadcast(v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- v:
		default:
		}
	}
}

// broadcaster streams model usage quota updates to SSE clients.
type broadcaster struct {
	*Stream[domain.ModelUsageQuota]
}

var _ ports.Broadcaster = (*broadcaster)(nil)

func NewBroadcaster() *broadcaster {
	return &broadcaster{Stream: NewStream[domain.ModelUsageQuota]()}
}

func (b *broadcaster) PublishRemainingToken(ctx context.Context, model domain.ModelUsageQuota) error {
	b.Broadcast(model)
	return nil
}

// RequestBroadcaster streams completed request information to SSE clients,
// replacing the previous in-memory request store. Nothing is retained: each
// client only receives requests that complete after it connects.
type RequestBroadcaster struct {
	*Stream[domain.DetailedRequestInfo]
}

var _ ports.RequestPublisher = (*RequestBroadcaster)(nil)

func NewRequestBroadcaster() *RequestBroadcaster {
	return &RequestBroadcaster{Stream: NewStream[domain.DetailedRequestInfo]()}
}

func (b *RequestBroadcaster) PublishRequest(ctx context.Context, info domain.DetailedRequestInfo) error {
	b.Broadcast(info)
	return nil
}

// SSE serves a Server-Sent Events stream that forwards every value broadcast on
// s to the connected client as a JSON `data:` frame.
func SSE[T any](s *Stream[T]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		// headers for SSE
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch := s.subscribe()
		defer s.unsubscribe(ch)

		// send a comment to establish the stream
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		notify := r.Context().Done()
		for {
			select {
			case <-notify:
				return
			case v := <-ch:
				bts, err := json.Marshal(v)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", bts)
				flusher.Flush()
			}
		}
	}
}
