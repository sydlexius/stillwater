package provider

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider/tagdict"
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
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchMetadata(context.Background(), "mbid-123", "Radiohead", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Biography should come from Last.fm (MusicBrainz returned empty)
	if result.Metadata.Biography != "Radiohead are an English rock band formed in Abingdon, Oxfordshire, in 1985." {
		t.Errorf("expected biography from Last.fm, got: %s", result.Metadata.Biography)
	}

	// Genres should be accumulated from both providers (MusicBrainz first, then Last.fm).
	// With tag aggregation, both "rock" (MusicBrainz, canonicalized to "Rock") and
	// "alternative" (Last.fm) are present.
	if len(result.Metadata.Genres) != 2 {
		t.Errorf("expected 2 genres (aggregated), got: %v", result.Metadata.Genres)
	}
	if result.Metadata.Genres[0] != "Rock" {
		t.Errorf("expected Rock first (MusicBrainz priority, canonicalized), got: %s", result.Metadata.Genres[0])
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

// TestFetchMetadata_MBNameAuthoritative verifies that MusicBrainz's Name and
// SortName win even when an earlier-iterated provider (e.g. wikipedia during
// the biography field) has already populated result.Metadata.Name via the
// first-provider-wins merge in applyField. MusicBrainz is the only provider
// that applies language-aware alias promotion to Name, so its value must
// survive when a user has metadata language preferences set; otherwise the
// canonical (un-promoted) form from another provider locks in first and the
// user-requested localized display name is silently dropped.
func TestFetchMetadata_MBNameAuthoritative(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Wikipedia: fires first via the default biography priority and returns
	// the canonical (un-promoted) Name. In production this is what happens
	// when the user's artist record holds a non-Latin canonical name -- the
	// wikipedia adapter reads it from the input and echoes it back.
	registry.Register(&mockProvider{
		name: NameWikipedia,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Canonical Name",
				Biography: "About the artist.",
			}, nil
		},
	})
	// MusicBrainz: returns the promoted Latin Name (mirrors what the MB
	// adapter's internal language-aware alias promotion produces when the
	// user has Latin-family prefs and MB has a matching alias).
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:     "Promoted Name",
				SortName: "Promoted Sort",
				Genres:   []string{"rock"},
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchMetadata(context.Background(), "test-mbid", "Canonical Name", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	if result.Metadata.Name != "Promoted Name" {
		t.Errorf("expected MB's Name to override earlier provider's Name, got %q", result.Metadata.Name)
	}
	if result.Metadata.SortName != "Promoted Sort" {
		t.Errorf("expected MB's SortName to override, got %q", result.Metadata.SortName)
	}
}

// TestFetchMetadata_MBNameAuthoritative_EmptyDoesNotClobber verifies the
// override respects empty values: when MusicBrainz returns an empty Name
// (error, not-found, or just no data), a Name already set by another
// provider survives. Without this guard the override would erase a perfectly
// good name on any refresh where MB is unreachable.
func TestFetchMetadata_MBNameAuthoritative_EmptyDoesNotClobber(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	registry.Register(&mockProvider{
		name: NameWikipedia,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Wikipedia Name",
				Biography: "About the artist.",
			}, nil
		},
	})
	// MusicBrainz returns nothing (empty struct), simulating a provider that
	// was queried successfully but had no data to contribute for Name.
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Genres: []string{"rock"}}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchMetadata(context.Background(), "test-mbid", "Whatever", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	if result.Metadata.Name != "Wikipedia Name" {
		t.Errorf("expected wikipedia Name preserved when MB has none, got %q", result.Metadata.Name)
	}
}

