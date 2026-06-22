package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/certificate"

	"github.com/sydlexius/stillwater/internal/config"
	"github.com/sydlexius/stillwater/internal/encryption"
)

// testEncryptor returns an Encryptor backed by a freshly generated key. Key
// generation is local (crypto/rand), so the tests stay network-free.
func testEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

// selfSignedCert returns a throwaway tls.Certificate (with Leaf populated) for
// exercising the served-certificate paths without contacting a CA. dnsName is
// added as a DNS SAN so VerifyHostname checks in ensureCertificate/loadCachedCert
// behave correctly for the configured identifier.
func selfSignedCert(t *testing.T, dnsName string, notAfter time.Time) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func TestNewLegoManager_RejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	logger := discardLogger()
	enc := testEncryptor(t)
	validCfg := &config.Config{
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
		Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
	}

	cases := []struct {
		name string
		cfg  *config.Config
		log  any
		enc  *encryption.Encryptor
	}{
		{"nil config", nil, logger, enc},
		{"nil logger", validCfg, nil, enc},
		{"nil encryptor", validCfg, logger, nil},
		{"no identifier", &config.Config{
			Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
		}, logger, enc},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var lg = logger
			if tc.log == nil {
				lg = nil
			}
			if _, err := newLegoManager(tc.cfg, lg, tc.enc); err == nil {
				t.Fatalf("newLegoManager(%s) = nil error; want error", tc.name)
			}
		})
	}
}

func TestNewLegoManager_DefaultsCacheDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := &config.Config{
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
		Database: config.DatabaseConfig{Path: filepath.Join(tmp, "stillwater.db")},
	}
	m, err := newLegoManager(cfg, discardLogger(), testEncryptor(t))
	if err != nil {
		t.Fatalf("newLegoManager: %v", err)
	}
	wantDir := filepath.Join(tmp, "acme-cache")
	if m.store.dir != wantDir {
		t.Errorf("store.dir = %q; want %q", m.store.dir, wantDir)
	}
	if info, err := os.Stat(wantDir); err != nil {
		t.Fatalf("cache dir not created: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("cache dir perm = %o; want 700", info.Mode().Perm())
	}
}

func TestNewLegoManager_CADefaults(t *testing.T) {
	t.Parallel()
	base := func() *config.Config {
		return &config.Config{
			Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
		}
	}
	t.Run("EAB defaults to ZeroSSL", func(t *testing.T) {
		t.Parallel()
		cfg := base()
		cfg.ACME = config.ACMEConfig{Domain: "host.example.com", EabKeyID: "kid", EabMacKey: "mac"}
		m, err := newLegoManager(cfg, discardLogger(), testEncryptor(t))
		if err != nil {
			t.Fatalf("newLegoManager: %v", err)
		}
		if m.caURL != zerosslDirectoryURL {
			t.Errorf("caURL = %q; want ZeroSSL %q", m.caURL, zerosslDirectoryURL)
		}
	})
	t.Run("no EAB defaults to Let's Encrypt", func(t *testing.T) {
		t.Parallel()
		cfg := base()
		cfg.ACME = config.ACMEConfig{IP: "203.0.113.5"}
		m, err := newLegoManager(cfg, discardLogger(), testEncryptor(t))
		if err != nil {
			t.Fatalf("newLegoManager: %v", err)
		}
		if !strings.Contains(m.caURL, "letsencrypt.org") {
			t.Errorf("caURL = %q; want a Let's Encrypt URL", m.caURL)
		}
	})
	t.Run("explicit CA honored", func(t *testing.T) {
		t.Parallel()
		cfg := base()
		cfg.ACME = config.ACMEConfig{Domain: "host.example.com", CA: "https://example.test/dir"}
		m, err := newLegoManager(cfg, discardLogger(), testEncryptor(t))
		if err != nil {
			t.Fatalf("newLegoManager: %v", err)
		}
		if m.caURL != "https://example.test/dir" {
			t.Errorf("caURL = %q; want the explicit URL", m.caURL)
		}
	})
}

