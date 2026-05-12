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

	"github.com/quic-go/quic-go/http3"
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
//
//nolint:contextcheck // boot-time entry point; ctx originates from main's signal handler and is the long-lived app context, not inherited from a caller's request ctx
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

	entries, err := buildEntries(cfg, handler, logger)
	if err != nil {
		return err
	}
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
// configuration. Today this is a single HTTP-or-HTTPS server (optionally
// paired with an ACME HTTP-01 challenge listener); future M47 PRs append
// additional entries (HTTP-to-HTTPS redirect, HTTP/3) here. Wrapping in a
// helper keeps RunListeners' top-level flow short and gives follow-up PRs
// an obvious place to splice in their entries without rewriting the
// shutdown harness.
//
// Port-80 multiplex contract: the ACME challenge listener and the HTTP
// redirect listener both want plain-HTTP on the same port (80 by default,
// or HTTPRedirect.Port if overridden). When ACME is on, the challenge
// listener does both jobs -- autocert.Manager.HTTPHandler(nil) serves
// /.well-known/acme-challenge/ requests and 301-redirects everything else
// to HTTPS. So we register the challenge listener and skip the dedicated
// redirect listener; double-binding the port would race for the socket.
// When ACME is off and HTTPRedirect.Port is set with BYO TLS active, the
// dedicated redirect listener is the right (and only) choice.
func buildEntries(cfg *config.Config, handler http.Handler, logger *slog.Logger) ([]listenerEntry, error) {
	// Build the cert manager first so any misconfiguration (missing cache
	// dir permissions, malformed directory URL) surfaces before we bind
	// any sockets.
	var certMgr CertManager
	if cfg.ACME.Domain != "" {
		mgr, err := NewAutocertManager(cfg, logger)
		if err != nil {
			return nil, fmt.Errorf("server: ACME setup: %w", err)
		}
		certMgr = mgr
	}

	primary := buildPrimaryListener(cfg, handler, logger, certMgr)
	entries := []listenerEntry{primary}

	switch {
	case certMgr != nil:
		// ACME path: the challenge listener owns port 80, multiplexing
		// HTTP-01 challenges with a 301-to-HTTPS fallback. validate()
		// already enforced ACME-vs-BYO mutual exclusion.
		entries = append(entries, buildAcmeChallengeListener(cfg, certMgr, logger))
	case cfg.Server.HTTPRedirect.Port > 0 && cfg.Server.TLS.Enabled():
		// BYO-TLS path: dedicated redirect listener. The TLS-active guard
		// is belt-and-suspenders -- validate() already rejects redirect
		// without TLS, but a config path that bypasses validate() should
		// not silently spin up a redirect-to-itself.
		entries = append(entries, buildRedirectListener(cfg, logger))
	}
	if entry, ok := buildHTTP3Listener(cfg, handler, logger); ok {
		entries = append(entries, entry)
	}
	return entries, nil
}

// EffectiveHTTP3Port returns the UDP port the HTTP/3 listener will bind when
// enabled, or 0 when HTTP/3 is disabled or TLS is not configured. Callers use
// this to populate the Alt-Svc middleware and the read-only TLS status card
// from the same source of truth as the listener layer.
func EffectiveHTTP3Port(cfg *config.Config) int {
	if cfg == nil || !cfg.Server.HTTP3.Enabled || !cfg.Server.TLS.Enabled() {
		return 0
	}
	if cfg.Server.HTTP3.Port != 0 {
		return cfg.Server.HTTP3.Port
	}
	if cfg.Server.TLS.Port != 0 {
		return cfg.Server.TLS.Port
	}
	return cfg.Server.Port
}

// buildHTTP3Listener returns an entry for an HTTP/3 (QUIC) UDP listener when
// SW_HTTP3_ENABLED is true and TLS is configured. The HTTP/3 server reuses
// the supplied http.Handler (so request-handling code is identical to the
// TCP HTTPS path) and serves on the same effective port over UDP. When
// HTTP/3 is disabled or TLS is missing the second return value is false and
// no listener is registered.
//
// quic-go's http3.Server.Close aborts in-flight requests rather than
// gracefully draining: it is the only shutdown primitive the library
// exposes, so we wrap it in the listenerEntry.shutdown signature.
func buildHTTP3Listener(cfg *config.Config, handler http.Handler, logger *slog.Logger) (listenerEntry, bool) {
	port := EffectiveHTTP3Port(cfg)
	if port == 0 {
		return listenerEntry{}, false
	}
	addr := fmt.Sprintf(":%d", port)

	// Reuse the HTTPS listener's TLS material (cert/key files). HTTP/3
	// mandates TLS 1.3, but quic-go enforces that internally; we only need
	// to provide the certificate. Setting NextProtos h3 is required so the
	// QUIC handshake advertises the right ALPN value.
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h3"},
	}
	cert, err := tls.LoadX509KeyPair(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
	if err != nil {
		// Defer the error to serve(): keeping the failure on the goroutine
		// path means the errgroup propagates it the same way it would for a
		// bind failure, instead of buildEntries returning early.
		return listenerEntry{
			name: "http3",
			addr: addr,
			serve: func() error {
				return fmt.Errorf("load TLS keypair for HTTP/3: %w", err)
			},
			shutdown: func(_ context.Context) error { return nil },
		}, true
	}
	tlsCfg.Certificates = []tls.Certificate{cert}

	srv := &http3.Server{
		Addr:      addr,
		Handler:   handler,
		TLSConfig: tlsCfg,
		Port:      port,
	}
	logger.Debug("HTTP/3 listener configured",
		slog.String("addr", addr),
		slog.String("cert_file", cfg.Server.TLS.CertFile),
	)
	return listenerEntry{
		name: "http3",
		addr: addr,
		// http3.Server has no ListenAndServeTLS-like distinction; the
		// TLSConfig.Certificates set above is sufficient.
		serve: srv.ListenAndServe,
		// http3.Server.Close ignores its context (no graceful drain
		// available); wrap the signature so the errgroup harness can
		// invoke it like every other entry.
		shutdown: func(_ context.Context) error { return srv.Close() },
	}, true
}

