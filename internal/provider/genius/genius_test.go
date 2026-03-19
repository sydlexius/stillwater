package genius

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
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	limiter := provider.NewRateLimiterMap()
	settings := provider.NewSettingsService(db, enc)
	if err := settings.SetAPIKey(context.Background(), provider.NameGenius, "test-token"); err != nil {
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

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate the Authorization header to catch regressions that drop it.
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case path == "/search":
			_, _ = w.Write(loadFixture(t, "search_radiohead.json"))
		case strings.HasPrefix(path, "/artists/"):
			id := strings.TrimPrefix(path, "/artists/")
			if id == "0" || id == "nonexistent" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(loadFixture(t, "artist_radiohead.json"))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestSearchArtist(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, testLogger(), srv.URL)

	results, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %s", results[0].Name)
	}
	if results[0].ProviderID != "604" {
		t.Errorf("expected provider ID 604, got %s", results[0].ProviderID)
	}
	if results[0].Source != "genius" {
		t.Errorf("expected source genius, got %s", results[0].Source)
	}
	// Exact match should score 100.
	if results[0].Score != 100 {
		t.Errorf("expected score 100 for exact match, got %d", results[0].Score)
	}
}

func TestSearchArtistDeduplicates(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, testLogger(), srv.URL)

	results, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	// The fixture has 3 hits for Radiohead (ID 604) and 1 for a different artist (ID 88888).
	// After deduplication, we should have exactly 2 unique artists.
	if len(results) != 2 {
		t.Errorf("expected 2 deduplicated results, got %d", len(results))
	}
}

func TestGetArtistByID(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, testLogger(), srv.URL)

	meta, err := a.GetArtist(context.Background(), "604")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %s", meta.Name)
	}
	if meta.Biography == "" {
		t.Error("expected non-empty biography")
	}
	if meta.ProviderID != "604" {
		t.Errorf("expected provider ID 604, got %s", meta.ProviderID)
	}
	if meta.URLs["genius"] == "" {
		t.Error("expected non-empty Genius URL")
	}
	if len(meta.Aliases) != 2 {
		t.Errorf("expected 2 aliases, got %d", len(meta.Aliases))
	}
}

func TestGetArtistByName(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, testLogger(), srv.URL)

	meta, err := a.GetArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %s", meta.Name)
	}
}