func TestLegoManager_TLSConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
		Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
	}
	m, err := newLegoManager(cfg, discardLogger(), testEncryptor(t))
	if err != nil {
		t.Fatalf("newLegoManager: %v", err)
	}
	tc := m.TLSConfig()
	if tc.GetCertificate == nil {
		t.Fatal("TLSConfig().GetCertificate is nil")
	}
	// No cert yet -> handshake error.
	if _, err := tc.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Error("GetCertificate before issuance = nil error; want error")
	}
	// Install a cert -> served.
	cert := selfSignedCert(t, "host.example.com", time.Now().Add(90*24*time.Hour))
	m.mu.Lock()
	m.cert = cert
	m.mu.Unlock()
	got, err := tc.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate after issuance: %v", err)
	}
	if got != cert {
		t.Error("GetCertificate did not return the installed certificate")
	}
}

func TestLegoManager_HTTPHandler_ServesChallenge(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	const token = "test-token-abc"
	const keyAuth = "test-token-abc.keyauthvalue"
	if err := m.Present("host.example.com", token, keyAuth); err != nil {
		t.Fatalf("Present: %v", err)
	}

	h := m.HTTPHandler(nil)

	// Known token -> keyAuth body.
	req := httptest.NewRequest(http.MethodGet, acmeChallengeURIPrefix+token, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("challenge status = %d; want 200", rec.Code)
	}
	if rec.Body.String() != keyAuth {
		t.Errorf("challenge body = %q; want %q", rec.Body.String(), keyAuth)
	}

	// Unknown token -> 404.
	req = httptest.NewRequest(http.MethodGet, acmeChallengeURIPrefix+"unknown", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown-token status = %d; want 404", rec.Code)
	}

	// After CleanUp the token is gone.
	if err := m.CleanUp("host.example.com", token, keyAuth); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, acmeChallengeURIPrefix+token, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("post-cleanup status = %d; want 404", rec.Code)
	}
}

func TestLegoManager_HTTPHandler_RedirectsNonChallenge(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	h := m.HTTPHandler(nil)

	req := httptest.NewRequest(http.MethodGet, "http://host.example.com:80/some/path?q=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("redirect status = %d; want 301", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "https://host.example.com/some/path?q=1" {
		t.Errorf("Location = %q; want https://host.example.com/some/path?q=1", loc)
	}
}

func TestLegoManager_HTTPHandler_FallbackPassesThrough(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	called := false
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})
	h := m.HTTPHandler(fallback)

	req := httptest.NewRequest(http.MethodGet, "http://host.example.com/app", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Error("fallback was not invoked for a non-challenge request")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d; want 418 (fallback)", rec.Code)
	}
}

func TestEncryptedStore_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &encryptedStore{dir: dir, encryptor: testEncryptor(t)}

	// Absent entry -> found=false, no error.
	if _, found, err := store.load("missing"); err != nil || found {
		t.Fatalf("load(missing) = found %v err %v; want false, nil", found, err)
	}

	payload := []byte(`{"secret":"value","bytes":[1,2,3]}`)
	if err := store.save("account", payload); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, found, err := store.load("account")
	if err != nil || !found {
		t.Fatalf("load = found %v err %v; want true, nil", found, err)
	}
	if string(got) != string(payload) {
		t.Errorf("round-trip = %q; want %q", got, payload)
	}

	// On-disk content must be encrypted, not plaintext.
	raw, err := os.ReadFile(filepath.Join(dir, "account.enc"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "value") {
		t.Error("on-disk blob contains plaintext; expected ciphertext")
	}

	// File perms must be 0600.
	info, err := os.Stat(filepath.Join(dir, "account.enc"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("enc file perm = %o; want 600", info.Mode().Perm())
	}
}

// TestEncryptedStore_AccountRoundTrip verifies the ACME account survives a
// save/load through the encrypted store, including the EC key PEM encoding, so a
// restart reuses the account instead of re-registering.
func TestEncryptedStore_AccountRoundTrip(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)

	// Build a fresh (unregistered) user, then persist + reload it.
	user, err := m.loadOrCreateUser()
	if err != nil {
		t.Fatalf("loadOrCreateUser: %v", err)
	}
	if user.registration != nil {
		t.Fatal("new user should be unregistered")
	}
	if err := m.saveAccount(user); err != nil {
		t.Fatalf("saveAccount: %v", err)
	}
	reloaded, err := m.loadOrCreateUser()
	if err != nil {
		t.Fatalf("reload loadOrCreateUser: %v", err)
	}
	if reloaded.email != user.email {
		t.Errorf("email = %q; want %q", reloaded.email, user.email)
	}
	// Keys must encode to the same PEM (same key reused on restart).
	origPEM, err := encodeECKey(user.key)
	if err != nil {
		t.Fatalf("encode orig key: %v", err)
	}
	reloadedPEM, err := encodeECKey(reloaded.key)
	if err != nil {
		t.Fatalf("encode reloaded key: %v", err)
	}
	if string(origPEM) != string(reloadedPEM) {
		t.Error("reloaded account key differs from the saved key")
	}
}

