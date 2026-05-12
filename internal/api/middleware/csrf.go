package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"mime"
	"net/http"
	"sync"
	"time"
)

const csrfTokenHeader = "X-CSRF-Token" //nolint:gosec // G101: not a credential, this is an HTTP header name
const csrfCookieName = "csrf_token"
const csrfTokenTTL = 24 * time.Hour

// csrfCleanupInterval is how often expired tokens are swept from the in-memory
// map. One hour is well below csrfTokenTTL (24h), which means expired tokens
// never linger long enough to bloat memory under sustained load, but the sweep
// is infrequent enough that the lock-contention cost is negligible.
const csrfCleanupInterval = time.Hour

// CSRF provides token-based CSRF protection for HTMX form submissions.
// It validates that state-changing requests include a matching CSRF token.
//
// A background goroutine started in NewCSRF periodically sweeps expired
// tokens from the in-memory map. Callers must invoke Close exactly once
// (typically tied to server shutdown) to stop the goroutine.
type CSRF struct {
	mu     sync.RWMutex
	tokens map[string]time.Time

	// stop is closed by Close to signal the cleanup goroutine to exit.
	// closeOnce guards against multiple Close calls so the channel is
	// closed at most once (closing a closed channel panics).
	stop      chan struct{}
	closeOnce sync.Once

	// trigger is an optional test hook: sending on it forces a cleanup
	// sweep without waiting for the ticker. Nil in production; tests
	// populate it via newCSRFForTest.
	trigger chan struct{}
	// done is closed by the cleanup goroutine right before it returns.
	// Tests use it to assert the goroutine has fully exited after Close.
	done chan struct{}
}

// NewCSRF creates a CSRF middleware instance and starts its background
// cleanup goroutine. Callers must invoke Close when the instance is no
// longer needed (typically wired to the server's shutdown context) to
// avoid leaking the goroutine.
func NewCSRF() *CSRF {
	c := &CSRF{
		tokens: make(map[string]time.Time),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go c.cleanupLoop(csrfCleanupInterval)
	return c
}

// newCSRFForTest is identical to NewCSRF except it accepts an explicit
// cleanup interval and exposes a trigger channel so tests can fire a
// sweep synchronously rather than waiting on wall-clock time.
func newCSRFForTest(interval time.Duration) *CSRF {
	c := &CSRF{
		tokens:  make(map[string]time.Time),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		trigger: make(chan struct{}, 1),
	}
	go c.cleanupLoop(interval)
	return c
}

// Close stops the background cleanup goroutine. It is safe to call multiple
// times; subsequent calls are no-ops. Close blocks until the goroutine has
// returned, which guarantees no further map mutations after Close returns.
func (c *CSRF) Close() {
	c.closeOnce.Do(func() {
		close(c.stop)
	})
	<-c.done
}

// cleanupLoop runs until stop is closed, sweeping expired tokens on every
// tick. The trigger channel (test-only) forces an immediate sweep.
func (c *CSRF) cleanupLoop(interval time.Duration) {
	defer close(c.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.mu.Lock()
			c.cleanExpiredLocked()
			c.mu.Unlock()
		case <-c.trigger:
			c.mu.Lock()
			c.cleanExpiredLocked()
			c.mu.Unlock()
		}
	}
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
		c.mu.RLock()
		_, exists := c.tokens[cookie.Value]
		c.mu.RUnlock()
		if exists {
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

func (c *CSRF) generate() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	token := hex.EncodeToString(b)

	c.mu.Lock()
	c.tokens[token] = time.Now()
	c.mu.Unlock()

	return token
}

func (c *CSRF) valid(token string) bool {
	c.mu.RLock()
	created, exists := c.tokens[token]
	c.mu.RUnlock()
	if !exists {
		return false
	}
	return time.Since(created) < csrfTokenTTL
}

func (c *CSRF) cleanExpiredLocked() {
	cutoff := time.Now().Add(-csrfTokenTTL)
	for t, created := range c.tokens {
		if created.Before(cutoff) {
			delete(c.tokens, t)
		}
	}
}

// isSecureRequest returns true if the request arrived over HTTPS,
// either directly or via a reverse proxy.
func isSecureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}