func TestGetArtistNotFound(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return empty search results so getArtistByName finds nothing.
		if r.URL.Path == "/search" {
			_, _ = w.Write([]byte(`{"response":{"hits":[]}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, testLogger(), srv.URL)

	_, err := a.GetArtist(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent artist")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetImagesReturnsNil(t *testing.T) {
	limiter, settings := setupTest(t)
	a := NewWithBaseURL(limiter, settings, testLogger(), "http://localhost")

	images, err := a.GetImages(context.Background(), "any")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if images != nil {
		t.Errorf("expected nil, got %v", images)
	}
}

func TestTestConnection(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, testLogger(), srv.URL)

	if err := a.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
}

func TestRequiresAuth(t *testing.T) {
	limiter, settings := setupTest(t)
	a := NewWithBaseURL(limiter, settings, testLogger(), "http://localhost")

	if !a.RequiresAuth() {
		t.Error("expected RequiresAuth to return true")
	}
}

func TestIsNumeric(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"604", true},
		{"0", true},
		{"123456", true},
		{"", false},
		{"abc", false},
		{"12a3", false},
		{"Radiohead", false},
	}
	for _, tt := range tests {
		if got := isNumeric(tt.input); got != tt.want {
			t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestGetArtistUUIDReturnsNotFound(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, testLogger(), srv.URL)

	// A MusicBrainz UUID should be rejected immediately without making an API call.
	_, err := a.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error for UUID input")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestIsUUID(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"a74b1b7f-71a5-4011-9441-d0b5e4122711", true},
		{"A74B1B7F-71A5-4011-9441-D0B5E4122711", true},
		{"604", false},
		{"Radiohead", false},
		{"", false},
		{"a74b1b7f71a540119441d0b5e4122711", false},    // no dashes
		{"a74b1b7f-71a5-4011-9441-d0b5e412271", false}, // too short
	}
	for _, tt := range tests {
		if got := isUUID(tt.input); got != tt.want {
			t.Errorf("isUUID(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestGetArtistByNameRejectsMismatch(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/search" {
			// Return Kim Kardashian when searching for "Adele".
			_, _ = w.Write(loadFixture(t, "search_adele_mismatch.json"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, testLogger(), srv.URL)

	// Searching "Adele" should not return Kim Kardashian's data.
	_, err := a.GetArtist(context.Background(), "Adele")
	if err == nil {
		t.Fatal("expected error when top result is a name mismatch")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtistByNameAcceptsCorrectMatch(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/search":
			_, _ = w.Write(loadFixture(t, "search_adele_correct.json"))
		case strings.HasPrefix(r.URL.Path, "/artists/"):
			_, _ = w.Write(loadFixture(t, "artist_adele.json"))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, testLogger(), srv.URL)

	meta, err := a.GetArtist(context.Background(), "Adele")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.Name != "Adele" {
		t.Errorf("expected Adele, got %s", meta.Name)
	}
}

func TestSearchArtistScoresReflectSimilarity(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Return mismatched results for an "Adele" search.
		_, _ = w.Write(loadFixture(t, "search_adele_mismatch.json"))
	}))
	defer srv.Close()
	a := NewWithBaseURL(limiter, settings, testLogger(), srv.URL)

	results, err := a.SearchArtist(context.Background(), "Adele")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	// "Kim Kardashian" should have a low similarity score to "Adele".
	if results[0].Score >= minNameSimilarity {
		t.Errorf("expected score below %d for Kim Kardashian vs Adele, got %d",
			minNameSimilarity, results[0].Score)
	}
}

func TestNameSimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		min  int
		max  int
	}{
		{"Radiohead", "Radiohead", 100, 100},
		{"radiohead", "Radiohead", 100, 100},
		{"The Beatles", "Beatles", 100, 100},
		{"Adele", "Kim Kardashian", 0, 30},
		{"Guns N' Roses", "Guns N Roses", 80, 100},
		{"AC/DC", "ACDC", 100, 100},
		{"!!!", "!!!", 100, 100},                 // punctuation-only: pre-normalization exact match
		{"!!!", "???", 0, 0},                     // different punctuation-only names: both normalize to empty
		{"Mot\u00f6rhead", "Motorhead", 80, 100}, // Unicode: single rune difference
		{"", "Radiohead", 0, 0},
		{"Radiohead", "", 0, 0},
		{"", "", 100, 100},
		// Boundary: score at and just below threshold.
		{"abcde", "abcXX", 60, 60}, // exactly at threshold (accepted)
		{"abcde", "aXXXX", 0, 59},  // below threshold (rejected)
	}
	for _, tt := range tests {
		score := nameSimilarity(tt.a, tt.b)
		if score < tt.min || score > tt.max {
			t.Errorf("nameSimilarity(%q, %q) = %d, want [%d, %d]",
				tt.a, tt.b, score, tt.min, tt.max)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Radiohead", "radiohead"},
		{"The Beatles", "beatles"},
		{"  Adele  ", "adele"},
		{"AC/DC", "acdc"},
		{"Guns N' Roses", "guns n roses"},
	}
	for _, tt := range tests {
		if got := normalizeName(tt.input); got != tt.want {
			t.Errorf("normalizeName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"radiohead", "radiohead", 0},
		{"adele", "kim kardashian", 12},
		{"mot\u00f6rhead", "motorhead", 1}, // single rune difference, not 2 bytes
	}
	for _, tt := range tests {
		if got := levenshtein(tt.a, tt.b); got != tt.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestAuthRequired(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	defer db.Close()
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
	// Do not set an API key -- should trigger ErrAuthRequired.
	a := NewWithBaseURL(limiter, settings, testLogger(), "http://localhost")

	_, err = a.SearchArtist(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error when no API key is set")
	}
	if _, ok := err.(*provider.ErrAuthRequired); !ok {
		t.Errorf("expected ErrAuthRequired, got %T: %v", err, err)
	}
}
