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
// It is >= 32 bytes to pass the minimum-length guard in NewCSRF.
const testSecret = "test-secret-for-csrf-hmac-signing" // 33 bytes

// testClock is a fixed-time Clock for tests. It always returns the same
// instant so time-dependent logic in CSRF is independent of wall-clock speed.
type testClock struct {
	now time.Time
}

func (c testClock) Now() time.Time { return c.now }

// fixedTestTime is the reference instant used across time-sensitive CSRF tests.
var fixedTestTime = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

// tokenParts splits a 3-part CSRF token and returns (tsPart, noncePart, sigPart, ok).
func tokenParts(token string) (string, string, string, bool) {
	parts := strings.SplitN(token, ":", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

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
		tsPart, noncePart, sigPart, ok := tokenParts(c.Value)
		if !ok {
			t.Fatalf("CSRF cookie is not 3-part {ts}:{nonce}:{sig}: %q", c.Value)
		}
		if _, err := strconv.ParseInt(tsPart, 10, 64); err != nil {
			t.Errorf("CSRF token timestamp part is not an integer: %q", tsPart)
		}
		// Nonce = 16 bytes = 32 hex chars.
		if len(noncePart) != 32 {
			t.Errorf("CSRF token nonce part length = %d, want 32 hex chars", len(noncePart))
		}
		// Signature = HMAC-SHA256 = 32 bytes = 64 hex chars.
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

	validToken := csrf.generate()
	tsPart, noncePart, _, _ := tokenParts(validToken)

	cases := []struct {
		name  string
		token string
	}{
		{"bogus string", "bogus-token"},
		{"only 1 part (no colon)", "1735729200"},
		{"only 2 parts (missing nonce/sig)", "1735729200:aabbccdd"},
		{"bad timestamp", "notanumber:" + strings.Repeat("aa", 16) + ":" + strings.Repeat("ff", 32)},
		{"empty signature", tsPart + ":" + noncePart + ":"},
		{"empty nonce", tsPart + "::" + strings.Repeat("ff", 32)},
		{"empty timestamp", ":" + noncePart + ":" + strings.Repeat("ff", 32)},
		{"bad hex in nonce", tsPart + ":notvalidhex!:" + strings.Repeat("ff", 32)},
		{"bad hex in signature", tsPart + ":" + noncePart + ":notvalidhex!"},
		{"tampered signature", tsPart + ":" + noncePart + ":" + strings.Repeat("aa", 32)},
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

// TestCSRF_ExpiredTokenRejected asserts that a token whose timestamp is at or
// beyond csrfTokenTTL is rejected, simulating a session surviving a long restart.
func TestCSRF_ExpiredTokenRejected(t *testing.T) {
	t.Parallel()

	clk := testClock{now: fixedTestTime}
	c := &CSRF{secret: []byte(testSecret), clock: clk}

	token := c.generate()

	// Exactly at TTL boundary: >= TTL means expired.
	clk.now = fixedTestTime.Add(csrfTokenTTL)
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

// TestCSRF_FreshTokenAccepted checks that a token just issued and one just under
// the TTL boundary are both accepted.
func TestCSRF_FreshTokenAccepted(t *testing.T) {
	t.Parallel()

	clk := testClock{now: fixedTestTime}
	c := &CSRF{secret: []byte(testSecret), clock: clk}

	token := c.generate()
	if !c.valid(token) {
		t.Error("freshly issued token should be valid")
	}

	clk.now = fixedTestTime.Add(csrfTokenTTL - time.Second)
	c.clock = clk
	if !c.valid(token) {
		t.Error("token just under TTL should still be valid")
	}
}

// TestCSRF_TamperedSignatureRejected verifies that replacing the HMAC component
// with the wrong bytes causes rejection even when the timestamp and nonce are valid.
func TestCSRF_TamperedSignatureRejected(t *testing.T) {
	t.Parallel()

	csrf := NewCSRF(testSecret)
	token := csrf.generate()

	tsPart, noncePart, _, _ := tokenParts(token)
	tampered := tsPart + ":" + noncePart + ":" + strings.Repeat("aa", 32) // wrong HMAC

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

// TestCSRF_TooShortSecretPanics verifies that NewCSRF panics when the secret is
// shorter than csrfMinSecretLen (F2: minimum 32-byte enforcement).
func TestCSRF_TooShortSecretPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewCSRF with short secret should panic but did not")
		}
	}()
	NewCSRF("tooshort") // 8 bytes < 32
}

// TestCSRF_DifferentSecretsProduceDifferentTokens checks that the same
// timestamp signed with two different secrets produces different HMACs.
func TestCSRF_DifferentSecretsProduceDifferentTokens(t *testing.T) {
	t.Parallel()

	clk := testClock{now: fixedTestTime}
	c1 := &CSRF{secret: []byte("secret-one-padded-to-32-bytes!!X"), clock: clk}
	c2 := &CSRF{secret: []byte("secret-two-padded-to-32-bytes!!Y"), clock: clk}

	tok1 := c1.generate()
	tok2 := c2.generate()

	// Tokens will differ due to different nonces OR different HMACs (or both).
	// The HMAC check is what matters: each instance must reject the other's token.
	if c1.valid(tok2) {
		t.Error("c1 should not validate a token signed by c2")
	}
	if c2.valid(tok1) {
		t.Error("c2 should not validate a token signed by c1")
	}
}

// TestCSRF_SameSecretConsistentValidation is the core restart-survival test:
// a token generated before a simulated restart validates on the new instance
// sharing the same secret (F3 property via design, not just F1 property).
func TestCSRF_SameSecretConsistentValidation(t *testing.T) {
	t.Parallel()

	clk := testClock{now: fixedTestTime}
	before := &CSRF{secret: []byte(testSecret), clock: clk}
	after := &CSRF{secret: []byte(testSecret), clock: clk} // new instance after restart

	token := before.generate()
	if !after.valid(token) {
		t.Error("token generated before restart should be valid on the new instance with the same secret")
	}
}

// TestCSRF_NonceUniquePerIssuance verifies that two tokens generated at the
// same instant carry different nonces (F3: per-issuance uniqueness).
func TestCSRF_NonceUniquePerIssuance(t *testing.T) {
	t.Parallel()

	clk := testClock{now: fixedTestTime}
	c := &CSRF{secret: []byte(testSecret), clock: clk}

	tok1 := c.generate()
	tok2 := c.generate()

	_, nonce1, _, _ := tokenParts(tok1)
	_, nonce2, _, _ := tokenParts(tok2)

	if nonce1 == nonce2 {
		t.Error("two tokens generated at the same instant should have different nonces")
	}
}

// TestCSRF_FutureTimestampWithinSkewAccepted verifies that a token whose
// timestamp is slightly in the future (within csrfMaxClockSkew) is accepted,
// since this is normal NTP drift territory.
func TestCSRF_FutureTimestampWithinSkewAccepted(t *testing.T) {
	t.Parallel()

	futureClk := testClock{now: fixedTestTime.Add(csrfMaxClockSkew - time.Minute)}
	gen := &CSRF{secret: []byte(testSecret), clock: futureClk}
	validator := &CSRF{secret: []byte(testSecret), clock: testClock{now: fixedTestTime}}

	token := gen.generate()
	if !validator.valid(token) {
		t.Error("token with future timestamp within clock-skew window should be accepted")
	}
}

// TestCSRF_FarFutureTimestampRejected verifies that a token timestamped beyond
// csrfMaxClockSkew in the future is rejected (F4: future-timestamp cap).
func TestCSRF_FarFutureTimestampRejected(t *testing.T) {
	t.Parallel()

	// Generate a token timestamped csrfMaxClockSkew+1s in the future.
	farFutureClk := testClock{now: fixedTestTime.Add(csrfMaxClockSkew + time.Second)}
	gen := &CSRF{secret: []byte(testSecret), clock: farFutureClk}
	validator := &CSRF{secret: []byte(testSecret), clock: testClock{now: fixedTestTime}}

	token := gen.generate()
	if validator.valid(token) {
		t.Error("token with far-future timestamp should be rejected")
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