// buildPrimaryListener returns the entry for the main HTTP/HTTPS server.
// When ACME is on (certMgr != nil) the listener serves HTTPS with a
// tls.Config sourced from the autocert manager. When direct (BYO) TLS is
// configured, the listener serves HTTPS using the cert/key files. In all
// other cases the listener serves plain HTTP.
//
// All TLS branches enforce TLS 1.2+ and ALPN advertising h2 then http/1.1.
func buildPrimaryListener(cfg *config.Config, handler http.Handler, logger *slog.Logger, certMgr CertManager) listenerEntry {
	acmeOn := certMgr != nil
	byoTLS := cfg.Server.TLS.Enabled()
	tlsConfigured := acmeOn || byoTLS

	// Effective bind port: when TLS is on but TLS.Port is unset, HTTPS
	// reuses Server.Port (collapse semantics). ACME paths follow the same
	// rule -- operators who set SW_ACME_DOMAIN without SW_TLS_PORT get
	// HTTPS on Server.Port (typically 1973) and pair that with their
	// real-internet port-80 forward for HTTP-01 challenges.
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

	switch {
	case acmeOn:
		// Layer Stillwater's house TLS policy on top of the autocert
		// GetCertificate callback. autocert's TLSConfig already includes
		// ALPN entries for the tls-alpn-01 challenge type; preserve them
		// so a future flip to tls-alpn-01 (no port-80 dependency) does
		// not need a code change here.
		base := certMgr.TLSConfig()
		base.MinVersion = tls.VersionTLS12
		// Prepend h2 + http/1.1 ahead of any ACME-specific protos so
		// real client traffic still negotiates HTTP/2 first.
		base.NextProtos = append([]string{"h2", "http/1.1"}, base.NextProtos...)
		srv.TLSConfig = base
		entry := listenerEntry{
			name: "https-acme",
			addr: addr,
			// Empty cert/key file paths tell ListenAndServeTLS to use
			// the GetCertificate callback baked into TLSConfig.
			serve:    func() error { return srv.ListenAndServeTLS("", "") },
			shutdown: srv.Shutdown,
		}
		logger.Debug("primary listener uses ACME (autocert)",
			slog.String("domain", cfg.ACME.Domain),
		)
		return entry
	case byoTLS:
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
	default:
		return listenerEntry{
			name:     "http",
			addr:     addr,
			serve:    srv.ListenAndServe,
			shutdown: srv.Shutdown,
		}
	}
}

// acmeChallengeDefaultPort is the standard plain-HTTP port that ACME CAs
// fetch HTTP-01 challenge tokens from. Operators behind a NAT can map an
// arbitrary public-facing port to whatever Stillwater binds, but the
// default is 80 because that is what every documented LE / Buypass
// example assumes.
const acmeChallengeDefaultPort = 80

// buildAcmeChallengeListener returns the plain-HTTP listener that serves
// the ACME HTTP-01 challenge handler. The handler 301-redirects every
// non-challenge request to HTTPS. When the operator has set
// HTTPRedirect.Port (the #929 redirect listener port) we re-use that port
// to avoid double-binding port 80; the autocert HTTPHandler's
// challenge-or-redirect contract subsumes the redirect listener's
// behavior, so #929's wiring should detect ACME and skip its own bind.
func buildAcmeChallengeListener(cfg *config.Config, certMgr CertManager, logger *slog.Logger) listenerEntry {
	port := cfg.Server.HTTPRedirect.Port
	if port == 0 {
		port = acmeChallengeDefaultPort
	}
	addr := fmt.Sprintf(":%d", port)

	// nil fallback yields autocert's built-in 301-to-HTTPS for every
	// non-challenge request. That keeps the port-80 listener useful even
	// outside renewal windows -- an operator who hits http://host/ gets
	// redirected rather than seeing a connection refused.
	mux := certMgr.HTTPHandler(nil)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// Challenge requests are tiny and finish quickly; modest
		// timeouts are safer than copying the primary listener's 180s
		// WriteTimeout (which exists only to absorb the refresh OOB
		// stream that does not flow through this listener).
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	logger.Debug("ACME challenge listener configured",
		slog.String("addr", addr),
		slog.String("domain", cfg.ACME.Domain),
	)
	return listenerEntry{
		name:     "acme-challenge",
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
