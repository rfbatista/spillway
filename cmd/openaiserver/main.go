package main

import (
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var (
	mu                sync.Mutex
	remainingRequests = 50
	remainingTokens   = 150000
	resetTime         = time.Now()
)

func echoHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	remainingRequests--
	reduction := 100 + rand.Intn(15000)
	remainingTokens -= reduction
	reqRem := remainingRequests
	tokRem := remainingTokens
	lastReset := resetTime
	mu.Unlock()

	timeElapsed := time.Since(lastReset).Seconds()
	resetIn := int(60 - timeElapsed)
	if resetIn < 0 {
		resetIn = 0
	}
	w.Header().Set("x-ratelimit-remaining-requests", strconv.Itoa(reqRem))
	w.Header().Set("x-ratelimit-remaining-tokens", strconv.Itoa(tokRem))
	w.Header().Set("x-ratelimit-reset-tokens", strconv.Itoa(resetIn))
	if remainingRequests <= 0 || remainingTokens <= 0 {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	slog.Info("remaining resources", "requests", reqRem, "tokens", tokRem, "reset_in", resetIn)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// Randomly determine response outcome
	outcome := rand.Intn(100)
	if outcome < 20 {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	} else if outcome < 40 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	} else if outcome < 50 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func main() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		for range ticker.C {
			mu.Lock()
			remainingRequests = 6000
			remainingTokens = 150000
			resetTime = time.Now()
			mu.Unlock()
			slog.Info("Rate limits replenished")
		}
	}()

	http.HandleFunc("/", echoHandler)
	slog.Info("openai server listening on :8888")
	if err := http.ListenAndServe(":8888", nil); err != nil {
		slog.Error("server encountered an error", "err", err)
	}
}
