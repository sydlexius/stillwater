package server

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"

	"github.com/sydlexius/stillwater/internal/config"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/filesystem"
)

// This file holds the second CertManager implementation, backed by
// github.com/go-acme/lego/v4. It exists because golang.org/x/crypto/acme/autocert
// (the autocertManager in cert_manager.go) cannot do two things Stillwater needs:
//
//   - register an ACME account with External Account Binding (EAB), which CAs
//     like ZeroSSL require; and
//   - order a certificate for an IP SAN (RFC 8738) rather than a DNS name, for
//     homelab operators with a routable WAN IP but no public DNS.
//
// Selection between the two implementations happens once in buildEntries (see
// listeners.go) based on config; both satisfy the CertManager interface so the
// listener layer never branches on the ACME flavor.
//
// Design notes (these diverge deliberately from the CodeRabbit issue plan; see
// the PR description for the rationale):
//
//   - NO NETWORK IN THE CONSTRUCTOR. lego.NewClient fetches the ACME directory
//     over the network, so all client construction, registration, and order
//     work happens in a background goroutine started from NewLegoManager and
//     bounded by the passed context. The constructor only validates inputs and
//     prepares the on-disk cache. This keeps server startup non-blocking (a
//     transient CA outage cannot prevent boot) and keeps unit tests free of
//     network I/O. GetCertificate fails the TLS handshake until the first
//     issuance completes -- the same externally observable behavior as
//     autocert's first-handshake acquisition.
//   - lego's importable library has no "CertificateStore" interface (that lives
//     in the lego CLI, not the package). Persistence is the caller's job: we
//     roll our own encryptedStore that AES-256-GCM-encrypts the ACME account
//     key and the issued certificate bundle at rest via *encryption.Encryptor.
//   - Renewal re-runs Obtain rather than Renew. Obtain handles both first
//     issuance and renewal with one code path; for HTTP-01 the challenge is
//     re-solved either way, so Renew's CSR reuse buys nothing here and Obtain
//     is simpler and self-healing.

const (
	// zerosslDirectoryURL is ZeroSSL's ACME v2 directory. It is the default
	// directory when EAB credentials are configured but SW_ACME_CA is not,
	// because EAB is ZeroSSL's canonical use case.
	zerosslDirectoryURL = "https://acme.zerossl.com/v2/DV90"

	// acmeChallengeURIPrefix is the well-known path prefix ACME CAs fetch
	// HTTP-01 challenge tokens from (RFC 8555 section 8.3).
	acmeChallengeURIPrefix = "/.well-known/acme-challenge/"

	// renewCheckInterval is how often the renewal goroutine wakes to check
	// certificate expiry. 12h is frequent enough to be self-healing after a
	// failed attempt without hammering the CA. This cadence applies only AFTER
	// the first certificate is installed; the initial acquisition retries far
	// sooner via initialObtainBackoff (see acquireInitialCert).
	renewCheckInterval = 12 * time.Hour

	// initialObtainBackoff / maxObtainBackoff bound the retry cadence for the
	// FIRST certificate. Until a cert exists every TLS handshake fails, so a
	// failed initial obtain must be retried far sooner than the 12h renewal
	// cadence -- otherwise a transient CA outage at boot leaves TLS dead for up
	// to 12h. The delay starts small and doubles, capped, so a brief outage
	// recovers in seconds while a prolonged one does not hammer the CA.
	initialObtainBackoff = 2 * time.Second
	maxObtainBackoff     = 5 * time.Minute

	// renewThreshold is the remaining-validity window below which a certificate
	// is renewed. 30 days is the common ACME convention and leaves ample slack
	// before a 90-day cert expires.
	renewThreshold = 30 * 24 * time.Hour

	// accountCacheName / certCacheName are the encrypted-store entry names for
	// the ACME account and the issued certificate bundle. The store appends a
	// .enc suffix on disk to distinguish encrypted blobs from plaintext caches.
	accountCacheName = "account"
	certCacheName    = "certificate"
)

