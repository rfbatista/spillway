// Command anthropicserver is a local fake Anthropic Messages backend. It echoes
// a fixed Anthropic-shaped response so the proxy's AnthropicMapper can be
// exercised end-to-end without network access.
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_fixture",
			"model":       "claude-fixture",
			"stop_reason": "end_turn",
			"content":     []any{map[string]any{"type": "text", "text": "fixture reply"}},
			"usage":       map[string]any{"input_tokens": 5, "output_tokens": 3},
		})
	})

	addr := ":8092"
	slog.Info("anthropicserver listening", slog.String("addr", addr))
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("anthropicserver failed", slog.Any("error", err))
		os.Exit(1)
	}
}
