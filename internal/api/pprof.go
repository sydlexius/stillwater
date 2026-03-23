package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"sync"
	"time"
)

// pprofOnce guards registerPprof so that Handler() can be called multiple
// times (e.g. in tests) without attempting to bind the pprof port twice.
var pprofOnce sync.Once

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
// When the provided context is canceled, the server is shut down gracefully
// with a deadline matching the server's WriteTimeout, allowing in-flight
// profile captures to finish. If graceful shutdown times out, the listener
// is forcibly closed.
//
// Usage (with the server running and SW_PPROF=1):
//
//	go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
//	go tool pprof http://localhost:6060/debug/pprof/heap
//	go tool pprof http://localhost:6060/debug/pprof/goroutine
//	go tool pprof http://localhost:6060/debug/pprof/allocs
func registerPprof(ctx context.Context, logger *slog.Logger) {
	pprofMux := http.NewServeMux()
	pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
	pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	addr := "127.0.0.1:6060"
	srv := &http.Server{
		Handler:      pprofMux,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 120 * time.Second, // profile endpoints can take up to 60s
	}

	// Bind synchronously so port conflicts are reported immediately during
	// startup rather than asynchronously in a goroutine.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		logger.Error("pprof listener failed to bind", "addr", addr, "error", err)
		return
	}
	logger.Warn("pprof endpoints enabled", "addr", addr)

	// Shutdown goroutine: waits for context cancellation, then drains
	// in-flight requests. The deadline matches WriteTimeout so in-flight
	// profile captures have time to finish. Uses context.WithoutCancel to
	// derive a non-canceled context for the shutdown deadline rather than
	// context.Background, preserving request-scoped values (gosec G118).
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), srv.WriteTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("pprof shutdown error", "error", err)
			// Graceful shutdown timed out; forcibly close the listener so
			// the port is released and the Serve goroutine can exit.
			if closeErr := srv.Close(); closeErr != nil {
				logger.Error("pprof forced close failed", "error", closeErr)
			}
		} else {
			logger.Info("pprof listener stopped")
		}
	}()

	// Serve blocks until the server is shut down. ErrServerClosed is the
	// expected result of a graceful Shutdown call -- not an error.
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof listener failed", "addr", ln.Addr(), "error", err)
		}
	}()
}
