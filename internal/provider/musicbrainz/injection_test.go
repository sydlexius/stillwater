package musicbrainz

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestInjection_MusicBrainz verifies that all outbound methods respect the
// fault-injection hook when SW_FORCE_PROVIDER_ERROR includes "musicbrainz".
func TestInjection_MusicBrainz(t *testing.T) {
	provider.SetInjectedProviders([]string{"musicbrainz"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), logger)

	ctx := context.Background()

	if _, err := a.SearchArtist(ctx, "test"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("SearchArtist: want ErrInjectedFailure, got %v", err)
	}
	if _, err := a.GetArtist(ctx, "test-mbid"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetArtist: want ErrInjectedFailure, got %v", err)
	}
	if _, err := a.GetImages(ctx, "test-mbid"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetImages: want ErrInjectedFailure, got %v", err)
	}
	if _, err := a.GetReleaseGroups(ctx, "test-mbid"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetReleaseGroups: want ErrInjectedFailure, got %v", err)
	}
}
