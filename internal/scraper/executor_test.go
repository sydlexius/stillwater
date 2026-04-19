package scraper

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
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
