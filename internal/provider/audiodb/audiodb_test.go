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
	if err := settings.SetAPIKey(context.Background(), provider.NameAudioDB, "test-premium-key"); err != nil {
		t.Fatalf("setting test key: %v", err)
	}
	return limiter, settings
}

func setupFreeTest(t *testing.T) (*provider.RateLimiterMap, *provider.SettingsService) {
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
	// No API key stored -- adapter should use free key 123.
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

// newTestServer creates a test server that handles both v1 and v2 path patterns.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerCapturing(t, nil, nil)
}

// newTestServerCapturing creates a test server that also records the API key header and request path.
func newTestServerCapturing(t *testing.T, capturedKey *string, capturedPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if capturedKey != nil {
			*capturedKey = r.Header.Get("X-API-KEY")
		}
		if capturedPath != nil {
			*capturedPath = r.URL.Path
		}

		switch {
		// v2 paths
		case strings.Contains(r.URL.Path, "/search/artist/"):
			// v2 search response uses the "search" top-level key
			v2Data := []byte(strings.Replace(string(loadFixture(t, "search_radiohead.json")), `"artists"`, `"search"`, 1))
			w.Write(v2Data)
		case strings.Contains(r.URL.Path, "/lookup/artist_mb/not-found"):
			w.Write([]byte(`{"lookup":null}`))
		case strings.Contains(r.URL.Path, "/lookup/artist_mb/"):
			w.Write(loadFixture(t, "lookup_radiohead.json"))
		// v1 paths
		case strings.Contains(r.URL.Path, "/search.php"):
			w.Write(loadFixture(t, "search_radiohead.json"))
		case strings.Contains(r.URL.Path, "/artist-mb.php"):
			if r.URL.Query().Get("i") == "not-found" {
				w.Write([]byte(`{"artists":null}`))
			} else {
				w.Write(loadFixture(t, "search_radiohead.json"))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
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

func TestPremiumKeyUsesV2Header(t *testing.T) {
	limiter, settings := setupTest(t)
	var capturedKey, capturedPath string
	srv := newTestServerCapturing(t, &capturedKey, &capturedPath)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if capturedKey != "test-premium-key" {
		t.Errorf("expected X-API-KEY header %q, got %q", "test-premium-key", capturedKey)
	}
	if !strings.Contains(capturedPath, "/search/artist/") {
		t.Errorf("expected v2 search path, got %q", capturedPath)
	}
}

func TestFreeKeyUsesV1URL(t *testing.T) {
	limiter, settings := setupFreeTest(t)
	var capturedKey, capturedPath string
	srv := newTestServerCapturing(t, &capturedKey, &capturedPath)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if capturedKey != "" {
		t.Errorf("expected no X-API-KEY header for free tier, got %q", capturedKey)
	}
	if !strings.Contains(capturedPath, "/search.php") {
		t.Errorf("expected v1 search.php path, got %q", capturedPath)
	}
}

func TestRequiresAuth(t *testing.T) {
	limiter, settings := setupTest(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, "http://unused")

	if a.RequiresAuth() {
		t.Error("expected RequiresAuth to return false (free tier available)")
	}
}