// TestECKeyRoundTrip locks in the EC key PEM encode/decode helpers and the
// non-EC rejection path.
func TestECKeyRoundTrip(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pemBytes, err := encodeECKey(key)
	if err != nil {
		t.Fatalf("encodeECKey: %v", err)
	}
	if block, _ := pem.Decode(pemBytes); block == nil || block.Type != "EC PRIVATE KEY" {
		t.Fatalf("encoded PEM is not an EC PRIVATE KEY block")
	}
	decoded, err := decodeECKey(pemBytes)
	if err != nil {
		t.Fatalf("decodeECKey: %v", err)
	}
	if _, ok := decoded.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("decoded key type = %T; want *ecdsa.PrivateKey", decoded)
	}
	// Garbage PEM is rejected.
	if _, err := decodeECKey([]byte("not a pem")); err == nil {
		t.Error("decodeECKey(garbage) = nil error; want error")
	}
}

// TestNewLegoManager_CancelledContextNoWork verifies the exported constructor
// returns a usable manager and that, with an already-canceled context, the
// background goroutine performs no work (the path that keeps the rest of the
// suite, and CI's bare runner, free of network I/O).
func TestNewLegoManager_CancelledContextNoWork(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
		Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the goroutine can do anything
	mgr, err := NewLegoManager(ctx, cfg, discardLogger(), testEncryptor(t))
	if err != nil {
		t.Fatalf("NewLegoManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("NewLegoManager returned nil manager")
	}
	if mgr.TLSConfig().GetCertificate == nil {
		t.Error("TLSConfig().GetCertificate is nil")
	}
}

// newTestLegoManager builds a network-free legoManager for handler/store tests.
func newTestLegoManager(t *testing.T) *legoManager {
	t.Helper()
	cfg := &config.Config{
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
		Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
	}
	m, err := newLegoManager(cfg, discardLogger(), testEncryptor(t))
	if err != nil {
		t.Fatalf("newLegoManager: %v", err)
	}
	return m
}

// makeCertPEM returns PEM-encoded certificate and ECDSA private key bytes for
// a self-signed certificate valid from 1 hour ago until notAfter. dnsName is
// set as both CommonName and a DNS SAN so VerifyHostname works correctly.
// Usable as storedCert.Certificate / storedCert.PrivateKey.
func makeCertPEM(t *testing.T, dnsName string, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestParseKeyPair(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := makeCertPEM(t, "test.example.com", time.Now().Add(90*24*time.Hour))

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		cert, err := parseKeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatalf("parseKeyPair: %v", err)
		}
		if cert.Leaf == nil {
			t.Error("Leaf should be populated by parseKeyPair")
		}
	})

	t.Run("invalid PEM", func(t *testing.T) {
		t.Parallel()
		if _, err := parseKeyPair([]byte("not-a-cert"), []byte("not-a-key")); err == nil {
			t.Error("parseKeyPair(garbage) = nil error; want error")
		}
	})
}

func TestLegoUser_Getters(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	u := &legoUser{email: "ops@example.com", key: key}
	if got := u.GetEmail(); got != "ops@example.com" {
		t.Errorf("GetEmail = %q; want ops@example.com", got)
	}
	if u.GetRegistration() != nil {
		t.Error("GetRegistration should be nil for an unregistered user")
	}
	if u.GetPrivateKey() != key {
		t.Error("GetPrivateKey did not return the expected key")
	}
}

