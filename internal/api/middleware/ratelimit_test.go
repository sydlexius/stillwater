package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimiter_AllowsBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx)

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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx)

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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl := NewLoginRateLimiter(ctx)

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
	// Should not panic with nil context
	rl := NewLoginRateLimiter(nil) //nolint:staticcheck // SA1012: testing nil context defense
	_ = rl
}

func TestClientIP_DirectConnection(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"

	got := clientIP(req)
	if got != "203.0.113.5" {
		t.Errorf("clientIP = %q, want %q", got, "203.0.113.5")
	}
}

func TestClientIP_XFFFromPrivateProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.10, 10.0.0.1")

	got := clientIP(req)
	// Should use rightmost IP from XFF when RemoteAddr is private
	if got != "10.0.0.1" {
		t.Errorf("clientIP = %q, want %q", got, "10.0.0.1")
	}
}

func TestClientIP_XFFIgnoredFromPublicIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")

	got := clientIP(req)
	// Should NOT honor XFF when RemoteAddr is public
	if got != "203.0.113.5" {
		t.Errorf("clientIP = %q, want %q (XFF should be ignored for public remote)", got, "203.0.113.5")
	}
}

func TestClientIP_XRealIPFromPrivateProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.1:1234"
	req.Header.Set("X-Real-Ip", "203.0.113.20")

	got := clientIP(req)
	if got != "203.0.113.20" {
		t.Errorf("clientIP = %q, want %q", got, "203.0.113.20")
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"192.168.1.1", true},
		{"172.16.0.1", true},
		{"203.0.113.1", false},
		{"8.8.8.8", false},
		{"not-an-ip", false},
		{"::1", true},
	}
	for _, tt := range tests {
		got := isPrivateIP(tt.ip)
		if got != tt.want {
			t.Errorf("isPrivateIP(%q) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}
