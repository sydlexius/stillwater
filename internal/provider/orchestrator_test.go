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
	name      ProviderName
	authReq   bool
	searchFn  func(ctx context.Context, name string) ([]ArtistSearchResult, error)
	getArtFn  func(ctx context.Context, id string) (*ArtistMetadata, error)
	getImgFn  func(ctx context.Context, id string) ([]ImageResult, error)
}

func (m *mockProvider) Name() ProviderName    { return m.name }
func (m *mockProvider) RequiresAuth() bool     { return m.authReq }

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

func setupOrchestratorTest(t *testing.T) (*Registry, *SettingsService) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT (datetime('now')))`)
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
				Biography: "Radiohead are an English rock band.",
				Genres:    []string{"alternative"},
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-123", "Radiohead")
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Biography should come from Last.fm (MusicBrainz returned empty)
	if result.Metadata.Biography != "Radiohead are an English rock band." {
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
				Biography: "From AudioDB",
				Formed:    "1985",
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-123", "Radiohead")
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// Should get data from AudioDB since MusicBrainz failed
	if result.Metadata.Biography != "From AudioDB" {
		t.Errorf("expected biography from AudioDB, got: %s", result.Metadata.Biography)
	}
}

func TestOrchestratorSearch(t *testing.T) {
	registry, settings := setupOrchestratorTest(t)

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
				Biography: "From MusicBrainz",
			}, nil
		},
	})
	registry.Register(&mockProvider{
		name: NameAudioDB,
		getArtFn: func(_ context.Context, _ string) (*ArtistMetadata, error) {
			return &ArtistMetadata{
				Name:      "Radiohead",
				Biography: "From AudioDB",
			}, nil
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := NewOrchestrator(registry, settings, logger)

	result, err := orch.FetchMetadata(context.Background(), "mbid-123", "Radiohead")
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	// AudioDB should win for biography due to custom priority
	if result.Metadata.Biography != "From AudioDB" {
		t.Errorf("expected biography from AudioDB (custom priority), got: %s", result.Metadata.Biography)
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
