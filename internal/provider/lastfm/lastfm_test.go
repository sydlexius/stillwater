package lastfm

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
	if err := settings.SetAPIKey(context.Background(), provider.NameLastFM, "test-key"); err != nil {
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
		w.Header().Set("Content-Type", "application/json")
		method := r.URL.Query().Get("method")
		switch method {
		case "artist.search":
			w.Write(loadFixture(t, "search_radiohead.json"))
		case "artist.getinfo":
			artist := r.URL.Query().Get("artist")
			mbid := r.URL.Query().Get("mbid")
			if artist == "nonexistent" || mbid == "nonexistent" {
				w.Write([]byte(`{"artist":{"name":""}}`))
				return
			}
			w.Write(loadFixture(t, "artist_radiohead.json"))
		default:
			w.WriteHeader(http.StatusBadRequest)
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
	if results[0].MusicBrainzID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("unexpected MBID: %s", results[0].MusicBrainzID)
	}
}

func TestGetArtistByMBID(t *testing.T) {
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
	if meta.Biography == "" {
		t.Error("expected non-empty biography")
	}
	if len(meta.Genres) != 3 {
		t.Errorf("expected 3 genres, got %d", len(meta.Genres))
	}
	if len(meta.SimilarArtists) != 2 {
		t.Errorf("expected 2 similar artists, got %d", len(meta.SimilarArtists))
	}
}

func TestGetArtistByName(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

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
	srv := newTestServer(t)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	_, err := a.GetArtist(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent artist")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}

func TestGetImagesReturnsNil(t *testing.T) {
	limiter, settings := setupTest(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, "http://localhost")

	images, err := a.GetImages(context.Background(), "any")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if images != nil {
		t.Errorf("expected nil, got %v", images)
	}
}

func TestIsUUID(t *testing.T) {
	if !provider.IsUUID("a74b1b7f-71a5-4011-9441-d0b5e4122711") {
		t.Error("expected true for valid UUID")
	}
	if provider.IsUUID("not-a-uuid") {
		t.Error("expected false for invalid UUID")
	}
	if provider.IsUUID("Radiohead") {
		t.Error("expected false for artist name")
	}
}

func TestCleanBio(t *testing.T) {
	bio := `Radiohead are great. <a href="https://www.last.fm/music/Radiohead">Read more</a>`
	cleaned := cleanBio(bio)
	if strings.Contains(cleaned, "<a href") {
		t.Errorf("expected cleaned bio, got: %s", cleaned)
	}
	if cleaned != "Radiohead are great." {
		t.Errorf("unexpected cleaned bio: %s", cleaned)
	}
}

func TestGetArtistByNameRejectsMismatch(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		method := r.URL.Query().Get("method")
		if method == "artist.getinfo" {
			// Return a completely different artist for a name-based lookup.
			_, _ = w.Write(loadFixture(t, "artist_mismatch.json"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	// Searching "Adele" should reject "Kim Kardashian" due to low similarity.
	_, err := a.GetArtist(context.Background(), "Adele")
	if err == nil {
		t.Fatal("expected error when result name does not match search term")
	}
	nf, ok := err.(*provider.ErrNotFound)
	if !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	} else if nf.ID != "Adele" {
		t.Errorf("expected ErrNotFound.ID to be %q, got %q", "Adele", nf.ID)
	}
}

func TestGetArtistByNameThresholdZeroDisablesValidation(t *testing.T) {
	limiter, settings := setupTest(t)
	// Set threshold to 0, which disables name similarity validation.
	if err := settings.SetNameSimilarityThreshold(context.Background(), 0); err != nil {
		t.Fatalf("setting threshold: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		method := r.URL.Query().Get("method")
		if method == "artist.getinfo" {
			// Return a completely different artist for a name-based lookup.
			_, _ = w.Write(loadFixture(t, "artist_mismatch.json"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	// With threshold=0, even a completely mismatched name should be accepted.
	meta, err := a.GetArtist(context.Background(), "Adele")
	if err != nil {
		t.Fatalf("expected success with threshold=0, got: %v", err)
	}
	if meta.Name != "Kim Kardashian" {
		t.Errorf("expected Kim Kardashian (the mismatched result), got %s", meta.Name)
	}
}

func TestGetArtistByMBIDSkipsValidation(t *testing.T) {
	limiter, settings := setupTest(t)
	// Use the mismatch fixture so the server returns "Kim Kardashian".
	// If MBID lookups incorrectly applied name validation, this would fail.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		method := r.URL.Query().Get("method")
		if method == "artist.getinfo" {
			_, _ = w.Write(loadFixture(t, "artist_mismatch.json"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	// MBID-based lookup should skip name validation entirely.
	meta, err := a.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("expected MBID lookup to skip name validation, got error: %v", err)
	}
	if meta.Name != "Kim Kardashian" {
		t.Errorf("expected Kim Kardashian (mismatched result accepted via MBID), got %s", meta.Name)
	}
}

func TestSearchArtistScoresReflectSimilarity(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadFixture(t, "search_adele.json"))
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	results, err := a.SearchArtist(context.Background(), "Adele")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Exact match "Adele" should score 100.
	if results[0].Score != 100 {
		t.Errorf("expected score 100 for exact match, got %d", results[0].Score)
	}
	// "Adele Adkins" should score less than 100 but above 0.
	if results[1].Score >= 100 || results[1].Score <= 0 {
		t.Errorf("expected partial score for 'Adele Adkins', got %d", results[1].Score)
	}
}

func TestGetArtistTagClassification(t *testing.T) {
	limiter, settings := setupTest(t)
	// Use a custom server that returns the Bjork fixture with mixed tags:
	// electronic (genre), trip-hop (style), ethereal (mood),
	// experimental (genre), seen live (ignore).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		method := r.URL.Query().Get("method")
		if method == "artist.getinfo" {
			_, _ = w.Write(loadFixture(t, "artist_bjork.json"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, settings, logger, srv.URL)

	meta, err := a.GetArtist(context.Background(), "87c5dedd-371d-4571-9e1f-e8de0ef7f5d0")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	// Genres: electronic, experimental (unknown tags default to genre)
	if len(meta.Genres) != 2 {
		t.Errorf("expected 2 genres, got %d: %v", len(meta.Genres), meta.Genres)
	}

	// Styles: trip-hop
	if len(meta.Styles) != 1 {
		t.Errorf("expected 1 style, got %d: %v", len(meta.Styles), meta.Styles)
	} else if meta.Styles[0] != "trip-hop" {
		t.Errorf("expected style 'trip-hop', got %q", meta.Styles[0])
	}

	// Moods: ethereal
	if len(meta.Moods) != 1 {
		t.Errorf("expected 1 mood, got %d: %v", len(meta.Moods), meta.Moods)
	} else if meta.Moods[0] != "ethereal" {
		t.Errorf("expected mood 'ethereal', got %q", meta.Moods[0])
	}
}

func TestSearchArtistExactMatchScore(t *testing.T) {
	limiter, settings := setupTest(t)
	srv := newTestServer(t) // returns Radiohead
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
	// Exact name match should score 100.
	if results[0].Score != 100 {
		t.Errorf("expected score 100 for exact match, got %d", results[0].Score)
	}
}
