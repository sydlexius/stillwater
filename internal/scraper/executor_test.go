package scraper

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
	_ "modernc.org/sqlite"
)

// mockProvider implements provider.Provider for executor tests.
type mockProvider struct {
	name     provider.ProviderName
	authReq  bool
	getArtFn func(ctx context.Context, id string) (*provider.ArtistMetadata, error)
	getImgFn func(ctx context.Context, id string) ([]provider.ImageResult, error)
}

func (m *mockProvider) Name() provider.ProviderName { return m.name }
func (m *mockProvider) RequiresAuth() bool          { return m.authReq }
func (m *mockProvider) SearchArtist(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}
func (m *mockProvider) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if m.getArtFn != nil {
		return m.getArtFn(ctx, id)
	}
	return nil, nil
}
func (m *mockProvider) GetImages(ctx context.Context, id string) ([]provider.ImageResult, error) {
	if m.getImgFn != nil {
		return m.getImgFn(ctx, id)
	}
	return nil, nil
}

func setupExecutorTest(t *testing.T) (*provider.Registry, *provider.SettingsService, *Service, *slog.Logger) {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	// Create both tables needed by the executor: scraper_config and settings.
	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE scraper_config (
			id TEXT PRIMARY KEY,
			scope TEXT NOT NULL UNIQUE,
			config_json TEXT NOT NULL DEFAULT '{}',
			overrides_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	svc := NewService(db, logger)
	if err := svc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	settings := provider.NewSettingsService(db, enc)
	registry := provider.NewRegistry()

	return registry, settings, svc, logger
}

// TestExecutorErrNotFoundMarksFieldAttempted verifies that when all providers
// return ErrNotFound for a field, the field still appears in AttemptedFields.
// This is critical for refresh-overwrite semantics: "provider said not found"
// means stale data should be cleared, unlike "provider unreachable" which
// preserves existing data.
func TestExecutorErrNotFoundMarksFieldAttempted(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	// Register AudioDB which returns ErrNotFound (no data for this artist).
	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, id string) (*provider.ArtistMetadata, error) {
			return nil, &provider.ErrNotFound{Provider: provider.NameAudioDB, ID: id}
		},
	})

	// Use a minimal config: only one field (styles), primary = AudioDB,
	// fallback chain has only AudioDB.
	ctx := context.Background()
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldStyles, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryMetadata},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryMetadata, Providers: []provider.ProviderName{provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)

	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	// "styles" should be in AttemptedFields because AudioDB was reached.
	found := false
	for _, f := range result.AttemptedFields {
		if f == string(FieldStyles) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'styles' in AttemptedFields after ErrNotFound, got %v", result.AttemptedFields)
	}

	// AudioDB should be in AttemptedProviders.
	provFound := false
	for _, p := range result.AttemptedProviders {
		if p == provider.NameAudioDB {
			provFound = true
			break
		}
	}
	if !provFound {
		t.Errorf("expected AudioDB in AttemptedProviders after ErrNotFound, got %v", result.AttemptedProviders)
	}
}

// TestExecutorPopulatedFields_DistinguishesAttemptedFromPopulated verifies the
// scraper-side half of the #952 graceful-fallback contract: when a provider
// is queried for a field but returns no data (ErrNotFound), the field is
// recorded in AttemptedFields but NOT in PopulatedFields, so the merge layer
// preserves any pre-existing user-curated value rather than wiping it.
//
// This is the scraper-path counterpart to TestOrchestratorPopulatedFieldsTracking.
// Without coverage here, a future regression that wires the wrong condition
// (e.g. tracking on fr.Queried instead of fr.Provider != "") would silently
// re-introduce the bio/tag-wipe bug for scraper-driven refreshes.
func TestExecutorPopulatedFields_DistinguishesAttemptedFromPopulated(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	// AudioDB returns ErrNotFound -- queried, no data.
	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, id string) (*provider.ArtistMetadata, error) {
			return nil, &provider.ErrNotFound{Provider: provider.NameAudioDB, ID: id}
		},
	})

	ctx := context.Background()
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldStyles, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryMetadata},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryMetadata, Providers: []provider.ProviderName{provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)
	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	contains := func(haystack []string, needle string) bool {
		for _, s := range haystack {
			if s == needle {
				return true
			}
		}
		return false
	}

	if !contains(result.AttemptedFields, string(FieldStyles)) {
		t.Errorf("expected %q in AttemptedFields (provider was queried), got %v", FieldStyles, result.AttemptedFields)
	}
	if contains(result.PopulatedFields, string(FieldStyles)) {
		t.Errorf("expected %q NOT in PopulatedFields (no data returned), got %v", FieldStyles, result.PopulatedFields)
	}
}

