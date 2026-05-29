package fanarttv

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

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
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	a.client = &http.Client{Timeout: 10 * time.Second}

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
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	a.client = &http.Client{Timeout: 10 * time.Second}

	_, err := a.GetImages(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for not-found")
	}
	var notFound *provider.ErrNotFound
	if !errors.As(err, &notFound) {
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
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	a.client = &http.Client{Timeout: 10 * time.Second}

	_, err = a.GetImages(context.Background(), "any-id")
	if err == nil {
		t.Fatal("expected error when no API key configured")
	}
	var authRequired *provider.ErrAuthRequired
	if !errors.As(err, &authRequired) {
		t.Errorf("expected ErrAuthRequired, got %T", err)
	}
}

// TestGetImagesRetriesOn429 proves that GetImages routes through
// provider.DoWithRetry and gives up after a bounded number of attempts when the
// server keeps returning HTTP 429.
//
// The handler always answers "Retry-After: 0" alongside the 429 status. A
// Retry-After of zero makes provider.DoWithRetry compute a zero backoff wait, so
// the retries fire back-to-back against the real clock without any actual sleep.
// That keeps the test fast and deterministic while still exercising the live
// retry loop. provider.DefaultRetryPolicy uses MaxAttempts=3, so the server must
// be hit exactly 3 times before the loop surfaces *ErrProviderUnavailable.
func TestGetImagesRetriesOn429(t *testing.T) {
	limiter, settings := setupTest(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		// Retry-After: 0 -> zero backoff, so DoWithRetry does not sleep.
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	a.client = &http.Client{Timeout: 10 * time.Second}

	_, err := a.GetImages(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error after exhausting retries on 429")
	}
	var unavailable *provider.ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Errorf("expected ErrProviderUnavailable, got %T: %v", err, err)
	}

	want := provider.DefaultRetryPolicy().MaxAttempts
	if got := int(hits.Load()); got != want {
		t.Errorf("expected server to be hit %d times (bounded retries), got %d", want, got)
	}
}

func TestSearchReturnsNil(t *testing.T) {
	limiter, settings := setupTest(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, "http://localhost")
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	a.client = &http.Client{Timeout: 10 * time.Second}

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
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	a.client = &http.Client{Timeout: 10 * time.Second}

	meta, err := a.GetArtist(context.Background(), "anything")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta != nil {
		t.Errorf("expected nil metadata, got %v", meta)
	}
}