// TestFetchMetadata_MBNameAuthoritative_MBErrorDoesNotClobber verifies the
// override respects provider errors: when MB returns an error (timeout, 5xx,
// unreachable), its cached providerResult has err != nil and the override
// must short-circuit, preserving a Name set by another provider. Without
// this guard a transient MB outage would erase the artist's Name on every
// affected refresh.
func TestFetchMetadata_MBNameAuthoritative_MBErrorDoesNotClobber(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	registry.Register(&mockProvider{
		name: NameWikipedia,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Wikipedia Name",
				Biography: "About the artist.",
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return nil, fmt.Errorf("musicbrainz timeout")
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchMetadata(context.Background(), "test-mbid", "Whatever", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	if result.Metadata.Name != "Wikipedia Name" {
		t.Errorf("expected wikipedia Name preserved on MB error, got %q", result.Metadata.Name)
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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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

func TestExtractFieldForComparison_Origin(t *testing.T) {
	fpr := &FieldProviderResult{}
	extractFieldForComparison(fpr, "origin", &ArtistMetadata{Origin: "United Kingdom"})
	if !fpr.HasData {
		t.Error("expected HasData = true for non-empty origin")
	}
	if fpr.Value != "United Kingdom" {
		t.Errorf("Value = %q, want %q", fpr.Value, "United Kingdom")
	}

	empty := &FieldProviderResult{}
	extractFieldForComparison(empty, "origin", &ArtistMetadata{})
	if empty.HasData {
		t.Error("expected HasData = false for empty origin")
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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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

// TestOrchestratorGetImagesNotCalledDoesNotMarkImageFieldAttempted verifies
// that when a provider has no MBID and no provider-specific ID (so GetImages
// is never invoked), image fields are NOT added to AttemptedFields. Without
// this guard, imageErr==nil would incorrectly mark image fields as attempted,
// potentially clearing existing image data even though no fetch was made.
func TestOrchestratorGetImagesNotCalledDoesNotMarkImageFieldAttempted(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Register AudioDB: GetArtist can succeed via name, but GetImages requires
	// a provider-specific numeric ID that is not available here.
	var getImagesCalled bool
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "Test Artist"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			getImagesCalled = true
			return []ImageResult{{Type: ImageThumb, URL: "https://example.com/thumb.jpg"}}, nil
		},
	})

	// Set thumb priority to AudioDB.
	if err := settings.SetPriority(context.Background(), "thumb", []ProviderName{NameAudioDB}); err != nil {
		t.Fatalf("SetPriority thumb: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	// Pass empty MBID and providerIDs with an empty AudioDB entry.
	// GetImages requires a numeric ID; without one it must not be called.
	providerIDs := map[ProviderName]string{
		NameAudioDB: "", // empty: no provider-specific ID known
	}
	result, err := orch.FetchMetadata(context.Background(), "", "Test Artist", providerIDs)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// GetImages must not have been called when no ID was available.
	if getImagesCalled {
		t.Error("GetImages should not have been called when no MBID and no provider-specific ID is available")
	}

	// "thumb" must NOT be in AttemptedFields because GetImages was never called.
	for _, f := range result.AttemptedFields {
		if f == "thumb" {
			t.Errorf("image field 'thumb' should NOT be in AttemptedFields when GetImages was never called, got %v", result.AttemptedFields)
			break
		}
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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
			ExtractProviderIDsFromURLs(meta)
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
		ExtractProviderIDsFromURLs(meta)
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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
//
// Uses the genres field because MusicBrainz is excluded from biography
// (it does not return biography data).
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
				Name:   "A-ha",
				Genres: []string{"synth-pop"},
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

	// Set up priorities so MusicBrainz is queried first (for genres),
	// then Discogs (for genres). The enrichment from MusicBrainz URL results
	// should feed Discogs the extracted numeric ID. Discogs must be disabled
	// for biography (which comes before genres in default order) so it is not
	// called before MusicBrainz has provided URL enrichment.
	if err := settings.SetDisabledProviders(context.Background(), "biography", []ProviderName{NameDiscogs}); err != nil {
		t.Fatalf("SetDisabledProviders biography: %v", err)
	}
	if err := settings.SetPriority(context.Background(), "genres", []ProviderName{NameMusicBrainz, NameDiscogs}); err != nil {
		t.Fatalf("SetPriority genres: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

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

// TestApplyFieldDetailFields verifies that applyField handles the detail
// fields (gender, type, years_active, born, died, disbanded) by setting the
// target field only when it is currently empty (first-match-wins) and returns
// true when a value was applied.
func TestApplyFieldDetailFields(t *testing.T) {
	cases := []struct {
		field    string
		meta     ArtistMetadata
		readBack func(*ArtistMetadata) string
	}{
		{"gender", ArtistMetadata{Gender: "Male"}, func(m *ArtistMetadata) string { return m.Gender }},
		{"type", ArtistMetadata{Type: "group"}, func(m *ArtistMetadata) string { return m.Type }},
		{"origin", ArtistMetadata{Origin: "United Kingdom"}, func(m *ArtistMetadata) string { return m.Origin }},
		{"years_active", ArtistMetadata{YearsActive: "1980-1990"}, func(m *ArtistMetadata) string { return m.YearsActive }},
		{"born", ArtistMetadata{Born: "1970-01-01"}, func(m *ArtistMetadata) string { return m.Born }},
		{"died", ArtistMetadata{Died: "2020-01-01"}, func(m *ArtistMetadata) string { return m.Died }},
		{"disbanded", ArtistMetadata{Disbanded: "2005-01-01"}, func(m *ArtistMetadata) string { return m.Disbanded }},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			result := &FetchResult{Metadata: &ArtistMetadata{URLs: make(map[string]string)}}
			pr := &providerResult{meta: &tc.meta}
			if !applyField(result, tc.field, pr, NameMusicBrainz) {
				t.Fatalf("applyField(%s) = false, want true", tc.field)
			}
			if got := tc.readBack(result.Metadata); got != tc.readBack(&tc.meta) {
				t.Errorf("after apply %s: got %q, want %q", tc.field, got, tc.readBack(&tc.meta))
			}
			src := findSource(result.Sources, tc.field)
			if src == nil || src.Provider != NameMusicBrainz {
				t.Errorf("applyField(%s) source = %v, want provider %s", tc.field, src, NameMusicBrainz)
			}
			// Second provider must not overwrite the first-match-wins value.
			pr2 := &providerResult{meta: &ArtistMetadata{
				Gender: "Female", Type: "solo", YearsActive: "1999", Born: "x",
				Died: "y", Disbanded: "z", Origin: "Canada",
			}}
			if applyField(result, tc.field, pr2, NameWikidata) {
				t.Errorf("applyField(%s) returned true on second provider, expected first-match-wins", tc.field)
			}
			if got := tc.readBack(result.Metadata); got != tc.readBack(&tc.meta) {
				t.Errorf("after second apply %s: got %q, want preserved %q", tc.field, got, tc.readBack(&tc.meta))
			}
		})
	}
}

// TestApplyFieldGenderClearedOnNonIndividualType verifies that applying a
// non-individual "type" value (e.g. "group") to a result that already holds a
// gender clears the gender value and its FieldSource provenance. This
// mirrors the scraper-executor normalization path, which forbids gender on
// group/orchestra/choir types.
func TestApplyFieldGenderClearedOnNonIndividualType(t *testing.T) {
	result := &FetchResult{Metadata: &ArtistMetadata{URLs: make(map[string]string)}}

	// First apply gender from one provider.
	prGender := &providerResult{meta: &ArtistMetadata{Gender: "Female"}}
	if !applyField(result, "gender", prGender, NameMusicBrainz) {
		t.Fatalf("applyField(gender) = false, want true")
	}
	if result.Metadata.Gender != "Female" {
		t.Fatalf("Gender = %q, want Female", result.Metadata.Gender)
	}
	if findSource(result.Sources, "gender") == nil {
		t.Fatalf("gender FieldSource missing after initial apply")
	}

	// Now apply a non-individual type; gender and its provenance must clear.
	prType := &providerResult{meta: &ArtistMetadata{Type: "group"}}
	if !applyField(result, "type", prType, NameWikidata) {
		t.Fatalf("applyField(type) = false, want true")
	}
	if result.Metadata.Gender != "" {
		t.Errorf("Gender = %q after non-individual type, want empty", result.Metadata.Gender)
	}
	if s := findSource(result.Sources, "gender"); s != nil {
		t.Errorf("gender FieldSource = %v after non-individual type, want nil", s)
	}
	if findSource(result.Sources, "type") == nil {
		t.Errorf("type FieldSource missing after apply")
	}
}

// TestApplyFieldGenderRejectedWhenTypeNonIndividual verifies that when the
// accumulated type is already a non-individual value, a later gender apply
// is rejected.
func TestApplyFieldGenderRejectedWhenTypeNonIndividual(t *testing.T) {
	result := &FetchResult{
		Metadata: &ArtistMetadata{Type: "group", URLs: make(map[string]string)},
		Sources:  []FieldSource{{Field: "type", Provider: NameMusicBrainz}},
	}
	pr := &providerResult{meta: &ArtistMetadata{Gender: "Male"}}
	if applyField(result, "gender", pr, NameWikidata) {
		t.Errorf("applyField(gender) = true with non-individual type, want false")
	}
	if result.Metadata.Gender != "" {
		t.Errorf("Gender = %q, want empty", result.Metadata.Gender)
	}
	if findSource(result.Sources, "gender") != nil {
		t.Errorf("gender FieldSource set on rejected apply")
	}
}

// TestApplyFieldGenderPreservedForIndividualTypes guards against the
// predicate-vocabulary regression where applyType / applyGender used a local
// orchestrator helper that only recognized "person", while the upstream
// MusicBrainz mapping layer (and internal/artist) treats "solo", "person",
// and "character" as individual types that carry gender. Without sharing the
// predicate, a solo artist's gender was silently cleared in applyType or
// blocked in applyGender depending on field-arrival order.
func TestApplyFieldGenderPreservedForIndividualTypes(t *testing.T) {
	for _, typ := range []string{"solo", "person", "character"} {
		t.Run("gender_first_type="+typ, func(t *testing.T) {
			result := &FetchResult{Metadata: &ArtistMetadata{URLs: make(map[string]string)}}
			applyField(result, "gender", &providerResult{meta: &ArtistMetadata{Gender: "Female"}}, NameMusicBrainz)
			applyField(result, "type", &providerResult{meta: &ArtistMetadata{Type: typ}}, NameWikidata)
			if result.Metadata.Gender != "Female" {
				t.Errorf("Gender = %q, want Female (type %q must preserve gender)", result.Metadata.Gender, typ)
			}
			if findSource(result.Sources, "gender") == nil {
				t.Errorf("gender FieldSource cleared for individual type %q", typ)
			}
		})
		t.Run("type_first_then_gender_type="+typ, func(t *testing.T) {
			result := &FetchResult{
				Metadata: &ArtistMetadata{Type: typ, URLs: make(map[string]string)},
				Sources:  []FieldSource{{Field: "type", Provider: NameMusicBrainz}},
			}
			if !applyField(result, "gender", &providerResult{meta: &ArtistMetadata{Gender: "Male"}}, NameWikidata) {
				t.Errorf("applyField(gender) = false for individual type %q, want true", typ)
			}
			if result.Metadata.Gender != "Male" {
				t.Errorf("Gender = %q, want Male (type %q must accept gender)", result.Metadata.Gender, typ)
			}
		})
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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
// Uses Wikipedia as the first provider because MusicBrainz is excluded from
// biography (it does not return biography data), and Wikipedia does not
// require an API key.
func TestFetchMetadataTextFieldStopsAtFirstMatch(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	firstCalled := false

	registry.Register(&mockProvider{
		name: NameWikipedia,
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

	if err := settings.SetPriority(context.Background(), "biography", []ProviderName{NameWikipedia, NameAudioDB}); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchMetadata(context.Background(), "mbid-test", "Test", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Biography should come from first provider (Wikipedia won).
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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
		default:
			// Other image types are not relevant to this assertion.
		}
	}
	if thumbCount != 2 {
		t.Errorf("expected 2 thumb images from two providers, got %d", thumbCount)
	}
	if fanartCount != 1 {
		t.Errorf("expected 1 fanart image, got %d", fanartCount)
	}
}

// TestFetchImagesRespectsProviderPriority verifies that FetchImages returns
// images from providers in the configured priority order. When the user sets
// AudioDB before FanartTV in the thumb priority, AudioDB images should appear
// first in the result slice.
func TestFetchImagesRespectsProviderPriority(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Store a dummy API key for FanartTV so it passes availability check.
	if err := settings.SetAPIKey(context.Background(), NameFanartTV, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// Track the order in which providers are called.
	var callOrder []ProviderName
	registry.Register(&mockProvider{
		name: NameFanartTV,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			callOrder = append(callOrder, NameFanartTV)
			return []ImageResult{
				{URL: "http://fanart.tv/thumb.jpg", Type: ImageThumb, Source: "fanarttv"},
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			callOrder = append(callOrder, NameAudioDB)
			return []ImageResult{
				{URL: "http://audiodb.com/thumb.jpg", Type: ImageThumb, Source: "audiodb"},
			}, nil
		},
	})

	// Configure thumb priority: AudioDB first, then FanartTV (reversed from default).
	if err := settings.SetPriority(context.Background(), "thumb", []ProviderName{NameAudioDB, NameFanartTV}); err != nil {
		t.Fatalf("SetPriority thumb: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchImages(context.Background(), "mbid-test", nil)
	if err != nil {
		t.Fatalf("FetchImages: %v", err)
	}

	// Both providers should be called.
	if len(callOrder) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(callOrder))
	}

	// AudioDB should be called first (higher priority in configured order).
	if callOrder[0] != NameAudioDB {
		t.Errorf("expected AudioDB called first, got %s", callOrder[0])
	}
	if callOrder[1] != NameFanartTV {
		t.Errorf("expected FanartTV called second, got %s", callOrder[1])
	}

	// Images should appear in priority order: AudioDB thumb first, FanartTV thumb second.
	if len(result.Images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(result.Images))
	}
	if result.Images[0].Source != "audiodb" {
		t.Errorf("expected first image from audiodb, got %s", result.Images[0].Source)
	}
	if result.Images[1].Source != "fanarttv" {
		t.Errorf("expected second image from fanarttv, got %s", result.Images[1].Source)
	}
}

// TestFetchImages_CrossFieldPriorityConflict verifies first-field-wins semantics
// when thumb and fanart have conflicting provider orders. Thumb priorities list
// AudioDB first while fanart priorities list FanartTV first. Because thumb is
// walked before fanart in imageProvidersInPriorityOrder, AudioDB should be
// called first globally (thumb's order wins for the first-seen provider).
func TestFetchImages_CrossFieldPriorityConflict(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Store a dummy API key for FanartTV so it passes availability check.
	if err := settings.SetAPIKey(context.Background(), NameFanartTV, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// Track the order in which providers are called.
	var callOrder []ProviderName
	registry.Register(&mockProvider{
		name: NameFanartTV,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			callOrder = append(callOrder, NameFanartTV)
			return []ImageResult{
				{URL: "http://fanart.tv/thumb.jpg", Type: ImageThumb, Source: "fanarttv"},
				{URL: "http://fanart.tv/fanart.jpg", Type: ImageFanart, Source: "fanarttv"},
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			callOrder = append(callOrder, NameAudioDB)
			return []ImageResult{
				{URL: "http://audiodb.com/thumb.jpg", Type: ImageThumb, Source: "audiodb"},
				{URL: "http://audiodb.com/fanart.jpg", Type: ImageFanart, Source: "audiodb"},
			}, nil
		},
	})

	// Conflicting priorities: thumb wants AudioDB first, fanart wants FanartTV first.
	if err := settings.SetPriority(context.Background(), "thumb", []ProviderName{NameAudioDB, NameFanartTV}); err != nil {
		t.Fatalf("SetPriority thumb: %v", err)
	}
	if err := settings.SetPriority(context.Background(), "fanart", []ProviderName{NameFanartTV, NameAudioDB}); err != nil {
		t.Fatalf("SetPriority fanart: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchImages(context.Background(), "mbid-test", nil)
	if err != nil {
		t.Fatalf("FetchImages: %v", err)
	}

	// Both providers should be called.
	if len(callOrder) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(callOrder))
	}

	// AudioDB should be called first because thumb (the first image field) lists
	// AudioDB first. FanartTV's fanart priority does not override this since
	// AudioDB was already positioned by thumb (first-field-wins).
	if callOrder[0] != NameAudioDB {
		t.Errorf("expected AudioDB called first (thumb priority wins), got %s", callOrder[0])
	}
	if callOrder[1] != NameFanartTV {
		t.Errorf("expected FanartTV called second, got %s", callOrder[1])
	}

	// Should have 4 images total: thumb + fanart from each provider.
	if len(result.Images) != 4 {
		t.Fatalf("expected 4 images, got %d", len(result.Images))
	}

	// First two images should be from AudioDB (called first), last two from FanartTV.
	if result.Images[0].Source != "audiodb" {
		t.Errorf("expected first image from audiodb, got %s", result.Images[0].Source)
	}
	if result.Images[1].Source != "audiodb" {
		t.Errorf("expected second image from audiodb, got %s", result.Images[1].Source)
	}
	if result.Images[2].Source != "fanarttv" {
		t.Errorf("expected third image from fanarttv, got %s", result.Images[2].Source)
	}
	if result.Images[3].Source != "fanarttv" {
		t.Errorf("expected fourth image from fanarttv, got %s", result.Images[3].Source)
	}
}

// TestFetchImagesDefaultOrderWithoutCustomPriority verifies that FetchImages
// uses the default priority order when no custom priority is configured.
func TestFetchImagesDefaultOrderWithoutCustomPriority(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// Store a dummy API key for FanartTV so it passes availability check.
	if err := settings.SetAPIKey(context.Background(), NameFanartTV, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	var callOrder []ProviderName
	registry.Register(&mockProvider{
		name: NameFanartTV,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			callOrder = append(callOrder, NameFanartTV)
			return []ImageResult{
				{URL: "http://fanart.tv/thumb.jpg", Type: ImageThumb, Source: "fanarttv"},
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			callOrder = append(callOrder, NameAudioDB)
			return []ImageResult{
				{URL: "http://audiodb.com/thumb.jpg", Type: ImageThumb, Source: "audiodb"},
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchImages(context.Background(), "mbid-test", nil)
	if err != nil {
		t.Fatalf("FetchImages: %v", err)
	}

	// Default thumb priority is FanartTV, AudioDB (see DefaultPriorities).
	// FanartTV should be called first.
	if len(callOrder) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(callOrder))
	}
	if callOrder[0] != NameFanartTV {
		t.Errorf("expected FanartTV called first (default priority), got %s", callOrder[0])
	}
	if callOrder[1] != NameAudioDB {
		t.Errorf("expected AudioDB called second (default priority), got %s", callOrder[1])
	}

	// Images should appear in default priority order.
	if len(result.Images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(result.Images))
	}
	if result.Images[0].Source != "fanarttv" {
		t.Errorf("expected first image from fanarttv, got %s", result.Images[0].Source)
	}
	if result.Images[1].Source != "audiodb" {
		t.Errorf("expected second image from audiodb, got %s", result.Images[1].Source)
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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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
	orch := NewOrchestrator(registry, settings, logger, nil)

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

func TestScrubSensitiveParams(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "api_key in fanart.tv URL",
			input: "Get \"https://webservice.fanart.tv/v3/music/abc?api_key=SECRET123\": context deadline exceeded",
			want:  "Get \"https://webservice.fanart.tv/v3/music/abc?api_key=REDACTED\": context deadline exceeded",
		},
		{
			name:  "apikey without underscore",
			input: "Get \"https://example.com/api?apikey=MYSECRET&format=json\": connection refused",
			want:  "Get \"https://example.com/api?apikey=REDACTED&format=json\": connection refused",
		},
		{
			name:  "token parameter",
			input: "request failed: https://api.example.com/data?token=ABC123XYZ",
			want:  "request failed: https://api.example.com/data?token=REDACTED",
		},
		{
			name:  "multiple sensitive params",
			input: "url?api_key=SECRET&format=json&token=ABC",
			want:  "url?api_key=REDACTED&format=json&token=REDACTED",
		},
		{
			name:  "no sensitive params",
			input: "connection refused",
			want:  "connection refused",
		},
		{
			name:  "non-sensitive query params left intact",
			input: "https://example.com/api?id=123&format=json: timeout",
			want:  "https://example.com/api?id=123&format=json: timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scrubSensitiveParams(tt.input)
			if got != tt.want {
				t.Errorf("scrubSensitiveParams(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestOrchestratorPopulatedFieldsTracking verifies that PopulatedFields
// distinguishes "queried with data" from "queried with empty result". This
// signal is the gate that prevents the refresh merge from wiping pre-existing
// values when a localized provider lookup returns nothing (#952 graceful
// fallback).
func TestOrchestratorPopulatedFieldsTracking(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	// MusicBrainz returns a name and genres but no biography (genres populated;
	// biography queried but excluded by IsExcludedForField anyway, see fallback).
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:   "Test Artist",
				Genres: []string{"rock"},
			}, nil
		},
	})
	// AudioDB succeeds at GetArtist but returns no biography, no styles, no
	// moods. The orchestrator queries it (so styles/moods are attempted) but
	// nothing comes back (so they must NOT appear in PopulatedFields).
	if err := settings.SetAPIKey(context.Background(), NameAudioDB, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "Test Artist"}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchMetadata(context.Background(), "mbid-1234", "Test Artist", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	contains := func(haystack []string, needle string) bool {
		for _, s := range haystack {
			if s == needle {
				return true
			}
		}
		return false
	}

	// "genres" must be both attempted AND populated (MusicBrainz returned data).
	if !contains(result.AttemptedFields, "genres") {
		t.Errorf("expected 'genres' in AttemptedFields, got %v", result.AttemptedFields)
	}
	if !contains(result.PopulatedFields, "genres") {
		t.Errorf("expected 'genres' in PopulatedFields (MusicBrainz returned data), got %v", result.PopulatedFields)
	}

	// "styles" / "moods" must be attempted (AudioDB was queried) but NOT
	// populated (AudioDB returned an empty struct). This is the bug fix: the
	// merge layer uses the populated set to decide whether to overwrite, so
	// excluding these fields from PopulatedFields preserves any pre-existing
	// styles/moods on the artist record.
	for _, field := range []string{"styles", "moods"} {
		if !contains(result.AttemptedFields, field) {
			t.Errorf("expected %q in AttemptedFields (provider was queried), got %v", field, result.AttemptedFields)
		}
		if contains(result.PopulatedFields, field) {
			t.Errorf("expected %q NOT in PopulatedFields (no data returned), got %v", field, result.PopulatedFields)
		}
	}
}

// TestOrchestratorPopulatedFields_DedupAcrossProviders verifies that an
// aggregated field (genres/styles/moods) populated by multiple providers in
// the same priority iteration appears in PopulatedFields exactly once. The
// per-iteration scope guards against a future regression that would replace
// the bool with an append (which would emit duplicate entries) -- the
// downstream merge layer treats PopulatedFields as a set, so duplicates
// would still work today, but the contract is single-entry.
func TestOrchestratorPopulatedFields_DedupAcrossProviders(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

	if err := settings.SetAPIKey(context.Background(), NameLastFM, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	// Both providers return genres for the same field; the genres aggregator
	// in applyField should be invoked twice but the field should be recorded
	// in PopulatedFields exactly once.
	registry.Register(&mockProvider{
		name: NameMusicBrainz,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "X", Genres: []string{"rock"}}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameLastFM,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "X", Genres: []string{"alternative"}}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, nil)

	result, err := orch.FetchMetadata(context.Background(), "mbid-1234", "X", nil)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	count := 0
	for _, f := range result.PopulatedFields {
		if f == "genres" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 'genres' entry in PopulatedFields, got %d (full list: %v)", count, result.PopulatedFields)
	}
	// Sanity: aggregation actually happened.
	if len(result.Metadata.Genres) < 2 {
		t.Errorf("expected genres aggregated across both providers, got %v", result.Metadata.Genres)
	}
}

// TestEnrichProviderIDs_ExtractsWikidataID verifies that a Wikidata URL
// relation returned by MusicBrainz is extracted into providerIDs so that
// subsequent Wikidata calls can look up the entity by its QID directly
// (required when the Wikidata entity lacks a P434 cross-link).
func TestEnrichProviderIDs_ExtractsWikidataID(t *testing.T) {
	providerIDs := make(map[ProviderName]string)
	meta := &ArtistMetadata{
		URLs: map[string]string{
			"wikidata": "https://www.wikidata.org/wiki/Q175044",
		},
	}

	EnrichProviderIDs(meta, providerIDs)

	if got := providerIDs[NameWikidata]; got != "Q175044" {
		t.Errorf("providerIDs[NameWikidata] = %q, want %q", got, "Q175044")
	}
}

// TestEnrichProviderIDs_IgnoresMalformedWikidataURL verifies that URLs that
// do not end in a Q-item ID (e.g. Special:Random pages) do not populate the
// Wikidata provider ID.
func TestEnrichProviderIDs_IgnoresMalformedWikidataURL(t *testing.T) {
	providerIDs := make(map[ProviderName]string)
	meta := &ArtistMetadata{
		URLs: map[string]string{
			"wikidata": "https://www.wikidata.org/wiki/Special:Random",
		},
	}

	EnrichProviderIDs(meta, providerIDs)

	if got, ok := providerIDs[NameWikidata]; ok && got != "" {
		t.Errorf("providerIDs[NameWikidata] = %q, want unset/empty", got)
	}
}

// TestEnrichProviderIDs_PreservesExistingWikidataID verifies that a pre-existing
// Wikidata ID in providerIDs is not overwritten by URL extraction, matching
// the first-write-wins behavior used for the other providers.
func TestEnrichProviderIDs_PreservesExistingWikidataID(t *testing.T) {
	providerIDs := map[ProviderName]string{
		NameWikidata: "Q999999",
	}
	meta := &ArtistMetadata{
		URLs: map[string]string{
			"wikidata": "https://www.wikidata.org/wiki/Q175044",
		},
	}

	EnrichProviderIDs(meta, providerIDs)

	if got := providerIDs[NameWikidata]; got != "Q999999" {
		t.Errorf("providerIDs[NameWikidata] = %q, want pre-existing %q", got, "Q999999")
	}
}

// TestExtractProviderIDsFromURLs_RejectsInvalidQID verifies that the Wikidata
// URL parser requires a well-formed Q-item identifier (Q followed by digits).
// Addresses the CodeRabbit review finding on PR #1177: before this hardening,
// a URL like /wiki/Qabc or /wiki/Qspecial:Random would accept "Qabc" or
// "Qspecial:Random" as a "QID" and propagate it into providerIDs, which would
// then drive a failed direct-entity SPARQL and mask the MBID fallback path.
func TestExtractProviderIDsFromURLs_RejectsInvalidQID(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"alpha chars", "https://www.wikidata.org/wiki/Qabc"},
		{"mixed alphanum", "https://www.wikidata.org/wiki/Q12a34"},
		{"colon suffix", "https://www.wikidata.org/wiki/Q123:suffix"},
		{"lowercase q", "https://www.wikidata.org/wiki/q12345"},
		{"just Q", "https://www.wikidata.org/wiki/Q"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := &ArtistMetadata{
				URLs: map[string]string{"wikidata": tc.url},
			}
			ExtractProviderIDsFromURLs(meta)
			if meta.WikidataID != "" {
				t.Errorf("URL %q produced WikidataID=%q, want empty (invalid QID must be rejected)",
					tc.url, meta.WikidataID)
			}
		})
	}
}

// TestExtractProviderIDsFromURLs_AcceptsValidQID verifies that the Wikidata
// URL parser still accepts well-formed Q-item identifiers after the
// validation tightening.
func TestExtractProviderIDsFromURLs_AcceptsValidQID(t *testing.T) {
	meta := &ArtistMetadata{
		URLs: map[string]string{"wikidata": "https://www.wikidata.org/wiki/Q175044"},
	}
	ExtractProviderIDsFromURLs(meta)
	if meta.WikidataID != "Q175044" {
		t.Errorf("WikidataID = %q, want Q175044", meta.WikidataID)
	}
}

func TestParseDiscogsURL(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID string
		wantOK bool
	}{
		{
			name:   "plain numeric segment",
			input:  "https://www.discogs.com/artist/24941",
			wantID: "24941",
			wantOK: true,
		},
		{
			name:   "slugged segment extracts numeric prefix",
			input:  "https://www.discogs.com/artist/24941-a-ha",
			wantID: "24941",
			wantOK: true,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:   "no slash in URL",
			input:  "discogs24941",
			wantOK: false,
		},
		{
			name:   "trailing slash yields empty segment",
			input:  "https://www.discogs.com/artist/",
			wantOK: false,
		},
		{
			name:   "non-numeric last segment",
			input:  "https://www.discogs.com/artist/artist-name",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := parseDiscogsURL(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
		})
	}
}

func TestParseWikidataURL(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID string
		wantOK bool
	}{
		{
			name:   "well-formed Q-item",
			input:  "https://www.wikidata.org/wiki/Q44190",
			wantID: "Q44190",
			wantOK: true,
		},
		{
			name:   "Q-item with query string",
			input:  "https://www.wikidata.org/wiki/Q44190?uselang=en",
			wantID: "Q44190",
			wantOK: true,
		},
		{
			name:   "Q-item with fragment",
			input:  "https://www.wikidata.org/wiki/Q44190#sitelinks",
			wantID: "Q44190",
			wantOK: true,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:   "wrong domain but valid path",
			input:  "https://example.com/wiki/Q44190",
			wantID: "Q44190",
			wantOK: true,
		},
		{
			name:   "malformed QID with letters",
			input:  "https://www.wikidata.org/wiki/Qabc",
			wantOK: false,
		},
		{
			name:   "bare Q without digits",
			input:  "https://www.wikidata.org/wiki/Q",
			wantOK: false,
		},
		{
			name:   "truncated path with no slash",
			input:  "Q44190",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := parseWikidataURL(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
		})
	}
}

func TestParseDeezerURL(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID string
		wantOK bool
	}{
		{
			name:   "numeric artist segment",
			input:  "https://www.deezer.com/artist/3106",
			wantID: "3106",
			wantOK: true,
		},
		{
			name:   "numeric segment with trailing non-digit",
			input:  "https://www.deezer.com/artist/3106-artist",
			wantID: "3106",
			wantOK: true,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:   "no slash",
			input:  "3106",
			wantOK: false,
		},
		{
			name:   "trailing slash yields empty segment",
			input:  "https://www.deezer.com/artist/",
			wantOK: false,
		},
		{
			name:   "non-numeric segment",
			input:  "https://www.deezer.com/artist/name",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := parseDeezerURL(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
		})
	}
}

func TestParseAllMusicURL(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID string
		wantOK bool
	}{
		{
			name:   "plain mn-ID segment",
			input:  "https://www.allmusic.com/artist/mn0000505828",
			wantID: "mn0000505828",
			wantOK: true,
		},
		{
			name:   "slugged segment with mn-ID suffix",
			input:  "https://www.allmusic.com/artist/dolly-parton-mn0000205560",
			wantID: "mn0000205560",
			wantOK: true,
		},
		{
			name:   "query string is stripped before parsing",
			input:  "https://www.allmusic.com/artist/mn0000505828?tab=biography",
			wantID: "mn0000505828",
			wantOK: true,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:   "slug containing mn but no valid ID",
			input:  "https://www.allmusic.com/artist/amnesia-band",
			wantOK: false,
		},
		{
			name:   "truncated path with no slash",
			input:  "mn0000505828",
			wantOK: false,
		},
		{
			name:   "mn prefix with wrong digit count",
			input:  "https://www.allmusic.com/artist/mn00005",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := parseAllMusicURL(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
		})
	}
}

func TestParseSpotifyURL(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID string
		wantOK bool
	}{
		{
			name:   "clean artist URL",
			input:  "https://open.spotify.com/artist/4Z8W4fKeB5YxbusRsdQVPb",
			wantID: "4Z8W4fKeB5YxbusRsdQVPb",
			wantOK: true,
		},
		{
			name:   "trailing slash is stripped",
			input:  "https://open.spotify.com/artist/4Z8W4fKeB5YxbusRsdQVPb/",
			wantID: "4Z8W4fKeB5YxbusRsdQVPb",
			wantOK: true,
		},
		{
			name:   "query param is stripped",
			input:  "https://open.spotify.com/artist/4Z8W4fKeB5YxbusRsdQVPb?si=abc123",
			wantID: "4Z8W4fKeB5YxbusRsdQVPb",
			wantOK: true,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:   "URL without /artist/ segment",
			input:  "https://open.spotify.com/album/4Z8W4fKeB5YxbusRsdQVPb",
			wantOK: false,
		},
		{
			name:   "ID too short",
			input:  "https://open.spotify.com/artist/tooshort",
			wantOK: false,
		},
		{
			name:   "ID contains invalid characters",
			input:  "https://open.spotify.com/artist/not-a-valid-spotify!!",
			wantOK: false,
		},
		{
			name:   "no slash in input",
			input:  "4Z8W4fKeB5YxbusRsdQVPb",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := parseSpotifyURL(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
		})
	}
}

// TestIsExcludedForField_BiographyStructuralGuards pins the orchestrator-side
// belt-and-suspenders defense for #1029: MusicBrainz and Wikidata are
// structurally incapable of returning biography (neither's mapArtist populates
// the field), so they MUST be excluded even if a user explicitly puts them in
// the biography priority list via the Settings UI. A future refactor that
// drops either name from fieldProviderExclusions reintroduces the wasted
// fetch + misleading "attempted" telemetry symptoms.
func TestIsExcludedForField_BiographyStructuralGuards(t *testing.T) {
	cases := []struct {
		field string
		prov  ProviderName
		want  bool
	}{
		{"biography", NameMusicBrainz, true},
		{"biography", NameWikidata, true},
		{"biography", NameWikipedia, false},
		{"biography", NameLastFM, false},
		{"biography", NameAudioDB, false},
		{"biography", NameDiscogs, false},
		{"biography", NameGenius, false},
		{"members", NameWikidata, false},
		{"genres", NameMusicBrainz, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.prov)+"/"+tc.field, func(t *testing.T) {
			if got := IsExcludedForField(tc.field, tc.prov); got != tc.want {
				t.Errorf("IsExcludedForField(%q, %q) = %v, want %v", tc.field, tc.prov, got, tc.want)
			}
		})
	}
}

// TestSynthesizeYearsActive covers the synthesis helper added for #1069.
// The helper is shared between the MusicBrainz full-refresh path and the
// per-field fetch pipeline (extractFieldForComparison) so both return
// consistent values.
func TestSynthesizeYearsActive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		meta    *ArtistMetadata
		want    string // expected synthesized value
		wantOK  bool   // true when synthesis should succeed
		comment string // why this outcome is expected
	}{
		{
			// Groups with both formed and disbanded produce a closed "YYYY-YYYY" range.
			name:    "group_formed_and_disbanded",
			meta:    &ArtistMetadata{Type: "group", Formed: "1985-01-01", Disbanded: "2003-06-15"},
			want:    "1985-2003",
			wantOK:  true,
			comment: "MB group with formed+disbanded -> closed range",
		},
		{
			// Groups with only a formed date are assumed still active.
			name:    "group_formed_only",
			meta:    &ArtistMetadata{Type: "group", Formed: "1997"},
			want:    "1997-present",
			wantOK:  true,
			comment: "MB group with formed only -> open-ended range",
		},
		{
			// Orchestra type also uses the group synthesis path.
			name:    "orchestra_formed_and_disbanded",
			meta:    &ArtistMetadata{Type: "orchestra", Formed: "1900", Disbanded: "1950"},
			want:    "1900-1950",
			wantOK:  true,
			comment: "orchestra treated as group type",
		},
		{
			// Individuals with both born and died produce a bounded range.
			name:    "individual_born_and_died",
			meta:    &ArtistMetadata{Type: "person", Born: "1942-08-01", Died: "2018-03-14"},
			want:    "1942-2018",
			wantOK:  true,
			comment: "individual with born+died -> bounded range",
		},
		{
			// A living individual with only a birth date: we cannot know whether
			// they are still active, so synthesis is deliberately skipped rather
			// than guessing "YYYY-present". See issue #1069 acceptance criteria
			// and the SynthesizeYearsActive doc comment.
			name:    "individual_born_only_no_synthesis",
			meta:    &ArtistMetadata{Type: "person", Born: "1959-10-03"},
			want:    "",
			wantOK:  false,
			comment: "individual with born only must NOT synthesize (ambiguous activity status)",
		},
		{
			// A group with no formed date cannot produce any range.
			name:    "group_no_formed_date",
			meta:    &ArtistMetadata{Type: "group", Disbanded: "2010"},
			want:    "",
			wantOK:  false,
			comment: "group without formed date cannot synthesize",
		},
		{
			// A type that is not a recognized group type and has no born+died.
			name:    "unknown_type_no_dates",
			meta:    &ArtistMetadata{Type: ""},
			want:    "",
			wantOK:  false,
			comment: "empty type with no dates produces nothing",
		},
		{
			// Nil metadata must not panic.
			name:    "nil_metadata",
			meta:    nil,
			want:    "",
			wantOK:  false,
			comment: "nil metadata must return (empty, false) safely",
		},
		{
			// Partial dates: only year portion is extracted ("YYYY-MM-DD" -> "YYYY").
			name:    "group_partial_dates_year_extracted",
			meta:    &ArtistMetadata{Type: "group", Formed: "1990-03-15", Disbanded: "2000-12-01"},
			want:    "1990-2000",
			wantOK:  true,
			comment: "year-only extraction from full dates",
		},
		{
			// Malformed date "late 1980s": yearFromDate validates that the leading
			// 4 characters are all ASCII digits; "late" is not, so no synthesis.
			// This is Finding 7: prevents garbage output like "late-present".
			name:    "malformed_date_late_1980s",
			meta:    &ArtistMetadata{Type: "group", Formed: "late 1980s"},
			want:    "",
			wantOK:  false,
			comment: "malformed date 'late 1980s' must not synthesize (digit check)",
		},
		{
			// Malformed date "circa 1990": same digit-validation path.
			name:    "malformed_date_circa_1990",
			meta:    &ArtistMetadata{Type: "group", Formed: "circa 1990"},
			want:    "",
			wantOK:  false,
			comment: "malformed date 'circa 1990' must not synthesize (digit check)",
		},
		{
			// Malformed date "19XX": placeholder digits fail the all-digit check
			// because 'X' is not an ASCII digit.
			name:    "malformed_date_19XX",
			meta:    &ArtistMetadata{Type: "group", Formed: "19XX"},
			want:    "",
			wantOK:  false,
			comment: "malformed date '19XX' must not synthesize (non-digit chars)",
		},
	}

	for _, tc := range cases {
		tc := tc // capture for parallel sub-test
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := SynthesizeYearsActive(tc.meta)
			if ok != tc.wantOK {
				t.Errorf("%s: ok = %v, want %v", tc.comment, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("%s: value = %q, want %q", tc.comment, got, tc.want)
			}
		})
	}
}

// TestExtractFieldForComparison_YearsActiveSynthesis verifies that the per-field
// fetch pipeline synthesizes years_active from formed/disbanded or born/died
// when a provider returns metadata without a literal years_active value.
// This is the core fix for #1069: providers like MusicBrainz that compute the
// value at full-refresh time must also produce a candidate in the per-field path.
func TestExtractFieldForComparison_YearsActiveSynthesis(t *testing.T) {
	t.Parallel()

	t.Run("literal_years_active_used_as_is", func(t *testing.T) {
		// When the provider populates years_active directly, it must be returned
		// unchanged and must NOT be marked synthesized.
		fpr := &FieldProviderResult{}
		extractFieldForComparison(fpr, "years_active", &ArtistMetadata{
			YearsActive: "2000-2010",
		})
		if !fpr.HasData {
			t.Error("expected HasData = true for literal years_active")
		}
		if fpr.Value != "2000-2010" {
			t.Errorf("Value = %q, want %q", fpr.Value, "2000-2010")
		}
		if fpr.Synthesized {
			t.Error("expected Synthesized = false for literal years_active")
		}
	})

	t.Run("group_synthesized_from_formed_disbanded", func(t *testing.T) {
		// A MusicBrainz result for a group has formed+disbanded but no literal
		// years_active. The per-field pipeline must synthesize a candidate.
		fpr := &FieldProviderResult{}
		extractFieldForComparison(fpr, "years_active", &ArtistMetadata{
			Type:      "group",
			Formed:    "1985",
			Disbanded: "2003",
		})
		if !fpr.HasData {
			t.Error("expected HasData = true for group synthesis")
		}
		if fpr.Value != "1985-2003" {
			t.Errorf("Value = %q, want %q", fpr.Value, "1985-2003")
		}
		if !fpr.Synthesized {
			t.Error("expected Synthesized = true when value derived from formed/disbanded")
		}
	})

	t.Run("wikipedia_infobox_missing_years_active_synthesized_from_formed", func(t *testing.T) {
		// Wikipedia infoboxes often lack a literal years_active key but may have
		// formed date. This simulates a Wikipedia result for a still-active group.
		fpr := &FieldProviderResult{}
		extractFieldForComparison(fpr, "years_active", &ArtistMetadata{
			Type:   "group",
			Formed: "1997",
			// No Disbanded, no YearsActive -- infobox only had 'formed'.
		})
		if !fpr.HasData {
			t.Error("expected HasData = true for Wikipedia synthesis from formed only")
		}
		if fpr.Value != "1997-present" {
			t.Errorf("Value = %q, want %q", fpr.Value, "1997-present")
		}
		if !fpr.Synthesized {
			t.Error("expected Synthesized = true")
		}
	})

	t.Run("individual_born_only_no_synthesis", func(t *testing.T) {
		// A solo artist (individual type) where only birth year is known.
		// Synthesis must be skipped to avoid a spurious "YYYY-present" guess.
		fpr := &FieldProviderResult{}
		extractFieldForComparison(fpr, "years_active", &ArtistMetadata{
			Type: "person",
			Born: "1959-10-03",
		})
		if fpr.HasData {
			t.Error("expected HasData = false for individual with born only")
		}
		if fpr.Value != "" {
			t.Errorf("expected empty Value for individual born-only case, got %q", fpr.Value)
		}
	})

	t.Run("empty_metadata_no_synthesis", func(t *testing.T) {
		// No dates at all: nothing to synthesize.
		fpr := &FieldProviderResult{}
		extractFieldForComparison(fpr, "years_active", &ArtistMetadata{})
		if fpr.HasData {
			t.Error("expected HasData = false when no date fields present")
		}
	})
}

// TestFetchMetadata_MembersAuthoritative verifies that the orchestrator propagates
// MembersAuthoritative from a provider's ArtistMetadata to the FetchResult, and
// that it correctly guards AttemptedFields based on the authoritative flag.
// This is the core behavior added for #1038.
func TestFetchMetadata_MembersAuthoritative(t *testing.T) {
	t.Parallel()

	t.Run("sparse_empty_does_not_mark_members_attempted", func(t *testing.T) {
		// A provider returning empty members without the authoritative flag
		// (e.g. MusicBrainz sparse relation data) must NOT add "members" to
		// AttemptedFields. Downstream consumers must be able to preserve existing
		// member rows rather than clearing them.
		registry, settings := setupOrchestratorTest(t)
		registry.Register(&mockProvider{
			name: NameMusicBrainz,
			getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
				return &ArtistMetadata{
					Name:                 "Sparse Band",
					Members:              nil,   // no members returned
					MembersAuthoritative: false, // sparse data
				}, nil
			},
		})
		if err := settings.SetPriority(context.Background(), "members", []ProviderName{NameMusicBrainz}); err != nil {
			t.Fatalf("SetPriority: %v", err)
		}

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		orch := NewOrchestrator(registry, settings, logger, nil)
		result, err := orch.FetchMetadata(context.Background(), "mb-sparse", "Sparse Band", nil)
		if err != nil {
			t.Fatalf("FetchMetadata: %v", err)
		}

		for _, f := range result.AttemptedFields {
			if f == "members" {
				t.Error("members must NOT be in AttemptedFields when provider returned sparse-empty (non-authoritative)")
				break
			}
		}
		if result.MembersAuthoritative {
			t.Error("MembersAuthoritative must be false when provider is non-authoritative")
		}
	})

	t.Run("authoritative_empty_marks_members_attempted", func(t *testing.T) {
		// A provider that asserts an empty roster authoritatively (e.g. the
		// artist was re-identified as a solo act) must add "members" to
		// AttemptedFields and set MembersAuthoritative=true so applyMemberRefresh
		// can clear stale rows.
		registry, settings := setupOrchestratorTest(t)
		registry.Register(&mockProvider{
			name: NameMusicBrainz,
			getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
				return &ArtistMetadata{
					Name:                 "Solo Artist",
					Members:              nil,  // empty roster
					MembersAuthoritative: true, // this empty is authoritative
				}, nil
			},
		})
		if err := settings.SetPriority(context.Background(), "members", []ProviderName{NameMusicBrainz}); err != nil {
			t.Fatalf("SetPriority: %v", err)
		}

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		orch := NewOrchestrator(registry, settings, logger, nil)
		result, err := orch.FetchMetadata(context.Background(), "mb-solo", "Solo Artist", nil)
		if err != nil {
			t.Fatalf("FetchMetadata: %v", err)
		}

		found := false
		for _, f := range result.AttemptedFields {
			if f == "members" {
				found = true
				break
			}
		}
		if !found {
			t.Error("members must be in AttemptedFields when provider asserted authoritative-empty")
		}
		if !result.MembersAuthoritative {
			t.Error("MembersAuthoritative must be true when provider asserted authoritative-empty")
		}
	})

	t.Run("non_empty_members_marks_attempted", func(t *testing.T) {
		// A provider returning a real member list always marks the field as
		// attempted regardless of the authoritative flag.
		registry, settings := setupOrchestratorTest(t)
		registry.Register(&mockProvider{
			name: NameMusicBrainz,
			getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
				return &ArtistMetadata{
					Name:    "Real Band",
					Members: []MemberInfo{{Name: "Alice"}, {Name: "Bob"}},
				}, nil
			},
		})
		if err := settings.SetPriority(context.Background(), "members", []ProviderName{NameMusicBrainz}); err != nil {
			t.Fatalf("SetPriority: %v", err)
		}

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		orch := NewOrchestrator(registry, settings, logger, nil)
		result, err := orch.FetchMetadata(context.Background(), "mb-band", "Real Band", nil)
		if err != nil {
			t.Fatalf("FetchMetadata: %v", err)
		}

		found := false
		for _, f := range result.AttemptedFields {
			if f == "members" {
				found = true
				break
			}
		}
		if !found {
			t.Error("members must be in AttemptedFields when provider returned real member data")
		}
		if len(result.Metadata.Members) != 2 {
			t.Errorf("expected 2 members, got %d", len(result.Metadata.Members))
		}
	})

	t.Run("provider_error_does_not_mark_members_attempted", func(t *testing.T) {
		// Finding 6: a provider that returns a hard error (timeout, 5xx, etc.)
		// must NOT mark "members" as attempted and must leave MembersAuthoritative
		// false. The orchestrator skips the entire provider on pr.err != nil
		// before reaching the members guard, so the field should not appear in
		// AttemptedFields. Existing member rows must be preserved.
		registry, settings := setupOrchestratorTest(t)
		registry.Register(&mockProvider{
			name: NameMusicBrainz,
			getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
				// Simulate a transient provider failure (timeout, 5xx, etc.).
				return nil, fmt.Errorf("connection refused")
			},
		})
		if err := settings.SetPriority(context.Background(), "members", []ProviderName{NameMusicBrainz}); err != nil {
			t.Fatalf("SetPriority: %v", err)
		}

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		orch := NewOrchestrator(registry, settings, logger, nil)
		result, err := orch.FetchMetadata(context.Background(), "mb-error", "Error Artist", nil)
		if err != nil {
			t.Fatalf("FetchMetadata: %v", err)
		}

		// "members" must NOT appear in AttemptedFields after a provider error.
		for _, f := range result.AttemptedFields {
			if f == "members" {
				t.Errorf("members must NOT be in AttemptedFields when provider errored, got %v", result.AttemptedFields)
				break
			}
		}
		// MembersAuthoritative must be false (provider did not contribute).
		if result.MembersAuthoritative {
			t.Error("MembersAuthoritative must be false when provider returned an error")
		}
	})
}

