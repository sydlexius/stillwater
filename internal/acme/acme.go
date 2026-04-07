// Package acme manages TLS certificates via the ACME protocol.
//
// It supports two modes:
//   - Domain-based certificates using autocert (Let's Encrypt or ZeroSSL)
//   - IP SAN certificates using the raw ACME client (ZeroSSL only)
//
// ZeroSSL is the only mainstream ACME CA that issues certificates for IP
// addresses. Let's Encrypt does not issue IP address certificates.
package acme

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	xacme "golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/filesystem"
)

const (
	// DirectoryLetsEncrypt is the ACME directory for Let's Encrypt (production).
	DirectoryLetsEncrypt = "https://acme-v02.api.letsencrypt.org/directory"

	// DirectoryZeroSSL is the ACME directory for ZeroSSL.
	DirectoryZeroSSL = "https://acme.zerossl.com/v2/DV90"

	// renewBeforeExpiry is how early to renew a certificate before it expires.
	renewBeforeExpiry = 30 * 24 * time.Hour

	// renewCheckInterval is how often to poll for cert renewal.
	renewCheckInterval = 24 * time.Hour
)

// Config holds ACME configuration derived from the application config.
type Config struct {
	// CA is "letsencrypt", "zerossl", or a custom ACME directory URL.
	CA string

	// Domain is the hostname to issue a certificate for (domain-based mode).
	// Leave empty to skip domain cert management.
	Domain string

	// Email is the contact address registered with the CA.
	Email string

	// CacheDir is the filesystem directory where ACME state is persisted.
	CacheDir string

	// EABKeyID is the External Account Binding key identifier (ZeroSSL).
	EABKeyID string

	// EABMACKey is the Base64URL-encoded EAB MAC key (ZeroSSL).
	EABMACKey string

	// IP is the public IP address to issue a certificate for (IP-based mode).
	// Leave empty to skip IP cert management.
	IP string
}

// Manager manages TLS certificate lifecycle via ACME.
type Manager struct {
	cfg    Config
	enc    *encryption.Encryptor
	logger *slog.Logger

	// domainMgr handles domain-based certs via autocert.
	domainMgr *autocert.Manager

	// challenges stores HTTP-01 challenge token responses for IP-based ACME.
	challenges *challengeStore

	// ipCert is the current in-memory IP certificate; protected by certMu.
	certMu sync.RWMutex
	ipCert *tls.Certificate
}

// NewManager creates an ACME Manager. enc may be nil only when neither IP
// nor domain cert management is active.
func NewManager(cfg Config, enc *encryption.Encryptor, logger *slog.Logger) (*Manager, error) {
	m := &Manager{
		cfg:        cfg,
		enc:        enc,
		logger:     logger,
		challenges: newChallengeStore(),
	}

	if cfg.Domain != "" {
		acmeClient := &xacme.Client{
			DirectoryURL: m.directoryURL(),
		}

		dmgr := &autocert.Manager{
			Prompt:   autocert.AcceptTOS,
			Cache:    autocert.DirCache(cfg.CacheDir),
			Client:   acmeClient,
			Email:    cfg.Email,
			HostPolicy: autocert.HostWhitelist(cfg.Domain),
		}

		if cfg.EABKeyID != "" && cfg.EABMACKey != "" {
			macKey, err := decodeEABMACKey(cfg.EABMACKey)
			if err != nil {
				return nil, fmt.Errorf("decoding EAB MAC key: %w", err)
			}
			dmgr.ExternalAccountBinding = &xacme.ExternalAccountBinding{
				KID: cfg.EABKeyID,
				Key: macKey,
			}
		}

		m.domainMgr = dmgr
	}

	return m, nil
}

// Start launches background certificate management goroutines. It returns
// immediately; certificate operations run asynchronously until ctx is done.
func (m *Manager) Start(ctx context.Context) {
	if m.cfg.IP != "" {
		go m.manageIPCert(ctx)
	}
}

// TLSConfig returns a *tls.Config that serves the managed certificates.
// When both domain and IP certs are active, domain SNI takes precedence;
// connections with no SNI (bare IP) receive the IP certificate.
func (m *Manager) TLSConfig() *tls.Config {
	if m.domainMgr != nil && m.cfg.IP == "" {
		return m.domainMgr.TLSConfig()
	}

	if m.cfg.IP != "" && m.domainMgr == nil {
		return &tls.Config{
			GetCertificate: m.getIPCert,
			MinVersion:     tls.VersionTLS12,
		}
	}

	// Both domain and IP: route by SNI.
	if m.domainMgr != nil && m.cfg.IP != "" {
		domainCfg := m.domainMgr.TLSConfig()
		domainGet := domainCfg.GetCertificate
		domainCfg.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName == "" {
				return m.getIPCert(hello)
			}
			return domainGet(hello)
		}
		return domainCfg
	}

	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// ChallengeHandler returns an http.Handler suitable for a plain-HTTP listener
