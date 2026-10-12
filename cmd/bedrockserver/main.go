// Command bedrockserver is a local fake Bedrock Converse backend. It echoes a
// fixed Converse-shaped response so the proxy's BedrockMapper can be exercised
// end-to-end without real AWS signing (use NoopSigner).
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/model/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": map[string]any{"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"text": "fixture reply"}},
			}},
			"stopReason": "end_turn",
			"usage":      map[string]any{"inputTokens": 5, "outputTokens": 3},
		})
	})

	addr := ":8093"
	slog.Info("bedrockserver listening", slog.String("addr", addr))
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("bedrockserver failed", slog.Any("error", err))
		os.Exit(1)
	}
}