// TestExecutorPopulatedFields_RecordsFieldWhenProviderReturnsData verifies the
// positive case: when a provider returns data, the field is recorded in
// PopulatedFields. Authorizes the merge layer to overwrite.
func TestExecutorPopulatedFields_RecordsFieldWhenProviderReturnsData(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			return &provider.ArtistMetadata{Name: "Test Artist", Styles: []string{"shoegaze"}}, nil
		},
	})

	ctx := context.Background()
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldStyles, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryMetadata},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryMetadata, Providers: []provider.ProviderName{provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)
	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	contains := func(haystack []string, needle string) bool {
		for _, s := range haystack {
			if s == needle {
				return true
			}
		}
		return false
	}

	if !contains(result.PopulatedFields, string(FieldStyles)) {
		t.Errorf("expected %q in PopulatedFields (data was returned), got %v", FieldStyles, result.PopulatedFields)
	}
}

// TestExecutorGetImagesTimeoutDoesNotMarkImageFieldAttempted verifies that when
// GetArtist succeeds but GetImages returns a transient error (e.g. timeout),
// the image field is NOT added to AttemptedFields. This prevents callers from
// clearing existing image data due to a transient provider outage.
func TestExecutorGetImagesTimeoutDoesNotMarkImageFieldAttempted(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	// Register AudioDB: GetArtist succeeds, GetImages times out.
	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			return &provider.ArtistMetadata{Name: "Test Artist"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]provider.ImageResult, error) {
			return nil, fmt.Errorf("context deadline exceeded")
		},
	})

	ctx := context.Background()
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldThumb, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryImages},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryImages, Providers: []provider.ProviderName{provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)

	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	// "thumb" must NOT be in AttemptedFields when GetImages failed transiently.
	for _, f := range result.AttemptedFields {
		if f == string(FieldThumb) {
			t.Errorf("expected 'thumb' NOT in AttemptedFields after GetImages timeout, got %v", result.AttemptedFields)
			break
		}
	}
}

// TestExecutorGetImagesErrNotFoundMarksImageFieldAttempted verifies that when
// GetImages returns ErrNotFound, the image field IS added to AttemptedFields.
func TestExecutorGetImagesErrNotFoundMarksImageFieldAttempted(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	// Register AudioDB: GetArtist succeeds, GetImages returns ErrNotFound.
	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			return &provider.ArtistMetadata{Name: "Test Artist"}, nil
		},
		getImgFn: func(_ context.Context, id string) ([]provider.ImageResult, error) {
			return nil, &provider.ErrNotFound{Provider: provider.NameAudioDB, ID: id}
		},
	})

	ctx := context.Background()
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldThumb, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryImages},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryImages, Providers: []provider.ProviderName{provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)

	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	// "thumb" MUST be in AttemptedFields because GetImages returned ErrNotFound.
	found := false
	for _, f := range result.AttemptedFields {
		if f == string(FieldThumb) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'thumb' in AttemptedFields after ErrNotFound from GetImages, got %v", result.AttemptedFields)
	}
}

// TestExecutorGetImagesDataMarksImageFieldAttempted verifies that when GetImages
// returns actual image data, the image field IS added to AttemptedFields.
func TestExecutorGetImagesDataMarksImageFieldAttempted(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	// Register AudioDB: GetArtist succeeds, GetImages returns image data.
	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			return &provider.ArtistMetadata{Name: "Test Artist"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]provider.ImageResult, error) {
			return []provider.ImageResult{
				{Type: provider.ImageThumb, URL: "https://example.com/thumb.jpg"},
			}, nil
		},
	})

	ctx := context.Background()
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldThumb, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryImages},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryImages, Providers: []provider.ProviderName{provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)

	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	// "thumb" MUST be in AttemptedFields because GetImages returned data.
	found := false
	for _, f := range result.AttemptedFields {
		if f == string(FieldThumb) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'thumb' in AttemptedFields after GetImages returned data, got %v", result.AttemptedFields)
	}

	// Image data should also be present in the result.
	if len(result.Images) == 0 {
		t.Errorf("expected images in result when GetImages returned data")
	}
}