// on port 80. It serves ACME HTTP-01 challenge tokens and, for all other
// requests, issues a permanent redirect to the HTTPS equivalent.
func (m *Manager) ChallengeHandler() http.Handler {
	var h http.Handler = http.HandlerFunc(redirectToHTTPS)

	// IP-based ACME: intercept challenge paths before the autocert handler
	// (or the redirect fallback) so token responses are served correctly.
	if m.cfg.IP != "" {
		h = m.challenges.handler(h)
	}

	// Domain-based ACME: autocert wraps the inner handler and intercepts
	// /.well-known/acme-challenge/ paths automatically.
	if m.domainMgr != nil {
		h = m.domainMgr.HTTPHandler(h)
	}

	return h
}

// IsEnabled reports whether any form of ACME certificate management is active.
func (m *Manager) IsEnabled() bool {
	return m.cfg.Domain != "" || m.cfg.IP != ""
}

// ---------------------------------------------------------------------------
// IP certificate management
// ---------------------------------------------------------------------------

// manageIPCert is the background goroutine that keeps the IP certificate
// current. It loads any cached cert on startup, obtains a new cert if needed,
// then checks once per day whether renewal is required.
func (m *Manager) manageIPCert(ctx context.Context) {
	if err := m.loadIPCert(); err != nil {
		m.logger.Warn("acme: could not load cached IP cert", slog.Any("error", err))
	}

	if m.needsIPRenewal() {
		if err := m.obtainIPCert(ctx); err != nil {
			m.logger.Error("acme: failed to obtain IP cert", slog.Any("error", err))
		}
	}

	ticker := time.NewTicker(renewCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if m.needsIPRenewal() {
				if err := m.obtainIPCert(ctx); err != nil {
					m.logger.Error("acme: failed to renew IP cert", slog.Any("error", err))
				}
			}
		}
	}
}

// obtainIPCert runs the full ACME flow to obtain a new IP SAN certificate from
// ZeroSSL (or the configured custom CA) and stores it on disk.
func (m *Manager) obtainIPCert(ctx context.Context) error {
	m.logger.Info("acme: obtaining IP certificate", slog.String("ip", m.cfg.IP))

	// 1. Prepare ACME client with account key.
	accountKey, err := m.loadOrCreateAccountKey()
	if err != nil {
		return fmt.Errorf("account key: %w", err)
	}

	client := &xacme.Client{
		Key:          accountKey,
		DirectoryURL: m.directoryURL(),
	}

	// 2. Register (or look up existing) account.
	acct := &xacme.Account{}
	if m.cfg.Email != "" {
		acct.Contact = []string{"mailto:" + m.cfg.Email}
	}

	if m.cfg.EABKeyID != "" && m.cfg.EABMACKey != "" {
		macKey, err := decodeEABMACKey(m.cfg.EABMACKey)
		if err != nil {
			return fmt.Errorf("decoding EAB MAC key: %w", err)
		}
		acct.ExternalAccountBinding = &xacme.ExternalAccountBinding{
			KID: m.cfg.EABKeyID,
			Key: macKey,
		}
	}

	if _, err := client.Register(ctx, acct, xacme.AcceptTOS); err != nil {
		return fmt.Errorf("registering ACME account: %w", err)
	}

	// 3. Create order for the IP identifier.
	order, err := client.AuthorizeOrder(ctx, xacme.IPIDs(m.cfg.IP))
	if err != nil {
		return fmt.Errorf("creating order: %w", err)
	}

	// 4. Complete each authorization via HTTP-01.
	for _, authzURL := range order.AuthzURLs {
		if err := m.completeHTTP01(ctx, client, authzURL); err != nil {
			return fmt.Errorf("completing authorization %s: %w", authzURL, err)
		}
	}

	// 5. Wait for the order to reach StatusReady.
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return fmt.Errorf("waiting for order: %w", err)
	}

	// 6. Generate a dedicated key for this certificate.
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating cert key: %w", err)
	}

	// 7. Build CSR with IP SAN.
	ip := net.ParseIP(m.cfg.IP)
	csrTemplate := &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: m.cfg.IP},
		IPAddresses: []net.IP{ip},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, certKey)
	if err != nil {
		return fmt.Errorf("creating CSR: %w", err)
	}

	// 8. Finalize order with the CSR.
	derChain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csrDER, true)
	if err != nil {
		return fmt.Errorf("finalizing order: %w", err)
	}

	// 9. Store cert and key on disk, update in-memory state.
	if err := m.storeIPCert(certKey, derChain); err != nil {
		return fmt.Errorf("storing cert: %w", err)
	}

	m.logger.Info("acme: IP certificate obtained", slog.String("ip", m.cfg.IP))
	return nil
}

