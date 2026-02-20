package fanarttv

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
	_ "modernc.org/sqlite"
)

func setupTest(t *testing.T) (*provider.RateLimiterMap, *provider.SettingsService) {
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
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	limiter := provider.NewRateLimiterMap()
	settings := provider.NewSettingsService(db, enc)
	// Pre-configure a test API key
	if err := settings.SetAPIKey(context.Background(), provider.NameFanartTV, "test-key"); err != nil {
		t.Fatalf("setting test key: %v", err)
	}
	return limiter, settings
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("loading fixture %s: %v", name, err)
	}
	return data
}

func TestGetImages(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "artist_radiohead.json"))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	images, err := a.GetImages(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}

	if len(images) != 6 {
		t.Fatalf("expected 6 images, got %d", len(images))
	}

	// Check types
	typeCounts := make(map[provider.ImageType]int)
	for _, img := range images {
		typeCounts[img.Type]++
		if img.Source != string(provider.NameFanartTV) {
			t.Errorf("expected source fanarttv, got %s", img.Source)
		}
	}
	if typeCounts[provider.ImageThumb] != 2 {
		t.Errorf("expected 2 thumbs, got %d", typeCounts[provider.ImageThumb])
	}
	if typeCounts[provider.ImageFanart] != 1 {
		t.Errorf("expected 1 fanart, got %d", typeCounts[provider.ImageFanart])
	}
	if typeCounts[provider.ImageHDLogo] != 1 {
		t.Errorf("expected 1 hdlogo, got %d", typeCounts[provider.ImageHDLogo])
	}
	if typeCounts[provider.ImageLogo] != 1 {
		t.Errorf("expected 1 logo, got %d", typeCounts[provider.ImageLogo])
	}
	if typeCounts[provider.ImageBanner] != 1 {
		t.Errorf("expected 1 banner, got %d", typeCounts[provider.ImageBanner])
	}
}

func TestGetImagesNotFound(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.GetImages(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for not-found")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}

func TestGetImagesNoKey(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	defer db.Close()
	_, err = db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT (datetime('now')))`)
	if err != nil {
		t.Fatalf("creating settings table: %v", err)
	}
	enc, _, _ := encryption.NewEncryptor("")
	limiter := provider.NewRateLimiterMap()
	settings := provider.NewSettingsService(db, enc)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, "http://localhost")

	_, err = a.GetImages(context.Background(), "any-id")
	if err == nil {
		t.Fatal("expected error when no API key configured")
	}
	if _, ok := err.(*provider.ErrAuthRequired); !ok {
		t.Errorf("expected ErrAuthRequired, got %T", err)
	}
}

func TestSearchReturnsNil(t *testing.T) {
	limiter, settings := setupTest(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, "http://localhost")

	results, err := a.SearchArtist(context.Background(), "anything")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results, got %v", results)
	}
}

func TestGetArtistReturnsNil(t *testing.T) {
	limiter, settings := setupTest(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, "http://localhost")

	meta, err := a.GetArtist(context.Background(), "anything")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta != nil {
		t.Errorf("expected nil metadata, got %v", meta)
	}
}
