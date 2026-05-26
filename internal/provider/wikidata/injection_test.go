package wikidata

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestInjection_Wikidata verifies that all outbound methods respect the
// fault-injection hook when SW_FORCE_PROVIDER_ERROR includes "wikidata".
func TestInjection_Wikidata(t *testing.T) {
	provider.SetInjectedProviders([]string{"wikidata"})
	t.Cleanup(func() { provider.SetInjectedProviders(nil) })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(provider.NewRateLimiterMap(), logger)

	ctx := context.Background()

	if _, err := a.SearchArtist(ctx, "test"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("SearchArtist: want ErrInjectedFailure, got %v", err)
	}
	// GetArtist requires a UUID or QID; injection fires before the validation check.
	if _, err := a.GetArtist(ctx, "Q12345"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetArtist: want ErrInjectedFailure, got %v", err)
	}
	// GetImages: injection fires before UUID validation; valid format used only for clarity.
	if _, err := a.GetImages(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, provider.ErrInjectedFailure) {
		t.Errorf("GetImages: want ErrInjectedFailure, got %v", err)
	}
}
