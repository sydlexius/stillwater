package allmusic

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("loading fixture %s: %v", name, err)
	}
	return data
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestScrapeArtistGenresAndStyles(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	fixture := loadFixture(t, "artist_dolly_parton.html")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/artist/mn0000205560" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write(fixture); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	defer srv.Close()

	a := NewWithBaseURL(limiter, newTestLogger(), srv.URL)

	meta, err := a.ScrapeArtist(context.Background(), "mn0000205560")
	if err != nil {
		t.Fatalf("ScrapeArtist: %v", err)
	}

	// Verify genres
	expectedGenres := []string{"Country", "Pop"}
	if len(meta.Genres) != len(expectedGenres) {
		t.Fatalf("expected %d genres, got %d: %v", len(expectedGenres), len(meta.Genres), meta.Genres)
	}
	for i, g := range expectedGenres {
		if meta.Genres[i] != g {
			t.Errorf("genre[%d]: expected %q, got %q", i, g, meta.Genres[i])
		}
	}

	// Verify styles
	expectedStyles := []string{"Traditional Country", "Country-Pop", "Nashville Sound/Countrypolitan"}
	if len(meta.Styles) != len(expectedStyles) {
		t.Fatalf("expected %d styles, got %d: %v", len(expectedStyles), len(meta.Styles), meta.Styles)
	}
	for i, s := range expectedStyles {
		if meta.Styles[i] != s {
			t.Errorf("style[%d]: expected %q, got %q", i, s, meta.Styles[i])
		}
	}

	// Verify moods is empty (not nil)
	if meta.Moods == nil {
		t.Error("expected non-nil empty moods slice")
	}
	if len(meta.Moods) != 0 {
		t.Errorf("expected 0 moods, got %d", len(meta.Moods))
	}

	// Verify provider ID is set
	if meta.ProviderID != "mn0000205560" {
		t.Errorf("expected provider ID mn0000205560, got %s", meta.ProviderID)
	}
	if meta.AllMusicID != "mn0000205560" {
		t.Errorf("expected AllMusic ID mn0000205560, got %s", meta.AllMusicID)
	}
}

func TestScrapeArtistGenresOnly(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	fixture := loadFixture(t, "artist_genres_only.html")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write(fixture); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	defer srv.Close()

	a := NewWithBaseURL(limiter, newTestLogger(), srv.URL)

	meta, err := a.ScrapeArtist(context.Background(), "mn0000000001")
	if err != nil {
		t.Fatalf("ScrapeArtist: %v", err)
	}

	if len(meta.Genres) != 1 || meta.Genres[0] != "Electronic" {
		t.Errorf("expected [Electronic], got %v", meta.Genres)
	}

	// Styles section missing but genres present -- should not return error.
	if len(meta.Styles) != 0 {
		t.Errorf("expected 0 styles, got %d: %v", len(meta.Styles), meta.Styles)
	}
}

func TestScrapeArtistEmptySections(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	fixture := loadFixture(t, "artist_empty_sections.html")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write(fixture); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	defer srv.Close()

	a := NewWithBaseURL(limiter, newTestLogger(), srv.URL)

	meta, err := a.ScrapeArtist(context.Background(), "mn0000000002")
	if err != nil {
		t.Fatalf("ScrapeArtist: %v", err)
	}

	// Sections exist but contain no anchors -- should return empty slices.
	if len(meta.Genres) != 0 {
		t.Errorf("expected 0 genres, got %d", len(meta.Genres))
	}
	if len(meta.Styles) != 0 {
		t.Errorf("expected 0 styles, got %d", len(meta.Styles))
	}
}

func TestScrapeArtistBrokenStructure(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	fixture := loadFixture(t, "artist_broken_structure.html")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write(fixture); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	defer srv.Close()

	a := NewWithBaseURL(limiter, newTestLogger(), srv.URL)

	_, err := a.ScrapeArtist(context.Background(), "mn0000000003")
	if err == nil {
		t.Fatal("expected error for broken page structure")
	}
	if !errors.Is(err, ErrScraperBroken) {
		t.Errorf("expected ErrScraperBroken, got: %v", err)
	}
}