// --- AIMD signal tests --------------------------------------------------------

// newTestOrchWithAIMD builds a minimal test Orchestrator with a real
// AIMDController so we can observe signal counts. Returns the orchestrator,
// its AIMD controller, the registry, and the settings service.
func newTestOrchWithAIMD(t *testing.T) (*Orchestrator, *AIMDController, *Registry, *SettingsService) {
	t.Helper()
	registry, settings := setupOrchestratorTest(t)
	clk := newFakeClock(time.Now())
	rlm := NewRateLimiterMap()
	ctrl := NewAIMDController(rlm, clk)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger, ctrl)
	return orch, ctrl, registry, settings
}

// aimdLastDecrease reads the lastDecrease timestamp for a provider from AIMD
// state. Returns zero if the provider state has not been initialized yet.
func aimdLastDecrease(ctrl *AIMDController, name ProviderName) time.Time {
	ctrl.mu.RLock()
	defer ctrl.mu.RUnlock()
	if s, ok := ctrl.states[name]; ok {
		return s.lastDecrease
	}
	return time.Time{}
}

// aimdSuccessCount reads the current successCount for a provider.
func aimdSuccessCount(ctrl *AIMDController, name ProviderName) int {
	ctrl.mu.RLock()
	defer ctrl.mu.RUnlock()
	if s, ok := ctrl.states[name]; ok {
		return s.successCount
	}
	return 0
}