// TestExecutorFallbackChainImageTimeout verifies that when the primary provider's
// GetImages times out but the fallback provider's GetImages returns image data,
// the image field IS in AttemptedFields and the fallback provider's image data
// is used. This covers the real-world scenario where one image provider is down
// (e.g. Fanart.tv) but a secondary provider (e.g. AudioDB) returns thumbnails.
func TestExecutorFallbackChainImageTimeout(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	// Primary provider: GetArtist succeeds, GetImages times out.
	registry.Register(&mockProvider{
		name:    provider.NameFanartTV,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			return &provider.ArtistMetadata{Name: "Test Artist"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]provider.ImageResult, error) {
			return nil, fmt.Errorf("context deadline exceeded")
		},
	})

	// Fallback provider: GetArtist succeeds, GetImages returns image data.
	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			return &provider.ArtistMetadata{Name: "Test Artist"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]provider.ImageResult, error) {
			return []provider.ImageResult{
				{Type: provider.ImageThumb, URL: "https://example.com/audiodb-thumb.jpg"},
			}, nil
		},
	})

	ctx := context.Background()
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldThumb, Primary: provider.NameFanartTV, Enabled: true, Category: CategoryImages},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryImages, Providers: []provider.ProviderName{provider.NameFanartTV, provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)

	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	// "thumb" must be in AttemptedFields because the fallback succeeded.
	found := false
	for _, f := range result.AttemptedFields {
		if f == string(FieldThumb) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'thumb' in AttemptedFields when fallback GetImages succeeded, got %v", result.AttemptedFields)
	}

	// Image data from the fallback provider must be present.
	if len(result.Images) == 0 {
		t.Errorf("expected images in result when fallback GetImages returned data")
	}
	if len(result.Images) > 0 && result.Images[0].URL != "https://example.com/audiodb-thumb.jpg" {
		t.Errorf("expected fallback provider image URL, got %q", result.Images[0].URL)
	}
}

// TestExecutorNetworkErrorDoesNotMarkFieldAttempted verifies that a real
// network error (not ErrNotFound) does NOT mark the field as attempted.
func TestExecutorNetworkErrorDoesNotMarkFieldAttempted(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	// Register AudioDB which returns a network error.
	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			return nil, fmt.Errorf("connection refused")
		},
	})

	ctx := context.Background()
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldStyles, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryMetadata},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryMetadata, Providers: []provider.ProviderName{provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)

	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	// "styles" should NOT be in AttemptedFields because the provider failed.
	for _, f := range result.AttemptedFields {
		if f == string(FieldStyles) {
			t.Errorf("expected 'styles' NOT in AttemptedFields after network error, got %v", result.AttemptedFields)
			break
		}
	}

	// AudioDB should NOT be in AttemptedProviders after a network error.
	for _, p := range result.AttemptedProviders {
		if p == provider.NameAudioDB {
			t.Errorf("expected AudioDB NOT in AttemptedProviders after network error, got %v", result.AttemptedProviders)
			break
		}
	}
}

func TestApplyMergeableFields_MusicBrainzNameAuthority(t *testing.T) {
	// MusicBrainz Name/SortName should always overwrite, even if the
	// result already has a Name from another provider.
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Name:     "AudioDB Name",
			SortName: "AudioDB Sort",
			URLs:     make(map[string]string),
		},
	}
	mbMeta := &provider.ArtistMetadata{
		Name:     "MusicBrainz Name",
		SortName: "MusicBrainz Sort",
	}
	applyMergeableFields(result, mbMeta, provider.NameMusicBrainz)

	if result.Metadata.Name != "MusicBrainz Name" {
		t.Errorf("MusicBrainz should overwrite Name; got %s", result.Metadata.Name)
	}
	if result.Metadata.SortName != "MusicBrainz Sort" {
		t.Errorf("MusicBrainz should overwrite SortName; got %s", result.Metadata.SortName)
	}
}

func TestApplyMergeableFields_NonMBOnlyFillsEmpty(t *testing.T) {
	// Non-MusicBrainz providers should only fill Name/SortName when empty.
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Name:     "Existing Name",
			SortName: "Existing Sort",
			URLs:     make(map[string]string),
		},
	}
	otherMeta := &provider.ArtistMetadata{
		Name:     "AudioDB Name",
		SortName: "AudioDB Sort",
	}
	applyMergeableFields(result, otherMeta, provider.NameAudioDB)

	if result.Metadata.Name != "Existing Name" {
		t.Errorf("non-MB should not overwrite existing Name; got %s", result.Metadata.Name)
	}
	if result.Metadata.SortName != "Existing Sort" {
		t.Errorf("non-MB should not overwrite existing SortName; got %s", result.Metadata.SortName)
	}
}

func TestApplyMergeableFields_NonMBFillsEmpty(t *testing.T) {
	// Non-MusicBrainz providers should fill empty Name/SortName.
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			URLs: make(map[string]string),
		},
	}
	otherMeta := &provider.ArtistMetadata{
		Name:     "AudioDB Name",
		SortName: "AudioDB Sort",
	}
	applyMergeableFields(result, otherMeta, provider.NameAudioDB)

	if result.Metadata.Name != "AudioDB Name" {
		t.Errorf("non-MB should fill empty Name; got %s", result.Metadata.Name)
	}
	if result.Metadata.SortName != "AudioDB Sort" {
		t.Errorf("non-MB should fill empty SortName; got %s", result.Metadata.SortName)
	}
}

