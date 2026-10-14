package infrastructure_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rfbatista/spillway/internal/infrastructure"
	"github.com/rfbatista/spillway/internal/proxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pprof on the admin server turns a memory or goroutine leak into a 30-second
// diagnosis. The admin server is internal-only (:9090, not internet-facing).
func TestAdminServer_ServesPprof(t *testing.T) {
	srv := infrastructure.NewAdminServer(":0", func() *proxy.BackendPool { return nil })
	require.NotNil(t, srv.Handler)

	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/goroutine?debug=1",
		"/debug/pprof/heap?debug=1",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			srv.Handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.NotEmpty(t, rec.Body.String())
		})
	}
}