// TestAIMDOrdinaryErrorDoesNotTriggerRecordFailure verifies that a non-rate-limit
// error (e.g. a plain fmt.Errorf, simulating a JSON parse or auth failure) from
// GetArtist does NOT drive a RecordFailure signal. Only *ErrProviderUnavailable
// must trigger the AIMD decrease path.
func TestAIMDOrdinaryErrorDoesNotTriggerRecordFailure(t *testing.T) {
	t.Parallel()
	orch, ctrl, registry, settings := newTestOrchWithAIMD(t)

	const prov = NameMusicBrainz
	registry.Register(&mockProvider{
		name: prov,
		// Return an ordinary error (not *ErrProviderUnavailable).
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return nil, fmt.Errorf("simulated JSON parse error")
		},
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return nil, fmt.Errorf("simulated auth error")
		},
	})

	if err := settings.SetAPIKey(context.Background(), prov, ""); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	cache := make(map[ProviderName]*providerResult)
	var mu sync.Mutex
	_ = orch.getProviderResult(context.Background(), prov, "mbid-test", "Artist Name", nil, cache, &mu)

	// lastDecrease must remain zero -- RecordFailure must NOT have been called.
	if !aimdLastDecrease(ctrl, prov).IsZero() {
		t.Fatalf("ordinary error incorrectly triggered RecordFailure; lastDecrease is non-zero")
	}
	// successCount must also be zero -- RecordSuccess must NOT have been called.
	if aimdSuccessCount(ctrl, prov) != 0 {
		t.Fatalf("ordinary error incorrectly triggered RecordSuccess; successCount=%d", aimdSuccessCount(ctrl, prov))
	}
}

