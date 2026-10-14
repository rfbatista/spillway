package infrastructure

import (
	"net/http"
	"net/http/pprof"

	"github.com/rfbatista/spillway/internal/handlers"
	"github.com/rfbatista/spillway/internal/proxy"
)

// NewAdminServer creates the admin HTTP server that exposes operational
// endpoints. It is served on a separate port from the proxy server and is not
// internet-facing.
//
// It also serves net/http/pprof. Registering it on this internal mux — rather
// than importing net/http/pprof for its side effect on http.DefaultServeMux —
// keeps the profiling endpoints off the public proxy port.
func NewAdminServer(addr string, getPool func() *proxy.BackendPool) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", handlers.ConfigHandler(getPool))

	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	return &http.Server{Addr: addr, Handler: mux}
}