// TestApplyFieldValueDetailFields covers the detail-field cases added to
// applyFieldValue (years_active, type, gender). Each case must return true
// and write the value when populated, and return false when empty.
func TestApplyFieldValueDetailFields(t *testing.T) {
	cases := []struct {
		name     string
		field    FieldName
		meta     provider.ArtistMetadata
		want     string
		readBack func(*provider.ArtistMetadata) string
	}{
		{
			name:     "years_active populated",
			field:    FieldYearsActive,
			meta:     provider.ArtistMetadata{YearsActive: "1980-1990"},
			want:     "1980-1990",
			readBack: func(m *provider.ArtistMetadata) string { return m.YearsActive },
		},
		{
			name:     "type populated",
			field:    FieldType,
			meta:     provider.ArtistMetadata{Type: "Group"},
			want:     "Group",
			readBack: func(m *provider.ArtistMetadata) string { return m.Type },
		},
		{
			name:     "gender populated",
			field:    FieldGender,
			meta:     provider.ArtistMetadata{Gender: "Female"},
			want:     "Female",
			readBack: func(m *provider.ArtistMetadata) string { return m.Gender },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := &provider.FetchResult{Metadata: &provider.ArtistMetadata{}}
			pr := &providerResult{meta: &tc.meta}
			if !applyFieldValue(tc.field, pr, result) {
				t.Fatalf("applyFieldValue(%s) populated = false, want true", tc.field)
			}
			if got := tc.readBack(result.Metadata); got != tc.want {
				t.Errorf("applyFieldValue(%s) wrote %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

// TestApplyFieldValueDetailFieldsEmpty verifies that the new detail-field
// branches return false without mutating the result when source metadata is
// empty, so an empty scrape never clobbers an existing value.
func TestApplyFieldValueDetailFieldsEmpty(t *testing.T) {
	fields := []FieldName{FieldYearsActive, FieldType, FieldGender}
	for _, f := range fields {
		t.Run(string(f), func(t *testing.T) {
			result := &provider.FetchResult{Metadata: &provider.ArtistMetadata{
				YearsActive: "keep", Type: "keep", Gender: "keep",
			}}
			pr := &providerResult{meta: &provider.ArtistMetadata{}}
			if applyFieldValue(f, pr, result) {
				t.Errorf("applyFieldValue(%s) with empty meta = true, want false", f)
			}
			if result.Metadata.YearsActive != "keep" ||
				result.Metadata.Type != "keep" ||
				result.Metadata.Gender != "keep" {
				t.Errorf("applyFieldValue(%s) mutated result on empty source: %+v", f, result.Metadata)
			}
		})
	}
}

// TestApplyProviderIDsAndURLs_MergesIDsURLsAliases verifies the post-#1158
// split: applyProviderIDsAndURLs always merges provider IDs, URL relations,
// and aliases into the aggregated result, with first-write-wins semantics
// on IDs so a later provider never clobbers a higher-priority provider's ID.
func TestApplyProviderIDsAndURLs_MergesIDsURLsAliases(t *testing.T) {
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			URLs: make(map[string]string),
		},
	}
	meta := &provider.ArtistMetadata{
		MusicBrainzID: "mbid-123",
		AudioDBID:     "adb-123",
		DiscogsID:     "disc-123",
		WikidataID:    "Q175044",
		DeezerID:      "5269",
		SpotifyID:     "spotify-id",
		URLs: map[string]string{
			"wikidata": "https://www.wikidata.org/wiki/Q175044",
			"deezer":   "https://www.deezer.com/artist/5269",
		},
		Aliases: []string{"12 Stones", "Twelve Stones"},
	}

	applyProviderIDsAndURLs(result, meta)

	if result.Metadata.WikidataID != "Q175044" {
		t.Errorf("WikidataID = %q, want Q175044", result.Metadata.WikidataID)
	}
	if result.Metadata.DeezerID != "5269" {
		t.Errorf("DeezerID = %q, want 5269", result.Metadata.DeezerID)
	}
	if result.Metadata.SpotifyID != "spotify-id" {
		t.Errorf("SpotifyID = %q, want spotify-id", result.Metadata.SpotifyID)
	}
	if len(result.Metadata.URLs) != 2 {
		t.Errorf("URLs len = %d, want 2", len(result.Metadata.URLs))
	}
	if len(result.Metadata.Aliases) != 2 {
		t.Errorf("Aliases len = %d, want 2", len(result.Metadata.Aliases))
	}
}

// TestApplyProviderIDsAndURLs_FirstWriteWins verifies first-write-wins so a
// lower-priority provider cannot clobber a higher-priority provider's IDs.
func TestApplyProviderIDsAndURLs_FirstWriteWins(t *testing.T) {
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			WikidataID: "Q-existing",
			URLs:       make(map[string]string),
		},
	}
	loserMeta := &provider.ArtistMetadata{
		WikidataID: "Q-new",
	}

	applyProviderIDsAndURLs(result, loserMeta)

	if result.Metadata.WikidataID != "Q-existing" {
		t.Errorf("WikidataID = %q, want Q-existing (first write wins)", result.Metadata.WikidataID)
	}
}

// TestApplyProviderIDsAndURLs_DoesNotMergeClassification verifies the split
// keeps classification fields (Name, Type, Gender, etc.) out of the always-
// merged bucket; those are applyMergeableFields's responsibility and must
// be gated on provider selection.
func TestApplyProviderIDsAndURLs_DoesNotMergeClassification(t *testing.T) {
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			URLs: make(map[string]string),
		},
	}
	meta := &provider.ArtistMetadata{
		Name:           "Should Not Merge",
		SortName:       "Should Not Merge",
		Type:           "Group",
		Gender:         "Female",
		Disambiguation: "Should Not Merge",
		YearsActive:    "1999-present",
	}

	applyProviderIDsAndURLs(result, meta)

	if result.Metadata.Name != "" || result.Metadata.Type != "" ||
		result.Metadata.Gender != "" || result.Metadata.Disambiguation != "" ||
		result.Metadata.YearsActive != "" || result.Metadata.SortName != "" {
		t.Errorf("applyProviderIDsAndURLs leaked classification fields: %+v", result.Metadata)
	}
}