func TestLegoManager_LoadCachedCert(t *testing.T) {
	t.Parallel()

	t.Run("cache miss returns nil", func(t *testing.T) {
		t.Parallel()
		m := newTestLegoManager(t)
		if err := m.loadCachedCert(); err != nil {
			t.Fatalf("loadCachedCert (miss) = %v; want nil", err)
		}
		m.mu.RLock()
		cert := m.cert
		m.mu.RUnlock()
		if cert != nil {
			t.Error("m.cert should remain nil after a cache miss")
		}
	})

	t.Run("bad JSON returns error", func(t *testing.T) {
		t.Parallel()
		m := newTestLegoManager(t)
		if err := m.store.save(certCacheName, []byte("not-json")); err != nil {
			t.Fatalf("store.save: %v", err)
		}
		if err := m.loadCachedCert(); err == nil {
			t.Error("loadCachedCert (bad JSON) = nil error; want error")
		}
	})

	t.Run("bad PEM returns error", func(t *testing.T) {
		t.Parallel()
		m := newTestLegoManager(t)
		sc := storedCert{Certificate: []byte("garbage-cert"), PrivateKey: []byte("garbage-key")}
		data, _ := json.Marshal(sc)
		if err := m.store.save(certCacheName, data); err != nil {
			t.Fatalf("store.save: %v", err)
		}
		if err := m.loadCachedCert(); err == nil {
			t.Error("loadCachedCert (bad PEM) = nil error; want error")
		}
	})

	t.Run("valid cert is installed", func(t *testing.T) {
		t.Parallel()
		m := newTestLegoManager(t)
		certPEM, keyPEM := makeCertPEM(t, "host.example.com", time.Now().Add(90*24*time.Hour))
		sc := storedCert{Certificate: certPEM, PrivateKey: keyPEM}
		data, err := json.Marshal(sc)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if err := m.store.save(certCacheName, data); err != nil {
			t.Fatalf("store.save: %v", err)
		}
		if err := m.loadCachedCert(); err != nil {
			t.Fatalf("loadCachedCert (valid): %v", err)
		}
		m.mu.RLock()
		cert := m.cert
		m.mu.RUnlock()
		if cert == nil {
			t.Fatal("m.cert should be non-nil after loading a valid cached cert")
		}
		if cert.Leaf == nil {
			t.Error("cert.Leaf should be populated")
		}
	})
}

// TestLegoManager_SaveCert verifies that saveCert persists the certificate and
// that loadCachedCert can reload it (end-to-end round-trip).
func TestLegoManager_SaveCert(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	certPEM, keyPEM := makeCertPEM(t, "host.example.com", time.Now().Add(90*24*time.Hour))

	res := &certificate.Resource{
		Domain:      "host.example.com",
		Certificate: certPEM,
		PrivateKey:  keyPEM,
	}
	if err := m.saveCert(res); err != nil {
		t.Fatalf("saveCert: %v", err)
	}

	// Clear the in-memory cert and reload from store to verify persistence.
	m.mu.Lock()
	m.cert = nil
	m.mu.Unlock()
	if err := m.loadCachedCert(); err != nil {
		t.Fatalf("loadCachedCert after saveCert: %v", err)
	}
	m.mu.RLock()
	cert := m.cert
	m.mu.RUnlock()
	if cert == nil {
		t.Error("cert not loaded after saveCert round-trip")
	}
}

// TestEnsureCertificate_ShortCircuits verifies the two early-return paths in
// ensureCertificate that avoid all network I/O.
func TestEnsureCertificate_ShortCircuits(t *testing.T) {
	t.Parallel()

	t.Run("fresh cert skips obtain", func(t *testing.T) {
		t.Parallel()
		m := newTestLegoManager(t)
		// Cert must match the manager's identifier ("host.example.com") so the
		// SAN check does not force a re-obtain.
		fresh := selfSignedCert(t, "host.example.com", time.Now().Add(60*24*time.Hour))
		m.mu.Lock()
		m.cert = fresh
		m.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Must return without calling buildClient (which would dial the network).
		m.ensureCertificate(ctx)
	})

	t.Run("canceled context skips obtain", func(t *testing.T) {
		t.Parallel()
		m := newTestLegoManager(t)
		// No cert and an already-canceled context: should return at the ctx.Err() check.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		m.ensureCertificate(ctx)
	})
}

