package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rfbatista/spillway/internal/handlers"
	"github.com/rfbatista/spillway/internal/proxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeGetPool(cfgs []proxy.BackendConfig) func() *proxy.BackendPool {
	pool, _ := proxy.NewBackendPoolFromConfigs(cfgs)
	return func() *proxy.BackendPool { return pool }
}

func TestConfigHandler_ReturnsRedactedConfig(t *testing.T) {
	cfgs := []proxy.BackendConfig{
		{
			Host:   "api.openai.com",
			Path:   "/v1",
			Scheme: "https",
			Model:  "gpt-4o",
			Headers: []proxy.BackendConfigHeaders{
				{Key: "Authorization", Value: "Bearer sk-secret"},
			},
			Queries: []proxy.BackendConfigQueries{
				{Key: "api-version", Value: "2024-01-01"},
			},
		},
	}

	h := handlers.ConfigHandler(makeGetPool(cfgs))
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	var result []proxy.BackendConfig
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&result))
	require.Len(t, result, 1)
	assert.Equal(t, "api.openai.com", result[0].Host)
	assert.Equal(t, "gpt-4o", result[0].Model)
	require.Len(t, result[0].Headers, 1)
	assert.Equal(t, "Authorization", result[0].Headers[0].Key)
	assert.Equal(t, "[REDACTED]", result[0].Headers[0].Value)
	require.Len(t, result[0].Queries, 1)
	assert.Equal(t, "api-version", result[0].Queries[0].Key)
	assert.Equal(t, "[REDACTED]", result[0].Queries[0].Value)
}

func TestConfigHandler_NilPool(t *testing.T) {
	h := handlers.ConfigHandler(func() *proxy.BackendPool { return nil })
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

func TestConfigHandler_EmptyBackends(t *testing.T) {
	h := handlers.ConfigHandler(makeGetPool([]proxy.BackendConfig{}))
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var result []proxy.BackendConfig
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&result))
	assert.Empty(t, result)
}
