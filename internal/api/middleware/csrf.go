package middleware

import (
	"crypto/hmac"
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
// valid when the user returns in the morning. The HMAC signature prevents any
// attacker from forging an unexpired token without the server secret.
const csrfTokenTTL = 4 * time.Hour

// Clock is the time source used by CSRF for token creation and expiry checks.
// The default production implementation returns time.Now().UTC().
type Clock interface {
	Now() time.Time
}

// realClock is the production Clock implementation.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// CSRF provides stateless HMAC-based CSRF protection for HTMX form submissions.
// Tokens are self-describing "{unix_timestamp}:{hmac_sha256_hex}" strings signed
// with the application's SessionSecret. Because all state needed to validate a
// token is encoded in the token itself, no in-memory map is required and tokens
// survive server restarts without invalidating existing user sessions.
//
// SameSite=Strict is the primary cross-site defense; HMAC signing is
// defense-in-depth. Callers must supply a non-empty secret; NewCSRF panics on
// an empty string so misconfigured deployments fail fast at startup.
type CSRF struct {
	secret []byte
	clock  Clock
}

// NewCSRF creates a CSRF middleware instance backed by HMAC-signed tokens.
// secret must be a non-empty string (typically cfg.Auth.SessionSecret); NewCSRF
// panics immediately if secret is empty so operators discover the missing
// configuration on startup rather than running with a worthless HMAC key.
func NewCSRF(secret string) *CSRF {
	if secret == "" {
		panic("csrf: SessionSecret must be configured")
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

// generate returns a new HMAC-signed token as "{unix_timestamp}:{hmac_sha256_hex}".
func (c *CSRF) generate() string {
	ts := strconv.FormatInt(c.clock.Now().Unix(), 10)
	mac := c.computeMAC(ts)
	return ts + ":" + hex.EncodeToString(mac)
}

// valid returns true if token is a well-formed, unexpired, correctly-signed CSRF token.
func (c *CSRF) valid(token string) bool {
	tsPart, sigPart, found := strings.Cut(token, ":")
	if !found || tsPart == "" || sigPart == "" {
		return false
	}

	ts, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return false
	}

	// Reject expired tokens.
	issued := time.Unix(ts, 0)
	if c.clock.Now().Sub(issued) >= csrfTokenTTL {
		return false
	}

	// Decode the submitted signature and compare in constant time to prevent
	// timing side-channels.
	actual, err := hex.DecodeString(sigPart)
	if err != nil {
		return false
	}
	expected := c.computeMAC(tsPart)
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
