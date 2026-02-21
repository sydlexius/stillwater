package middleware

import (
	"context"
	"net"
	"net/http"
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
}

// NewLoginRateLimiter creates a rate limiter that cleans up stale entries periodically.
// The cleanup goroutine stops when the provided context is canceled.
func NewLoginRateLimiter(ctx context.Context) *LoginRateLimiter {
	if ctx == nil {
		ctx = context.Background()
	}
	rl := &LoginRateLimiter{
		limiters: make(map[string]*ipLimiter),
	}
	go rl.cleanup(ctx)
	return rl
}

// Middleware returns an HTTP middleware that rate-limits requests by client IP.
// Allows 5 requests per minute with a burst of 5.
func (rl *LoginRateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
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

func clientIP(r *http.Request) string {
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if remoteIP == "" {
		remoteIP = r.RemoteAddr
	}

	// Only honor proxy headers when the direct connection is from a private/loopback IP,
	// which indicates a trusted reverse proxy is in front of the application.
	if isPrivateIP(remoteIP) {
		// Use the rightmost XFF IP (added by the nearest trusted proxy) to resist spoofing.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			ip := strings.TrimSpace(parts[len(parts)-1])
			if ip != "" {
				return ip
			}
		}
		if xri := r.Header.Get("X-Real-Ip"); xri != "" {
			return xri
		}
	}

	return remoteIP
}

// isPrivateIP checks if an IP address is in a private or loopback range.
func isPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
