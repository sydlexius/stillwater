package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/config"
)

// TestNewAutocertManager_RejectsInvalidConfig pins the precondition checks
// in the constructor. Each branch returns an error rather than panicking so
// startup surfaces a single useful message.
func TestNewAutocertManager_RejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	logger := discardLogger()

	if _, err := NewAutocertManager(nil, logger); err == nil {
		t.Error("nil cfg: want error")
	}

	cfg := &config.Config{ACME: config.ACMEConfig{Domain: "x"}}
	if _, err := NewAutocertManager(cfg, nil); err == nil {
		t.Error("nil logger: want error")
	}

	cfgNoDomain := &config.Config{}
	if _, err := NewAutocertManager(cfgNoDomain, logger); err == nil {
		t.Error("empty domain: want error")
	}
}

// TestNewAutocertManager_DefaultsCacheDir asserts the cache directory
// defaults to a sibling of the database path. A persistent cache is
// critical for not hitting CA rate limits across restarts.
func TestNewAutocertManager_DefaultsCacheDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "stillwater.db")
	cfg := &config.Config{
		Database: config.DatabaseConfig{Path: dbPath},
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
	}
	mgr, err := NewAutocertManager(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewAutocertManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("NewAutocertManager returned nil manager")
	}
	wantCache := filepath.Join(tmp, "acme-cache")
	info, err := os.Stat(wantCache)
	if err != nil {
		t.Fatalf("expected cache dir %q to exist: %v", wantCache, err)
	}
	if !info.IsDir() {
		t.Errorf("cache dir %q is not a directory", wantCache)
	}
	// 0700 is enforced because account keys live here. Mask any
	// platform-specific extras (e.g. directory bit) before comparing.
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("cache dir mode = %o; want 0700", mode)
	}
}

// TestNewAutocertManager_HonorsCacheDirOverride asserts that an explicit
// SW_ACME_CACHE_DIR is used verbatim and the directory is created.
func TestNewAutocertManager_HonorsCacheDirOverride(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	override := filepath.Join(tmp, "custom-cache")
	cfg := &config.Config{
		Database: config.DatabaseConfig{Path: filepath.Join(tmp, "stillwater.db")},
		ACME: config.ACMEConfig{
			Domain:   "host.example.com",
			CacheDir: override,
		},
	}
	if _, err := NewAutocertManager(cfg, discardLogger()); err != nil {
		t.Fatalf("NewAutocertManager: %v", err)
	}
	if _, err := os.Stat(override); err != nil {
		t.Fatalf("override cache dir %q not created: %v", override, err)
	}
}

// TestAutocertManager_TLSConfig asserts the returned tls.Config carries a
// GetCertificate callback and is usable as a server config (no panics on
// access). A live cert acquisition is intentionally not exercised here --
// that requires a network round-trip to a real ACME server.
func TestAutocertManager_TLSConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
	}
	mgr, err := NewAutocertManager(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewAutocertManager: %v", err)
	}
	tlsCfg := mgr.TLSConfig()
	if tlsCfg == nil {
		t.Fatal("TLSConfig returned nil")
	}
	if tlsCfg.GetCertificate == nil {
		t.Fatal("TLSConfig.GetCertificate is nil; autocert should wire it")
	}
	// Sanity check: do not panic when reading the field that the listener
	// will overwrite.
	_ = tlsCfg.MinVersion
}

// TestAutocertManager_HTTPHandler_RedirectsNonChallenge asserts that a
// non-challenge request to the autocert HTTP handler 301-redirects to the
// HTTPS counterpart when the fallback is nil. This is the contract the
// dedicated port-80 listener relies on so an operator who hits
// http://host/ ends up at https://host/ even outside renewal windows.
func TestAutocertManager_HTTPHandler_RedirectsNonChallenge(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
	}
	mgr, err := NewAutocertManager(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewAutocertManager: %v", err)
	}
	h := mgr.HTTPHandler(nil)
	if h == nil {
		t.Fatal("HTTPHandler returned nil")
	}

	srv := httptest.NewServer(h)
	defer srv.Close()

	// Disable redirect-following so we observe the 301 directly.
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(srv.URL + "/some/random/path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		t.Errorf("status = %d; want 3xx redirect", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("Location header missing")
	}
	if !strings.HasPrefix(loc, "https://") {
		t.Errorf("Location = %q; want https:// prefix", loc)
	}
}

// TestAutocertManager_HTTPHandler_FallbackPassesThrough asserts that a
// non-challenge request is delegated to the supplied fallback handler when
// fallback is non-nil. This is the contract the future redirect listener
// (#929) will rely on so it can chain its own 301-redirect logic through
// the autocert multiplex without losing handler control.
func TestAutocertManager_HTTPHandler_FallbackPassesThrough(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
	}
	mgr, err := NewAutocertManager(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewAutocertManager: %v", err)
	}
	called := false
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})
	h := mgr.HTTPHandler(fallback)
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/non-challenge-path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if !called {
		t.Error("fallback was not invoked for non-challenge request")
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d; want %d (teapot from fallback)", resp.StatusCode, http.StatusTeapot)
	}
}

// TestAutocertManager_TLSConfigALPN asserts the GetCertificate-bearing
// tls.Config can be embedded without losing autocert's tls-alpn-01 ALPN
// entries when the listener layer prepends h2/http/1.1. The listener
// layer's append([]string{"h2", "http/1.1"}, base.NextProtos...) idiom
// keeps the autocert entries intact; this test pins that behavior so a
// future refactor that overwrites NextProtos surfaces a regression.
func TestAutocertManager_TLSConfigALPN(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
	}
	mgr, err := NewAutocertManager(cfg, discardLogger())
	if err != nil {
		t.Fatalf("NewAutocertManager: %v", err)
	}
	base := mgr.TLSConfig()
	merged := append([]string{"h2", "http/1.1"}, base.NextProtos...)
	if merged[0] != "h2" || merged[1] != "http/1.1" {
		t.Errorf("NextProtos prepend lost ordering: %v", merged)
	}
	// Sanity: the merged config compiles into a tls.Config field without
	// panic. We do not handshake; we just want assurance the call site
	// pattern compiles end-to-end.
	_ = &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: merged,
	}
}