// legoManager is the CertManager backed by go-acme/lego. It serves the most
// recently obtained certificate from an in-memory, mutex-protected field and
// keeps it fresh via a background renewal goroutine.
type legoManager struct {
	cfg       *config.Config
	logger    *slog.Logger
	encryptor *encryption.Encryptor
	store     *encryptedStore

	// identifier is the single DNS name or IP the certificate is ordered for.
	identifier string
	// caURL is the resolved ACME directory URL.
	caURL string

	// mu guards cert, the currently-served certificate. nil until the first
	// successful obtain (or a successful load of a cached cert).
	mu   sync.RWMutex
	cert *tls.Certificate

	// chalMu guards tokens, the HTTP-01 challenge token -> keyAuth map written
	// by the lego challenge.Provider hooks (Present/CleanUp) and read by the
	// HTTP handler serving /.well-known/acme-challenge/.
	chalMu sync.RWMutex
	tokens map[string]string

	// ensureFn performs one obtain/renew attempt. It is a field (defaulting to
	// m.ensureCertificate) purely so unit tests can inject a deterministic,
	// network-free attempt and exercise acquireInitialCert's backoff loop.
	ensureFn func(context.Context)
	// initialBackoff / maxBackoff parameterize the initial-acquisition backoff;
	// zero values fall back to initialObtainBackoff / maxObtainBackoff. Tests
	// shrink them so the retry loop runs in milliseconds.
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// Compile-time assertions that legoManager satisfies both the CertManager
// interface and lego's challenge.Provider interface (it is its own HTTP-01
// solver, integrated with HTTPHandler).
var (
	_ CertManager        = (*legoManager)(nil)
	_ challenge.Provider = (*legoManager)(nil)
)

// NewLegoManager constructs a lego-backed CertManager and starts its background
// acquisition+renewal goroutine bounded by ctx. It performs NO network I/O: the
// goroutine builds the ACME client, registers the account, and orders the
// certificate. Validation failures (nil dependencies, missing identifier,
// un-creatable cache dir) are returned synchronously.
//
// exactly one of cfg.ACME.Domain or cfg.ACME.IP must be set; config validation
// enforces the mutual exclusivity, and this constructor treats Domain as taking
// precedence if both somehow arrive set.
func NewLegoManager(ctx context.Context, cfg *config.Config, logger *slog.Logger, encryptor *encryption.Encryptor) (CertManager, error) {
	m, err := newLegoManager(cfg, logger, encryptor)
	if err != nil {
		return nil, err
	}
	go m.run(ctx)
	return m, nil
}

// newLegoManager builds and validates the manager WITHOUT starting the
// background goroutine. It is the network-free core that unit tests exercise
// directly; NewLegoManager wraps it and launches the goroutine.
func newLegoManager(cfg *config.Config, logger *slog.Logger, encryptor *encryption.Encryptor) (*legoManager, error) {
	if cfg == nil {
		return nil, errors.New("server: nil config")
	}
	if logger == nil {
		return nil, errors.New("server: nil logger")
	}
	if encryptor == nil {
		// The lego path encrypts cached credentials at rest; without an
		// encryptor it would have to write secrets in plaintext. Refuse.
		return nil, errors.New("server: nil encryptor")
	}

	identifier := cfg.ACME.Domain
	if identifier == "" {
		identifier = cfg.ACME.IP
	}
	if identifier == "" {
		return nil, errors.New("server: ACME (lego) requires SW_ACME_DOMAIN or SW_ACME_IP")
	}

	cacheDir := cfg.ACME.CacheDir
	if cacheDir == "" {
		// Co-locate with the database, matching the autocert path so a
		// bind-mount that persists the db also persists the ACME cache.
		cacheDir = filepath.Join(filepath.Dir(cfg.Database.Path), "acme-cache")
	}
	// 0700: ACME account keys + certificate private keys live here (encrypted,
	// but defense in depth). Restrict to the Stillwater process owner.
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("server: create ACME cache dir %q: %w", cacheDir, err)
	}

	caURL := cfg.ACME.CA
	if caURL == "" {
		if cfg.ACME.UsesEAB() {
			caURL = zerosslDirectoryURL
		} else {
			caURL = lego.LEDirectoryProduction
		}
	}

	m := &legoManager{
		cfg:        cfg,
		logger:     logger,
		encryptor:  encryptor,
		store:      &encryptedStore{dir: cacheDir, encryptor: encryptor},
		identifier: identifier,
		caURL:      caURL,
		tokens:     make(map[string]string),
	}
	// Default the obtain step to the real network-backed implementation. Tests
	// override this field to inject a deterministic attempt.
	m.ensureFn = m.ensureCertificate

	// Log only non-sensitive facts. NEVER log EabKeyID/EabMacKey values -- the
	// MAC key is a secret (mirrors autocert logging only email_configured).
	logger.Info("ACME (lego) configured",
		slog.String("identifier", identifier),
		slog.Bool("ip_san", cfg.ACME.IP != ""),
		slog.Bool("eab", cfg.ACME.UsesEAB()),
		slog.String("cache_dir", cacheDir),
		slog.String("directory_url", caURL),
		slog.Bool("email_configured", cfg.ACME.Email != ""),
	)

	return m, nil
}

