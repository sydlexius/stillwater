package middleware

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func TestCSRF_SafeMethodSetsToken(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF()
	handler := csrf.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == csrfCookieName {
			found = true
			if c.Value == "" {
				t.Error("CSRF cookie value is empty")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("SameSite = %v, want Strict", c.SameSite)
			}
		}
	}
	if !found {
		t.Error("CSRF cookie not set on GET request")
	}
}

func TestCSRF_UnsafeMethodWithoutToken(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF()
	handler := csrf.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestCSRF_UnsafeMethodWithValidHeaderToken(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF()
	token := csrf.generate()

	handler := csrf.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(csrfTokenHeader, token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestCSRF_UnsafeMethodWithInvalidToken(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF()
	handler := csrf.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	req.Header.Set(csrfTokenHeader, "bogus-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestCSRF_ExistingValidCookieNotReplaced(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF()
	token := csrf.generate()

	handler := csrf.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Should not set a new cookie since the existing token is valid
	for _, c := range w.Result().Cookies() {
		if c.Name == csrfCookieName {
			t.Error("should not re-set cookie when valid token exists")
		}
	}
}

// TestCSRF_CleanupRemovesExpiredTokens seeds 1000 already-expired tokens,
// fires the cleanup loop via the test-only trigger channel, and asserts
// the entire map is purged. This replaces the old len%100==0 heuristic
// that only ran opportunistically inside generate().
//
// Determinism: the test uses newCSRFForTest (which exposes a trigger
// channel) so the sweep runs synchronously instead of waiting on the
// real ticker. No time.Sleep is used; we Gosched-yield and re-check the
// map size in a bounded loop.
func TestCSRF_CleanupRemovesExpiredTokens(t *testing.T) {
	t.Parallel()
	c := newCSRFForTest(time.Hour) // ticker effectively never fires during the test
	t.Cleanup(c.Close)

	// Seed 1000 tokens with creation times well past csrfTokenTTL so the
	// sweep marks every one of them as expired.
	expired := time.Now().Add(-2 * csrfTokenTTL)
	c.mu.Lock()
	for i := 0; i < 1000; i++ {
		c.tokens[generateRandomTokenForTest(t)] = expired
	}
	if got := len(c.tokens); got != 1000 {
		c.mu.Unlock()
		t.Fatalf("seed: len(tokens) = %d, want 1000", got)
	}
	c.mu.Unlock()

	// Fire the cleanup loop synchronously via the test hook.
	c.trigger <- struct{}{}

	// The cleanup goroutine grabs c.mu under Lock; spin-yield until we
	// observe the post-sweep state. Bounded by a large iteration count
	// so a regression cannot hang the suite.
	const maxIters = 10_000
	for i := 0; i < maxIters; i++ {
		c.mu.RLock()
		n := len(c.tokens)
		c.mu.RUnlock()
		if n == 0 {
			return
		}
		runtime.Gosched()
	}
	c.mu.RLock()
	n := len(c.tokens)
	c.mu.RUnlock()
	t.Fatalf("after cleanup trigger: len(tokens) = %d, want 0", n)
}

// TestCSRF_CleanupKeepsLiveTokens verifies that the sweep only removes
// expired tokens; tokens issued within csrfTokenTTL must remain.
func TestCSRF_CleanupKeepsLiveTokens(t *testing.T) {
	t.Parallel()
	c := newCSRFForTest(time.Hour)
	t.Cleanup(c.Close)

	live := c.generate() // freshly issued, well inside TTL

	// Add 500 expired tokens alongside the live one.
	expired := time.Now().Add(-2 * csrfTokenTTL)
	c.mu.Lock()
	for i := 0; i < 500; i++ {
		c.tokens[generateRandomTokenForTest(t)] = expired
	}
	c.mu.Unlock()

	c.trigger <- struct{}{}

	const maxIters = 10_000
	for i := 0; i < maxIters; i++ {
		c.mu.RLock()
		_, stillLive := c.tokens[live]
		n := len(c.tokens)
		c.mu.RUnlock()
		if n == 1 && stillLive {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("live token was evicted or expired tokens were not swept")
}

// TestCSRF_CloseStopsGoroutine asserts Close is idempotent and that it
// blocks until the cleanup goroutine has fully exited. The second call
// must not panic on the already-closed stop channel.
func TestCSRF_CloseStopsGoroutine(t *testing.T) {
	t.Parallel()
	c := newCSRFForTest(time.Millisecond) // fast ticker so any leak is obvious under -race
	c.Close()
	// Second Close must be a no-op. The done channel is closed before
	// the first Close returns, so the second Close returns immediately.
	c.Close()
	select {
	case <-c.done:
		// expected: goroutine has returned
	default:
		t.Fatal("Close returned but cleanup goroutine is still running")
	}
}

// generateRandomTokenForTest returns a unique opaque string. We use
// crypto/rand via the production generator so test keys live in the
// same key space as real tokens; collisions across the seed loop are
// statistically impossible at 32 random bytes.
func generateRandomTokenForTest(t *testing.T) string {
	t.Helper()
	// A throwaway CSRF with no cleanup goroutine is enough: generate()
	// only touches the local map under its own mutex.
	c := &CSRF{tokens: map[string]time.Time{}}
	return c.generate()
}

func TestCSRF_HeadAndOptionsAreSafe(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF()
	handler := csrf.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, method := range []string{http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(method, "/", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want %d", method, w.Code, http.StatusOK)
		}
	}
}
