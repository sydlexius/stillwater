package discogs

import (
	"context"
	"database/sql"
	"errors"
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
	if err := settings.SetAPIKey(context.Background(), provider.NameDiscogs, "test-token"); err != nil {
		t.Fatalf("setting test token: %v", err)
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

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Discogs token=test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/database/search"):
			_, _ = w.Write(loadFixture(t, "search_radiohead.json"))
		case strings.HasPrefix(r.URL.Path, "/masters/5001"):
			_, _ = w.Write(loadFixture(t, "master_5001.json"))
		case strings.HasPrefix(r.URL.Path, "/masters/5002"):
			_, _ = w.Write(loadFixture(t, "master_5002.json"))
		case strings.HasPrefix(r.URL.Path, "/masters/5003"):
			_, _ = w.Write(loadFixture(t, "master_5003.json"))
		case strings.HasSuffix(r.URL.Path, "/releases"):
			_, _ = w.Write(loadFixture(t, "artist_releases.json"))
		case strings.HasPrefix(r.URL.Path, "/artists/"):
			_, _ = w.Write(loadFixture(t, "artist_radiohead.json"))
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
	if results[0].ProviderID != "3840" {
		t.Errorf("expected provider ID 3840, got %s", results[0].ProviderID)
	}
}

func TestGetArtist(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	meta, err := a.GetArtist(context.Background(), "3840")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %s", meta.Name)
	}
	if meta.DiscogsID != "3840" {
		t.Errorf("expected Discogs ID 3840, got %s", meta.DiscogsID)
	}
	if meta.Biography == "" {
		t.Error("expected non-empty biography")
	}
	if len(meta.Members) != 5 {
		t.Errorf("expected 5 members, got %d", len(meta.Members))
	}
}

func TestGetImages(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	images, err := a.GetImages(context.Background(), "3840")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}
	if images[0].Width != 500 {
		t.Errorf("expected width 500, got %d", images[0].Width)
	}
}

func TestGetArtistRejectsUUID(t *testing.T) {
	limiter, settings := setupTest(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := New(limiter, settings, logger)

	// A MusicBrainz UUID should be rejected immediately without making an
	// HTTP request.
	_, err := a.GetArtist(context.Background(), "cc2c9c3c-b7bc-4b8b-84d8-4fbd8779e493")
	if err == nil {
		t.Fatal("expected error for UUID ID, got nil")
	}
	var notFound *provider.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("expected ErrNotFound, got: %T: %v", err, err)
	}
}

func TestGetImagesRejectsUUID(t *testing.T) {
	limiter, settings := setupTest(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := New(limiter, settings, logger)

	_, err := a.GetImages(context.Background(), "cc2c9c3c-b7bc-4b8b-84d8-4fbd8779e493")
	if err == nil {
		t.Fatal("expected error for UUID ID, got nil")
	}
	var notFound *provider.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("expected ErrNotFound, got: %T: %v", err, err)
	}
}

func TestGetArtistByNameFallback(t *testing.T) {
	limiter, settings := setupTest(t)

	// Mock server that handles both search and artist endpoints.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/database/search") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"results":[{"id":15885,"title":"Adele","type":"artist"}]}`))
			return
		}
		if strings.Contains(r.URL.Path, "/artists/15885") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":15885,"name":"Adele","profile":"English singer-songwriter","urls":[],"images":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	// Passing an artist name (non-numeric, non-UUID) should trigger name search.
	meta, err := a.GetArtist(context.Background(), "Adele")
	if err != nil {
		t.Fatalf("GetArtist by name: %v", err)
	}
	if meta.Name != "Adele" {
		t.Errorf("expected name 'Adele', got %q", meta.Name)
	}
	if meta.DiscogsID != "15885" {
		t.Errorf("expected DiscogsID '15885', got %q", meta.DiscogsID)
	}
}

func TestGetArtistByNameNoResults(t *testing.T) {
	limiter, settings := setupTest(t)

	// Mock server returns empty search results.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.GetArtist(context.Background(), "NonexistentArtist")
	if err == nil {
		t.Fatal("expected error for name with no search results, got nil")
	}
	var notFound *provider.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("expected ErrNotFound, got: %T: %v", err, err)
	}
}

func TestSupportsNameLookup(t *testing.T) {
	limiter, settings := setupTest(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := New(limiter, settings, logger)

	if !a.SupportsNameLookup() {
		t.Error("Discogs adapter should support name lookup")
	}
}

func TestGetArtistWithStyles(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	meta, err := a.GetArtist(context.Background(), "3840")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	// The test fixtures have 3 master releases with overlapping styles:
	// master_5001: Alternative Rock, Art Rock, Experimental
	// master_5002: Art Rock, Experimental, IDM
	// master_5003: Alternative Rock, Art Rock, Indie Rock
	// Expected counts: Art Rock(3), Alternative Rock(2), Experimental(2), IDM(1), Indie Rock(1)
	if len(meta.Styles) == 0 {
		t.Fatal("expected styles from release aggregation, got none")
	}
	// Art Rock should appear first (highest count = 3).
	if meta.Styles[0] != "Art Rock" {
		t.Errorf("expected first style 'Art Rock', got %q", meta.Styles[0])
	}
	// Should have 5 distinct styles total.
	if len(meta.Styles) != 5 {
		t.Errorf("expected 5 styles, got %d: %v", len(meta.Styles), meta.Styles)
	}
}

func TestTopStyles(t *testing.T) {
	counts := map[string]int{
		"Art Rock":         3,
		"Alternative Rock": 2,
		"Experimental":     2,
		"IDM":              1,
		"Indie Rock":       1,
	}
	got := topStyles(counts, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 styles, got %d: %v", len(got), got)
	}
	if got[0] != "Art Rock" {
		t.Errorf("expected first 'Art Rock', got %q", got[0])
	}
	// Second and third should be the tied-at-2 entries in alphabetical order.
	if got[1] != "Alternative Rock" {
		t.Errorf("expected second 'Alternative Rock', got %q", got[1])
	}
	if got[2] != "Experimental" {
		t.Errorf("expected third 'Experimental', got %q", got[2])
	}
}

func TestTopStylesEmpty(t *testing.T) {
	got := topStyles(nil, 10)
	if got != nil {
		t.Errorf("expected nil for empty counts, got %v", got)
	}
}

func TestIsNumericID(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"24941", true},
		{"0", true},
		{"123456789", true},
		{"", false},
		{"cc2c9c3c-b7bc-4b8b-84d8-4fbd8779e493", false},
		{"Radiohead", false},
		{"24941-a-ha", false},
		{"12.34", false},
	}
	for _, tt := range tests {
		if got := isNumericID(tt.input); got != tt.want {
			t.Errorf("isNumericID(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