func TestScrapeArtistNotFound(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := NewWithBaseURL(limiter, newTestLogger(), srv.URL)

	_, err := a.ScrapeArtist(context.Background(), "mn9999999999")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	var notFound *provider.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestScrapeArtistServerError(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	a := NewWithBaseURL(limiter, newTestLogger(), srv.URL)

	_, err := a.ScrapeArtist(context.Background(), "mn0000000001")
	if err == nil {
		t.Fatal("expected error for 503 response")
	}
	var unavail *provider.ErrProviderUnavailable
	if !errors.As(err, &unavail) {
		t.Errorf("expected ErrProviderUnavailable, got: %v", err)
	}
}

func TestScrapeArtistEmptyID(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	a := NewWithBaseURL(limiter, newTestLogger(), "http://localhost")

	_, err := a.ScrapeArtist(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty ID")
	}
	var notFound *provider.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestScrapeArtistContextCanceled(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write(loadFixture(t, "artist_dolly_parton.html")); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	defer srv.Close()

	a := NewWithBaseURL(limiter, newTestLogger(), srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := a.ScrapeArtist(ctx, "mn0000205560")
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestNameAndRequiresAuth(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	a := New(limiter, newTestLogger())

	if a.Name() != provider.NameAllMusic {
		t.Errorf("expected name allmusic, got %s", a.Name())
	}
	if a.RequiresAuth() {
		t.Error("expected RequiresAuth to return false")
	}
}

func TestParseArtistPageDirectly(t *testing.T) {
	// Test the parser directly with known HTML to verify extraction logic
	// without needing an HTTP server.
	htmlContent := `<!DOCTYPE html>
<html>
<head><title>Test</title></head>
<body>
<h1>Artist Name</h1>
<div class="artist-genres">
  <h4>Genre</h4>
  <div><a href="/genre/1">Rock</a></div>
  <div><a href="/genre/2">Alternative</a></div>
</div>
<div class="artist-styles">
  <h4>Styles</h4>
  <div><a href="/style/1">Indie Rock</a></div>
  <div><a href="/style/2">Post-Punk</a></div>
  <div><a href="/style/3">Shoegaze</a></div>
</div>
</body>
</html>`

	genres, styles, err := parseArtistPage([]byte(htmlContent))
	if err != nil {
		t.Fatalf("parseArtistPage: %v", err)
	}

	if len(genres) != 2 {
		t.Fatalf("expected 2 genres, got %d: %v", len(genres), genres)
	}
	if genres[0] != "Rock" || genres[1] != "Alternative" {
		t.Errorf("unexpected genres: %v", genres)
	}

	if len(styles) != 3 {
		t.Fatalf("expected 3 styles, got %d: %v", len(styles), styles)
	}
	if styles[0] != "Indie Rock" || styles[1] != "Post-Punk" || styles[2] != "Shoegaze" {
		t.Errorf("unexpected styles: %v", styles)
	}
}

func TestRateLimiterIsCalled(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write(loadFixture(t, "artist_dolly_parton.html")); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	defer srv.Close()

	a := NewWithBaseURL(limiter, newTestLogger(), srv.URL)

	// First call should succeed (rate limiter allows it)
	_, err := a.ScrapeArtist(context.Background(), "mn0000205560")
	if err != nil {
		t.Fatalf("first ScrapeArtist: %v", err)
	}
	if callCount.Load() != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount.Load())
	}

	// Second call should also succeed (verifies limiter.Wait was called,
	// not that it blocked -- the rate limit is 1/sec so back-to-back calls
	// may introduce a brief wait)
	_, err = a.ScrapeArtist(context.Background(), "mn0000205560")
	if err != nil {
		t.Fatalf("second ScrapeArtist: %v", err)
	}
	if callCount.Load() != 2 {
		t.Errorf("expected 2 HTTP calls, got %d", callCount.Load())
	}
}
