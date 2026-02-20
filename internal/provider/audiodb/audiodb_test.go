package audiodb

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	enc, _, _ := encryption.NewEncryptor("")
	limiter := provider.NewRateLimiterMap()
	settings := provider.NewSettingsService(db, enc)
	if err := settings.SetAPIKey(context.Background(), provider.NameAudioDB, "test-key"); err != nil {
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

// newTestServerV2 creates a test server that mimics TheAudioDB V2 API.
// It verifies that the API key is sent in the X-API-KEY header and uses
// V2-style path-based routes (/search/artist/, /lookup/artist_mb/).
func newTestServerV2(t *testing.T, capturedKey *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Capture the API key from the V2 header
		if capturedKey != nil {
			*capturedKey = r.Header.Get("X-API-KEY")
		}

		switch {
		case strings.Contains(r.URL.Path, "/search/artist/"):
			w.Write(loadFixture(t, "search_radiohead.json"))
		case strings.Contains(r.URL.Path, "/lookup/artist_mb/not-found"):
			w.Write([]byte(`{"artists":null}`))
		case strings.Contains(r.URL.Path, "/lookup/artist_mb/"):
			w.Write(loadFixture(t, "search_radiohead.json"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestServer(t *testing.T) *httptest.Server {
	return newTestServerV2(t, nil)
}

func TestSearchArtist(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	results, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %s", results[0].Name)
	}
	if results[0].ProviderID != "111239" {
		t.Errorf("expected provider ID 111239, got %s", results[0].ProviderID)
	}
}

func TestGetArtist(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	meta, err := a.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %s", meta.Name)
	}
	if meta.AudioDBID != "111239" {
		t.Errorf("expected AudioDB ID 111239, got %s", meta.AudioDBID)
	}
	if meta.Biography == "" {
		t.Error("expected non-empty biography")
	}
	if len(meta.Genres) == 0 {
		t.Error("expected genres")
	}
	if meta.Formed != "1985" {
		t.Errorf("expected formed 1985, got %s", meta.Formed)
	}
}

func TestGetArtistNotFound(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.GetArtist(context.Background(), "not-found")
	if err == nil {
		t.Fatal("expected error for not-found")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}

func TestGetImages(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
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
}

func TestAPIKeyInHeader(t *testing.T) {
	limiter, settings := setupTest(t)
	var capturedKey string
	srv := newTestServerV2(t, &capturedKey)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if capturedKey != "test-key" {
		t.Errorf("expected X-API-KEY header to be %q, got %q", "test-key", capturedKey)
	}
}

func TestRequiresAuth(t *testing.T) {
	limiter, settings := setupTest(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, "http://unused")

	if !a.RequiresAuth() {
		t.Error("expected RequiresAuth to return true")
	}
}

func TestNoKeyReturnsAuthError(t *testing.T) {
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
	limiter := provider.NewRateLimiterMap()
	settings := provider.NewSettingsService(db, enc)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := newTestServer(t)
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err = a.SearchArtist(context.Background(), "Radiohead")
	if err == nil {
		t.Fatal("expected error when no key configured")
	}
	if _, ok := err.(*provider.ErrAuthRequired); !ok {
		t.Errorf("expected ErrAuthRequired, got %T: %v", err, err)
	}
}