// TestAIMDRateLimitErrorTriggerRecordFailure verifies that an
// *ErrProviderUnavailable from GetArtist DOES drive RecordFailure.
func TestAIMDRateLimitErrorTriggerRecordFailure(t *testing.T) {
	t.Parallel()
	orch, ctrl, registry, _ := newTestOrchWithAIMD(t)

	const prov = NameMusicBrainz
	registry.Register(&mockProvider{
		name: prov,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return nil, &ErrProviderUnavailable{
				Provider:   prov,
				Cause:      fmt.Errorf("429 Too Many Requests"),
				RetryAfter: 5 * time.Second,
			}
		},
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return nil, nil
		},
	})

	cache := make(map[ProviderName]*providerResult)
	var mu sync.Mutex
	_ = orch.getProviderResult(context.Background(), prov, "mbid-test", "Artist Name", nil, cache, &mu)

	// lastDecrease must be non-zero -- RecordFailure must have been called.
	if aimdLastDecrease(ctrl, prov).IsZero() {
		t.Fatalf("rate-limit error did not trigger RecordFailure; lastDecrease is still zero")
	}
}

// TestAIMDSingleSignalPerProviderCall verifies that getProviderResult emits
// exactly ONE AIMD signal (not two) even when both GetArtist and GetImages
// succeed. Emitting two RecordSuccess calls would halve the effective
// aimdSuccessThreshold and allow a fail+success to cancel out.
func TestAIMDSingleSignalPerProviderCall(t *testing.T) {
	t.Parallel()
	orch, ctrl, registry, _ := newTestOrchWithAIMD(t)

	const prov = NameFanartTV
	registry.Register(&mockProvider{
		name: prov,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{Name: "Artist"}, nil
		},
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return []ImageResult{{URL: "http://example.com/img.jpg"}}, nil
		},
	})

	// Drive one full getProviderResult call.
	cache := make(map[ProviderName]*providerResult)
	var mu sync.Mutex
	_ = orch.getProviderResult(context.Background(), prov, "mbid-test", "Artist Name", nil, cache, &mu)

	// successCount must be exactly 1 (one RecordSuccess fired, not two).
	got := aimdSuccessCount(ctrl, prov)
	if got != 1 {
		t.Fatalf("expected exactly 1 AIMD success signal, got %d", got)
	}
}

