package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimiter_AllowsBurst(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, nil)

	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Burst of 5 should be allowed
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = "203.0.113.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want %d", i+1, w.Code, http.StatusOK)
		}
	}
}

func TestRateLimiter_BlocksAfterBurst(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, nil)

	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust the burst
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = "203.0.113.2:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	// Next request should be rate limited
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "203.0.113.2:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
}

func TestRateLimiter_DifferentIPsIndependent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, nil)

	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust IP1 burst
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = "203.0.113.3:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	// IP2 should still be allowed
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "203.0.113.4:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (different IP should not be rate limited)", w.Code, http.StatusOK)
	}
}

func TestRateLimiter_NilContext(t *testing.T) {
	t.Parallel()
	// Should not panic with nil context
	rl := NewLoginRateLimiter(nil, nil) //nolint:staticcheck // SA1012: testing nil context defense
	_ = rl
}

// TestRateLimiter_SpoofedXFFFromUntrustedPeerDoesNotBypass is the end-to-end
// #2171 guard: an untrusted LAN client cannot escape its own rate-limit bucket
// by rotating X-Forwarded-For. All requests share the direct-peer bucket.
func TestRateLimiter_SpoofedXFFFromUntrustedPeerDoesNotBypass(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Trust only 10.0.0.0/8; the attacker connects from 192.168.x (untrusted).
	rl := NewLoginRateLimiter(ctx, []string{"10.0.0.0/8"})

	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust the burst from a single untrusted peer, each time with a fresh
	// spoofed XFF that would (if honored) land in a different bucket.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.RemoteAddr = "192.168.1.50:1234"
		req.Header.Set("X-Forwarded-For", "203.0.113."+string(rune('1'+i)))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	// The 6th request, again with a novel spoofed XFF, must still be blocked:
	// the direct peer's bucket is exhausted regardless of the header.
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "192.168.1.50:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.200")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d (spoofed XFF from untrusted peer must not bypass limiting)", w.Code, http.StatusTooManyRequests)
	}
}

func TestClientIP_DirectConnection(t *testing.T) {
	t.Parallel()
	rl := &LoginRateLimiter{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"

	got := rl.clientIP(req)
	if got != "203.0.113.5" {
		t.Errorf("clientIP = %q, want %q", got, "203.0.113.5")
	}
}

// TestClientIP_TrustedProxyHonorsXFF: when the direct peer is inside a trusted
// range, the forwarded client IP is honored.
func TestClientIP_TrustedProxyHonorsXFF(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, []string{"10.0.0.0/8"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.10")

	got := rl.clientIP(req)
	if got != "203.0.113.10" {
		t.Errorf("clientIP = %q, want %q (trusted peer XFF should be honored)", got, "203.0.113.10")
	}
}

// TestClientIP_UntrustedPeerIgnoresXFF is the #2171 unit-level guard: a peer
// that is NOT inside a trusted range cannot spoof XFF; its direct IP is used.
func TestClientIP_UntrustedPeerIgnoresXFF(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, []string{"10.0.0.0/8"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.9:1234" // not in 10.0.0.0/8
	req.Header.Set("X-Forwarded-For", "203.0.113.10")

	got := rl.clientIP(req)
	if got != "192.168.1.9" {
		t.Errorf("clientIP = %q, want %q (untrusted peer XFF must be ignored)", got, "192.168.1.9")
	}
}

// TestClientIP_XRealIPFromTrustedProxy: X-Real-Ip is honored from a trusted peer.
func TestClientIP_XRealIPFromTrustedProxy(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, []string{"192.168.0.0/16"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.1:1234"
	req.Header.Set("X-Real-Ip", "203.0.113.20")

	got := rl.clientIP(req)
	if got != "203.0.113.20" {
		t.Errorf("clientIP = %q, want %q", got, "203.0.113.20")
	}
}

// TestClientIP_NoTrustedProxiesIgnoresXFF: the default (empty trusted set)
// trusts no proxy, so forwarded headers are ignored even from loopback.
func TestClientIP_NoTrustedProxiesIgnoresXFF(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.10")

	got := rl.clientIP(req)
	if got != "127.0.0.1" {
		t.Errorf("clientIP = %q, want %q (no trusted proxies means XFF ignored)", got, "127.0.0.1")
	}
}

// TestClientIP_CanonicalizesForwardedIP: two representations of the SAME address
// forwarded by a trusted proxy (an IPv4-mapped IPv6 form and its plain IPv4 form)
// must canonicalize to a single rate-limit key, so they share one bucket rather
// than each getting an independent allowance. Without the parse/canonicalize step
// the raw header strings differ and this returns two distinct keys.
func TestClientIP_CanonicalizesForwardedIP(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, []string{"10.0.0.0/8"})

	mapped := httptest.NewRequest(http.MethodGet, "/", nil)
	mapped.RemoteAddr = "10.0.0.5:1234"
	mapped.Header.Set("X-Forwarded-For", "::ffff:203.0.113.77")

	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	plain.RemoteAddr = "10.0.0.5:1234"
	plain.Header.Set("X-Forwarded-For", "203.0.113.77")

	gotMapped := rl.clientIP(mapped)
	gotPlain := rl.clientIP(plain)
	if gotMapped != gotPlain {
		t.Fatalf("clientIP not canonicalized: mapped=%q plain=%q (want equal so they share one bucket)", gotMapped, gotPlain)
	}
	if gotPlain != "203.0.113.77" {
		t.Errorf("clientIP = %q, want %q (canonical IPv4 form)", gotPlain, "203.0.113.77")
	}
}

// TestClientIP_GarbageForwardedIPFallsBackToPeer: a non-IP X-Forwarded-For value
// from a trusted peer must NOT be used as a rate-limit key (attacker-controlled
// garbage would fragment/pollute the bucket map); the direct peer IP is used
// instead. Without the parse/validate step the raw "not-an-ip" string would be
// returned as the key.
func TestClientIP_GarbageForwardedIPFallsBackToPeer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, []string{"10.0.0.0/8"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "not-an-ip")

	got := rl.clientIP(req)
	if got != "10.0.0.5" {
		t.Errorf("clientIP = %q, want %q (garbage XFF must fall back to direct peer)", got, "10.0.0.5")
	}
}

func TestIsTrustedProxy(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, []string{"10.0.0.0/8", "192.168.0.0/16", "::1/128"})
	tests := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"192.168.1.1", true},
		{"::1", true},
		{"172.16.0.1", false}, // private but not configured
		{"203.0.113.1", false},
		{"8.8.8.8", false},
		{"not-an-ip", false},
	}
	for _, tt := range tests {
		if got := rl.isTrustedProxy(tt.ip); got != tt.want {
			t.Errorf("isTrustedProxy(%q) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

// TestNewLoginRateLimiter_SkipsMalformedPrefix: a bad entry (which config
// validation should already have rejected) is skipped rather than panicking or
// disabling limiting; the valid entries still take effect.
func TestNewLoginRateLimiter_SkipsMalformedPrefix(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx, []string{"not-a-cidr", "10.0.0.0/8"})
	if !rl.isTrustedProxy("10.0.0.1") {
		t.Error("valid prefix 10.0.0.0/8 should be honored despite a malformed sibling entry")
	}
	if rl.isTrustedProxy("192.168.1.1") {
		t.Error("malformed entry must not widen the trusted set")
	}
}
