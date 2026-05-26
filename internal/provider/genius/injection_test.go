package genius

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestInjection_Genius verifies that all outbound methods respect the
// fault-injection hook when SW_FORCE_PROVIDER_ERROR includes "genius".
func TestInjection_Genius(t *testing.T) {
	provider.SetInjectedProviders([]string{"genius"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), nil, logger)

	ctx := context.Background()

	if _, err := a.SearchArtist(ctx, "test"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("SearchArtist: want ErrInjectedFailure, got %v", err)
	}
	// GetArtist with a non-UUID, non-numeric ID to exercise the by-name path,
	// but injection fires before the routing decision.
	if _, err := a.GetArtist(ctx, "test-artist"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetArtist: want ErrInjectedFailure, got %v", err)
	}
	if _, err := a.GetImages(ctx, "test-id"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetImages: want ErrInjectedFailure, got %v", err)
	}
}
