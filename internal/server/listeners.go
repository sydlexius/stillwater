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
	"net"
	"net/http"
	"strconv"
	"strings"
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
	entries := []listenerEntry{buildPrimaryListener(cfg, handler, logger)}
	// The redirect listener is opt-in (HTTPRedirect.Port > 0) AND only meaningful
	// when TLS is active -- there is nowhere to redirect to without an HTTPS
	// listener. validate() rejects the misconfiguration up front; this guard is
	// belt-and-suspenders so a future config path that bypasses validate() does
	// not silently spin up a redirect-to-itself.
	if cfg.Server.HTTPRedirect.Port > 0 && cfg.Server.TLS.Enabled() {
		entries = append(entries, buildRedirectListener(cfg, logger))
	}
	return entries
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

// buildRedirectListener returns an entry for the optional plain-HTTP listener
// that 301s every request to the HTTPS listener. It is only registered when
// TLS is active and HTTPRedirect.Port is non-zero (see buildEntries).
//
// The effective HTTPS port is the same value the primary listener resolved:
// TLS.Port when set, otherwise Server.Port (collapse mode). validate() rejects
// the redirect-port-equals-TLS-port collision so this listener is guaranteed
// to bind a distinct port.
func buildRedirectListener(cfg *config.Config, logger *slog.Logger) listenerEntry {
	httpsPort := cfg.Server.TLS.Port
	if httpsPort == 0 {
		httpsPort = cfg.Server.Port
	}
	addr := fmt.Sprintf(":%d", cfg.Server.HTTPRedirect.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           redirectHandler(httpsPort),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	logger.Debug("redirect listener configured",
		slog.String("addr", addr),
		slog.Int("https_port", httpsPort),
	)
	return listenerEntry{
		name:     "http-redirect",
		addr:     addr,
		serve:    srv.ListenAndServe,
		shutdown: srv.Shutdown,
	}
}

// redirectHandler returns an http.Handler that issues a 301 Moved Permanently
// to the same path on https://<host>:<httpsPort>. Query string and fragment
// from the original request are preserved via r.RequestURI (which includes
// the raw path + query exactly as the client sent it).
//
// Host parsing handles three shapes:
//   - "example.com"        -- no port, use as-is
//   - "example.com:80"     -- strip the explicit port, replace with httpsPort
//   - "[::1]:80"           -- IPv6 literal, brackets preserved by net.SplitHostPort
//
// When httpsPort is 443 the port suffix is omitted from the Location header so
// the browser address bar shows the canonical URL ("https://example.com/path"
// instead of "https://example.com:443/path").
func redirectHandler(httpsPort int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject anything that is not a normal origin-form request. CONNECT
		// (RequestURI = "host:port") and server-form OPTIONS (RequestURI = "*")
		// would splice into a malformed Location otherwise.
		if !strings.HasPrefix(r.RequestURI, "/") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		// Reject Hosts whose hostname portion contains characters that have no
		// place in a URL authority. net/http already blocks CRLF in header
		// values, but spaces, control bytes, and stray quotes would still
		// produce a Location that browsers either reject or interpret in
		// surprising ways.
		if !isValidHostName(host) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// Re-add brackets for IPv6 literals so the resulting URL is well-formed.
		if isIPv6Literal(host) {
			host = "[" + host + "]"
		}
		var target string
		if httpsPort == 443 {
			target = "https://" + host + r.RequestURI
		} else {
			target = "https://" + host + ":" + strconv.Itoa(httpsPort) + r.RequestURI
		}
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusMovedPermanently)
	})
}

// isValidHostName accepts a hostname or IP literal containing only the ASCII
// characters legal in a URL host: letters, digits, dot, hyphen, colon (for
// IPv6 literals after net.SplitHostPort), and the brackets net/url permits.
// Rejects empty input and anything containing whitespace, slashes, or other
// reserved/control bytes.
func isValidHostName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.', c == '-', c == ':':
		default:
			return false
		}
	}
	return true
}

// isIPv6Literal reports whether host is a bare IPv6 address (no brackets,
// no port). net.SplitHostPort strips the brackets when present, so after that
// step a colon in the remaining string means we are looking at an IPv6 host
// that needs its brackets restored before we splice it back into a URL.
func isIPv6Literal(host string) bool {
	return net.ParseIP(host) != nil && strings.Contains(host, ":")
}
