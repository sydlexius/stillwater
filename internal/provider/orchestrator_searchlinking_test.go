package provider

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
)

// TestSearchForLinking_ReturnsPerProviderStatus covers the new contract added
// for issue #1663: SearchForLinking now reports a ProviderSearchStatus per
// queried provider so callers can distinguish "no matches" from "the provider
// errored and the empty list is incomplete". The handler then surfaces a
// banner above the disambiguation results.
//
// Each subtest registers a fresh set of mockProviders, calls SearchForLinking,
// and asserts both the results slice and the per-provider status shape.
func TestSearchForLinking_ReturnsPerProviderStatus(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Helper that returns a fresh orchestrator with the supplied providers
	// registered. Setup is per-subtest so each scenario gets an isolated
	// registry and is safe to run with t.Parallel().
	newOrch := func(t *testing.T, mocks []*mockProvider) *Orchestrator {
		t.Helper()
		registry, settings := setupOrchestratorTest(t)
		for _, m := range mocks {
			registry.Register(m)
		}
		return NewOrchestrator(registry, settings, logger, nil)
	}

	t.Run("all providers succeed", func(t *testing.T) {
		t.Parallel()
		orch := newOrch(t, []*mockProvider{
			{
				name: NameMusicBrainz,
				searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
					return []ArtistSearchResult{{Name: "Hiromi", Source: "musicbrainz", Score: 100}}, nil
				},
			},
			{
				name: NameDiscogs,
				searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
					return []ArtistSearchResult{{Name: "Hiromi", Source: "discogs", Score: 80}}, nil
				},
			},
		})

		results, statuses, err := orch.SearchForLinking(
			context.Background(),
			"Hiromi",
			[]ProviderName{NameMusicBrainz, NameDiscogs},
		)
		if err != nil {
			t.Fatalf("unexpected function error: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(results))
		}
		if len(statuses) != 2 {
			t.Fatalf("expected 2 statuses, got %d", len(statuses))
		}
		for i, s := range statuses {
			if s.Errored {
				t.Errorf("status[%d] (%s): expected Errored=false, got true (msg=%q)", i, s.Provider, s.ScrubbedMessage)
			}
			if s.ScrubbedMessage != "" {
				t.Errorf("status[%d] (%s): expected empty ScrubbedMessage on success, got %q", i, s.Provider, s.ScrubbedMessage)
			}
		}
		// Status ordering must match the input provider slice so the UI
		// can render a deterministic banner list.
		if statuses[0].Provider != NameMusicBrainz || statuses[1].Provider != NameDiscogs {
			t.Errorf("status ordering does not match input: got %v, %v", statuses[0].Provider, statuses[1].Provider)
		}
	})

	t.Run("one provider errors, others succeed", func(t *testing.T) {
		t.Parallel()
		orch := newOrch(t, []*mockProvider{
			{
				name: NameMusicBrainz,
				searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
					return []ArtistSearchResult{{Name: "Hiromi", Source: "musicbrainz", Score: 100}}, nil
				},
			},
			{
				name: NameDiscogs,
				searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
					return nil, errors.New("api.discogs.com: connection refused")
				},
			},
		})

		results, statuses, err := orch.SearchForLinking(
			context.Background(),
			"Hiromi",
			[]ProviderName{NameMusicBrainz, NameDiscogs},
		)
		if err != nil {
			t.Fatalf("unexpected function-level error (should be nil for per-provider failures): %v", err)
		}
		// The successful MB result must still be returned even though
		// Discogs errored -- partial results are better than an empty
		// list with no banner.
		if len(results) != 1 {
			t.Fatalf("expected 1 result from MB, got %d", len(results))
		}
		if len(statuses) != 2 {
			t.Fatalf("expected 2 statuses, got %d", len(statuses))
		}
		if statuses[0].Errored {
			t.Errorf("MB status should not be Errored, got %+v", statuses[0])
		}
		if !statuses[1].Errored {
			t.Errorf("Discogs status should be Errored, got %+v", statuses[1])
		}
		if statuses[1].ScrubbedMessage == "" {
			t.Errorf("Discogs Errored status should carry a ScrubbedMessage")
		}
	})

	t.Run("all providers error", func(t *testing.T) {
		t.Parallel()
		orch := newOrch(t, []*mockProvider{
			{
				name: NameMusicBrainz,
				searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
					return nil, errors.New("musicbrainz.org: timeout")
				},
			},
			{
				name: NameDiscogs,
				searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
					return nil, errors.New("api.discogs.com: 401 unauthorized")
				},
			},
		})

		results, statuses, err := orch.SearchForLinking(
			context.Background(),
			"Hiromi",
			[]ProviderName{NameMusicBrainz, NameDiscogs},
		)
		if err != nil {
			t.Fatalf("unexpected function-level error: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("expected 0 results when every provider errors, got %d", len(results))
		}
		if len(statuses) != 2 {
			t.Fatalf("expected 2 statuses, got %d", len(statuses))
		}
		for i, s := range statuses {
			if !s.Errored {
				t.Errorf("status[%d] (%s): expected Errored=true, got false", i, s.Provider)
			}
		}
	})

	t.Run("unregistered provider name is skipped without status", func(t *testing.T) {
		t.Parallel()
		// Only MB is registered; the caller also asks for Discogs and a
		// totally-unknown name. Both unregistered names must drop out
		// silently -- emitting a status for them would confuse the
		// banner (the user did not misconfigure those providers; the
		// orchestrator simply does not know them).
		orch := newOrch(t, []*mockProvider{
			{
				name: NameMusicBrainz,
				searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
					return []ArtistSearchResult{{Name: "Hiromi", Source: "musicbrainz"}}, nil
				},
			},
		})

		results, statuses, err := orch.SearchForLinking(
			context.Background(),
			"Hiromi",
			[]ProviderName{NameMusicBrainz, NameDiscogs, ProviderName("does-not-exist")},
		)
		if err != nil {
			t.Fatalf("unexpected function-level error: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected 1 result from MB, got %d", len(results))
		}
		if len(statuses) != 1 {
			t.Fatalf("expected exactly 1 status (only MB was registered), got %d", len(statuses))
		}
		if statuses[0].Provider != NameMusicBrainz {
			t.Errorf("expected the one status to be for MB, got %v", statuses[0].Provider)
		}
	})

	t.Run("scrubbed message redacts api keys", func(t *testing.T) {
		t.Parallel()
		orch := newOrch(t, []*mockProvider{
			{
				name: NameDiscogs,
				searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
					return nil, errors.New("Get \"https://api.discogs.com/database/search?q=hiromi&token=SECRET123\": no such host")
				},
			},
		})

		_, statuses, err := orch.SearchForLinking(
			context.Background(),
			"Hiromi",
			[]ProviderName{NameDiscogs},
		)
		if err != nil {
			t.Fatalf("unexpected function-level error: %v", err)
		}
		if len(statuses) != 1 || !statuses[0].Errored {
			t.Fatalf("expected one errored status, got %+v", statuses)
		}
		// The raw token must not survive ScrubError -- this is the
		// load-bearing contract that lets the template render the
		// message safely.
		if contains := statuses[0].ScrubbedMessage; containsToken(contains) {
			t.Errorf("ScrubbedMessage leaked the token: %q", contains)
		}
	})

	// "Provider succeeds with zero results" is the branch that distinguishes
	// "the lookup ran and found nothing" from "the lookup failed". It is the
	// negative case for the #1663 banner: callers must NOT render the
	// providers-unreachable warning when the empty list is legitimate.
	t.Run("provider succeeds with zero results", func(t *testing.T) {
		t.Parallel()
		orch := newOrch(t, []*mockProvider{
			{
				name: NameMusicBrainz,
				searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
					return nil, nil
				},
			},
		})

		results, statuses, err := orch.SearchForLinking(
			context.Background(),
			"Hiromi",
			[]ProviderName{NameMusicBrainz},
		)
		if err != nil {
			t.Fatalf("unexpected function-level error: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("expected zero results, got %d", len(results))
		}
		if len(statuses) != 1 {
			t.Fatalf("expected one status, got %d", len(statuses))
		}
		if statuses[0].Errored {
			t.Errorf("expected Errored=false on success-with-no-results, got true (msg=%q)", statuses[0].ScrubbedMessage)
		}
		if statuses[0].ScrubbedMessage != "" {
			t.Errorf("expected empty ScrubbedMessage on success, got %q", statuses[0].ScrubbedMessage)
		}
	})
}