// TestAIMDFetchImagesRateLimitSignal verifies that FetchImages sends
// RecordFailure on *ErrProviderUnavailable and RecordSuccess on a normal result.
func TestAIMDFetchImagesRateLimitSignal(t *testing.T) {
	t.Parallel()
	orch, ctrl, registry, settings := newTestOrchWithAIMD(t)

	// FanartTV requires an API key; store a dummy so it passes availability check.
	const prov = NameFanartTV
	if err := settings.SetAPIKey(context.Background(), prov, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	registry.Register(&mockProvider{
		name: prov,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return nil, &ErrProviderUnavailable{
				Provider:   prov,
				Cause:      fmt.Errorf("429"),
				RetryAfter: time.Second,
			}
		},
	})

	_, _ = orch.FetchImages(context.Background(), "mbid-test", nil)

	if aimdLastDecrease(ctrl, prov).IsZero() {
		t.Fatalf("FetchImages: rate-limit error did not trigger RecordFailure")
	}
}

// TestAIMDFetchImagesOrdinaryErrorNoSignal verifies that FetchImages does NOT
// send RecordFailure for a non-rate-limit error.
func TestAIMDFetchImagesOrdinaryErrorNoSignal(t *testing.T) {
	t.Parallel()
	orch, ctrl, registry, settings := newTestOrchWithAIMD(t)

	// FanartTV requires an API key; store a dummy so it passes availability check.
	const prov = NameFanartTV
	if err := settings.SetAPIKey(context.Background(), prov, "test-key"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	registry.Register(&mockProvider{
		name: prov,
		getImgFn: func(_ context.Context, _ string) ([]ImageResult, error) {
			return nil, fmt.Errorf("generic error")
		},
	})

	_, _ = orch.FetchImages(context.Background(), "mbid-test", nil)

	if !aimdLastDecrease(ctrl, prov).IsZero() {
		t.Fatalf("FetchImages: ordinary error incorrectly triggered RecordFailure")
	}
	if aimdSuccessCount(ctrl, prov) != 0 {
		t.Fatalf("FetchImages: ordinary error incorrectly triggered RecordSuccess")
	}
}

// TestAIMDSearchRateLimitSignal verifies that Search sends RecordFailure on
// *ErrProviderUnavailable and no signal on an ordinary error.
func TestAIMDSearchRateLimitSignal(t *testing.T) {
	t.Parallel()
	orch, ctrl, registry, settings := newTestOrchWithAIMD(t)

	const prov = NameMusicBrainz
	registry.Register(&mockProvider{
		name: prov,
		searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
			return nil, &ErrProviderUnavailable{
				Provider:   prov,
				Cause:      fmt.Errorf("503"),
				RetryAfter: time.Second,
			}
		},
	})
	// MusicBrainz does not require auth so no API key needed; mark available.
	if err := settings.SetAPIKey(context.Background(), prov, ""); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	_, _ = orch.Search(context.Background(), "Radiohead")

	if aimdLastDecrease(ctrl, prov).IsZero() {
		t.Fatalf("Search: rate-limit error did not trigger RecordFailure")
	}
}

