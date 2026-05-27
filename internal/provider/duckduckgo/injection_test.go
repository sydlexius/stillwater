package duckduckgo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestInjection_DuckDuckGo verifies that SearchImages respects the
// fault-injection hook when SW_FORCE_PROVIDER_ERROR includes "duckduckgo".
func TestInjection_DuckDuckGo(t *testing.T) {
	provider.SetInjectedProviders([]string{"duckduckgo"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), logger)

	ctx := context.Background()

	if _, err := a.SearchImages(ctx, "test artist", provider.ImageThumb); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("SearchImages: want ErrInjectedFailure, got %v", err)
	}
}
