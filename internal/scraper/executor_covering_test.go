package scraper

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// This file holds the real-executor tests that cover behaviors which, before
// #3292, were asserted only by tests driving the legacy Orchestrator.FetchMetadata
// loop. Each test drives the production Executor.ScrapeAll through the shared
// mockProvider and asserts its fixture precondition so it cannot pass vacuously.

// saveFieldsConfig saves a global config of the given fields sharing one
// metadata/image chain per category, and sets the matching provider priorities
// so the effective order is exactly the order given.
func saveFieldsConfig(t *testing.T, svc *Service, fields []FieldConfig, chains []FallbackChain) {
	t.Helper()
	cfg := &ScraperConfig{Scope: ScopeGlobal, Fields: fields, FallbackChains: chains}
	if err := svc.SaveConfig(context.Background(), ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}

// TestScrapeAll_AggregatesFanartFromMultipleProviders verifies that an image
// field collects candidates from every provider in the chain, not only the
// first, and that the recorded source is the first contributor. Covers the
// behavior of the legacy TestFetchMetadataAggregatesImagesFromMultipleProviders (#3292).
func TestScrapeAll_AggregatesFanartFromMultipleProviders(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)
	ctx := context.Background()

	var mbImagesCalled, audioImagesCalled bool
	registry.Register(&mockProvider{
		name: provider.NameMusicBrainz,
		getArtFn: func(context.Context, string) (*provider.ArtistMetadata, error) {
			return &provider.ArtistMetadata{Name: "a-ha"}, nil
		},
		getImgFn: func(context.Context, string) ([]provider.ImageResult, error) {
			mbImagesCalled = true
			return []provider.ImageResult{{URL: "http://example.test/mb-fanart1.jpg", Type: provider.ImageFanart}}, nil
		},
	})
	registry.Register(&mockProvider{
		name: provider.NameAudioDB,
		getArtFn: func(context.Context, string) (*provider.ArtistMetadata, error) {
			return &provider.ArtistMetadata{Name: "a-ha"}, nil
		},
		getImgFn: func(context.Context, string) ([]provider.ImageResult, error) {
			audioImagesCalled = true
			return []provider.ImageResult{
				{URL: "http://example.test/adb-fanart2.jpg", Type: provider.ImageFanart},
				{URL: "http://example.test/adb-fanart3.jpg", Type: provider.ImageFanart},
			}, nil
		},
	})
	chain := []provider.ProviderName{provider.NameMusicBrainz, provider.NameAudioDB}
	if err := settings.SetPriority(ctx, "fanart", chain); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	saveFieldsConfig(t, svc,
		[]FieldConfig{{Field: FieldFanart, Primary: chain[0], Enabled: true, Category: CategoryImages}},
		[]FallbackChain{{Category: CategoryImages, Providers: chain}})

	exec := NewExecutor(svc, registry, settings, logger, nil)
	result, err := exec.ScrapeAll(ctx, "mbid-aha", "a-ha", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	// Preconditions: both providers were actually asked for images.
	if !mbImagesCalled || !audioImagesCalled {
		t.Fatalf("precondition: both providers must be queried for images (mb=%v audiodb=%v)", mbImagesCalled, audioImagesCalled)
	}
	got := map[string]bool{}
	for _, img := range result.Images {
		if img.Type == provider.ImageFanart {
			got[img.URL] = true
		}
	}
	for _, want := range []string{
		"http://example.test/mb-fanart1.jpg",
		"http://example.test/adb-fanart2.jpg",
		"http://example.test/adb-fanart3.jpg",
	} {
		if !got[want] {
			t.Errorf("fanart %q missing from aggregated images: %v", want, result.Images)
		}
	}
	if len(result.Images) != 3 {
		t.Errorf("got %d images, want 3 (one provider's images must not displace the others)", len(result.Images))
	}
	if src := sourceFor(result, "fanart"); src != provider.NameMusicBrainz {
		t.Errorf("fanart source = %q, want the first contributor %q", src, provider.NameMusicBrainz)
	}
}

// sourceFor returns the provider recorded for field in result.Sources, or "".
func sourceFor(result *provider.FetchResult, field string) provider.ProviderName {
	for _, s := range result.Sources {
		if s.Field == field {
			return s.Provider
		}
	}
	return ""
}

// TestScrapeAll_RecordsFallbackProviderAsFieldSource verifies that when the
// primary provider has nothing for a field, the field source names the fallback
// provider that actually supplied it. Covers the behavior of the legacy
// TestOrchestratorFallback source assertion (#3292).
func TestScrapeAll_RecordsFallbackProviderAsFieldSource(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)
	ctx := context.Background()

	var primaryQueried bool
	registry.Register(&mockProvider{
		name: provider.NameWikipedia,
		getArtFn: func(context.Context, string) (*provider.ArtistMetadata, error) {
			primaryQueried = true
			return &provider.ArtistMetadata{Name: "Radiohead"}, nil // no biography
		},
	})
	registry.Register(&mockProvider{
		name: provider.NameAudioDB,
		getArtFn: func(context.Context, string) (*provider.ArtistMetadata, error) {
			return &provider.ArtistMetadata{Name: "Radiohead", Biography: longBio}, nil
		},
	})
	chain := []provider.ProviderName{provider.NameWikipedia, provider.NameAudioDB}
	if err := settings.SetPriority(ctx, "biography", chain); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	saveBiographyConfig(t, svc, chain...)

	exec := NewExecutor(svc, registry, settings, logger, nil)
	result, err := exec.ScrapeAll(ctx, "mbid-123", "Radiohead", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	if !primaryQueried {
		t.Fatal("precondition: the primary provider must have been queried and come back empty")
	}
	if result.Metadata.Biography != longBio {
		t.Fatalf("precondition: biography = %q, want the fallback's text", result.Metadata.Biography)
	}
	if src := sourceFor(result, "biography"); src != provider.NameAudioDB {
		t.Errorf("biography source = %q, want the fallback provider %q", src, provider.NameAudioDB)
	}
}

// TestScrapeAll_ClearsGenderForNonIndividualType verifies the post-merge gender
// normalization. One provider contributes gender and another contributes a
// group type; the per-provider merge guard only blocks gender when the type is
// already known, so which provider merges first (Go map iteration, random)
// decides whether the guard or the final clear removes it. The run is repeated
// so both orders occur; every order must end with no gender. A person type is
// the control: gender must survive, proving it really is merged. Covers the
// behavior of the legacy TestApplyFieldGenderClearedOnNonIndividualType (#3292).
func TestScrapeAll_ClearsGenderForNonIndividualType(t *testing.T) {
	cases := []struct {
		typ        string
		wantGender string
	}{
		{"group", ""},
		{"person", "Female"},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			registry, settings, svc, logger := setupExecutorTest(t)
			ctx := context.Background()

			registry.Register(&mockProvider{
				name: provider.NameWikipedia,
				getArtFn: func(context.Context, string) (*provider.ArtistMetadata, error) {
					return &provider.ArtistMetadata{Biography: longBio, Gender: "Female"}, nil
				},
			})
			registry.Register(&mockProvider{
				name: provider.NameAudioDB,
				getArtFn: func(context.Context, string) (*provider.ArtistMetadata, error) {
					return &provider.ArtistMetadata{Genres: []string{"rock"}, Type: tc.typ}, nil
				},
			})
			chain := []provider.ProviderName{provider.NameWikipedia, provider.NameAudioDB}
			for _, f := range []string{"biography", "genres"} {
				if err := settings.SetPriority(ctx, f, chain); err != nil {
					t.Fatalf("SetPriority: %v", err)
				}
			}
			// Each provider wins exactly one field so both are "selected" and
			// both have their classification fields merged.
			saveFieldsConfig(t, svc, []FieldConfig{
				{Field: FieldBiography, Primary: provider.NameWikipedia, Enabled: true, Category: CategoryMetadata},
				{Field: FieldGenres, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryMetadata},
			}, []FallbackChain{{Category: CategoryMetadata, Providers: chain}})

			exec := NewExecutor(svc, registry, settings, logger, nil)
			for i := 0; i < 60; i++ {
				result, err := exec.ScrapeAll(ctx, "mbid-x", "Artist", ScopeGlobal, nil)
				if err != nil {
					t.Fatalf("ScrapeAll: %v", err)
				}
				// Preconditions: both providers won a field and Type was merged.
				if sourceFor(result, "biography") != provider.NameWikipedia || sourceFor(result, "genres") != provider.NameAudioDB {
					t.Fatalf("precondition: both providers must win a field, sources = %v", result.Sources)
				}
				if result.Metadata.Type != tc.typ {
					t.Fatalf("precondition: Type = %q, want %q", result.Metadata.Type, tc.typ)
				}
				if result.Metadata.Gender != tc.wantGender {
					t.Fatalf("run %d: type %q: Gender = %q, want %q", i, tc.typ, result.Metadata.Gender, tc.wantGender)
				}
			}
		})
	}
}