// TLSConfig returns a tls.Config whose GetCertificate callback serves the
// currently-held certificate under an RLock. Until the background goroutine
// completes its first successful obtain, GetCertificate returns an error and
// the TLS handshake fails -- the same first-hit behavior as autocert. The
// caller layers on MinVersion / NextProtos at the listener.
func (m *legoManager) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			m.mu.RLock()
			defer m.mu.RUnlock()
			if m.cert == nil {
				return nil, errors.New("server: ACME certificate not yet available")
			}
			return m.cert, nil
		},
	}
}

// HTTPHandler serves HTTP-01 challenge responses from the in-memory token map
// and delegates everything else to fallback (or 301-redirects to HTTPS when
// fallback is nil), matching the autocert HTTPHandler contract so the port-80
// listener stays useful outside renewal windows.
func (m *legoManager) HTTPHandler(fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, acmeChallengeURIPrefix) {
			token := strings.TrimPrefix(r.URL.Path, acmeChallengeURIPrefix)
			m.chalMu.RLock()
			keyAuth, ok := m.tokens[token]
			m.chalMu.RUnlock()
			if ok {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(keyAuth))
				return
			}
			// Unknown token: a probe or a stale request. 404 rather than
			// leaking anything.
			http.NotFound(w, r)
			return
		}
		if fallback != nil {
			fallback.ServeHTTP(w, r)
			return
		}
		// No fallback: 301 every non-challenge request to HTTPS, mirroring
		// autocert's redirect so a dedicated port-80 listener still redirects.
		// The host is PINNED to the configured identifier (the single host/IP we
		// hold a certificate for), NOT the request's Host header. Only the
		// path+query are carried over; since the authority is a compile-time-
		// shaped constant from config, this cannot redirect off-host -- there is
		// no open-redirect vector. gosec's taint analysis flags any request-
		// derived data in a Location header and cannot see that the host is
		// fixed, so suppress G710 with that justification.
		// Bracket an IPv6 literal so the resulting authority is RFC 3986-valid
		// (a bare "2001:db8::1" host is not a valid authority; it must be
		// "[2001:db8::1]"). This manager supports IP-SAN certs (RFC 8738), so the
		// identifier can be an IPv6 literal. A DNS name or IPv4 literal has no
		// host-significant colon, so it is left untouched.
		host := m.identifier
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			host = "[" + host + "]"
		}
		target := "https://" + host + r.URL.RequestURI()
		//nolint:gosec // G710: host is the fixed configured identifier (not request input); only path/query is carried, which cannot change the authority.
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}

// Present satisfies challenge.Provider: it records the HTTP-01 keyAuth for token
// so the HTTP handler can serve it when the CA fetches the challenge URL.
func (m *legoManager) Present(_, token, keyAuth string) error {
	m.chalMu.Lock()
	defer m.chalMu.Unlock()
	m.tokens[token] = keyAuth
	return nil
}

// CleanUp satisfies challenge.Provider: it removes a solved/abandoned token.
func (m *legoManager) CleanUp(_, token, _ string) error {
	m.chalMu.Lock()
	defer m.chalMu.Unlock()
	delete(m.tokens, token)
	return nil
}

// run is the background lifecycle: load any cached cert, obtain/renew once, then
// poll on a ticker. It exits promptly when ctx is canceled and performs no work
// if ctx is already done (the path unit tests rely on to stay network-free).
func (m *legoManager) run(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	// Serve a cached cert immediately on restart if one is present and valid.
	if err := m.loadCachedCert(); err != nil {
		m.logger.Warn("ACME (lego) could not load cached certificate",
			slog.String("error", err.Error()))
	}

	// Acquire the FIRST certificate with bounded exponential backoff. Until a
	// cert is installed every TLS handshake fails, so a failed initial obtain
	// must not wait the full 12h renewal cadence before retrying. Returns false
	// only when ctx is canceled before a cert is installed.
	if !m.acquireInitialCert(ctx) {
		return
	}

	// A cert is now installed; fall back to the steady-state 12h renewal cadence.
	ticker := time.NewTicker(renewCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.ensureFn(ctx)
		}
	}
}

