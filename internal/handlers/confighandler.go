package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/rfbatista/spillway/internal/proxy"
)

// ConfigHandler returns an http.HandlerFunc that serves the current backend config
// with sensitive header/query values replaced by "[REDACTED]".
func ConfigHandler(getPool func() *proxy.BackendPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pool := getPool()
		if pool == nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		cfgs := pool.Configs()
		redacted := make([]proxy.BackendConfig, len(cfgs))
		for i, c := range cfgs {
			rc := c
			rc.Headers = make([]proxy.BackendConfigHeaders, len(c.Headers))
			for j, h := range c.Headers {
				rc.Headers[j] = proxy.BackendConfigHeaders{Key: h.Key, Value: "[REDACTED]"}
			}
			rc.Queries = make([]proxy.BackendConfigQueries, len(c.Queries))
			for j, q := range c.Queries {
				rc.Queries[j] = proxy.BackendConfigQueries{Key: q.Key, Value: "[REDACTED]"}
			}
			redacted[i] = rc
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(redacted)
	}
}