// TestLegoManager_Run_FreshCert exercises the run() ticker loop with a
// pre-installed valid certificate, so ensureCertificate short-circuits without
// making any network calls.
func TestLegoManager_Run_FreshCert(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	fresh := selfSignedCert(t, "host.example.com", time.Now().Add(60*24*time.Hour))
	m.mu.Lock()
	m.cert = fresh
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.run(ctx)
	}()
	// Give the goroutine time to pass loadCachedCert and enter the ticker loop
	// before canceling; loadCachedCert + ensureCertificate(fresh) are both
	// sub-millisecond, so 50ms is generous even on slow CI machines.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after context cancellation")
	}
}

// TestLegoManager_AcquireInitialCert_RetriesBeforeRenewCadence asserts that a
// failing initial obtain is retried with short backoff (not the 12h renewal
// cadence) until a certificate is installed. It injects a deterministic obtain
// hook that fails the first two attempts and installs a cert on the third, and
// shrinks the backoff to milliseconds so the loop runs without network I/O.
func TestLegoManager_AcquireInitialCert_RetriesBeforeRenewCadence(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	m.initialBackoff = time.Millisecond
	m.maxBackoff = 5 * time.Millisecond

	var attempts int
	m.ensureFn = func(_ context.Context) {
		attempts++
		if attempts < 3 {
			return // simulate a transient obtain failure: no cert installed
		}
		cert := selfSignedCert(t, "host.example.com", time.Now().Add(60*24*time.Hour))
		m.mu.Lock()
		m.cert = cert
		m.mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ok := m.acquireInitialCert(ctx); !ok {
		t.Fatal("acquireInitialCert = false; want true once a cert is installed")
	}
	if attempts != 3 {
		t.Errorf("ensureFn attempts = %d; want 3 (two failures then success)", attempts)
	}
	if !m.hasCert() {
		t.Error("hasCert() = false after acquireInitialCert succeeded")
	}
}

// TestLegoManager_AcquireInitialCert_CancelStopsRetry asserts the backoff loop
// is context-cancellable: when obtain never succeeds, a canceled context makes
// acquireInitialCert return false promptly instead of retrying forever.
func TestLegoManager_AcquireInitialCert_CancelStopsRetry(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	m.initialBackoff = 10 * time.Millisecond
	m.maxBackoff = 10 * time.Millisecond
	m.ensureFn = func(_ context.Context) {} // never installs a cert

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- m.acquireInitialCert(ctx) }()
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Error("acquireInitialCert = true after cancel; want false (no cert installed)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquireInitialCert did not return after context cancellation")
	}
}

// TestEncryptedStore_TamperedCiphertext verifies that a corrupt on-disk blob
// is rejected, covering the decrypt-error path in load().
func TestEncryptedStore_TamperedCiphertext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &encryptedStore{dir: dir, encryptor: testEncryptor(t)}

	if err := store.save("item", []byte(`{"key":"value"}`)); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Overwrite the file with bytes that are not a valid AES-GCM ciphertext.
	path := filepath.Join(dir, "item.enc")
	if err := os.WriteFile(path, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := store.load("item"); err == nil {
		t.Error("load(tampered) = nil error; want decrypt error")
	}
}

// TestEncryptedStore_SaveWriteError covers the os.WriteFile error path in save()
// by using a read-only cache directory.
func TestEncryptedStore_SaveWriteError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot make dir read-only:", err)
	}
	defer os.Chmod(dir, 0o700) // restore so t.TempDir cleanup can remove it
	store := &encryptedStore{dir: dir, encryptor: testEncryptor(t)}
	if err := store.save("item", []byte("data")); err == nil {
		t.Error("save to read-only dir = nil error; want write error")
	}
}

// TestEncodeECKey_NonECType covers the type-assertion failure branch in encodeECKey.
func TestEncodeECKey_NonECType(t *testing.T) {
	t.Parallel()
	rsakey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	if _, err := encodeECKey(rsakey); err == nil {
		t.Error("encodeECKey(RSA key) = nil error; want type-assertion error")
	}
}