// TestApplyMergeableFields_DoesNotTouchIDs verifies that after the #1158
// split, the selection-gated applyMergeableFields no longer handles provider
// IDs. Those are now exclusively applyProviderIDsAndURLs's job, which runs
// unconditionally.
// TestExecutorHonorsConfiguredPriority verifies the #1030 fix: the scraper
// executor must consult the UI-configured provider.priority.<field> settings
// (exposed by SettingsService.GetPriorities) when deciding which provider to
// query first for a field, instead of using the hardcoded ScraperConfig.Primary
// value. The setup configures Last.fm as the scraper-config primary for
// biography but overrides priority to put AudioDB first; the test asserts
// AudioDB was called first AND its biography won.
func TestExecutorHonorsConfiguredPriority(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	// Track call order so we can prove AudioDB was queried first.
	var callOrder []provider.ProviderName
	var callMu sync.Mutex
	recordCall := func(name provider.ProviderName) {
		callMu.Lock()
		defer callMu.Unlock()
		callOrder = append(callOrder, name)
	}

	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			recordCall(provider.NameAudioDB)
			return &provider.ArtistMetadata{Biography: "AudioDB-sourced biography text long enough to clear the IsJunkBiography minimum length filter."}, nil
		},
	})
	registry.Register(&mockProvider{
		name:    provider.NameLastFM,
		authReq: true,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			recordCall(provider.NameLastFM)
			return &provider.ArtistMetadata{Biography: "Last.fm-sourced biography text long enough to clear the IsJunkBiography minimum length filter."}, nil
		},
	})

	ctx := context.Background()
	// Stash a Last.fm API key so AvailableProviderNames includes it.
	if err := settings.SetAPIKey(ctx, provider.NameLastFM, "lfm-key"); err != nil {
		t.Fatalf("SetAPIKey lastfm: %v", err)
	}
	if err := settings.SetAPIKey(ctx, provider.NameAudioDB, "adb-key"); err != nil {
		t.Fatalf("SetAPIKey audiodb: %v", err)
	}

	// Scraper config: Last.fm is the primary for biography (mirrors the
	// hardcoded DefaultConfig() value the bug report calls out).
	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldBiography, Primary: provider.NameLastFM, Enabled: true, Category: CategoryMetadata},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryMetadata, Providers: []provider.ProviderName{provider.NameLastFM, provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// User reorders biography priority: AudioDB first, then Last.fm.
	if err := settings.SetPriority(ctx, "biography", []provider.ProviderName{provider.NameAudioDB, provider.NameLastFM}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)
	result, err := exec.ScrapeAll(ctx, "mbid-1234", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	if result.Metadata.Biography == "" || !strings.HasPrefix(result.Metadata.Biography, "AudioDB-sourced") {
		t.Errorf("expected biography from AudioDB (configured first in priority), got %q",
			result.Metadata.Biography)
	}

	callMu.Lock()
	defer callMu.Unlock()
	if len(callOrder) == 0 || callOrder[0] != provider.NameAudioDB {
		t.Errorf("expected AudioDB to be called first, got call order %v", callOrder)
	}
}

// TestExecutorPriorityFallbackPreservesUnlistedProviders verifies that when a
// provider is present in the scraper-config fallback chain but absent from the
// user's priority list, it is still appended to the effective ordering so
// newly registered providers are not silently skipped.
func TestExecutorPriorityFallbackPreservesUnlistedProviders(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	var callOrder []provider.ProviderName
	var callMu sync.Mutex
	recordCall := func(name provider.ProviderName) {
		callMu.Lock()
		defer callMu.Unlock()
		callOrder = append(callOrder, name)
	}

	registry.Register(&mockProvider{
		name:    provider.NameMusicBrainz,
		authReq: false,
		getArtFn: func(_ context.Context, id string) (*provider.ArtistMetadata, error) {
			recordCall(provider.NameMusicBrainz)
			return nil, &provider.ErrNotFound{Provider: provider.NameMusicBrainz, ID: id}
		},
	})
	// Wikidata is intentionally chosen here: the default genres priority
	// chain (DefaultPriorities()) is [MusicBrainz, LastFM, AudioDB,
	// Discogs, Spotify, Wikipedia], so Wikidata is NOT auto-appended by
	// GetPriorities' default-reconciliation. That makes it a true "only
	// in the FallbackChain, never in the priority list" provider, which
	// is the exact scenario this test is meant to exercise.
	registry.Register(&mockProvider{
		name:    provider.NameWikidata,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			recordCall(provider.NameWikidata)
			return &provider.ArtistMetadata{Genres: []string{"jazz"}}, nil
		},
	})

	ctx := context.Background()

	// Priority list contains only MusicBrainz; Wikidata is only in the
	// scraper-config fallback chain.
	if err := settings.SetPriority(ctx, "genres", []provider.ProviderName{provider.NameMusicBrainz}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldGenres, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
		},
		FallbackChains: []FallbackChain{
			{Category: CategoryMetadata, Providers: []provider.ProviderName{provider.NameMusicBrainz, provider.NameWikidata}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)
	result, err := exec.ScrapeAll(ctx, "mbid-x", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	if len(result.Metadata.Genres) == 0 || result.Metadata.Genres[0] != "jazz" {
		t.Errorf("expected genres from Wikidata chain fallback, got %v", result.Metadata.Genres)
	}

	callMu.Lock()
	defer callMu.Unlock()
	if len(callOrder) < 2 || callOrder[0] != provider.NameMusicBrainz || callOrder[1] != provider.NameWikidata {
		t.Errorf("expected call order [MusicBrainz, Wikidata], got %v", callOrder)
	}
}

// TestExecutorPriorityFallback_AllDisabledSkipsField verifies the
// configured-empty branch: when a field has a priority configured but every
// provider is in the Disabled set, the field must be skipped entirely. The
// regression this guards against is the prior code path falling back to the
// scraper-config chain on len(priority)==0, which silently re-enabled the
// providers the user had just disabled.
func TestExecutorPriorityFallback_AllDisabledSkipsField(t *testing.T) {
	registry, settings, svc, logger := setupExecutorTest(t)

	var callOrder []provider.ProviderName
	var callMu sync.Mutex
	recordCall := func(name provider.ProviderName) {
		callMu.Lock()
		defer callMu.Unlock()
		callOrder = append(callOrder, name)
	}

	// Both providers must exist as registered mocks so the executor can
	// reach the chain-walking code path. If either were missing it would
	// short-circuit elsewhere and the test would not actually exercise
	// the configured-empty branch we care about.
	registry.Register(&mockProvider{
		name:    provider.NameMusicBrainz,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			recordCall(provider.NameMusicBrainz)
			return &provider.ArtistMetadata{Genres: []string{"should-not-appear"}}, nil
		},
	})
	registry.Register(&mockProvider{
		name:    provider.NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
			recordCall(provider.NameAudioDB)
			return &provider.ArtistMetadata{Genres: []string{"should-not-appear"}}, nil
		},
	})

	ctx := context.Background()

	// Configure the genres priority and then disable every provider in
	// it. After the default-reconciliation in GetPriorities, the list
	// is [MusicBrainz, LastFM, AudioDB, Discogs, Spotify, Wikipedia];
	// disabling all of those leaves EnabledProviders() empty for
	// "genres". The executor must treat this as "skip the field," NOT
	// "fall back to the scraper-config chain."
	if err := settings.SetPriority(ctx, "genres", []provider.ProviderName{provider.NameMusicBrainz}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	allGenresDefaults := []provider.ProviderName{
		provider.NameMusicBrainz, provider.NameLastFM, provider.NameAudioDB,
		provider.NameDiscogs, provider.NameSpotify, provider.NameWikipedia,
	}
	if err := settings.SetDisabledProviders(ctx, "genres", allGenresDefaults); err != nil {
		t.Fatalf("SetDisabledProviders: %v", err)
	}

	cfg := &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldGenres, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
		},
		FallbackChains: []FallbackChain{
			// Chain still mentions MusicBrainz + AudioDB. The bug being
			// guarded against would route through this chain when
			// EnabledProviders() returned empty, calling both providers
			// and merging their results.
			{Category: CategoryMetadata, Providers: []provider.ProviderName{provider.NameMusicBrainz, provider.NameAudioDB}},
		},
	}
	if err := svc.SaveConfig(ctx, ScopeGlobal, cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	exec := NewExecutor(svc, registry, settings, logger)
	result, err := exec.ScrapeAll(ctx, "mbid-x", "Test Artist", ScopeGlobal, nil)
	if err != nil {
		t.Fatalf("ScrapeAll: %v", err)
	}

	if len(result.Metadata.Genres) != 0 {
		t.Errorf("expected no genres when every provider for the field is disabled, got %v", result.Metadata.Genres)
	}

	callMu.Lock()
	defer callMu.Unlock()
	if len(callOrder) != 0 {
		t.Errorf("expected zero provider calls for fully-disabled field, got %v", callOrder)
	}
}

