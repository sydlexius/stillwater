// CertManager abstracts the source of TLS certificates so the listener
// startup code does not branch on which ACME flavor (or BYO) is configured.
// The autocert (Let's Encrypt / Buypass) implementation lives in this file;
// a future ZeroSSL/lego implementation (#1564) will satisfy the same
// interface without touching listeners.go.
//
// The split is a textbook strategy pattern: the config layer picks an
// implementation once at construction, and the listener consumes the
// interface. A third implementation (DNS-01, mTLS, etc.) drops in without
// listener edits.
package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/sydlexius/stillwater/internal/config"
)

// CertManager produces a tls.Config for the HTTPS listener and an HTTP
// handler that services ACME HTTP-01 challenges. The challenge handler
// MUST be served on port 80 (or whatever public port the CA uses to fetch
// challenges) for cert acquisition or renewal to succeed.
//
// Implementations that do not use HTTP-01 (for example a future DNS-01
// path) may return a no-op handler; the listener layer treats a non-nil
// handler as "mount me on the redirect listener if one exists, otherwise
// spin up a dedicated port-80 listener that ONLY serves HTTP-01 plus a
// 301 to HTTPS for everything else".
type CertManager interface {
	// TLSConfig returns a tls.Config populated with a GetCertificate
	// callback. The caller is expected to set MinVersion / NextProtos to
	// match Stillwater's house style; this method only owns the
	// cert-acquisition surface.
	TLSConfig() *tls.Config

	// HTTPHandler returns an HTTP handler that satisfies HTTP-01
	// challenges. Non-challenge traffic is delegated to fallback when
	// fallback is non-nil; otherwise the handler MUST 301 every other
	// request to its HTTPS counterpart so a port-80 dedicated listener
	// stays useful even outside of cert renewal windows. Passing nil for
	// fallback yields the autocert-default redirect-to-HTTPS behavior.
	HTTPHandler(fallback http.Handler) http.Handler
}

// NewAutocertManager constructs a CertManager backed by
// golang.org/x/crypto/acme/autocert. It validates the ACME configuration
// and ensures the cache directory exists; cert acquisition itself is
// deferred until the first TLS handshake reaches the listener.
//
// The cache directory is critical: ACME issuers (notably Let's Encrypt)
// rate-limit certificate orders aggressively, and autocert reads cached
// certs to avoid re-ordering on every restart. The default location is
// "<dir(cfg.Database.Path)>/acme-cache" so containerized deploys
// (typically /config) and bare-metal deploys (arbitrary paths) both
// persist the cache alongside the database without further configuration.
//
// Email is recommended but not strictly required: ACME CAs use it for
// expiry notifications and revocation. autocert tolerates an empty value.
func NewAutocertManager(cfg *config.Config, logger *slog.Logger) (CertManager, error) {
	if cfg == nil {
		return nil, errors.New("server: nil config")
	}
	if logger == nil {
		return nil, errors.New("server: nil logger")
	}
	domain := cfg.ACME.Domain
	if domain == "" {
		return nil, errors.New("server: ACME domain is required")
	}

	cacheDir := cfg.ACME.CacheDir
	if cacheDir == "" {
		// Co-locate with the database so a bind-mount that persists the
		// db also persists the ACME cache. Operators who set a custom
		// SW_DB_PATH automatically pick up a sibling acme-cache without
		// having to set SW_ACME_CACHE_DIR too.
		cacheDir = filepath.Join(filepath.Dir(cfg.Database.Path), "acme-cache")
	}
	// 0700: ACME account keys + private keys live here. Restrict to the
	// Stillwater process owner; group/world readability would be a
	// secret-leak waiting to happen.
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("server: create ACME cache dir %q: %w", cacheDir, err)
	}

	mgr := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(domain),
		Cache:      autocert.DirCache(cacheDir),
		Email:      cfg.ACME.Email,
	}
	// Optional directory URL (staging vs production, or a Buypass URL).
	// autocert accepts a directory URL via the embedded acme.Client; an
	// empty Client is fine for Let's Encrypt production (autocert's
	// default).
	if directoryURL := cfg.ACME.CA; directoryURL != "" {
		mgr.Client = &acme.Client{DirectoryURL: directoryURL}
	}

	// Email is operator PII -- log only whether it was supplied, not the
	// address itself. Per CLAUDE.md: scrub sensitive values from logs.
	logger.Info("ACME (autocert) configured",
		slog.String("domain", domain),
		slog.String("cache_dir", cacheDir),
		slog.Bool("email_configured", cfg.ACME.Email != ""),
		slog.String("directory_url", cfg.ACME.CA),
	)

	return &autocertManager{mgr: mgr}, nil
}

// autocertManager is the production CertManager backed by autocert.
type autocertManager struct {
	mgr *autocert.Manager
}

// TLSConfig delegates to autocert. The returned config has GetCertificate
// set; the caller layers on MinVersion / NextProtos at the listener.
func (a *autocertManager) TLSConfig() *tls.Config {
	return a.mgr.TLSConfig()
}

// HTTPHandler exposes autocert's HTTP-01 challenge handler. autocert's own
// implementation handles the fallback contract: a nil fallback yields a
// 301-to-HTTPS for every non-challenge request, which is exactly what a
// dedicated port-80 listener should do.
func (a *autocertManager) HTTPHandler(fallback http.Handler) http.Handler {
	return a.mgr.HTTPHandler(fallback)
}
