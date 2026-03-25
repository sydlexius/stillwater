package provider

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/encryption"
	_ "modernc.org/sqlite"
)

// mockProvider implements the Provider interface for testing.
type mockProvider struct {
	name     ProviderName
	authReq  bool
	searchFn func(ctx context.Context, name string) ([]ArtistSearchResult, error)
	getArtFn func(ctx context.Context, id string) (*ArtistMetadata, error)
	getImgFn func(ctx context.Context, id string) ([]ImageResult, error)
}

func (m *mockProvider) Name() ProviderName { return m.name }
func (m *mockProvider) RequiresAuth() bool { return m.authReq }

func (m *mockProvider) SearchArtist(ctx context.Context, name string) ([]ArtistSearchResult, error) {
	if m.searchFn != nil {
		return m.searchFn(ctx, name)
	}
	return nil, nil
}

func (m *mockProvider) GetArtist(ctx context.Context, id string) (*ArtistMetadata, error) {
	if m.getArtFn != nil {
		return m.getArtFn(ctx, id)
	}
	return nil, nil
}

func (m *mockProvider) GetImages(ctx context.Context, id string) ([]ImageResult, error) {
	if m.getImgFn != nil {
		return m.getImgFn(ctx, id)
	}
	return nil, nil
}

// mockNameLookupProvider wraps mockProvider and implements NameLookupProvider
// so the MBID-to-name retry logic can detect it via type assertion.
type mockNameLookupProvider struct {
	mockProvider
}

func (m *mockNameLookupProvider) SupportsNameLookup() bool { return true }