// hasCert reports whether a certificate is currently installed.
func (m *legoManager) hasCert() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cert != nil
}

// acquireInitialCert drives the first certificate installation with bounded
// exponential backoff, returning true once a cert is held (a cached cert loaded
// by run, or a freshly obtained one) and false if ctx is canceled first. The
// backoff starts at initialBackoff (default initialObtainBackoff) and doubles
// up to maxBackoff (default maxObtainBackoff), so a transient CA outage at boot
// is retried in seconds-to-minutes instead of waiting the 12h renewal cadence.
func (m *legoManager) acquireInitialCert(ctx context.Context) bool {
	// A valid cached cert may already be installed by loadCachedCert; if so the
	// first attempt below no-ops and we return immediately.
	backoff := m.initialBackoff
	if backoff <= 0 {
		backoff = initialObtainBackoff
	}
	maxBackoff := m.maxBackoff
	if maxBackoff <= 0 {
		maxBackoff = maxObtainBackoff
	}
	for {
		if ctx.Err() != nil {
			return false
		}
		m.ensureFn(ctx)
		if m.hasCert() {
			return true
		}
		m.logger.Warn("ACME (lego) initial certificate not yet available; retrying with backoff",
			slog.String("identifier", m.identifier),
			slog.Duration("retry_in", backoff))
		// Sleep out the backoff but wake immediately on cancellation.
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// ensureCertificate obtains a certificate if none is held, the current one is
// within the renewal threshold, or the current one does not cover the configured
// identifier (e.g. the identifier changed since the cert was issued).
func (m *legoManager) ensureCertificate(ctx context.Context) {
	m.mu.RLock()
	cert := m.cert
	m.mu.RUnlock()
	if cert != nil && cert.Leaf != nil &&
		time.Until(cert.Leaf.NotAfter) > renewThreshold &&
		cert.Leaf.VerifyHostname(m.identifier) == nil {
		return // still fresh and covers the configured identifier
	}
	if ctx.Err() != nil {
		return
	}

	client, user, err := m.buildClient()
	if err != nil {
		m.logger.Error("ACME (lego) client setup failed", slog.String("error", err.Error()))
		return
	}
	if err := m.obtain(client, user); err != nil {
		m.logger.Error("ACME (lego) certificate obtain failed",
			slog.String("identifier", m.identifier),
			slog.String("error", err.Error()))
		return
	}
}

// buildClient constructs the lego client (network: fetches the ACME directory),
// loads or creates the account, registers it if necessary (with EAB when
// configured), and wires the HTTP-01 solver. A freshly-registered account is
// persisted encrypted.
func (m *legoManager) buildClient() (*lego.Client, *legoUser, error) {
	user, err := m.loadOrCreateUser()
	if err != nil {
		return nil, nil, fmt.Errorf("load account: %w", err)
	}

	legoCfg := lego.NewConfig(user)
	legoCfg.CADirURL = m.caURL

	client, err := lego.NewClient(legoCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("new lego client: %w", err)
	}

	if user.registration == nil {
		if err := m.register(client, user); err != nil {
			return nil, nil, fmt.Errorf("register account: %w", err)
		}
		if err := m.saveAccount(user); err != nil {
			// Non-fatal: we can still obtain this run; we just won't reuse the
			// account on restart, which means a re-register next boot.
			m.logger.Warn("ACME (lego) failed to persist account",
				slog.String("error", err.Error()))
		}
	}

	if err := client.Challenge.SetHTTP01Provider(m); err != nil {
		return nil, nil, fmt.Errorf("set HTTP-01 provider: %w", err)
	}
	return client, user, nil
}

// register performs ACME account registration, using External Account Binding
// when EAB credentials are configured and standard registration otherwise.
func (m *legoManager) register(client *lego.Client, user *legoUser) error {
	if m.cfg.ACME.UsesEAB() {
		reg, err := client.Registration.RegisterWithExternalAccountBinding(registration.RegisterEABOptions{
			TermsOfServiceAgreed: true,
			Kid:                  m.cfg.ACME.EabKeyID,
			HmacEncoded:          m.cfg.ACME.EabMacKey,
		})
		if err != nil {
			return err
		}
		user.registration = reg
		return nil
	}
	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return err
	}
	user.registration = reg
	return nil
}

// obtain orders a certificate for the configured identifier, installs it as the
// served certificate, and persists it encrypted. lego auto-detects an IP
// identifier when the value parses as an IP (RFC 8738), so the same call covers
// both the DNS and IP-SAN cases.
func (m *legoManager) obtain(client *lego.Client, _ *legoUser) error {
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: []string{m.identifier},
		Bundle:  true,
	})
	if err != nil {
		return err
	}
	cert, err := parseKeyPair(res.Certificate, res.PrivateKey)
	if err != nil {
		return fmt.Errorf("parse issued certificate: %w", err)
	}
	m.mu.Lock()
	m.cert = cert
	m.mu.Unlock()

	if err := m.saveCert(res); err != nil {
		m.logger.Warn("ACME (lego) failed to persist certificate",
			slog.String("error", err.Error()))
	}
	m.logger.Info("ACME (lego) certificate obtained",
		slog.String("identifier", m.identifier))
	return nil
}

