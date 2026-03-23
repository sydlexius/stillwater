package api

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"time"
)

// pprofEnabled returns true when the SW_PPROF environment variable is set
// to "1" or "true". This controls whether Go's built-in profiling endpoints
// are exposed on a dedicated localhost listener. These endpoints should only
// be enabled in development or during targeted performance analysis -- never
// in production.
func pprofEnabled() bool {
	v := os.Getenv("SW_PPROF")
	return v == "1" || v == "true"
}

// registerPprof starts a dedicated HTTP listener on localhost:6060 for the
// standard Go pprof handlers. Using a separate listener ensures pprof is
// never reachable via the public-facing port even if accidentally enabled
// in a container.
//
// Usage (with the server running and SW_PPROF=1):
//
//	go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
//	go tool pprof http://localhost:6060/debug/pprof/heap
//	go tool pprof http://localhost:6060/debug/pprof/goroutine
//	go tool pprof http://localhost:6060/debug/pprof/allocs
func registerPprof(logger *slog.Logger) {
	pprofMux := http.NewServeMux()
	pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
	pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	go func() {
		addr := "127.0.0.1:6060"
		logger.Warn("pprof endpoints enabled", "addr", addr)
		srv := &http.Server{
			Addr:         addr,
			Handler:      pprofMux,
			ReadTimeout:  60 * time.Second,
			WriteTimeout: 120 * time.Second, // profile endpoints can take up to 60s
		}
		if err := srv.ListenAndServe(); err != nil {
			logger.Error("pprof listener failed", "error", err)
		}
	}()
}