// TestAIMDSearchForLinkingRateLimitSignal verifies that SearchForLinking sends
// RecordFailure on *ErrProviderUnavailable and RecordSuccess on success.
func TestAIMDSearchForLinkingRateLimitSignal(t *testing.T) {
	t.Parallel()
	orch, ctrl, registry, _ := newTestOrchWithAIMD(t)

	const prov = NameMusicBrainz

	t.Run("rate-limit fires RecordFailure", func(t *testing.T) {
		registry.Register(&mockProvider{
			name: prov,
			searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
				return nil, &ErrProviderUnavailable{
					Provider:   prov,
					Cause:      fmt.Errorf("429"),
					RetryAfter: time.Second,
				}
			},
		})

		_, _, _ = orch.SearchForLinking(context.Background(), "Artist", []ProviderName{prov})

		if aimdLastDecrease(ctrl, prov).IsZero() {
			t.Fatalf("SearchForLinking: rate-limit error did not trigger RecordFailure")
		}
	})

	t.Run("success fires RecordSuccess", func(t *testing.T) {
		clk := newFakeClock(time.Now())
		rlm := NewRateLimiterMap()
		ctrl2 := NewAIMDController(rlm, clk)
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		reg2 := NewRegistry()
		_, settings2 := setupOrchestratorTest(t)
		orch2 := NewOrchestrator(reg2, settings2, logger, ctrl2)

		reg2.Register(&mockProvider{
			name: prov,
			searchFn: func(_ context.Context, _ string) ([]ArtistSearchResult, error) {
				return []ArtistSearchResult{{Name: "Artist"}}, nil
			},
		})

		_, _, _ = orch2.SearchForLinking(context.Background(), "Artist", []ProviderName{prov})

		if aimdSuccessCount(ctrl2, prov) != 1 {
			t.Fatalf("SearchForLinking: expected 1 RecordSuccess signal, got %d", aimdSuccessCount(ctrl2, prov))
		}
	})
}

