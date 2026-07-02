package middleware

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// LoginRateLimiter provides IP-based rate limiting for authentication endpoints.
type LoginRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*ipLimiter
	// trustedProxies is the set of CIDR ranges whose direct connections are
	// trusted to set X-Forwarded-For / X-Real-Ip. Parsed once at construction
	// from the operator-configured strings (already CIDR-validated by config).
	// Empty means no proxy is trusted and forwarded headers are always ignored.
	trustedProxies []netip.Prefix
}

// NewLoginRateLimiter creates a rate limiter that cleans up stale entries periodically.
// The cleanup goroutine stops when the provided context is canceled. trustedProxies
// are CIDR strings (already validated by config.validateTrustedProxies); any that
// fail to parse here are skipped defensively so a single bad entry cannot disable
// rate limiting entirely.
//
//nolint:contextcheck // boot-time constructor; ctx is the long-lived app context that governs the cleanup goroutine, not a request ctx
func NewLoginRateLimiter(ctx context.Context, trustedProxies []string) *LoginRateLimiter {
	if ctx == nil {
		ctx = context.Background()
	}
	prefixes := make([]netip.Prefix, 0, len(trustedProxies))
	for _, p := range trustedProxies {
		if prefix, err := netip.ParsePrefix(p); err == nil {
			prefixes = append(prefixes, prefix)
		}
	}
	rl := &LoginRateLimiter{
		limiters:       make(map[string]*ipLimiter),
		trustedProxies: prefixes,
	}
	go rl.cleanup(ctx)
	return rl
}

// Middleware returns an HTTP middleware that rate-limits requests by client IP.
// Allows 5 requests per minute with a burst of 5.
func (rl *LoginRateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.clientIP(r)
		limiter := rl.getLimiter(ip)
		if !limiter.Allow() {
			http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (rl *LoginRateLimiter) getLimiter(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, exists := rl.limiters[ip]
	if !exists {
		// 5 requests per minute (1 every 12 seconds) with burst of 5
		entry = &ipLimiter{
			limiter: rate.NewLimiter(rate.Every(12*time.Second), 5),
		}
		rl.limiters[ip] = entry
	}
	entry.lastSeen = time.Now()
	return entry.limiter
}

func (rl *LoginRateLimiter) cleanup(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rl.mu.Lock()
			for ip, entry := range rl.limiters {
				if time.Since(entry.lastSeen) > 15*time.Minute {
					delete(rl.limiters, ip)
				}
			}
			rl.mu.Unlock()
		}
	}
}

func (rl *LoginRateLimiter) clientIP(r *http.Request) string {
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if remoteIP == "" {
		remoteIP = r.RemoteAddr
	}

	// Only honor proxy headers when the DIRECT peer is inside an operator-configured
	// trusted-proxy range. A LAN client that is not itself a trusted proxy can no
	// longer spoof X-Forwarded-For to escape its own rate-limit bucket (#2171).
	if rl.isTrustedProxy(remoteIP) {
		// Use the rightmost XFF IP (added by the nearest trusted proxy) to resist spoofing.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if canon, ok := canonicalIP(strings.TrimSpace(parts[len(parts)-1])); ok {
				return canon
			}
		}
		if xri := r.Header.Get("X-Real-Ip"); xri != "" {
			if canon, ok := canonicalIP(strings.TrimSpace(xri)); ok {
				return canon
			}
		}
	}

	return remoteIP
}

// canonicalIP parses s as an IP address and returns its canonical string form,
// or ok=false if s is not a valid IP. Canonicalizing collapses alternate
// representations of the same address (e.g. the IPv4-mapped IPv6 "::ffff:1.2.3.4"
// and "1.2.3.4") onto a single rate-limit bucket via Unmap. Rejecting non-IP
// values makes the caller fall back to the direct peer address rather than
// keying the limiter on attacker-controlled garbage, which would otherwise
// fragment or pollute the bucket map.
func canonicalIP(s string) (string, bool) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return "", false
	}
	return addr.Unmap().String(), true
}

// isTrustedProxy reports whether the direct-peer address falls inside any
// configured trusted-proxy CIDR range. An unparsable address or an empty
// trusted set returns false, so forwarded headers are ignored by default.
func (rl *LoginRateLimiter) isTrustedProxy(ipStr string) bool {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false
	}
	for _, prefix := range rl.trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
