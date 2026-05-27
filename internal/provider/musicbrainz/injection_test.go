package musicbrainz

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestInjection_MusicBrainz verifies that real outbound methods respect the
// fault-injection hook when SW_FORCE_PROVIDER_ERROR includes "musicbrainz".
//
// GetImages is a documented no-op for MusicBrainz (no image hosting), so
// injection is intentionally NOT consulted on it -- exercising injection on
// a stub would test the harness, not the silent-failure surfaces.
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
	if _, err := a.GetReleaseGroups(ctx, "test-mbid"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetReleaseGroups: want ErrInjectedFailure, got %v", err)
	}
}

// TestStubBypassesInjection_MusicBrainz pins that GetImages keeps returning
// (nil, nil) even when injection is active, so a caller that treats a
// known-no-op as "skip" stays on the same code path under the smoke
// harness as it does in production.
func TestStubBypassesInjection_MusicBrainz(t *testing.T) {
	provider.SetInjectedProviders([]string{"musicbrainz"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), logger)

	if got, err := a.GetImages(context.Background(), "test-mbid"); err != nil || got != nil {
		t.Errorf("GetImages stub: want (nil, nil), got (%v, %v)", got, err)
	}
}