// TestApplyTagSliceField_VocabFilter verifies the orchestrator tag-merge path
// applies the user's vocab exclude filter and count cap (issue #1130). This is
// the orchestrator half of the dual-path integration: a refresh runs through
// the scraper-executor, but the orchestrator path must filter identically.
func TestApplyTagSliceField_VocabFilter(t *testing.T) {
	t.Run("exclude pattern drops matching tags", func(t *testing.T) {
		result := &FetchResult{
			Metadata:         &ArtistMetadata{},
			MetadataVocabCfg: &tagdict.VocabConfig{Exclude: []string{"junk*"}},
		}
		pr := &providerResult{meta: &ArtistMetadata{Genres: []string{"Rock", "junk tag", "Pop"}}}

		applyTagSliceField(result, "genres", pr, NameMusicBrainz)

		for _, g := range result.Metadata.Genres {
			if strings.Contains(strings.ToLower(g), "junk") {
				t.Fatalf("orchestrator path did not apply the vocab exclude filter: %v", result.Metadata.Genres)
			}
		}
		if len(result.Metadata.Genres) != 2 {
			t.Fatalf("expected 2 genres after exclude, got %v", result.Metadata.Genres)
		}
	})

	t.Run("count cap truncates", func(t *testing.T) {
		result := &FetchResult{
			Metadata:         &ArtistMetadata{},
			MetadataVocabCfg: &tagdict.VocabConfig{MaxGenres: 2},
		}
		pr := &providerResult{meta: &ArtistMetadata{Genres: []string{"Rock", "Pop", "Jazz", "Blues"}}}

		applyTagSliceField(result, "genres", pr, NameMusicBrainz)

		got := result.Metadata.Genres
		if len(got) != 2 || got[0] != "Rock" || got[1] != "Pop" {
			t.Fatalf("count cap should keep exactly the first two genres [Rock Pop], got %v", got)
		}
	})
}