// completeHTTP01 handles a single HTTP-01 authorization challenge.
func (m *Manager) completeHTTP01(ctx context.Context, client *xacme.Client, authzURL string) error {
	authz, err := client.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("getting authorization: %w", err)
	}

	if authz.Status == xacme.StatusValid {
		return nil // already authorized
	}

	// Find the HTTP-01 challenge.
	var chal *xacme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "http-01" {
			chal = c
			break
		}
	}
	if chal == nil {
		return errors.New("no http-01 challenge offered by CA")
	}

	keyAuth, err := client.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return fmt.Errorf("computing key authorization: %w", err)
	}

	// Serve the challenge token while we prove ownership.
	m.challenges.set(chal.Token, keyAuth)
	defer m.challenges.delete(chal.Token)

	if _, err := client.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accepting challenge: %w", err)
	}

	if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
		return fmt.Errorf("waiting for authorization: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Certificate storage helpers
// ---------------------------------------------------------------------------

// storeIPCert encodes cert and key as PEM and persists them atomically.
func (m *Manager) storeIPCert(key *ecdsa.PrivateKey, derChain [][]byte) error {
	// Encode certificate chain.
	var certPEM bytes.Buffer
	for _, der := range derChain {
		if err := pem.Encode(&certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
			return fmt.Errorf("encoding cert PEM: %w", err)
		}
	}

	// Encode private key.
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshaling cert key: %w", err)
	}
	var keyPEM bytes.Buffer
	if err := pem.Encode(&keyPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return fmt.Errorf("encoding key PEM: %w", err)
	}

	// Write atomically.
	if err := filesystem.WriteFileAtomic(m.ipCertPath(), certPEM.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing cert file: %w", err)
	}
	if err := filesystem.WriteFileAtomic(m.ipKeyPath(), keyPEM.Bytes(), 0o600); err != nil {
		return fmt.Errorf("writing key file: %w", err)
	}

	// Parse into a *tls.Certificate and cache in memory.
	cert, err := tls.X509KeyPair(certPEM.Bytes(), keyPEM.Bytes())
	if err != nil {
		return fmt.Errorf("parsing TLS cert pair: %w", err)
	}

	m.certMu.Lock()
	m.ipCert = &cert
	m.certMu.Unlock()

	return nil
}

// loadIPCert reads the cached IP cert/key from disk and stores it in memory.
// It returns nil (not an error) when no cached cert exists yet.
func (m *Manager) loadIPCert() error {
	certPEM, err := os.ReadFile(m.ipCertPath()) //nolint:gosec // G304: path derived from trusted config
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading cached cert: %w", err)
	}

	keyPEM, err := os.ReadFile(m.ipKeyPath()) //nolint:gosec // G304: path derived from trusted config
	if err != nil {
		return fmt.Errorf("reading cached key: %w", err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("parsing cached cert pair: %w", err)
	}

	m.certMu.Lock()
	m.ipCert = &cert
	m.certMu.Unlock()

	return nil
}

// needsIPRenewal reports whether the IP certificate is absent or expiring
// within renewBeforeExpiry.
func (m *Manager) needsIPRenewal() bool {
	m.certMu.RLock()
	defer m.certMu.RUnlock()

	if m.ipCert == nil || len(m.ipCert.Certificate) == 0 {
		return true
	}

	x509Cert, err := x509.ParseCertificate(m.ipCert.Certificate[0])
	if err != nil {
		return true
	}

	return time.Now().After(x509Cert.NotAfter.Add(-renewBeforeExpiry))
}

// getIPCert is a tls.Config.GetCertificate callback that returns the current
// in-memory IP certificate.
func (m *Manager) getIPCert(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.certMu.RLock()
	defer m.certMu.RUnlock()

	if m.ipCert == nil {
		return nil, fmt.Errorf("acme: no certificate available yet for IP %s", m.cfg.IP)
	}

	return m.ipCert, nil
}

// ---------------------------------------------------------------------------
// Account key management
// ---------------------------------------------------------------------------

