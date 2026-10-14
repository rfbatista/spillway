package configsource_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rfbatista/spillway/internal/configsource"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppConfigAgentSource_Load_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"backends":[{"host":"api.openai.com","path":"/v1","scheme":"https","model":"gpt-4o"}]}`))
	}))
	defer srv.Close()

	src := configsource.NewAppConfigAgentSourceWithURL(srv.URL)
	cfgs, err := src.Load(context.Background())
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	assert.Equal(t, "api.openai.com", cfgs[0].Host)
}

func TestAppConfigAgentSource_Load_NoChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	src := configsource.NewAppConfigAgentSourceWithURL(srv.URL)
	_, err := src.Load(context.Background())
	assert.True(t, errors.Is(err, configsource.ErrNoChange))
}

func TestAppConfigAgentSource_Load_AgentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	src := configsource.NewAppConfigAgentSourceWithURL(srv.URL)
	_, err := src.Load(context.Background())
	assert.ErrorContains(t, err, "AppConfig agent returned status 500")
}

func TestAppConfigAgentSource_Load_Invalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	src := configsource.NewAppConfigAgentSourceWithURL(srv.URL)
	_, err := src.Load(context.Background())
	assert.ErrorContains(t, err, "parsing config")
}

func TestAppConfigAgentSource_Load_YAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backends:\n  - host: api.openai.com\n    model: gpt-4o\n"))
	}))
	defer srv.Close()

	src := configsource.NewAppConfigAgentSourceWithURL(srv.URL)
	cfgs, err := src.Load(context.Background())
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	assert.Equal(t, "api.openai.com", cfgs[0].Host)
}

func TestAppConfigAgentSource_Load_Unreachable(t *testing.T) {
	src := configsource.NewAppConfigAgentSourceWithURL("http://127.0.0.1:19999")
	_, err := src.Load(context.Background())
	assert.ErrorContains(t, err, "calling AppConfig agent")
}