// TestLoadOrCreateUser_StorageErrors verifies that a corrupt cached account
// self-heals: a fresh unregistered account is returned (no hard error).
func TestLoadOrCreateUser_StorageErrors(t *testing.T) {
	t.Parallel()

	assertFreshUser := func(t *testing.T, user *legoUser, err error, label string) {
		t.Helper()
		if err != nil {
			t.Fatalf("loadOrCreateUser (%s) = error %v; want self-heal (fresh user)", label, err)
		}
		if user == nil || user.key == nil {
			t.Errorf("loadOrCreateUser (%s) should return a fresh user on corrupt cache", label)
		}
		if user != nil && user.registration != nil {
			t.Errorf("loadOrCreateUser (%s) self-healed user should be unregistered", label)
		}
	}

	t.Run("bad JSON in store self-heals", func(t *testing.T) {
		t.Parallel()
		m := newTestLegoManager(t)
		if err := m.store.save(accountCacheName, []byte("not-json")); err != nil {
			t.Fatalf("store.save: %v", err)
		}
		user, err := m.loadOrCreateUser()
		assertFreshUser(t, user, err, "bad JSON")
	})

	t.Run("bad private key PEM in store self-heals", func(t *testing.T) {
		t.Parallel()
		m := newTestLegoManager(t)
		sa := storedAccount{Email: "ops@example.com", PrivateKey: []byte("garbage-pem")}
		data, _ := json.Marshal(sa)
		if err := m.store.save(accountCacheName, data); err != nil {
			t.Fatalf("store.save: %v", err)
		}
		user, err := m.loadOrCreateUser()
		assertFreshUser(t, user, err, "bad PEM")
	})
}

// TestSaveAccount_NonECKey covers the encodeECKey error path in saveAccount
// when the account key is not an ECDSA key.
func TestSaveAccount_NonECKey(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	rsakey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	u := &legoUser{email: "ops@example.com", key: rsakey}
	if err := m.saveAccount(u); err == nil {
		t.Error("saveAccount(RSA key) = nil error; want error from encodeECKey")
	}
}

