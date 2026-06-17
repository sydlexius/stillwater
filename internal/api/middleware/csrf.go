package middleware

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const csrfTokenHeader = "X-CSRF-Token" //nolint:gosec // G101: not a credential, this is an HTTP header name
const csrfCookieName = "csrf_token"

// csrfTokenTTL is the validity window for a CSRF token. 4h covers typical
// overnight maintenance restarts: a token issued before a 2am deploy is still
// valid when the user returns in the morning.
const csrfTokenTTL = 4 * time.Hour

// csrfMaxClockSkew is how far in the future a token's timestamp may lie before
// it is rejected. A small allowance accommodates NTP drift across clustered
// nodes; a token timestamped far in the future would otherwise be permanently
// valid and could be generated offline by an attacker who obtained the secret.
const csrfMaxClockSkew = 5 * time.Minute

// csrfMinSecretLen is the minimum byte length for the CSRF signing secret.
// Shorter keys produce weak HMACs; 32 bytes (256 bits) matches the HMAC-SHA256
// output size and is the accepted floor for symmetric secret strength.
const csrfMinSecretLen = 32

// Clock is the time source used by CSRF for token creation and expiry checks.
// The default production implementation returns time.Now().UTC().
type Clock interface {
	Now() time.Time
}

// realClock is the production Clock implementation.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// CSRF provides stateless HMAC-based CSRF protection for HTMX form submissions.
//
// Token format: "{unix_timestamp}:{16-byte-nonce-hex}:{hmac_sha256_hex}"
//
//   - unix_timestamp   – seconds since epoch (decimal)
//   - nonce            – 16 random bytes encoded as 32 lowercase hex chars,
//     generated fresh per issuance so every token is unique even
//     when two requests arrive in the same second
//   - hmac_sha256_hex  – HMAC-SHA256 of "{unix_timestamp}:{nonce}" using
//     the application's SessionSecret
//
// Because all state needed for validation is encoded in the token, no
// server-side map is required and tokens survive server restarts provided the
// secret is unchanged. SameSite=Strict is the primary cross-site defense;
// HMAC signing is defense-in-depth.
type CSRF struct {
	secret []byte
	clock  Clock
}

// NewCSRF creates a CSRF middleware instance. secret must be a non-empty string
// of at least 32 bytes (typically cfg.Auth.SessionSecret after auto-generation).
// NewCSRF panics at startup on an empty or too-short secret so misconfigured
// deployments fail fast rather than silently using a weak HMAC key.
func NewCSRF(secret string) *CSRF {
	if secret == "" {
		panic("csrf: SessionSecret must be configured")
	}
	if len(secret) < csrfMinSecretLen {
		panic("csrf: SessionSecret too short: minimum 32 bytes required")
	}
	return &CSRF{
		secret: []byte(secret),
		clock:  realClock{},
	}
}

// Close is a no-op retained for API compatibility. The stateless HMAC
// implementation has no background goroutines to stop.
func (c *CSRF) Close() {}

// Middleware returns the CSRF handler that validates tokens on unsafe methods.
func (c *CSRF) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Safe methods do not require CSRF validation
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			c.ensureToken(w, r)
			next.ServeHTTP(w, r)
			return
		}

		// Validate CSRF token on state-changing requests.
		// Prefer the header; fall back to form value only for form-encoded requests.
		token := r.Header.Get(csrfTokenHeader)
		if token == "" {
			if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/x-www-form-urlencoded" {
				r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
				token = r.FormValue("csrf_token")
			}
		}

		if token == "" || !c.valid(token) {
			http.Error(w, `{"error":"invalid CSRF token"}`, http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (c *CSRF) ensureToken(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(csrfCookieName); err == nil && cookie.Value != "" {
		if c.valid(cookie.Value) {
			return
		}
	}

	token := c.generate()
	// gosec G124: HttpOnly is intentionally false because the HTMX client must
	// read the cookie value to send it back as the X-CSRF-Token header.
	// Secure is derived from request scheme so plain-HTTP dev installs work.
	// SameSite=Strict provides the cross-origin protection.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly intentionally false (JS-readable); SameSite=Strict + Secure-on-HTTPS protect the token.
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
		Secure:   isSecureRequest(r),
	})
}

// generate returns a new HMAC-signed token as "{ts}:{nonce_hex}:{hmac_hex}".
// The nonce is 16 fresh random bytes so every token is unique per issuance.
func (c *CSRF) generate() string {
	ts := strconv.FormatInt(c.clock.Now().Unix(), 10)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		panic("csrf: crypto/rand failed: " + err.Error())
	}
	nonceHex := hex.EncodeToString(nonce)
	payload := ts + ":" + nonceHex
	mac := c.computeMAC(payload)
	return payload + ":" + hex.EncodeToString(mac)
}

// valid returns true if token is a well-formed, unexpired, correctly-signed
// CSRF token that was issued within the allowed clock skew window.
func (c *CSRF) valid(token string) bool {
	// Token format: "{ts}:{nonce}:{hmac}" — exactly 3 colon-delimited fields.
	parts := strings.SplitN(token, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	tsPart, noncePart, sigPart := parts[0], parts[1], parts[2]

	ts, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return false
	}

	now := c.clock.Now()
	issued := time.Unix(ts, 0)

	// Reject expired tokens.
	if now.Sub(issued) >= csrfTokenTTL {
		return false
	}
	// Reject tokens whose timestamp lies too far in the future (cap clock skew).
	if issued.Sub(now) > csrfMaxClockSkew {
		return false
	}

	// Nonce must be valid hex.
	if _, err := hex.DecodeString(noncePart); err != nil {
		return false
	}

	// Decode the submitted signature and compare in constant time to prevent
	// timing side-channels.
	actual, err := hex.DecodeString(sigPart)
	if err != nil {
		return false
	}
	expected := c.computeMAC(tsPart + ":" + noncePart)
	return hmac.Equal(expected, actual)
}

// computeMAC returns the HMAC-SHA256 of msg using the stored secret.
func (c *CSRF) computeMAC(msg string) []byte {
	h := hmac.New(sha256.New, c.secret)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// isSecureRequest returns true if the request arrived over HTTPS,
// either directly or via a reverse proxy.
func isSecureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}
