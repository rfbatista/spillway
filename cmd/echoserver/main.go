package main

import (
	"io"
	"log/slog"
	"net/http"
)

func echoHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func main() {
	http.HandleFunc("/", echoHandler)
	slog.Info("echo server listening on :8888")
	if err := http.ListenAndServe(":8888", nil); err != nil {
		slog.Error("server encountered an error", "err", err)
	}
}
