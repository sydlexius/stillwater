package wikidata

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/sparql-results+json")
		query := r.URL.Query().Get("query")
		if query == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Check for the "not found" MBID
		if contains(query, "not-found-mbid") {
			_, _ = w.Write([]byte(`{"results":{"bindings":[]}}`))
			return
		}
		_, _ = w.Write(loadFixture(t, "artist_radiohead.json"))
	}))
}

// newImageTestServers creates a SPARQL server and a Commons API server for
// testing GetImages. The sparqlFixture determines which SPARQL response is
// returned. The Commons server routes requests based on the filename in the
// "titles" query parameter.
func newImageTestServers(t *testing.T, sparqlFixture string) (sparqlSrv, commonsSrv *httptest.Server) {
	t.Helper()

	commonsSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		titles := r.URL.Query().Get("titles")

		// Route based on the requested filename.
		switch {
		case strings.Contains(titles, "Radiohead_2016.jpg"):
			_, _ = w.Write(loadFixture(t, "commons_radiohead_photo.json"))
		case strings.Contains(titles, "Radiohead_logo.png"):
			_, _ = w.Write(loadFixture(t, "commons_radiohead_logo.json"))
		case strings.Contains(titles, "Artist_photo.jpg"):
			_, _ = w.Write(loadFixture(t, "commons_artist_photo.json"))
		case strings.Contains(titles, "Band_logo.png"):
			_, _ = w.Write(loadFixture(t, "commons_band_logo.json"))
		default:
			// Return a "not found" Commons response (page ID -1).
			_, _ = w.Write([]byte(`{"query":{"pages":{"-1":{"title":"File:Unknown","missing":""}}}}`))
		}
	}))

	sparqlSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/sparql-results+json")
		query := r.URL.Query().Get("query")
		if query == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if contains(query, "not-found-mbid") {
			_, _ = w.Write([]byte(`{"results":{"bindings":[]}}`))
			return
		}
		_, _ = w.Write(loadFixture(t, sparqlFixture))
	}))

	return sparqlSrv, commonsSrv
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstr(s, substr)
}

func searchSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestGetArtist(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoint(limiter, logger, srv.URL)

	meta, err := a.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %s", meta.Name)
	}
	if meta.WikidataID != "Q44190" {
		t.Errorf("expected Q44190, got %s", meta.WikidataID)
	}
	if meta.Formed != "1985" {
		t.Errorf("expected formed 1985, got %s", meta.Formed)
	}
	if meta.Country != "United Kingdom" {
		t.Errorf("expected United Kingdom, got %s", meta.Country)
	}
	if len(meta.Genres) != 3 {
		t.Fatalf("expected 3 genres, got %d", len(meta.Genres))
	}
}

func TestGetArtistNotFound(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoint(limiter, logger, srv.URL)

	_, err := a.GetArtist(context.Background(), "not-found-mbid")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}

func TestSearchReturnsNil(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoint(limiter, logger, "http://localhost")

	results, err := a.SearchArtist(context.Background(), "anything")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil, got %v", results)
	}
}

func TestGetImagesBothP18AndP154(t *testing.T) {
	sparqlSrv, commonsSrv := newImageTestServers(t, "images_both.json")
	defer sparqlSrv.Close()
	defer commonsSrv.Close()

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoints(limiter, logger, sparqlSrv.URL, commonsSrv.URL)

	images, err := a.GetImages(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(images))
	}

	// First image should be the thumb (P18).
	if images[0].Type != provider.ImageThumb {
		t.Errorf("expected thumb type, got %s", images[0].Type)
	}
	if images[0].URL != "https://upload.wikimedia.org/wikipedia/commons/a/a3/Radiohead_2016.jpg" {
		t.Errorf("unexpected thumb URL: %s", images[0].URL)
	}
	if images[0].Width != 1200 || images[0].Height != 800 {
		t.Errorf("unexpected thumb dimensions: %dx%d", images[0].Width, images[0].Height)
	}
	if images[0].Source != "wikidata" {
		t.Errorf("expected source wikidata, got %s", images[0].Source)
	}

	// Second image should be the logo (P154).
	if images[1].Type != provider.ImageLogo {
		t.Errorf("expected logo type, got %s", images[1].Type)
	}
	if images[1].URL != "https://upload.wikimedia.org/wikipedia/commons/b/b1/Radiohead_logo.png" {
		t.Errorf("unexpected logo URL: %s", images[1].URL)
	}
	if images[1].Width != 400 || images[1].Height != 150 {
		t.Errorf("unexpected logo dimensions: %dx%d", images[1].Width, images[1].Height)
	}
}