// TestScrapeAll_BackfillsProviderIDsFromURLRelations verifies the final pass of
// ScrapeAll: MusicBrainz publishes URL relations whose last path segment is a
// provider's native ID, and those IDs land on the result even though that
// provider's adapter is never registered or queried. Deleting the
// ExtractProviderIDsFromURLs call must turn this red. Covers the behavior of
// the legacy integration-tagged TestIntegration_Orchestrator_AHa backfill (#3292).
func TestScrapeAll_BackfillsProviderIDsFromURLRelations(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)
	ctx := context.Background()

	registry.Register(&mockProvider{
		name: provider.NameMusicBrainz,
		getArtFn: func(context.Context, string) (*provider.ArtistMetadata, error) {
			return &provider.ArtistMetadata{
				Name:   "A-ha",
				Genres: []string{"synth-pop"},
				URLs: map[string]string{
					"discogs":  "https://www.discogs.com/artist/24941-a-ha",
					"wikidata": "https://www.wikidata.org/wiki/Q175044",
				},
			}, nil
		},
	})
	chain := []provider.ProviderName{provider.NameMusicBrainz}
	if err := settings.SetPriority(ctx, "genres", chain); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	saveFieldsConfig(t, svc,
		[]FieldConfig{{Field: FieldGenres, Primary: chain[0], Enabled: true, Category: CategoryMetadata}},
		[]FallbackChain{{Category: CategoryMetadata, Providers: chain}})

	exec := NewExecutor(svc, registry, settings, logger, nil)
	result, err := exec.ScrapeAll(ctx, "cc2c9c3c-b7bc-4b8b-84d8-4fbd8779e493", "A-ha", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	// Preconditions: the URL relations reached the result, no ID was supplied
	// directly by MusicBrainz, and the providers behind the IDs were never
	// registered (so only the backfill can have produced them).
	if result.Metadata.URLs["discogs"] == "" || result.Metadata.URLs["wikidata"] == "" {
		t.Fatalf("precondition: URL relations missing from result: %v", result.Metadata.URLs)
	}
	if registry.Get(provider.NameDiscogs) != nil || registry.Get(provider.NameWikidata) != nil {
		t.Fatal("precondition: discogs/wikidata adapters must not be registered")
	}
	if result.Metadata.DiscogsID != "24941" {
		t.Errorf("DiscogsID = %q, want 24941 backfilled from the URL relation", result.Metadata.DiscogsID)
	}
	if result.Metadata.WikidataID != "Q175044" {
		t.Errorf("WikidataID = %q, want Q175044 backfilled from the URL relation", result.Metadata.WikidataID)
	}
}