// TestNewLegoManager_MkdirAllError covers the os.MkdirAll error branch in
// newLegoManager by placing a regular file where the cache directory should go.
func TestNewLegoManager_MkdirAllError(t *testing.T) {
	t.Parallel()
	dbDir := t.TempDir()
	// Place a plain file at the path MkdirAll would need to traverse.
	blockPath := filepath.Join(dbDir, "acme-cache")
	if err := os.WriteFile(blockPath, []byte("blocker"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg := &config.Config{
		ACME:     config.ACMEConfig{Domain: "host.example.com"},
		Database: config.DatabaseConfig{Path: filepath.Join(dbDir, "stillwater.db")},
	}
	if _, err := newLegoManager(cfg, discardLogger(), testEncryptor(t)); err == nil {
		t.Error("newLegoManager with blocked cache dir = nil error; want error")
	}
}

// TestLoadCachedCert_StoreLoadError covers the store.load error path in
// loadCachedCert by injecting an invalid (non-base64) ciphertext directly.
func TestLoadCachedCert_StoreLoadError(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	// Write bytes that are not valid base64/GCM ciphertext directly to the file,
	// bypassing the store's encrypt-then-write to force a Decrypt error.
	if err := os.WriteFile(m.store.path(certCacheName), []byte{0xFF, 0xFE, 0xFD}, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := m.loadCachedCert(); err == nil {
		t.Error("loadCachedCert (store.load error) = nil error; want error")
	}
}

// TestEncodeECKey_MarshalError covers the x509.MarshalECPrivateKey error branch
// in encodeECKey. A zero-value *ecdsa.PrivateKey has a nil Curve field; Go's
// x509 package returns "unknown elliptic curve" without panicking, hitting the
// return that is unreachable with any normally-generated key.
func TestEncodeECKey_MarshalError(t *testing.T) {
	t.Parallel()
	if _, err := encodeECKey(&ecdsa.PrivateKey{}); err == nil {
		t.Error("encodeECKey(nil-curve key) = nil error; want x509 marshal error")
	}
}

// TestLoadOrCreateUser_StoreLoadError verifies that a corrupt account ciphertext
// (decrypt failure) self-heals: a fresh unregistered account is returned, no
// hard error.
func TestLoadOrCreateUser_StoreLoadError(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	if err := os.WriteFile(m.store.path(accountCacheName), []byte{0xFF, 0xFE, 0xFD}, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	user, err := m.loadOrCreateUser()
	if err != nil {
		t.Fatalf("loadOrCreateUser (store.load error) = error %v; want self-heal (fresh user)", err)
	}
	if user == nil || user.key == nil {
		t.Error("loadOrCreateUser (store.load error) should return a fresh user when cache is corrupt")
	}
	if user != nil && user.registration != nil {
		t.Error("self-healed user should be unregistered")
	}
}

// TestEnsureCertificate_BuildClientError verifies that when buildClient fails,
// ensureCertificate logs the error and returns cleanly. A corrupt account cache
// now self-heals (F2), so lego.NewClient is called; the CA URL is set to an
// unreachable loopback address so the failure happens locally without real
// network I/O.
func TestEnsureCertificate_BuildClientError(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)
	// Use an unreachable local address so lego.NewClient fails immediately on
	// connection refused without contacting a real ACME CA.
	m.caURL = "http://127.0.0.1:1/dir"
	// m.cert is nil and the context is live: ensureCertificate will call
	// buildClient, which fails at lego.NewClient (connection refused).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ensureCertificate(ctx)
}

// TestLegoManager_Run_LoadCachedCertError exercises the warning path in run()
// when loadCachedCert returns an error (corrupt cert cache). A fresh cert is
// pre-installed so ensureCertificate short-circuits without network I/O.
func TestLegoManager_Run_LoadCachedCertError(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t)

	// Corrupt the cert cache so loadCachedCert returns an error inside run.
	if err := os.WriteFile(m.store.path(certCacheName), []byte{0xFF, 0xFE, 0xFD}, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Pre-install a fresh cert (matching identifier) so ensureCertificate short-circuits.
	fresh := selfSignedCert(t, "host.example.com", time.Now().Add(60*24*time.Hour))
	m.mu.Lock()
	m.cert = fresh
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.run(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after context cancellation")
	}
}

// TestEncryptedStore_LoadReadError covers the os.ReadFile error branch (when
// the file exists but is unreadable) in encryptedStore.load.
func TestEncryptedStore_LoadReadError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &encryptedStore{dir: dir, encryptor: testEncryptor(t)}
	if err := store.save("item", []byte("secret")); err != nil {
		t.Fatalf("save: %v", err)
	}
	path := filepath.Join(dir, "item.enc")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Skip("cannot make file unreadable:", err)
	}
	defer os.Chmod(path, 0o600) // restore for cleanup
	if _, _, err := store.load("item"); err == nil {
		t.Error("load(unreadable) = nil error; want error")
	}
}

// TestLoadCachedCert_IdentifierMismatch verifies that a cached certificate
// whose DNS SAN does not match the configured identifier is not installed.
// loadCachedCert must treat this as a cache miss so ensureCertificate re-issues
// for the current identifier instead of serving a cert for a stale name.
func TestLoadCachedCert_IdentifierMismatch(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t) // identifier: "host.example.com"

	// Persist a cert for "other.example.com" — valid but wrong identifier.
	certPEM, keyPEM := makeCertPEM(t, "other.example.com", time.Now().Add(90*24*time.Hour))
	sc := storedCert{Certificate: certPEM, PrivateKey: keyPEM}
	data, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := m.store.save(certCacheName, data); err != nil {
		t.Fatalf("store.save: %v", err)
	}
	if err := m.loadCachedCert(); err != nil {
		t.Fatalf("loadCachedCert: %v", err)
	}
	m.mu.RLock()
	cert := m.cert
	m.mu.RUnlock()
	if cert != nil {
		t.Error("loadCachedCert installed a cert for the wrong identifier; want cache miss (m.cert == nil)")
	}
}