func setupOrchestratorTest(t *testing.T) (*Registry, *SettingsService) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT (datetime('now')))`)
	if err != nil {
		t.Fatalf("creating settings table: %v", err)
	}
	enc, _, _ := encryption.NewEncryptor("")
	settings := NewSettingsService(db, enc)
	registry := NewRegistry()
	return registry, settings
}

func TestOrchestratorFallback(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// LastFM requires an API key; store a dummy so it passes availability check.
	if err := settings.SetAPIKey(context.Background(), NameLastFM, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// First provider returns empty biography, second has one
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:          "Radiohead",
				MusicBrainzID: "mbid-123",
				Genres:        []string{"rock"},
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameLastFM,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Radiohead",
				Biography: "Radiohead are an English rock band formed in Abingdon, Oxfordshire, in 1985.",
				Genres:    []string{"alternative"},
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-123", "Radiohead", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Biography should come from Last.fm (MusicBrainz returned empty)
	if result.Metadata.Biography != "Radiohead are an English rock band formed in Abingdon, Oxfordshire, in 1985." {
		t.Errorf("expected biography from Last.fm, got: %s", result.Metadata.Biography)
	}

	// Genres should be accumulated from both providers (MusicBrainz first, then Last.fm).
	// With tag aggregation, both "rock" (MusicBrainz) and "alternative" (Last.fm) are present.
	if len(result.Metadata.Genres) != 2 {
		t.Errorf("expected 2 genres (aggregated), got: %v", result.Metadata.Genres)
	}
	if result.Metadata.Genres[0] != "rock" {
		t.Errorf("expected rock first (MusicBrainz priority), got: %s", result.Metadata.Genres[0])
	}
	if result.Metadata.Genres[1] != "alternative" {
		t.Errorf("expected alternative second (Last.fm), got: %s", result.Metadata.Genres[1])
	}

	// Check sources recorded correctly
	bioSource := findSource(result.Sources, "biography")
	if bioSource == nil || bioSource.Provider != NameLastFM {
		t.Errorf("expected biography source from lastfm, got: %v", bioSource)
	}
	// First genres source should be MusicBrainz (highest priority provider)
	genreSource := findSource(result.Sources, "genres")
	if genreSource == nil || genreSource.Provider != NameMusicBrainz {
		t.Errorf("expected genres source from musicbrainz, got: %v", genreSource)
	}
}

// TestOrchestratorTagAggregation verifies that genres and moods are accumulated
// across all providers with canonical spelling normalization and deduplication,
// rather than stopping at the first provider with data.
func TestOrchestratorTagAggregation(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	if err := settings.SetAPIKey(context.Background(), NameLastFM, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// MusicBrainz returns "rock" and "hip hop" (should canonicalize to "Hip-Hop").
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:   "TestArtist",
				Genres: []string{"Rock", "hip hop"},
				Moods:  []string{"Energetic"},
			}, nil
		},
	})
	// Last.fm returns "Hip-Hop" (duplicate after canonicalization) and a new genre "Electronic".
	registry.Register(&mockProvider{
		name: NameLastFM,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:   "TestArtist",
				Genres: []string{"Hip-Hop", "Electronic"},
				Moods:  []string{"energetic", "Chill"},
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-agg", "TestArtist", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// "hip hop" from MusicBrainz canonicalizes to "Hip-Hop".
	// "Hip-Hop" from Last.fm is a duplicate and must be deduplicated.
	// "Electronic" from Last.fm is new and must be appended.
	// Expected: Rock, Hip-Hop, Electronic (3 entries, not 4)
	if len(result.Metadata.Genres) != 3 {
		t.Fatalf("expected 3 genres after dedup, got %d: %v", len(result.Metadata.Genres), result.Metadata.Genres)
	}
	if result.Metadata.Genres[0] != "Rock" {
		t.Errorf("expected Rock first, got %q", result.Metadata.Genres[0])
	}
	if result.Metadata.Genres[1] != "Hip-Hop" {
		t.Errorf("expected Hip-Hop second (canonicalized from hip hop), got %q", result.Metadata.Genres[1])
	}
	if result.Metadata.Genres[2] != "Electronic" {
		t.Errorf("expected Electronic third, got %q", result.Metadata.Genres[2])
	}

	// Moods: "Energetic" from MusicBrainz, "energetic" from Last.fm (dup), "Chill" from Last.fm (new).
	// Expected: Energetic, Chill (2 entries)
	if len(result.Metadata.Moods) != 2 {
		t.Fatalf("expected 2 moods after dedup, got %d: %v", len(result.Metadata.Moods), result.Metadata.Moods)
	}
	if result.Metadata.Moods[0] != "Energetic" {
		t.Errorf("expected Energetic first, got %q", result.Metadata.Moods[0])
	}
	if result.Metadata.Moods[1] != "Chill" {
		t.Errorf("expected Chill second, got %q", result.Metadata.Moods[1])
	}
}

func TestOrchestratorProviderError(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// First provider errors, second succeeds
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return nil, &ErrProviderUnavailable{Provider: NameMusicBrainz, Cause: fmt.Errorf("timeout")}
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Radiohead",
				Biography: "AudioDB biography for this artist with enough content to pass quality checks.",
				Formed:    "1985",
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-123", "Radiohead", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Should get data from AudioDB since MusicBrainz failed
	if result.Metadata.Biography != "AudioDB biography for this artist with enough content to pass quality checks." {
		t.Errorf("expected biography from AudioDB, got: %s", result.Metadata.Biography)
	}
}

func TestOrchestratorSearch(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// LastFM requires an API key; store a dummy so it passes availability check.
	if err := settings.SetAPIKey(context.Background(), NameLastFM, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
			return []ArtistSearchResult{
				{Name: "Radiohead", Source: "musicbrainz", Score: 100},
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameLastFM,
		searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
			return []ArtistSearchResult{
				{Name: "Radiohead", Source: "lastfm", Score: 100},
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	results, err := orch.Search(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestOrchestratorCustomPriority(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Override biography priority to prefer AudioDB
	err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameAudioDB, NameMusicBrainz})
	if err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Radiohead",
				Biography: "MusicBrainz biography for this artist with enough content to pass quality checks.",
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Radiohead",
				Biography: "AudioDB biography for this artist with enough content to pass quality checks.",
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-123", "Radiohead", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// AudioDB should win for biography due to custom priority
	if result.Metadata.Biography != "AudioDB biography for this artist with enough content to pass quality checks." {
		t.Errorf("expected biography from AudioDB (custom priority), got: %s", result.Metadata.Biography)
	}
}

func TestOrchestratorMBIDFallbackToName(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Genius requires an API key; store a dummy so it passes availability check.
	if err := settings.SetAPIKey(context.Background(), NameGenius, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// Override biography priority: Genius first, then MusicBrainz.
	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameGenius, NameMusicBrainz}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	// Genius returns ErrNotFound for MBID, then succeeds with name.
	// Uses mockNameLookupProvider so the NameLookupProvider type assertion succeeds.
	geniusCalls := 0
	registry.Register(&mockNameLookupProvider{
		mockProvider: mockProvider{
			name: NameGenius,
			getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
				geniusCalls++
				if id == "mbid-uuid-1234" {
					return nil, &ErrNotFound{Provider: NameGenius, ID: id}
				}
				// Called with artist name on retry
				return &ArtistMetadata{
					Name:      "Radiohead",
					Biography: "Genius biography for this artist with enough content to pass the quality checks.",
				}, nil
			},
		},
	})
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:          "Radiohead",
				MusicBrainzID: "mbid-uuid-1234",
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-uuid-1234", "Radiohead", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Biography should come from Genius after MBID->name retry.
	if result.Metadata.Biography != "Genius biography for this artist with enough content to pass the quality checks." {
		t.Errorf("expected biography from Genius, got: %s", result.Metadata.Biography)
	}

	// Genius should have been called twice: once with MBID (not-found), once with name.
	if geniusCalls != 2 {
		t.Errorf("expected 2 Genius GetArtist calls (MBID + name retry), got %d", geniusCalls)
	}
}

// TestOrchestratorMBIDNoRetryWithoutNameLookup verifies that the MBID-to-name
// retry does NOT fire for providers that do not implement NameLookupProvider.
// Uses AudioDB as the example since Discogs now implements NameLookupProvider.
func TestOrchestratorMBIDNoRetryWithoutNameLookup(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Use a plain mockProvider (no NameLookupProvider) that returns ErrNotFound.
	audioDBCalls := 0
	registry.Register(&mockProvider{
		name:    NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
			audioDBCalls++
			return nil, &ErrNotFound{Provider: NameAudioDB, ID: id}
		},
	})

	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameAudioDB}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	_, _ = orch.FetchMetadata(context.Background(), "mbid-uuid-1234", "Radiohead", nil)

	// AudioDB should only be called once (MBID attempt). No name retry.
	if audioDBCalls != 1 {
		t.Errorf("expected 1 AudioDB GetArtist call (no name retry), got %d", audioDBCalls)
	}
}

// TestFetchFieldFromProviders_ErrNotFoundSuppressed verifies that a provider
// returning ErrNotFound yields a result with no Error (treated as "no data"),
// while other errors are still surfaced.
func TestFetchFieldFromProviders_ErrNotFoundSuppressed(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// AudioDB returns ErrNotFound (artist not in their database).
	registry.Register(&mockProvider{
		name:    NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
			return nil, &ErrNotFound{Provider: NameAudioDB, ID: id}
		},
	})

	// Last.fm returns a real error (e.g. timeout).
	// Requires an API key to pass availability check.
	if err := settings.SetAPIKey(context.Background(), NameLastFM, "dummy-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}
	registry.Register(&mockProvider{
		name:    NameLastFM,
		authReq: true,
		getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
			return nil, fmt.Errorf("connection timeout")
		},
	})

	// Set priorities to only AudioDB and LastFM. Also disable any providers
	// that may have been appended from defaults (e.g. MusicBrainz, Discogs)
	// to keep this test focused on ErrNotFound suppression.
	if err := settings.SetPriority(context.Background(), "styles", []ProviderName{NameAudioDB, NameLastFM}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	if err := settings.SetDisabledProviders(context.Background(), "styles", []ProviderName{NameDiscogs, NameMusicBrainz}); err != nil {
		t.Fatalf("SetDisabledProviders: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	results, err := orch.FetchFieldFromProviders(context.Background(), "mbid-1234", "Test Artist", "styles", nil)
	if err != nil {
		t.Fatalf("FetchFieldFromProviders: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// AudioDB: ErrNotFound should be suppressed (no error, no data).
	audioDB := results[0]
	if audioDB.Provider != NameAudioDB {
		t.Fatalf("expected first result to be AudioDB, got %s", audioDB.Provider)
	}
	if audioDB.Error != "" {
		t.Errorf("AudioDB ErrNotFound should be suppressed, got Error=%q", audioDB.Error)
	}
	if audioDB.HasData {
		t.Errorf("AudioDB should have HasData=false")
	}

	// Last.fm: real error should be surfaced.
	lastFM := results[1]
	if lastFM.Provider != NameLastFM {
		t.Fatalf("expected second result to be Last.fm, got %s", lastFM.Provider)
	}
	if lastFM.Error == "" {
		t.Errorf("Last.fm real error should be surfaced, got empty Error")
	}
}

func findSource(sources []FieldSource, field string) *FieldSource {
	for _, s := range sources {
		if s.Field == field {
			return &s
		}
	}
	return nil
}

// TestOrchestratorGetImagesTimeoutDoesNotMarkImageFieldAttempted verifies that
// when GetArtist succeeds but GetImages returns a transient error (e.g. timeout),
// the image fields are NOT added to AttemptedFields. This prevents callers from
// clearing existing image data due to a transient provider outage.
func TestOrchestratorGetImagesTimeoutDoesNotMarkImageFieldAttempted(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// MusicBrainz GetArtist succeeds; GetImages times out.
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "Test Artist"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return nil, fmt.Errorf("context deadline exceeded")
		},
	})

	// Set priority so that thumb/fanart only use MusicBrainz.
	if err := settings.SetPriority(context.Background(), "thumb", []ProviderName{NameMusicBrainz}); err != nil {
		t.Fatalf("SetPriority thumb: %v", err)
	}
	if err := settings.SetPriority(context.Background(), "fanart", []ProviderName{NameMusicBrainz}); err != nil {
		t.Fatalf("SetPriority fanart: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-1234", "Test Artist", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Image fields must NOT be in AttemptedFields when GetImages failed transiently.
	for _, f := range result.AttemptedFields {
		if f == "thumb" || f == "fanart" || f == "logo" || f == "banner" {
			t.Errorf("image field %q should NOT be in AttemptedFields after GetImages timeout, got %v", f, result.AttemptedFields)
		}
	}

	// MusicBrainz should be in AttemptedProviders because GetArtist succeeded.
	found := false
	for _, p := range result.AttemptedProviders {
		if p == NameMusicBrainz {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected MusicBrainz in AttemptedProviders (GetArtist succeeded), got %v", result.AttemptedProviders)
	}
}

// TestOrchestratorGetImagesErrNotFoundMarksImageFieldAttempted verifies that
// when GetImages returns ErrNotFound, the image field IS added to AttemptedFields.
// ErrNotFound is a definitive "no images exist" response that should allow
// stale image data to be cleared on refresh.
func TestOrchestratorGetImagesErrNotFoundMarksImageFieldAttempted(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// MusicBrainz GetArtist succeeds; GetImages returns ErrNotFound.
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "Test Artist"}, nil
		},
		getImgFn: func(_ context.Context, id string) ([]ImageResult, error) {
			return nil, &ErrNotFound{Provider: NameMusicBrainz, ID: id}
		},
	})

	if err := settings.SetPriority(context.Background(), "thumb", []ProviderName{NameMusicBrainz}); err != nil {
		t.Fatalf("SetPriority thumb: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-1234", "Test Artist", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// "thumb" MUST be in AttemptedFields because GetImages returned ErrNotFound.
	found := false
	for _, f := range result.AttemptedFields {
		if f == "thumb" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'thumb' in AttemptedFields after ErrNotFound from GetImages, got %v", result.AttemptedFields)
	}
}

// TestOrchestratorGetImagesDataMarksImageFieldAttempted verifies that when
// GetImages returns actual image data, the image field IS added to AttemptedFields.
func TestOrchestratorGetImagesDataMarksImageFieldAttempted(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// MusicBrainz GetArtist succeeds; GetImages returns image data.
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "Test Artist"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return []ImageResult{
				{Type: ImageThumb, URL: "https://example.com/thumb.jpg"},
			}, nil
		},
	})

	if err := settings.SetPriority(context.Background(), "thumb", []ProviderName{NameMusicBrainz}); err != nil {
		t.Fatalf("SetPriority thumb: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-1234", "Test Artist", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// "thumb" MUST be in AttemptedFields because GetImages returned data.
	found := false
	for _, f := range result.AttemptedFields {
		if f == "thumb" {
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

func TestOrchestratorProviderIDPrecedence(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// AudioDB requires no key (free tier). Register it with a mock that records
	// which ID it receives.
	var audioDBReceivedID string
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
			audioDBReceivedID = id
			return &ArtistMetadata{
				Name:      "Adele",
				AudioDBID: "111493",
				Biography: "Correct biography from AudioDB with enough content to pass the quality gate checks.",
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	// Pass a wrong MBID but the correct AudioDB numeric ID in providerIDs.
	// The orchestrator should prefer the provider-specific ID.
	providerIDs := map[ProviderName]string{
		NameAudioDB: "111493",
	}
	result, err := orch.FetchMetadata(context.Background(), "wrong-mbid-123", "Adele", providerIDs)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// AudioDB should have received its own numeric ID, not the wrong MBID
	if audioDBReceivedID != "111493" {
		t.Errorf("AudioDB received ID %q, want %q", audioDBReceivedID, "111493")
	}

	if result.Metadata.Biography != "Correct biography from AudioDB with enough content to pass the quality gate checks." {
		t.Errorf("expected biography from AudioDB, got: %s", result.Metadata.Biography)
	}
}

func TestOrchestratorNilProviderIDsPreservesBehavior(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// With nil providerIDs, the orchestrator should use MBID as before.
	var receivedID string
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
			receivedID = id
			return &ArtistMetadata{
				Name:      "Radiohead",
				Biography: "MusicBrainz bio for this artist with enough length to pass quality checks.",
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	_, err := orch.FetchMetadata(context.Background(), "mbid-123", "Radiohead", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	if receivedID != "mbid-123" {
		t.Errorf("provider received ID %q, want %q (MBID)", receivedID, "mbid-123")
	}
}

func TestOrchestratorEmptyProviderIDFallsBackToMBID(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	var receivedID string
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
			receivedID = id
			return &ArtistMetadata{Name: "Test"}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	// Provider ID entry exists but is empty -- should fall back to MBID.
	providerIDs := map[ProviderName]string{
		NameAudioDB: "",
	}
	_, err := orch.FetchMetadata(context.Background(), "mbid-456", "Test", providerIDs)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	if receivedID != "mbid-456" {
		t.Errorf("provider received ID %q, want %q (MBID fallback)", receivedID, "mbid-456")
	}
}

func TestBuildProviderIDMap(t *testing.T) {
	m := BuildProviderIDMap("111493", "24941", "3106", "4Z8W4fKeB5YxbusRsdQVPb")
	if m[NameAudioDB] != "111493" {
		t.Errorf("AudioDB = %q, want 111493", m[NameAudioDB])
	}
	if m[NameDiscogs] != "24941" {
		t.Errorf("Discogs = %q, want 24941", m[NameDiscogs])
	}
	if m[NameDeezer] != "3106" {
		t.Errorf("Deezer = %q, want 3106", m[NameDeezer])
	}
	if m[NameSpotify] != "4Z8W4fKeB5YxbusRsdQVPb" {
		t.Errorf("Spotify = %q, want 4Z8W4fKeB5YxbusRsdQVPb", m[NameSpotify])
	}

	// Empty strings are included (FetchImages uses empty value as "skip" signal).
	m2 := BuildProviderIDMap("", "24941", "", "")
	if m2[NameAudioDB] != "" {
		t.Errorf("AudioDB = %q, want empty", m2[NameAudioDB])
	}
	if m2[NameDiscogs] != "24941" {
		t.Errorf("Discogs = %q, want 24941", m2[NameDiscogs])
	}
	if len(m2) != 4 {
		t.Errorf("map length = %d, want 4 (all providers always included)", len(m2))
	}
}

func TestExtractProviderIDsFromURLs(t *testing.T) {
	tests := []struct {
		name           string
		urls           map[string]string
		wantDiscogsID  string
		wantWikidataID string
		wantDeezerID   string
		wantAllMusicID string
		wantSpotifyID  string
	}{
		{
			name:          "plain numeric discogs URL",
			urls:          map[string]string{"discogs": "https://www.discogs.com/artist/24941"},
			wantDiscogsID: "24941",
		},
		{
			name:          "slugged discogs URL extracts numeric prefix only",
			urls:          map[string]string{"discogs": "https://www.discogs.com/artist/24941-a-ha"},
			wantDiscogsID: "24941",
		},
		{
			name:           "wikidata Q-item",
			urls:           map[string]string{"wikidata": "https://www.wikidata.org/wiki/Q44190"},
			wantWikidataID: "Q44190",
		},
		{
			name:           "wikidata URL with query string strips it",
			urls:           map[string]string{"wikidata": "https://www.wikidata.org/wiki/Q44190?uselang=en"},
			wantWikidataID: "Q44190",
		},
		{
			name:         "deezer artist URL",
			urls:         map[string]string{"deezer": "https://www.deezer.com/artist/3106"},
			wantDeezerID: "3106",
		},
		{
			name:          "spotify artist URL",
			urls:          map[string]string{"spotify": "https://open.spotify.com/artist/4Z8W4fKeB5YxbusRsdQVPb"},
			wantSpotifyID: "4Z8W4fKeB5YxbusRsdQVPb",
		},
		{
			name:          "spotify URL with trailing slash",
			urls:          map[string]string{"spotify": "https://open.spotify.com/artist/4Z8W4fKeB5YxbusRsdQVPb/"},
			wantSpotifyID: "4Z8W4fKeB5YxbusRsdQVPb",
		},
		{
			name:          "spotify URL with query params",
			urls:          map[string]string{"spotify": "https://open.spotify.com/artist/4Z8W4fKeB5YxbusRsdQVPb?si=abc"},
			wantSpotifyID: "4Z8W4fKeB5YxbusRsdQVPb",
		},
		{
			name: "spotify URL with invalid ID (non-base62)",
			urls: map[string]string{"spotify": "https://open.spotify.com/artist/not-a-valid-spotify!!"},
		},
		{
			name: "spotify URL with invalid ID (wrong length)",
			urls: map[string]string{"spotify": "https://open.spotify.com/artist/tooshort"},
		},
		{
			name:           "plain allmusic artist URL",
			urls:           map[string]string{"allmusic": "https://www.allmusic.com/artist/mn0000505828"},
			wantAllMusicID: "mn0000505828",
		},
		{
			name:           "slugged allmusic artist URL",
			urls:           map[string]string{"allmusic": "https://www.allmusic.com/artist/dolly-parton-mn0000205560"},
			wantAllMusicID: "mn0000205560",
		},
		{
			name: "allmusic URL with mn in slug but no valid ID",
			urls: map[string]string{"allmusic": "https://www.allmusic.com/artist/amnesia-band"},
		},
		{
			name: "all providers",
			urls: map[string]string{
				"discogs":  "https://www.discogs.com/artist/24941-a-ha",
				"wikidata": "https://www.wikidata.org/wiki/Q44190",
				"deezer":   "https://www.deezer.com/artist/3106",
				"allmusic": "https://www.allmusic.com/artist/mn0000505828",
				"spotify":  "https://open.spotify.com/artist/4Z8W4fKeB5YxbusRsdQVPb",
			},
			wantDiscogsID:  "24941",
			wantWikidataID: "Q44190",
			wantDeezerID:   "3106",
			wantAllMusicID: "mn0000505828",
			wantSpotifyID:  "4Z8W4fKeB5YxbusRsdQVPb",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := &ArtistMetadata{URLs: tt.urls}
			extractProviderIDsFromURLs(meta)
			if meta.DiscogsID != tt.wantDiscogsID {
				t.Errorf("DiscogsID: got %q, want %q", meta.DiscogsID, tt.wantDiscogsID)
			}
			if meta.WikidataID != tt.wantWikidataID {
				t.Errorf("WikidataID: got %q, want %q", meta.WikidataID, tt.wantWikidataID)
			}
			if meta.DeezerID != tt.wantDeezerID {
				t.Errorf("DeezerID: got %q, want %q", meta.DeezerID, tt.wantDeezerID)
			}
			if meta.AllMusicID != tt.wantAllMusicID {
				t.Errorf("AllMusicID: got %q, want %q", meta.AllMusicID, tt.wantAllMusicID)
			}
			if meta.SpotifyID != tt.wantSpotifyID {
				t.Errorf("SpotifyID: got %q, want %q", meta.SpotifyID, tt.wantSpotifyID)
			}
		})
	}

	t.Run("existing IDs are not overwritten", func(t *testing.T) {
		meta := &ArtistMetadata{
			DiscogsID:  "existing",
			WikidataID: "Q999",
			DeezerID:   "111",
			AllMusicID: "mn0000000001",
			SpotifyID:  "0OdUWJ0sBjDrqHygGUXeCF",
			URLs: map[string]string{
				"discogs":  "https://www.discogs.com/artist/24941",
				"wikidata": "https://www.wikidata.org/wiki/Q44190",
				"deezer":   "https://www.deezer.com/artist/3106",
				"allmusic": "https://www.allmusic.com/artist/mn0000505828",
				"spotify":  "https://open.spotify.com/artist/4Z8W4fKeB5YxbusRsdQVPb",
			},
		}
		extractProviderIDsFromURLs(meta)
		if meta.DiscogsID != "existing" {
			t.Errorf("DiscogsID was overwritten: got %q", meta.DiscogsID)
		}
		if meta.WikidataID != "Q999" {
			t.Errorf("WikidataID was overwritten: got %q", meta.WikidataID)
		}
		if meta.DeezerID != "111" {
			t.Errorf("DeezerID was overwritten: got %q", meta.DeezerID)
		}
		if meta.AllMusicID != "mn0000000001" {
			t.Errorf("AllMusicID was overwritten: got %q", meta.AllMusicID)
		}
		if meta.SpotifyID != "0OdUWJ0sBjDrqHygGUXeCF" {
			t.Errorf("SpotifyID was overwritten: got %q", meta.SpotifyID)
		}
	})
}

func TestOrchestratorRejectsJunkBiography(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Genius requires an API key; store a dummy so it passes availability check.
	if err := settings.SetAPIKey(context.Background(), NameGenius, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// First provider returns junk biography "?", second has a real one.
	registry.Register(&mockNameLookupProvider{
		mockProvider: mockProvider{
			name: NameGenius,
			getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
				return &ArtistMetadata{
					Name:      "Noise Ratchet",
					Biography: "?",
				}, nil
			},
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Noise Ratchet",
				Biography: "Noise Ratchet was an American rock band from Orange County, California, formed in 1998.",
			}, nil
		},
	})

	// Set priority: Genius first, then AudioDB -- simulates the scenario where
	// Genius is tried before AudioDB and returns junk.
	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameGenius, NameAudioDB}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-noise", "Noise Ratchet", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Genius "?" should be rejected; biography should come from AudioDB.
	if result.Metadata.Biography != "Noise Ratchet was an American rock band from Orange County, California, formed in 1998." {
		t.Errorf("expected biography from AudioDB, got: %q", result.Metadata.Biography)
	}

	bioSource := findSource(result.Sources, "biography")
	if bioSource == nil || bioSource.Provider != NameAudioDB {
		t.Errorf("expected biography source from audiodb, got: %v", bioSource)
	}
}

func TestOrchestratorRejectsShortBiography(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// LastFM requires an API key; store a dummy.
	if err := settings.SetAPIKey(context.Background(), NameLastFM, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// First provider returns a too-short biography, second has a real one.
	registry.Register(&mockProvider{
		name: NameLastFM,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Test Artist",
				Biography: "A rock band.",
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Test Artist",
				Biography: "Test Artist is a musical project known for blending electronic and rock elements into something unique.",
			}, nil
		},
	})

	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameLastFM, NameAudioDB}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-test", "Test Artist", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Short bio from Last.fm should be rejected; AudioDB should provide the bio.
	if result.Metadata.Biography != "Test Artist is a musical project known for blending electronic and rock elements into something unique." {
		t.Errorf("expected biography from AudioDB, got: %q", result.Metadata.Biography)
	}
}

func TestOrchestratorAllJunkBiographiesLeaveFieldEmpty(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Genius requires an API key.
	if err := settings.SetAPIKey(context.Background(), NameGenius, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// Both providers return junk.
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Obscure Band",
				Biography: "N/A",
			}, nil
		},
	})
	registry.Register(&mockNameLookupProvider{
		mockProvider: mockProvider{
			name: NameGenius,
			getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
				return &ArtistMetadata{
					Name:      "Obscure Band",
					Biography: "?",
				}, nil
			},
		},
	})

	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameAudioDB, NameGenius}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-obscure", "Obscure Band", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// All bios are junk -- field should remain empty.
	if result.Metadata.Biography != "" {
		t.Errorf("expected empty biography when all providers return junk, got: %q", result.Metadata.Biography)
	}

	bioSource := findSource(result.Sources, "biography")
	if bioSource != nil {
		t.Errorf("expected no biography source, got: %v", bioSource)
	}
}

// TestOrchestratorCrossProviderIDEnrichment verifies that provider IDs
// extracted from one provider's URL results are used when calling subsequent
// providers. In this case, MusicBrainz returns a Discogs URL containing the
// numeric Discogs ID, and Discogs should receive that numeric ID instead of
// the MBID (which would always 404).
func TestOrchestratorCrossProviderIDEnrichment(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Discogs requires auth; store a dummy key.
	if err := settings.SetAPIKey(context.Background(), NameDiscogs, "test-token"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// MusicBrainz returns metadata with a Discogs URL containing the numeric ID.
	registry.Register(&mockProvider{
		name:    NameMusicBrainz,
		authReq: false,
		getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "A-ha",
				Biography: "Norwegian band",
				URLs: map[string]string{
					"discogs": "https://www.discogs.com/artist/24941-a-ha",
					"deezer":  "https://www.deezer.com/artist/75798",
				},
			}, nil
		},
	})

	// Discogs records which ID it receives. It should get "24941" (from the
	// URL), not the MBID.
	var discogsReceivedID string
	registry.Register(&mockNameLookupProvider{
		mockProvider: mockProvider{
			name:    NameDiscogs,
			authReq: true,
			getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
				discogsReceivedID = id
				return &ArtistMetadata{
					Name:      "A-ha",
					DiscogsID: "24941",
				}, nil
			},
		},
	})

	// Set up priorities so MusicBrainz is queried first (for biography),
	// then Discogs (for biography fallback).
	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameMusicBrainz, NameDiscogs}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	// No stored provider IDs -- only the MBID.
	_, err := orch.FetchMetadata(context.Background(), "cc2c9c3c-b7bc-4b8b-84d8-4fbd8779e493", "A-ha", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Discogs should have received the numeric ID extracted from MusicBrainz's
	// Discogs URL, not the raw MBID.
	if discogsReceivedID != "24941" {
		t.Errorf("expected Discogs to receive ID '24941' (from MusicBrainz URL), got %q", discogsReceivedID)
	}
}

// TestEnrichProviderIDs verifies the EnrichProviderIDs function extracts
// provider IDs from URLs and updates the providerIDs map.
func TestEnrichProviderIDs(t *testing.T) {
	providerIDs := make(map[ProviderName]string)

	meta := &ArtistMetadata{
		URLs: map[string]string{
			"discogs": "https://www.discogs.com/artist/24941-a-ha",
			"deezer":  "https://www.deezer.com/artist/3106",
			"spotify": "https://open.spotify.com/artist/2jzc5TC5TVFLXQlBNgQBiB",
		},
	}

	EnrichProviderIDs(meta, providerIDs)

	if providerIDs[NameDiscogs] != "24941" {
		t.Errorf("expected Discogs ID '24941', got %q", providerIDs[NameDiscogs])
	}
	if providerIDs[NameDeezer] != "3106" {
		t.Errorf("expected Deezer ID '3106', got %q", providerIDs[NameDeezer])
	}
	if providerIDs[NameSpotify] != "2jzc5TC5TVFLXQlBNgQBiB" {
		t.Errorf("expected Spotify ID '2jzc5TC5TVFLXQlBNgQBiB', got %q", providerIDs[NameSpotify])
	}
}

// TestEnrichProviderIDsNoOverwrite verifies that EnrichProviderIDs does not
// overwrite existing entries in the providerIDs map.
func TestEnrichProviderIDsNoOverwrite(t *testing.T) {
	providerIDs := map[ProviderName]string{
		NameDiscogs: "99999", // pre-existing stored ID
	}

	meta := &ArtistMetadata{
		URLs: map[string]string{
			"discogs": "https://www.discogs.com/artist/24941",
		},
	}

	EnrichProviderIDs(meta, providerIDs)

	// Should keep the original value, not overwrite with URL-extracted one.
	if providerIDs[NameDiscogs] != "99999" {
		t.Errorf("expected Discogs ID to remain '99999', got %q", providerIDs[NameDiscogs])
	}
}

// TestEnrichProviderIDsNilInputs verifies that EnrichProviderIDs does not
// panic when called with nil metadata or a nil providerIDs map.
func TestEnrichProviderIDsNilInputs(t *testing.T) {
	// nil metadata should be a no-op.
	ids := map[ProviderName]string{}
	EnrichProviderIDs(nil, ids)
	if len(ids) != 0 {
		t.Errorf("expected empty map after nil meta, got %v", ids)
	}

	// nil providerIDs map should be a no-op.
	meta := &ArtistMetadata{
		URLs: map[string]string{"discogs": "https://www.discogs.com/artist/24941"},
	}
	EnrichProviderIDs(meta, nil) // should not panic
}

// TestEnrichProviderIDsEmptyStringValues verifies that EnrichProviderIDs fills
// in empty-string entries, which is how ProviderIDMap() represents unknown IDs.
func TestEnrichProviderIDsEmptyStringValues(t *testing.T) {
	providerIDs := map[ProviderName]string{
		NameDiscogs: "",            // unknown -- should be filled
		NameDeezer:  "",            // unknown -- should be filled
		NameSpotify: "existing-id", // known -- should be preserved
	}

	meta := &ArtistMetadata{
		URLs: map[string]string{
			"discogs": "https://www.discogs.com/artist/24941",
			"deezer":  "https://www.deezer.com/artist/3106",
			"spotify": "https://open.spotify.com/artist/2jzc5TC5TVFLXQlBNgQBiB",
		},
	}

	EnrichProviderIDs(meta, providerIDs)

	if providerIDs[NameDiscogs] != "24941" {
		t.Errorf("expected empty Discogs entry to be filled with '24941', got %q", providerIDs[NameDiscogs])
	}
	if providerIDs[NameDeezer] != "3106" {
		t.Errorf("expected empty Deezer entry to be filled with '3106', got %q", providerIDs[NameDeezer])
	}
	if providerIDs[NameSpotify] != "existing-id" {
		t.Errorf("expected non-empty Spotify entry to be preserved as 'existing-id', got %q", providerIDs[NameSpotify])
	}
}

func TestIsImageFieldName(t *testing.T) {
	imageFields := []string{"thumb", "fanart", "logo", "banner"}
	for _, f := range imageFields {
		if !isImageFieldName(f) {
			t.Errorf("isImageFieldName(%q) = false, want true", f)
		}
	}
	textFields := []string{"biography", "genres", "styles", "moods", "members", "formed", "born"}
	for _, f := range textFields {
		if isImageFieldName(f) {
			t.Errorf("isImageFieldName(%q) = true, want false", f)
		}
	}
}

// TestApplyFieldImageTypeFilter verifies that applyField returns true only
// when the provider has images of the requested type, not just any images.
func TestApplyFieldImageTypeFilter(t *testing.T) {
	result := &FetchResult{
		Metadata: &ArtistMetadata{URLs: make(map[string]string)},
	}
	// Provider has fanart images but no thumb images.
	pr := &providerResult{
		meta: &ArtistMetadata{Name: "Test"},
		images: []ImageResult{
			{URL: "http://example.com/fanart1.jpg", Type: ImageFanart, Source: "test"},
			{URL: "http://example.com/fanart2.jpg", Type: ImageFanart, Source: "test"},
		},
	}

	// applyField for "thumb" should return false because there are no thumb images.
	if applyField(result, "thumb", pr, NameAudioDB) {
		t.Error("applyField(thumb) returned true, but provider has no thumb images")
	}
	if len(result.Images) != 0 {
		t.Errorf("expected 0 images after thumb miss, got %d", len(result.Images))
	}

	// applyField for "fanart" should return true and add both images.
	if !applyField(result, "fanart", pr, NameAudioDB) {
		t.Error("applyField(fanart) returned false, but provider has fanart images")
	}
	if len(result.Images) != 2 {
		t.Errorf("expected 2 fanart images, got %d", len(result.Images))
	}
}

// TestFetchMetadataAggregatesImagesFromMultipleProviders verifies that the
// FetchMetadata priority loop collects image candidates from all enabled
// providers rather than stopping at the first provider with matching images.
func TestFetchMetadataAggregatesImagesFromMultipleProviders(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Register two providers that both return fanart images.
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "a-ha"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return []ImageResult{
				{URL: "http://audiodb.com/fanart1.jpg", Type: ImageFanart, Source: "musicbrainz"},
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "a-ha"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return []ImageResult{
				{URL: "http://audiodb.com/fanart2.jpg", Type: ImageFanart, Source: "audiodb"},
				{URL: "http://audiodb.com/fanart3.jpg", Type: ImageFanart, Source: "audiodb"},
			}, nil
		},
	})

	// Set fanart priority: MusicBrainz first, then AudioDB.
	if err := settings.SetPriority(context.Background(), "fanart", []ProviderName{NameMusicBrainz, NameAudioDB}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-aha", "a-ha", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// All 3 fanart images from both providers should be present.
	fanartCount := 0
	for _, img := range result.Images {
		if img.Type == ImageFanart {
			fanartCount++
		}
	}
	if fanartCount != 3 {
		t.Errorf("expected 3 fanart images from two providers, got %d", fanartCount)
	}
}

// TestFetchMetadataTextFieldStopsAtFirstMatch verifies that text fields
// (e.g., biography) still stop at the first provider with data.
func TestFetchMetadataTextFieldStopsAtFirstMatch(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	firstCalled := false

	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			firstCalled = true
			return &ArtistMetadata{
				Name:      "Test",
				Biography: "This is a sufficiently long biography from the first provider to pass quality checks.",
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Test",
				Biography: "This is a different biography from the second provider.",
			}, nil
		},
	})

	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameMusicBrainz, NameAudioDB}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-test", "Test", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Biography should come from first provider (MusicBrainz won).
	if !firstCalled {
		t.Error("expected first provider to be called")
	}
	// The second provider's GetArtist may have been called (due to caching/other fields),
	// but the biography should still come from the first provider.
	if result.Metadata.Biography != "This is a sufficiently long biography from the first provider to pass quality checks." {
		t.Errorf("expected biography from first provider, got: %q", result.Metadata.Biography)
	}
}

// TestFetchImagesCollectsFromAllProviders verifies that the standalone
// FetchImages method returns candidates from every available provider.
func TestFetchImagesCollectsFromAllProviders(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return []ImageResult{
				{URL: "http://mb.com/thumb.jpg", Type: ImageThumb, Source: "musicbrainz"},
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return []ImageResult{
				{URL: "http://audiodb.com/thumb.jpg", Type: ImageThumb, Source: "audiodb"},
				{URL: "http://audiodb.com/fanart.jpg", Type: ImageFanart, Source: "audiodb"},
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchImages(context.Background(), "mbid-test", nil)
	if err != nil {
		t.Fatalf("FetchImages: %v", err)
	}

	// Should have 3 total images: 1 thumb from MB, 1 thumb + 1 fanart from AudioDB.
	if len(result.Images) != 3 {
		t.Errorf("expected 3 images total, got %d", len(result.Images))
	}

	thumbCount := 0
	fanartCount := 0
	for _, img := range result.Images {
		switch img.Type {
		case ImageThumb:
			thumbCount++
		case ImageFanart:
			fanartCount++
		}
	}
	if thumbCount != 2 {
		t.Errorf("expected 2 thumb images from two providers, got %d", thumbCount)
	}
	if fanartCount != 1 {
		t.Errorf("expected 1 fanart image, got %d", fanartCount)
	}
}

// TestApplyFieldImageDoesNotBlockOnWrongType verifies that a provider
// with images of one type does not block collection of a different type
// from subsequent providers.
func TestApplyFieldImageDoesNotBlockOnWrongType(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Provider 1 has fanart but no thumb. Provider 2 has thumb.
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "Test"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return []ImageResult{
				{URL: "http://mb.com/fanart.jpg", Type: ImageFanart, Source: "musicbrainz"},
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "Test"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return []ImageResult{
				{URL: "http://audiodb.com/thumb.jpg", Type: ImageThumb, Source: "audiodb"},
			}, nil
		},
	})

	// Set thumb priority: MusicBrainz first, then AudioDB.
	if err := settings.SetPriority(context.Background(), "thumb", []ProviderName{NameMusicBrainz, NameAudioDB}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-test", "Test", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// AudioDB thumb should be present. Before the fix, MusicBrainz having
	// fanart images would cause applyField to return true for "thumb",
	// blocking AudioDB from contributing its actual thumb image.
	thumbCount := 0
	for _, img := range result.Images {
		if img.Type == ImageThumb {
			thumbCount++
		}
	}
	if thumbCount < 1 {
		t.Error("expected at least 1 thumb image from AudioDB, got 0")
	}
}

// TestOrchestratorErrNotFoundMarksFieldAttempted verifies that when a provider
// returns ErrNotFound (artist not in that provider's database), the field is
// still counted as "attempted" in AttemptedFields. This enables refresh-overwrite
// semantics: "provider was reached and said not found" clears stale data.
func TestOrchestratorErrNotFoundMarksFieldAttempted(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	registry.Register(&mockProvider{
		name:    NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
			return nil, &ErrNotFound{Provider: NameAudioDB, ID: id}
		},
	})

	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameAudioDB}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-1234", "Test Artist", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	if !containsString(result.AttemptedFields, "biography") {
		t.Errorf("expected 'biography' in AttemptedFields after ErrNotFound, got %v", result.AttemptedFields)
	}

	found := false
	for _, p := range result.AttemptedProviders {
		if p == NameAudioDB {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected AudioDB in AttemptedProviders after ErrNotFound, got %v", result.AttemptedProviders)
	}

	if result.Metadata.Biography != "" {
		t.Errorf("expected empty biography, got %q", result.Metadata.Biography)
	}
}

// TestOrchestratorNetworkErrorDoesNotMarkFieldAttempted verifies that a real
// network error (timeout, connection refused) does NOT mark the field as
// attempted.
func TestOrchestratorNetworkErrorDoesNotMarkFieldAttempted(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	registry.Register(&mockProvider{
		name:    NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return nil, fmt.Errorf("connection timeout")
		},
	})

	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameAudioDB}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-1234", "Test Artist", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	if containsString(result.AttemptedFields, "biography") {
		t.Errorf("expected 'biography' NOT in AttemptedFields after network error, got %v", result.AttemptedFields)
	}
}

// TestOrchestratorErrNotFoundCountsAsAttemptedProvider verifies that a provider
// returning ErrNotFound is still included in AttemptedProviders.
func TestOrchestratorErrNotFoundCountsAsAttemptedProvider(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	registry.Register(&mockProvider{
		name:    NameAudioDB,
		authReq: false,
		getArtFn: func(_ context.Context, id string) (*ArtistMetadata, error) {
			return nil, &ErrNotFound{Provider: NameAudioDB, ID: id}
		},
	})

	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameAudioDB}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-1234", "Test Artist", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	found := false
	for _, p := range result.AttemptedProviders {
		if p == NameAudioDB {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected AudioDB in AttemptedProviders after ErrNotFound, got %v", result.AttemptedProviders)
	}
}

// TestFetchImagesQueriesAllProviders verifies that FetchImages queries every
// available provider so callers (image search UI, ImageFixer) receive the
// full set of candidates for quality sorting and user selection.
func TestFetchImagesQueriesAllProviders(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Provider A returns all four image types.
	registry.Register(&mockProvider{
		name:    NameFanartTV,
		authReq: true,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return []ImageResult{
				{Type: ImageThumb, URL: "http://example.com/thumb.jpg"},
				{Type: ImageFanart, URL: "http://example.com/fanart.jpg"},
				{Type: ImageLogo, URL: "http://example.com/logo.png"},
				{Type: ImageBanner, URL: "http://example.com/banner.jpg"},
			}, nil
		},
	})
	if err := settings.SetAPIKey(context.Background(), NameFanartTV, "test-key"); err != nil {
		t.Fatalf("SetAPIKey FanartTV: %v", err)
	}

	// Provider B should still be called even though all types are covered,
	// because FetchImages always queries all providers.
	audioDBCalled := false
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			audioDBCalled = true
			return []ImageResult{
				{Type: ImageThumb, URL: "http://example.com/thumb2.jpg"},
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchImages(context.Background(), "mbid-test", nil)
	if err != nil {
		t.Fatalf("FetchImages: %v", err)
	}

	// All 5 images (4 from FanartTV + 1 from AudioDB) should be collected.
	if len(result.Images) != 5 {
		t.Errorf("expected 5 images (all providers queried), got %d", len(result.Images))
	}
	if !audioDBCalled {
		t.Error("AudioDB should have been called -- FetchImages queries all providers")
	}
}

func TestIsAllMusicID(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"mn0000505828", true},   // valid: mn + 10 digits
		{"mn00005", false},       // too short
		{"mn00005058281", false}, // too long
		{"ab0000505828", false},  // wrong prefix
		{"mn000050582a", false},  // non-digit after mn
		{"", false},              // empty string
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := isAllMusicID(tt.input); got != tt.want {
				t.Errorf("isAllMusicID(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