func TestApplyMergeableFields_DoesNotTouchIDs(t *testing.T) {
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			URLs: make(map[string]string),
		},
	}
	meta := &provider.ArtistMetadata{
		MusicBrainzID: "mbid-should-not-merge",
		WikidataID:    "Q-should-not-merge",
		DeezerID:      "should-not-merge",
	}

	applyMergeableFields(result, meta, provider.NameAudioDB)

	if result.Metadata.MusicBrainzID != "" ||
		result.Metadata.WikidataID != "" ||
		result.Metadata.DeezerID != "" {
		t.Errorf("applyMergeableFields leaked provider IDs: %+v", result.Metadata)
	}
}

// TestApplyFieldValue_PerFieldTable verifies every entry in fieldAppliers:
// each field must apply when populated and skip when empty.
func TestApplyFieldValue_PerFieldTable(t *testing.T) {
	type fieldCase struct {
		field    FieldName
		populate func(*provider.ArtistMetadata)
		readBack func(*provider.ArtistMetadata) bool // returns true when value was written
	}

	cases := []fieldCase{
		{
			field:    FieldGenres,
			populate: func(m *provider.ArtistMetadata) { m.Genres = []string{"rock"} },
			readBack: func(m *provider.ArtistMetadata) bool { return len(m.Genres) == 1 && m.Genres[0] == "rock" },
		},
		{
			field:    FieldStyles,
			populate: func(m *provider.ArtistMetadata) { m.Styles = []string{"shoegaze"} },
			readBack: func(m *provider.ArtistMetadata) bool { return len(m.Styles) == 1 },
		},
		{
			field:    FieldMoods,
			populate: func(m *provider.ArtistMetadata) { m.Moods = []string{"melancholic"} },
			readBack: func(m *provider.ArtistMetadata) bool { return len(m.Moods) == 1 },
		},
		{
			field:    FieldMembers,
			populate: func(m *provider.ArtistMetadata) { m.Members = []provider.MemberInfo{{Name: "Alice"}, {Name: "Bob"}} },
			readBack: func(m *provider.ArtistMetadata) bool { return len(m.Members) == 2 },
		},
		{
			field:    FieldFormed,
			populate: func(m *provider.ArtistMetadata) { m.Formed = "1995" },
			readBack: func(m *provider.ArtistMetadata) bool { return m.Formed == "1995" },
		},
		{
			field:    FieldBorn,
			populate: func(m *provider.ArtistMetadata) { m.Born = "1970-01-01" },
			readBack: func(m *provider.ArtistMetadata) bool { return m.Born == "1970-01-01" },
		},
		{
			field:    FieldDied,
			populate: func(m *provider.ArtistMetadata) { m.Died = "2020-06-01" },
			readBack: func(m *provider.ArtistMetadata) bool { return m.Died == "2020-06-01" },
		},
		{
			field:    FieldDisbanded,
			populate: func(m *provider.ArtistMetadata) { m.Disbanded = "2005" },
			readBack: func(m *provider.ArtistMetadata) bool { return m.Disbanded == "2005" },
		},
		{
			field:    FieldYearsActive,
			populate: func(m *provider.ArtistMetadata) { m.YearsActive = "1995-2005" },
			readBack: func(m *provider.ArtistMetadata) bool { return m.YearsActive == "1995-2005" },
		},
		{
			field:    FieldType,
			populate: func(m *provider.ArtistMetadata) { m.Type = "Group" },
			readBack: func(m *provider.ArtistMetadata) bool { return m.Type == "Group" },
		},
		{
			field:    FieldGender,
			populate: func(m *provider.ArtistMetadata) { m.Gender = "Female" },
			readBack: func(m *provider.ArtistMetadata) bool { return m.Gender == "Female" },
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.field)+"/populated", func(t *testing.T) {
			meta := &provider.ArtistMetadata{}
			tc.populate(meta)
			pr := &providerResult{meta: meta}
			result := &provider.FetchResult{Metadata: &provider.ArtistMetadata{}}
			if !applyFieldValue(tc.field, pr, result) {
				t.Fatalf("applyFieldValue(%s) = false, want true", tc.field)
			}
			if !tc.readBack(result.Metadata) {
				t.Errorf("applyFieldValue(%s): value not written to result", tc.field)
			}
		})

		t.Run(string(tc.field)+"/empty", func(t *testing.T) {
			pr := &providerResult{meta: &provider.ArtistMetadata{}}
			result := &provider.FetchResult{Metadata: &provider.ArtistMetadata{}}
			if applyFieldValue(tc.field, pr, result) {
				t.Fatalf("applyFieldValue(%s) with empty meta = true, want false", tc.field)
			}
		})
	}
}

