package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
)

const csrfTokenHeader = "X-CSRF-Token" //nolint:gosec // G101: not a credential, this is an HTTP header name
const csrfCookieName = "csrf_token"

// CSRF provides token-based CSRF protection for HTMX form submissions.
// It validates that state-changing requests include a matching CSRF token.
type CSRF struct {
	mu     sync.RWMutex
	tokens map[string]bool
}

// NewCSRF creates a CSRF middleware instance.
func NewCSRF() *CSRF {
	return &CSRF{tokens: make(map[string]bool)}
}

// Middleware returns the CSRF handler that validates tokens on unsafe methods.
func (c *CSRF) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Safe methods do not require CSRF validation
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			c.ensureToken(w, r)
			next.ServeHTTP(w, r)
			return
		}

		// Validate CSRF token on state-changing requests
		token := r.Header.Get(csrfTokenHeader)
		if token == "" {
			token = r.FormValue("csrf_token")
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
		c.mu.RLock()
		exists := c.tokens[cookie.Value]
		c.mu.RUnlock()
		if exists {
			return
		}
	}

	token := c.generate()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false, // JS needs to read this for HTMX headers
		SameSite: http.SameSiteStrictMode,
		Secure:   true,
	})
}

func (c *CSRF) generate() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	token := hex.EncodeToString(b)

	c.mu.Lock()
	c.tokens[token] = true
	c.mu.Unlock()

	return token
}

func (c *CSRF) valid(token string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tokens[token]
}