func TestGetImagesP18Only(t *testing.T) {
	sparqlSrv, commonsSrv := newImageTestServers(t, "images_p18_only.json")
	defer sparqlSrv.Close()
	defer commonsSrv.Close()

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoints(limiter, logger, sparqlSrv.URL, commonsSrv.URL)

	images, err := a.GetImages(context.Background(), "test-mbid")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}
	if images[0].Type != provider.ImageThumb {
		t.Errorf("expected thumb type, got %s", images[0].Type)
	}
	if images[0].URL != "https://upload.wikimedia.org/wikipedia/commons/c/c1/Artist_photo.jpg" {
		t.Errorf("unexpected URL: %s", images[0].URL)
	}
}

func TestGetImagesP154Only(t *testing.T) {
	sparqlSrv, commonsSrv := newImageTestServers(t, "images_p154_only.json")
	defer sparqlSrv.Close()
	defer commonsSrv.Close()

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoints(limiter, logger, sparqlSrv.URL, commonsSrv.URL)

	images, err := a.GetImages(context.Background(), "test-mbid")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}
	if images[0].Type != provider.ImageLogo {
		t.Errorf("expected logo type, got %s", images[0].Type)
	}
	if images[0].URL != "https://upload.wikimedia.org/wikipedia/commons/d/d1/Band_logo.png" {
		t.Errorf("unexpected URL: %s", images[0].URL)
	}
}

func TestGetImagesNoProperties(t *testing.T) {
	sparqlSrv, commonsSrv := newImageTestServers(t, "images_none.json")
	defer sparqlSrv.Close()
	defer commonsSrv.Close()

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoints(limiter, logger, sparqlSrv.URL, commonsSrv.URL)

	_, err := a.GetImages(context.Background(), "test-mbid")
	if err == nil {
		t.Fatal("expected error for artist with no image properties")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetImagesNotFoundMBID(t *testing.T) {
	sparqlSrv, commonsSrv := newImageTestServers(t, "images_both.json")
	defer sparqlSrv.Close()
	defer commonsSrv.Close()

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoints(limiter, logger, sparqlSrv.URL, commonsSrv.URL)

	_, err := a.GetImages(context.Background(), "not-found-mbid")
	if err == nil {
		t.Fatal("expected error for unknown MBID")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestExtractCommonsFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"http://commons.wikimedia.org/wiki/Special:FilePath/Radiohead_2016.jpg", "Radiohead_2016.jpg"},
		{"http://commons.wikimedia.org/wiki/Special:FilePath/Band%20Logo.png", "Band%20Logo.png"},
		{"Standalone.jpg", "Standalone.jpg"},
	}
	for _, tt := range tests {
		got := extractCommonsFilename(tt.input)
		if got != tt.want {
			t.Errorf("extractCommonsFilename(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractQID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"http://www.wikidata.org/entity/Q44190", "Q44190"},
		{"Q44190", "Q44190"},
	}
	for _, tt := range tests {
		got := extractQID(tt.input)
		if got != tt.want {
			t.Errorf("extractQID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractYear(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1985-01-01T00:00:00Z", "1985"},
		{"2000", "2000"},
	}
	for _, tt := range tests {
		got := extractYear(tt.input)
		if got != tt.want {
			t.Errorf("extractYear(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
