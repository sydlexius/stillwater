package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testSecret is the HMAC key used throughout the CSRF test suite.
const testSecret = "test-secret-for-csrf-hmac-signing"

// testClock is a fixed-time Clock for tests. It always returns the same
// instant so time-dependent logic in CSRF is independent of wall-clock speed.
type testClock struct {
	now time.Time
}

func (c testClock) Now() time.Time { return c.now }

// fixedTestTime is the reference instant used across time-sensitive CSRF tests.
var fixedTestTime = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

func TestCSRF_SafeMethodSetsToken(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF(testSecret)
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
		if c.Name != csrfCookieName {
			continue
		}
		found = true
		// Token must be in "{timestamp}:{hmac_hex}" format.
		tsPart, sigPart, ok := strings.Cut(c.Value, ":")
		if !ok {
			t.Fatalf("CSRF cookie value has no colon: %q", c.Value)
		}
		if _, err := strconv.ParseInt(tsPart, 10, 64); err != nil {
			t.Errorf("CSRF token timestamp part is not an integer: %q", tsPart)
		}
		if len(sigPart) != 64 {
			t.Errorf("CSRF token signature part length = %d, want 64 hex chars", len(sigPart))
		}
		if c.SameSite != http.SameSiteStrictMode {
			t.Errorf("SameSite = %v, want Strict", c.SameSite)
		}
	}
	if !found {
		t.Error("CSRF cookie not set on GET request")
	}
}

func TestCSRF_UnsafeMethodWithoutToken(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF(testSecret)
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
	csrf := NewCSRF(testSecret)
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
	csrf := NewCSRF(testSecret)
	handler := csrf.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name  string
		token string
	}{
		{"bogus string", "bogus-token"},
		{"no colon", "1735729200"},
		{"bad timestamp", "notanumber:aabbccdd"},
		{"wrong part count (no colon)", "onepartonly"},
		{"empty signature", "1735729200:"},
		{"empty timestamp", ":aabbccdd"},
		{"bad hex in signature", "1735729200:notvalidhex!"},
		{"tampered signature", "1735729200:" + strings.Repeat("ff", 32)},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		req.Header.Set(csrfTokenHeader, tc.token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("case %q: status = %d, want %d", tc.name, w.Code, http.StatusForbidden)
		}
	}
}

func TestCSRF_ExistingValidCookieNotReplaced(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF(testSecret)
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

	// Should not set a new cookie because the existing HMAC-signed token is valid.
	for _, c := range w.Result().Cookies() {
		if c.Name == csrfCookieName {
			t.Error("should not re-set cookie when a valid HMAC token is already present")
		}
	}
}

func TestCSRF_HeadAndOptionsAreSafe(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF(testSecret)
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

// TestCSRF_ExpiredTokenRejected asserts that a token whose timestamp is older
// than csrfTokenTTL is rejected, simulating a restart that happened 4h ago.
func TestCSRF_ExpiredTokenRejected(t *testing.T) {
	t.Parallel()

	clk := testClock{now: fixedTestTime}
	c := &CSRF{secret: []byte(testSecret), clock: clk}

	// Issue a token at fixedTestTime, then advance the clock beyond TTL.
	token := c.generate()

	clk.now = fixedTestTime.Add(csrfTokenTTL) // exactly at TTL boundary: still invalid (>= not <)
	c.clock = clk
	if c.valid(token) {
		t.Error("token at exactly TTL boundary should be rejected (>= TTL)")
	}

	clk.now = fixedTestTime.Add(csrfTokenTTL + time.Second)
	c.clock = clk
	if c.valid(token) {
		t.Error("token older than csrfTokenTTL should be rejected")
	}
}

// TestCSRF_FreshTokenAccepted checks that a token just issued is accepted.
func TestCSRF_FreshTokenAccepted(t *testing.T) {
	t.Parallel()

	clk := testClock{now: fixedTestTime}
	c := &CSRF{secret: []byte(testSecret), clock: clk}

	token := c.generate()
	if !c.valid(token) {
		t.Error("freshly issued token should be valid")
	}

	// Token just under the TTL boundary should also be valid.
	clk.now = fixedTestTime.Add(csrfTokenTTL - time.Second)
	c.clock = clk
	if !c.valid(token) {
		t.Error("token just under TTL should still be valid")
	}
}

// TestCSRF_TamperedSignatureRejected verifies that modifying the HMAC component
// of a well-formed token causes it to be rejected.
func TestCSRF_TamperedSignatureRejected(t *testing.T) {
	t.Parallel()

	csrf := NewCSRF(testSecret)
	token := csrf.generate()

	// Replace the signature with the correct-length but wrong bytes.
	tsPart, _, _ := strings.Cut(token, ":")
	tampered := tsPart + ":" + strings.Repeat("aa", 32) // 64 hex chars, wrong value

	if csrf.valid(tampered) {
		t.Error("token with tampered signature should be rejected")
	}
}

// TestCSRF_EmptySecretPanics verifies that NewCSRF panics immediately when
// called with an empty secret, so misconfigured deployments fail at startup.
func TestCSRF_EmptySecretPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewCSRF(\"\") should panic but did not")
		}
	}()
	NewCSRF("")
}