// TestApplyFieldValue_BiographyJunkFilter verifies that the junk-biography
// predicate prevents short or template-like bios from being applied.
func TestApplyFieldValue_BiographyJunkFilter(t *testing.T) {
	t.Run("valid biography applied", func(t *testing.T) {
		bio := "A long and meaningful biography that clearly passes the minimum length threshold for the junk filter."
		pr := &providerResult{meta: &provider.ArtistMetadata{Biography: bio}}
		result := &provider.FetchResult{Metadata: &provider.ArtistMetadata{}}
		if !applyFieldValue(FieldBiography, pr, result) {
			t.Fatal("applyFieldValue(biography) with valid bio = false, want true")
		}
		if result.Metadata.Biography != bio {
			t.Errorf("biography not written; got %q", result.Metadata.Biography)
		}
	})

	t.Run("empty biography skipped", func(t *testing.T) {
		pr := &providerResult{meta: &provider.ArtistMetadata{Biography: ""}}
		result := &provider.FetchResult{Metadata: &provider.ArtistMetadata{}}
		if applyFieldValue(FieldBiography, pr, result) {
			t.Fatal("applyFieldValue(biography) with empty bio = true, want false")
		}
	})

	t.Run("junk biography skipped", func(t *testing.T) {
		// provider.IsJunkBiography returns true for very short strings.
		pr := &providerResult{meta: &provider.ArtistMetadata{Biography: "short"}}
		result := &provider.FetchResult{Metadata: &provider.ArtistMetadata{}}
		// Regardless of whether "short" triggers the junk filter, an empty bio
		// must also be skipped; this exercises the filter path specifically.
		_ = applyFieldValue(FieldBiography, pr, result)
		// The result biography must not have been set to a junk value.
		if result.Metadata.Biography == "short" && provider.IsJunkBiography("short") {
			t.Error("junk biography was applied to result")
		}
	})
}