// loadCachedCert installs a previously-cached certificate (if present and
// decodable) as the served certificate, so a restart serves TLS immediately
// without waiting on a fresh order.
func (m *legoManager) loadCachedCert() error {
	data, found, err := m.store.load(certCacheName)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	var sc storedCert
	if err := json.Unmarshal(data, &sc); err != nil {
		return fmt.Errorf("unmarshal cached cert: %w", err)
	}
	cert, err := parseKeyPair(sc.Certificate, sc.PrivateKey)
	if err != nil {
		return fmt.Errorf("parse cached cert: %w", err)
	}
	// coversIdentifier is true when the Leaf is absent (cannot check) OR the
	// cert's SANs include the configured identifier. VerifyHostname checks SAN
	// match only, NOT expiry, so we pair it with an explicit NotAfter check that
	// mirrors ensureCertificate's renewal gate -- otherwise an expired-but-still-
	// named cached cert would be served on restart.
	coversIdentifier := cert.Leaf == nil || cert.Leaf.VerifyHostname(m.identifier) == nil
	expired := cert.Leaf != nil && time.Now().After(cert.Leaf.NotAfter)
	if !coversIdentifier {
		m.logger.Info("ACME (lego) cached certificate does not cover configured identifier; will re-issue",
			slog.String("identifier", m.identifier))
		return nil
	}
	if expired {
		m.logger.Info("ACME (lego) cached certificate is expired; will re-issue",
			slog.String("identifier", m.identifier),
			slog.Time("not_after", cert.Leaf.NotAfter))
		return nil
	}
	m.mu.Lock()
	m.cert = cert
	m.mu.Unlock()
	return nil
}

