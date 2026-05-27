package wikipedia

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestInjection_Wikipedia verifies that real outbound methods respect the
// fault-injection hook when SW_FORCE_PROVIDER_ERROR includes "wikipedia".
//
// SearchArtist and GetImages are documented no-ops here (lookup requires
// an MBID resolved through Wikidata; Wikipedia is not used for artist
// images), so injection is intentionally NOT consulted on them --
// exercising injection on a stub would test the harness, not the
// silent-failure surfaces.
func TestInjection_Wikipedia(t *testing.T) {
	provider.SetInjectedProviders([]string{"wikipedia"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), nil, logger)

	ctx := context.Background()

	if _, err := a.GetArtist(ctx, "test-id"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetArtist: want ErrInjectedFailure, got %v", err)
	}
}

// TestStubsBypassInjection_Wikipedia pins that the documented no-op stubs
// keep returning (nil, nil) even when injection is active, so a caller
// that treats a known-no-op as "skip" stays on the same code path under
// the smoke harness as it does in production.
func TestStubsBypassInjection_Wikipedia(t *testing.T) {
	provider.SetInjectedProviders([]string{"wikipedia"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), nil, logger)

	ctx := context.Background()

	if got, err := a.SearchArtist(ctx, "test"); err != nil || got != nil {
		t.Errorf("SearchArtist stub: want (nil, nil), got (%v, %v)", got, err)
	}
	if got, err := a.GetImages(ctx, "test-id"); err != nil || got != nil {
		t.Errorf("GetImages stub: want (nil, nil), got (%v, %v)", got, err)
	}
}