// TestApplyFieldValue_ImageFields verifies that image fields are routed
// through pr.images (not pr.meta) and only matching image types are appended.
func TestApplyFieldValue_ImageFields(t *testing.T) {
	images := []provider.ImageResult{
		{Type: provider.ImageThumb, URL: "https://example.com/thumb.jpg"},
		{Type: provider.ImageFanart, URL: "https://example.com/fanart.jpg"},
		{Type: provider.ImageLogo, URL: "https://example.com/logo.png"},
		{Type: provider.ImageBanner, URL: "https://example.com/banner.jpg"},
	}

	imageCases := []struct {
		field   FieldName
		wantURL string
	}{
		{FieldThumb, "https://example.com/thumb.jpg"},
		{FieldFanart, "https://example.com/fanart.jpg"},
		{FieldLogo, "https://example.com/logo.png"},
		{FieldBanner, "https://example.com/banner.jpg"},
	}

	for _, tc := range imageCases {
		t.Run(string(tc.field)+"/populated", func(t *testing.T) {
			pr := &providerResult{images: images}
			result := &provider.FetchResult{Metadata: &provider.ArtistMetadata{}}
			if !applyFieldValue(tc.field, pr, result) {
				t.Fatalf("applyFieldValue(%s) = false, want true", tc.field)
			}
			if len(result.Images) == 0 || result.Images[0].URL != tc.wantURL {
				t.Errorf("applyFieldValue(%s): want URL %q, got images %v", tc.field, tc.wantURL, result.Images)
			}
		})

		t.Run(string(tc.field)+"/empty", func(t *testing.T) {
			pr := &providerResult{images: nil}
			result := &provider.FetchResult{Metadata: &provider.ArtistMetadata{}}
			if applyFieldValue(tc.field, pr, result) {
				t.Fatalf("applyFieldValue(%s) with no images = true, want false", tc.field)
			}
		})
	}
}