// loadOrCreateUser returns the cached ACME account if one exists, otherwise a
// fresh account with a newly generated P-256 key (unregistered). A corrupt or
// unreadable cache (decrypt/unmarshal/decode failure) is treated the same as
// absent: a loud Warn is emitted and a fresh account is generated so issuance
// can proceed without operator intervention.
func (m *legoManager) loadOrCreateUser() (*legoUser, error) {
	const corruptMsg = "ACME (lego) existing account cache is unreadable/corrupt - regenerating and re-registering a fresh account"

	// Scope storeErr to the if-chain so it does not outlive the corrupt-cache
	// handling block and trigger a nilerr lint hit on the fall-through return.
	if data, found, storeErr := m.store.load(accountCacheName); storeErr != nil {
		m.logger.Warn(corruptMsg, slog.String("error", storeErr.Error()))
	} else if found {
		var sa storedAccount
		if jsonErr := json.Unmarshal(data, &sa); jsonErr != nil {
			m.logger.Warn(corruptMsg, slog.String("error", jsonErr.Error()))
		} else if key, keyErr := decodeECKey(sa.PrivateKey); keyErr != nil {
			m.logger.Warn(corruptMsg, slog.String("error", keyErr.Error()))
		} else {
			return &legoUser{email: sa.Email, registration: sa.Registration, key: key}, nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate account key: %w", err)
	}
	return &legoUser{email: m.cfg.ACME.Email, key: key}, nil
}

// saveAccount persists the (registered) account encrypted at rest.
func (m *legoManager) saveAccount(user *legoUser) error {
	keyPEM, err := encodeECKey(user.key)
	if err != nil {
		return err
	}
	blob, err := json.Marshal(storedAccount{
		Email:        user.email,
		Registration: user.registration,
		PrivateKey:   keyPEM,
	})
	if err != nil {
		return err
	}
	return m.store.save(accountCacheName, blob)
}

// saveCert persists the issued certificate bundle encrypted at rest. The lego
// certificate.Resource marks its key/cert byte fields json:"-", so we copy them
// into a serializable shape.
func (m *legoManager) saveCert(res *certificate.Resource) error {
	blob, err := json.Marshal(storedCert{
		Domain:            res.Domain,
		CertURL:           res.CertURL,
		CertStableURL:     res.CertStableURL,
		PrivateKey:        res.PrivateKey,
		Certificate:       res.Certificate,
		IssuerCertificate: res.IssuerCertificate,
	})
	if err != nil {
		return err
	}
	return m.store.save(certCacheName, blob)
}

// legoUser implements lego's registration.User interface.
type legoUser struct {
	email        string
	registration *registration.Resource
	key          crypto.PrivateKey
}

func (u *legoUser) GetEmail() string                        { return u.email }
func (u *legoUser) GetRegistration() *registration.Resource { return u.registration }
func (u *legoUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

// storedAccount is the on-disk, JSON-serializable form of an ACME account.
type storedAccount struct {
	Email        string                 `json:"email"`
	Registration *registration.Resource `json:"registration"`
	PrivateKey   []byte                 `json:"private_key"` // PEM-encoded EC key
}

// storedCert is the on-disk, JSON-serializable form of an issued certificate
// bundle (the lego certificate.Resource hides these byte fields from JSON).
type storedCert struct {
	Domain            string `json:"domain"`
	CertURL           string `json:"cert_url"`
	CertStableURL     string `json:"cert_stable_url"`
	PrivateKey        []byte `json:"private_key"`
	Certificate       []byte `json:"certificate"`
	IssuerCertificate []byte `json:"issuer_certificate"`
}

// encryptedStore reads and writes named blobs to a directory, AES-256-GCM
// encrypting their contents at rest via *encryption.Encryptor. Entries are
// written with a .enc suffix and 0600 permissions.
type encryptedStore struct {
	dir       string
	encryptor *encryption.Encryptor
}

func (s *encryptedStore) path(name string) string {
	return filepath.Join(s.dir, name+".enc")
}

// save encrypts data and writes it to <dir>/<name>.enc with 0600 perms. The
// write goes through filesystem.WriteFileAtomic (tmp/bak/rename) so a crash mid-
// write can never leave a truncated or partially-encrypted blob in place: the
// previous cached cert/account survives intact until the new one is fully
// written and atomically renamed over it.
func (s *encryptedStore) save(name string, data []byte) error {
	enc, err := s.encryptor.Encrypt(string(data))
	if err != nil {
		return fmt.Errorf("encrypt %s: %w", name, err)
	}
	if err := filesystem.WriteFileAtomic(s.path(name), []byte(enc), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// load reads and decrypts <dir>/<name>.enc. The boolean is false (with nil
// error) when the entry does not exist, so callers can distinguish "absent"
// from "corrupt".
func (s *encryptedStore) load(name string) ([]byte, bool, error) {
	raw, err := os.ReadFile(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", name, err)
	}
	dec, err := s.encryptor.Decrypt(string(raw))
	if err != nil {
		return nil, false, fmt.Errorf("decrypt %s: %w", name, err)
	}
	return []byte(dec), true, nil
}

// parseKeyPair builds a tls.Certificate from PEM cert + key bytes and populates
// Leaf so expiry checks (renewal) do not re-parse on every read.
func parseKeyPair(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if len(cert.Certificate) > 0 {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("parse certificate leaf: %w", err)
		}
		cert.Leaf = leaf
	}
	return &cert, nil
}

// encodeECKey PEM-encodes an ECDSA account private key.
func encodeECKey(key crypto.PrivateKey) ([]byte, error) {
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("unexpected account key type %T", key)
	}
	der, err := x509.MarshalECPrivateKey(ec)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// decodeECKey parses a PEM-encoded ECDSA account private key.
func decodeECKey(pemBytes []byte) (crypto.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid account key PEM")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}
