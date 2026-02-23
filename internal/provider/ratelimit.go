package provider

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// Default rate limits per provider (requests per second).
var defaultRateLimits = map[ProviderName]rate.Limit{
	NameMusicBrainz: 1,
	NameFanartTV:    3,
	NameAudioDB:     2,
	NameDiscogs:     1,
	NameLastFM:      5,
	NameWikidata:    5,
	NameDuckDuckGo:  1,
}

// RateLimiterMap holds one rate.Limiter per provider, created once at startup.
type RateLimiterMap struct {
	mu       sync.RWMutex
	limiters map[ProviderName]*rate.Limiter
}

// NewRateLimiterMap creates all provider rate limiters.
func NewRateLimiterMap() *RateLimiterMap {
	m := &RateLimiterMap{
		limiters: make(map[ProviderName]*rate.Limiter, len(defaultRateLimits)),
	}
	for name, limit := range defaultRateLimits {
		m.limiters[name] = rate.NewLimiter(limit, 1)
	}
	return m
}

// Wait blocks until the rate limiter for the given provider allows a request,
// or the context is canceled.
func (m *RateLimiterMap) Wait(ctx context.Context, name ProviderName) error {
	m.mu.RLock()
	limiter, ok := m.limiters[name]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	return limiter.Wait(ctx)
}