// loadOrCreateAccountKey loads the ACME account private key from the cache
// directory (decrypting it), or generates and persists a new one.
func (m *Manager) loadOrCreateAccountKey() (*ecdsa.PrivateKey, error) {
	keyPath := filepath.Join(m.cfg.CacheDir, "account.key.enc")

	data, err := os.ReadFile(keyPath) //nolint:gosec // G304: path derived from trusted config
	if err == nil {
		key, decErr := m.decryptAccountKey(data)
		if decErr == nil {
			return key, nil
		}
		m.logger.Warn("acme: could not decrypt cached account key; regenerating",
			slog.Any("error", decErr))
	}

	// Generate new ECDSA P-256 account key.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating account key: %w", err)
	}

	if persistErr := m.persistAccountKey(key, keyPath); persistErr != nil {
		m.logger.Warn("acme: could not persist account key", slog.Any("error", persistErr))
	}

	return key, nil
}

// persistAccountKey PEM-encodes key, encrypts it, and writes it atomically.
func (m *Manager) persistAccountKey(key *ecdsa.PrivateKey, path string) error {
	if m.enc == nil {
		return errors.New("no encryptor available for account key storage")
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshaling account key: %w", err)
	}

	var buf bytes.Buffer
	if err := pem.Encode(&buf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return fmt.Errorf("encoding account key PEM: %w", err)
	}

	ciphertext, err := m.enc.Encrypt(buf.String())
	if err != nil {
		return fmt.Errorf("encrypting account key: %w", err)
	}

	return filesystem.WriteFileAtomic(path, []byte(ciphertext), 0o600)
}

// decryptAccountKey decrypts an encrypted PEM-encoded ECDSA private key.
func (m *Manager) decryptAccountKey(data []byte) (*ecdsa.PrivateKey, error) {
	if m.enc == nil {
		return nil, errors.New("no encryptor available")
	}

	plaintext, err := m.enc.Decrypt(string(data))
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}

	block, _ := pem.Decode([]byte(plaintext))
	if block == nil {
		return nil, errors.New("no PEM block in decrypted account key")
	}

	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing account key: %w", err)
	}

	return key, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// directoryURL returns the ACME directory URL for the configured CA.
// Both "letsencrypt" and the empty string default to Let's Encrypt's directory.
// "zerossl" returns ZeroSSL's directory. Any other non-empty value is returned
// as-is and treated as a custom ACME directory URL.
func (m *Manager) directoryURL() string {
	switch m.cfg.CA {
	case "zerossl":
		return DirectoryZeroSSL
	case "letsencrypt", "":
		return DirectoryLetsEncrypt
	default:
		return m.cfg.CA // treat as a custom URL
	}
}

func (m *Manager) ipCertPath() string {
	return filepath.Join(m.cfg.CacheDir, "ip-cert.pem")
}

func (m *Manager) ipKeyPath() string {
	return filepath.Join(m.cfg.CacheDir, "ip-key.pem")
}

// redirectToHTTPS sends a 301 redirect to the HTTPS equivalent of the request.
func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + r.Host + r.RequestURI
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// decodeEABMACKey decodes a ZeroSSL-style EAB MAC key. ZeroSSL provides the
// key as Base64URL without padding; this function tries that encoding first,
// then falls back to standard Base64.
func decodeEABMACKey(encoded string) ([]byte, error) {
	if key, err := base64.RawURLEncoding.DecodeString(encoded); err == nil {
		return key, nil
	}
	if key, err := base64.URLEncoding.DecodeString(encoded); err == nil {
		return key, nil
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("EAB MAC key is not valid Base64 or Base64URL: %w", err)
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// challengeStore -- HTTP-01 challenge token registry
// ---------------------------------------------------------------------------

// challengeStore holds the set of active HTTP-01 challenge token responses.
type challengeStore struct {
	mu     sync.RWMutex
	tokens map[string]string // token -> keyAuthorization
}

func newChallengeStore() *challengeStore {
	return &challengeStore{tokens: make(map[string]string)}
}

func (s *challengeStore) set(token, keyAuth string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = keyAuth
}

func (s *challengeStore) delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}

// handler returns an http.Handler that intercepts ACME HTTP-01 challenge
// requests and falls through to next for all other requests.
func (s *challengeStore) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ACME challenge paths: /.well-known/acme-challenge/<token>
		const prefix = "/.well-known/acme-challenge/"
		if len(r.URL.Path) > len(prefix) && r.URL.Path[:len(prefix)] == prefix {
			token := r.URL.Path[len(prefix):]
			s.mu.RLock()
			keyAuth, ok := s.tokens[token]
			s.mu.RUnlock()
			if ok {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				if _, err := w.Write([]byte(keyAuth)); err != nil {
					// The connection was lost after headers were sent; the ACME
					// verifier will retry. Nothing further can be sent.
					return
				}
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
