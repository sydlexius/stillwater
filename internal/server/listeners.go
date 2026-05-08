// Package server runs Stillwater's HTTP listener(s).
//
// RunListeners is the entrypoint. It registers one listener per active
// configuration (today a primary HTTP or HTTPS server; the harness is built
// to absorb future HTTP-redirect and HTTP/3 listeners without rewriting the
// run/shutdown loop), starts them as siblings under a single errgroup, and
// shuts them all down in parallel when the supplied context is canceled.
//
// The errgroup choice is deliberate: when one listener fails (for example
// the HTTPS listener cannot bind because port 443 is held by another
// process) we want sibling listeners to also exit, so the operator sees a
// single fatal error rather than a half-running daemon.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sydlexius/stillwater/internal/config"
)

// shutdownTimeout caps how long Shutdown waits for in-flight requests to
// drain. Long-running responses (notably the refresh OOB stream, capped by
// the server's WriteTimeout) are abandoned at the deadline.
const shutdownTimeout = 10 * time.Second

// listenerEntry models a single registered listener inside RunListeners.
// Each entry knows how to start (Serve) and how to gracefully Shutdown.
// Future listeners (HTTP-to-HTTPS redirect, ACME-managed cert, HTTP/3)
// register additional entries without touching the run/shutdown harness.
type listenerEntry struct {
	name     string
	addr     string
	serve    func() error // returns http.ErrServerClosed on graceful exit
	shutdown func(ctx context.Context) error
}

// RunListeners starts every configured listener and blocks until ctx is
// canceled or the first listener returns a non-graceful error. On exit it
// shuts down all listeners in parallel with a shared 10s deadline. The
// first non-http.ErrServerClosed start error wins; otherwise every
// non-nil shutdown error is wrapped together via errors.Join.
func RunListeners(ctx context.Context, cfg *config.Config, handler http.Handler, logger *slog.Logger) error {
	if cfg == nil {
		return errors.New("server: nil config")
	}
	if handler == nil {
		return errors.New("server: nil handler")
	}
	if logger == nil {
		// A nil logger is a programming error. Refuse to start so the
		// listener layer never silently swallows boot diagnostics.
		return errors.New("server: nil logger")
	}

	entries := buildEntries(cfg, handler, logger)
	if len(entries) == 0 {
		return errors.New("server: no listeners configured")
	}

	// Run every listener as a sibling goroutine. errgroup propagates the
	// first non-nil error and, via its derived context, cancels the others
	// so a partial-startup failure surfaces as a single fatal exit.
	g, gctx := errgroup.WithContext(ctx)
	for _, e := range entries {
		logger.Info("listener starting",
			slog.String("name", e.name),
			slog.String("addr", e.addr),
		)
		g.Go(func() error {
			err := e.serve()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("%s listener: %w", e.name, err)
			}
			return nil
		})
	}

	// Wait for either the parent ctx to be canceled (normal shutdown
	// signal) or for one of the listeners to fail. gctx folds both cases
	// into a single Done signal.
	<-gctx.Done()
	logger.Info("shutting down listeners", slog.Int("count", len(entries)))

	// Shut down every listener in parallel under one shared deadline. We
	// deliberately use context.Background() (not gctx) for the shutdown
	// deadline because gctx is already canceled at this point and a
	// canceled context would cause Shutdown to return immediately without
	// draining.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Aggregate every shutdown error: with N listeners coming online over
	// the milestone, swallowing all but the first hides ground truth from
	// whatever wraps run().
	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		shutdownErrs []error
	)
	for _, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.shutdown(shutdownCtx); err != nil {
				logger.Error("listener shutdown error",
					slog.String("name", e.name),
					slog.String("addr", e.addr),
					slog.String("error", err.Error()),
				)
				mu.Lock()
				shutdownErrs = append(shutdownErrs, fmt.Errorf("%s shutdown: %w", e.name, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Reap the errgroup so any non-graceful start error surfaces. The
	// shutdown errors win only when no start error fired -- a startup
	// failure is the more actionable signal for operators.
	if startErr := g.Wait(); startErr != nil {
		return startErr
	}
	return errors.Join(shutdownErrs...)
}

// buildEntries assembles the listener registration list from the runtime
// configuration. Today this is a single HTTP-or-HTTPS server; future M47
// PRs append additional entries (redirect, HTTP/3) here. Wrapping in a
// helper keeps RunListeners' top-level flow short and gives the follow-up
// PRs an obvious place to splice in their entries without rewriting the
// shutdown harness.
func buildEntries(cfg *config.Config, handler http.Handler, logger *slog.Logger) []listenerEntry {
	return []listenerEntry{buildPrimaryListener(cfg, handler, logger)}
}

// buildPrimaryListener returns the entry for the main HTTP/HTTPS server.
// When TLS is configured (cert and key set), the listener serves HTTPS with
// a minimum of TLS 1.2 and ALPN advertising h2 + http/1.1. Otherwise it
// serves plain HTTP.
func buildPrimaryListener(cfg *config.Config, handler http.Handler, logger *slog.Logger) listenerEntry {
	tlsConfigured := cfg.Server.TLS.Enabled()

	// Effective bind port: when TLS is on but TLS.Port is unset, HTTPS
	// reuses Server.Port (collapse semantics).
	port := cfg.Server.Port
	if tlsConfigured && cfg.Server.TLS.Port != 0 {
		port = cfg.Server.TLS.Port
	}
	addr := fmt.Sprintf(":%d", port)

	// WriteTimeout must accommodate the full refresh OOB stream, which can
	// exceed 30s for large group artists under MusicBrainz rate limits.
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 180 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	if tlsConfigured {
		// MinVersion 1.2 keeps Stillwater out of the deprecated TLS 1.0/1.1
		// surface. NextProtos advertises HTTP/2 first, then HTTP/1.1, so
		// modern clients negotiate h2 and legacy clients still work.
		srv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1"},
		}
		entry := listenerEntry{
			name: "https",
			addr: addr,
			serve: func() error {
				return srv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
			},
			shutdown: srv.Shutdown,
		}
		logger.Debug("primary listener uses TLS",
			slog.String("cert_file", cfg.Server.TLS.CertFile),
			slog.String("key_file", cfg.Server.TLS.KeyFile),
		)
		return entry
	}

	return listenerEntry{
		name:     "http",
		addr:     addr,
		serve:    srv.ListenAndServe,
		shutdown: srv.Shutdown,
	}
}