// TestSearchForLinking_NilRegistry covers the structural-failure branch where
// the orchestrator was constructed without a provider registry. Callers
// depend on receiving a function-level error (not a silent empty result) so
// they can distinguish "no providers configured" from "every provider
// errored", which routes differently in the bulk-identify pipeline.
func TestSearchForLinking_NilRegistry(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// Construct an Orchestrator with a nil registry. NewOrchestrator does
	// not guard against this in production wiring; the test exercises the
	// SearchForLinking nil-receiver branch directly.
	orch := &Orchestrator{registry: nil, logger: logger}

	results, statuses, err := orch.SearchForLinking(
		context.Background(),
		"Hiromi",
		[]ProviderName{NameMusicBrainz},
	)
	if err == nil {
		t.Fatalf("expected non-nil function-level error from nil registry, got nil")
	}
	if results != nil {
		t.Errorf("expected nil results on nil-registry error, got %v", results)
	}
	if statuses != nil {
		t.Errorf("expected nil statuses on nil-registry error, got %v", statuses)
	}
}

// containsToken is a tiny helper for the scrub-message subtest; lifting it
// keeps the assertion line readable.
func containsToken(s string) bool {
	const token = "SECRET123"
	for i := 0; i+len(token) <= len(s); i++ {
		if s[i:i+len(token)] == token {
			return true
		}
	}
	return false
}
