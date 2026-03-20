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

	// Genres should come from MusicBrainz (first in priority)
	if len(result.Metadata.Genres) != 1 || result.Metadata.Genres[0] != "rock" {
		t.Errorf("expected genres from MusicBrainz, got: %v", result.Metadata.Genres)
	}

	// Check sources recorded correctly
	bioSource := findSource(result.Sources, "biography")
	if bioSource == nil || bioSource.Provider != NameLastFM {
		t.Errorf("expected biography source from lastfm, got: %v", bioSource)
	}
	genreSource := findSource(result.Sources, "genres")
	if genreSource == nil || genreSource.Provider != NameMusicBrainz {
		t.Errorf("expected genres source from musicbrainz, got: %v", genreSource)
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

func findSource(sources []FieldSource, field string) *FieldSource {
	for _, s := range sources {
		if s.Field == field {
			return &s
		}
	}
	return nil
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
			name: "all four providers",
			urls: map[string]string{
				"discogs":  "https://www.discogs.com/artist/24941-a-ha",
				"wikidata": "https://www.wikidata.org/wiki/Q44190",
				"deezer":   "https://www.deezer.com/artist/3106",
				"spotify":  "https://open.spotify.com/artist/4Z8W4fKeB5YxbusRsdQVPb",
			},
			wantDiscogsID:  "24941",
			wantWikidataID: "Q44190",
			wantDeezerID:   "3106",
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
			SpotifyID:  "0OdUWJ0sBjDrqHygGUXeCF",
			URLs: map[string]string{
				"discogs":  "https://www.discogs.com/artist/24941",
				"wikidata": "https://www.wikidata.org/wiki/Q44190",
				"deezer":   "https://www.deezer.com/artist/3106",
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
