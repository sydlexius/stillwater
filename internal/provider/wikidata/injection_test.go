package wikidata

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestInjection_Wikidata verifies that real outbound methods respect the
// fault-injection hook when SW_FORCE_PROVIDER_ERROR includes "wikidata".
//
// SearchArtist is a documented no-op (SPARQL lookup requires an MBID --
// use GetArtist instead), so injection is intentionally NOT consulted on
// it -- exercising injection on a stub would test the harness, not the
// silent-failure surfaces.
func TestInjection_Wikidata(t *testing.T) {
	provider.SetInjectedProviders([]string{"wikidata"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), logger)

	ctx := context.Background()

	// GetArtist requires a UUID or QID; injection fires before the validation check.
	if _, err := a.GetArtist(ctx, "Q12345"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetArtist: want ErrInjectedFailure, got %v", err)
	}
	// GetImages: injection fires before UUID validation; valid format used only for clarity.
	if _, err := a.GetImages(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetImages: want ErrInjectedFailure, got %v", err)
	}
}

// TestStubBypassesInjection_Wikidata pins that SearchArtist keeps
// returning (nil, nil) even when injection is active, so a caller that
// treats a known-no-op as "skip" stays on the same code path under the
// smoke harness as it does in production.
func TestStubBypassesInjection_Wikidata(t *testing.T) {
	provider.SetInjectedProviders([]string{"wikidata"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), logger)

	if got, err := a.SearchArtist(context.Background(), "test"); err != nil || got != nil {
		t.Errorf("SearchArtist stub: want (nil, nil), got (%v, %v)", got, err)
	}
}
