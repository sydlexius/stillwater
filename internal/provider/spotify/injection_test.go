package spotify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestInjection_Spotify verifies that all outbound methods respect the
// fault-injection hook when SW_FORCE_PROVIDER_ERROR includes "spotify".
func TestInjection_Spotify(t *testing.T) {
	provider.SetInjectedProviders([]string{"spotify"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), nil, logger)

	ctx := context.Background()

	if _, err := a.SearchArtist(ctx, "test"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("SearchArtist: want ErrInjectedFailure, got %v", err)
	}
	// GetArtist with a valid Spotify ID format (22 char base-62) to pass the
	// IsSpotifyID guard before reaching injection. Injection fires first.
	if _, err := a.GetArtist(ctx, "4Z8W4fkeB5YxbusRsdQVPb"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetArtist: want ErrInjectedFailure, got %v", err)
	}
	if _, err := a.GetImages(ctx, "4Z8W4fkeB5YxbusRsdQVPb"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetImages: want ErrInjectedFailure, got %v", err)
	}
}
