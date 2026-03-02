package provider

import (
	"testing"

	"golang.org/x/time/rate"
)

func TestSetLimit(t *testing.T) {
	m := NewRateLimiterMap()

	// Default MusicBrainz limit is 1 req/s.
	defaultLimit := DefaultLimit(NameMusicBrainz)
	if defaultLimit != 1 {
		t.Fatalf("expected default MusicBrainz limit 1, got %v", defaultLimit)
	}

	// Set a custom limit.
	m.SetLimit(NameMusicBrainz, 10)

	// The new limiter should exist and be usable (we just verify it doesn't panic).
	m.mu.RLock()
	limiter := m.limiters[NameMusicBrainz]
	m.mu.RUnlock()
	if limiter == nil {
		t.Fatal("expected non-nil limiter after SetLimit")
	}
	if limiter.Limit() != 10 {
		t.Errorf("expected limit 10, got %v", limiter.Limit())
	}
}

func TestDefaultLimit(t *testing.T) {
	tests := []struct {
		name  ProviderName
		want  rate.Limit
		found bool
	}{
		{NameMusicBrainz, 1, true},
		{NameFanartTV, 3, true},
		{ProviderName("unknown"), 0, false},
	}

	for _, tt := range tests {
		got := DefaultLimit(tt.name)
		if got != tt.want {
			t.Errorf("DefaultLimit(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