// TestCSRF_DifferentSecretsProduceDifferentTokens checks that the same
// timestamp signed with two different secrets produces different HMACs, so
// tokens from a different (or rotated) server cannot be replayed.
func TestCSRF_DifferentSecretsProduceDifferentTokens(t *testing.T) {
	t.Parallel()

	clk := testClock{now: fixedTestTime}
	c1 := &CSRF{secret: []byte("secret-one"), clock: clk}
	c2 := &CSRF{secret: []byte("secret-two"), clock: clk}

	tok1 := c1.generate()
	tok2 := c2.generate()

	if tok1 == tok2 {
		t.Error("tokens with different secrets should differ")
	}

	// Cross-validation: each instance must reject the other's token.
	if c1.valid(tok2) {
		t.Error("c1 should not validate a token signed by c2")
	}
	if c2.valid(tok1) {
		t.Error("c2 should not validate a token signed by c1")
	}
}

// TestCSRF_SameSecretConsistentValidation confirms that a token generated by
// one CSRF instance is accepted by a second instance sharing the same secret,
// which is the core property that makes restart-survival work.
func TestCSRF_SameSecretConsistentValidation(t *testing.T) {
	t.Parallel()

	clk := testClock{now: fixedTestTime}
	before := &CSRF{secret: []byte(testSecret), clock: clk}
	after := &CSRF{secret: []byte(testSecret), clock: clk} // simulates the new instance after restart

	token := before.generate()
	if !after.valid(token) {
		t.Error("token generated before restart should be valid on the new instance with the same secret")
	}
}

// TestCSRF_FutureTimestampValid checks that a token with a slightly future
// timestamp (e.g., clock skew between servers) is not rejected. Since the
// HMAC prevents forging, a valid signature on a future timestamp is benign.
func TestCSRF_FutureTimestampValid(t *testing.T) {
	t.Parallel()

	clk := testClock{now: fixedTestTime}
	// Generate a token with a timestamp 1 minute in the future relative to
	// the validator's clock. This should still be valid (not expired).
	futureClk := testClock{now: fixedTestTime.Add(time.Minute)}
	gen := &CSRF{secret: []byte(testSecret), clock: futureClk}
	validator := &CSRF{secret: []byte(testSecret), clock: clk}

	token := gen.generate()
	// time.Since(future) is negative, which is < csrfTokenTTL, so it passes.
	if !validator.valid(token) {
		t.Error("token with slightly future timestamp should be accepted (within TTL window)")
	}
}

// TestCSRF_CloseIsNoOp verifies Close can be called multiple times without
// panicking (retained for API compatibility with callers that still invoke it).
func TestCSRF_CloseIsNoOp(t *testing.T) {
	t.Parallel()
	csrf := NewCSRF(testSecret)
	csrf.Close()
	csrf.Close() // must not panic
}