// TestLoadCachedCert_ExpiredRejected verifies that a cached certificate whose
// SAN still covers the configured identifier but whose NotAfter is in the past
// is treated as a cache miss (not installed), so the obtain path re-issues
// instead of serving an expired cert on restart. VerifyHostname alone does not
// check expiry, so loadCachedCert must pair it with an explicit NotAfter check.
func TestLoadCachedCert_ExpiredRejected(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t) // identifier: "host.example.com"

	// Persist a cert for the correct identifier but already expired.
	certPEM, keyPEM := makeCertPEM(t, "host.example.com", time.Now().Add(-time.Hour))
	sc := storedCert{Certificate: certPEM, PrivateKey: keyPEM}
	data, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := m.store.save(certCacheName, data); err != nil {
		t.Fatalf("store.save: %v", err)
	}
	if err := m.loadCachedCert(); err != nil {
		t.Fatalf("loadCachedCert: %v", err)
	}
	m.mu.RLock()
	cert := m.cert
	m.mu.RUnlock()
	if cert != nil {
		t.Error("loadCachedCert installed an expired cert; want cache miss (m.cert == nil)")
	}
}

// TestLegoManager_HTTPHandler_RedirectBracketsIPv6 verifies the HTTPS redirect
// builds an RFC 3986-valid authority: an IPv6 IP-SAN identifier must be bracketed
// in the Location header, while IPv4 and DNS identifiers must NOT be bracketed.
func TestLegoManager_HTTPHandler_RedirectBracketsIPv6(t *testing.T) {
	t.Parallel()

	// managerWithIdentifier builds a network-free manager whose served identifier
	// is the given host (IP or DNS name).
	managerWithIdentifier := func(t *testing.T, acme config.ACMEConfig) *legoManager {
		t.Helper()
		cfg := &config.Config{
			ACME:     acme,
			Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "stillwater.db")},
		}
		m, err := newLegoManager(cfg, discardLogger(), testEncryptor(t))
		if err != nil {
			t.Fatalf("newLegoManager: %v", err)
		}
		return m
	}

	cases := []struct {
		name    string
		acme    config.ACMEConfig
		wantLoc string
	}{
		{
			name:    "IPv6 IP-SAN is bracketed",
			acme:    config.ACMEConfig{IP: "2001:db8::1"},
			wantLoc: "https://[2001:db8::1]/some/path?q=1",
		},
		{
			name:    "IPv4 IP-SAN is not bracketed",
			acme:    config.ACMEConfig{IP: "203.0.113.5"},
			wantLoc: "https://203.0.113.5/some/path?q=1",
		},
		{
			name:    "DNS name is not bracketed",
			acme:    config.ACMEConfig{Domain: "host.example.com"},
			wantLoc: "https://host.example.com/some/path?q=1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := managerWithIdentifier(t, tc.acme)
			h := m.HTTPHandler(nil)

			req := httptest.NewRequest(http.MethodGet, "http://request-host.invalid:80/some/path?q=1", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMovedPermanently {
				t.Fatalf("redirect status = %d; want 301", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != tc.wantLoc {
				t.Errorf("Location = %q; want %q", loc, tc.wantLoc)
			}
		})
	}
}

// TestEnsureCertificate_IdentifierMismatch verifies that a held certificate
// whose SAN does not match the configured identifier triggers re-obtain even
// when the certificate is otherwise fresh (within the renewal threshold).
// The CA URL is pointed at an unreachable loopback address so buildClient fails
// locally without any real network I/O.
func TestEnsureCertificate_IdentifierMismatch(t *testing.T) {
	t.Parallel()
	m := newTestLegoManager(t) // identifier: "host.example.com"
	m.caURL = "http://127.0.0.1:1/dir"

	// Fresh cert, but for the wrong identifier.
	mismatch := selfSignedCert(t, "other.example.com", time.Now().Add(60*24*time.Hour))
	m.mu.Lock()
	m.cert = mismatch
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Must NOT short-circuit on the fresh-cert check because the SAN doesn't
	// match m.identifier. It will call buildClient (which fails at lego.NewClient
	// with connection refused) and return cleanly.
	m.ensureCertificate(ctx)
}
