package genius

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestInjection_Genius verifies that real outbound methods respect the
// fault-injection hook when SW_FORCE_PROVIDER_ERROR includes "genius".
//
// GetImages is a documented no-op (Genius hosts no artist images), so
// injection is intentionally NOT consulted on it -- exercising injection
// on a stub would test the harness, not the silent-failure surfaces.
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
}

// TestStubBypassesInjection_Genius pins that GetImages keeps returning
// (nil, nil) even when injection is active, so a caller that treats a
// known-no-op as "skip" stays on the same code path under the smoke
// harness as it does in production.
func TestStubBypassesInjection_Genius(t *testing.T) {
	provider.SetInjectedProviders([]string{"genius"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), nil, logger)

	if got, err := a.GetImages(context.Background(), "test-id"); err != nil || got != nil {
		t.Errorf("GetImages stub: want (nil, nil), got (%v, %v)", got, err)
	}
}
