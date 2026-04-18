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

	// Pre-load fixture in the test goroutine so t.Fatalf is not called from
	// the httptest handler goroutine (which causes undefined behavior).
	artistData := loadFixture(t, "artist_radiohead.json")

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/sparql-results+json")
		query := r.URL.Query().Get("query")
		if query == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Check for the "not found" MBID (valid UUID format that returns no results)
		if strings.Contains(query, "00000000-0000-0000-0000-000000000000") {
			_, _ = w.Write([]byte(`{"results":{"bindings":[]}}`))
			return
		}
		_, _ = w.Write(artistData)
	}))
}

// newImageTestServers creates a SPARQL server and a Commons API server for
// testing GetImages. The sparqlFixture determines which SPARQL response is
// returned. The Commons server routes requests based on the filename in the
// "titles" query parameter.
//
// All fixtures are pre-loaded in the calling (test) goroutine so that
// t.Fatalf is never invoked from an httptest handler goroutine.
func newImageTestServers(t *testing.T, sparqlFixture string) (sparqlSrv, commonsSrv *httptest.Server) {
	t.Helper()

	// Pre-load every fixture the handlers will need.
	sparqlData := loadFixture(t, sparqlFixture)
	radioheadPhoto := loadFixture(t, "commons_radiohead_photo.json")
	radioheadLogo := loadFixture(t, "commons_radiohead_logo.json")
	artistPhoto := loadFixture(t, "commons_artist_photo.json")
	bandLogo := loadFixture(t, "commons_band_logo.json")

	commonsSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		titles := r.URL.Query().Get("titles")

		// Route based on the requested filename.
		switch {
		case strings.Contains(titles, "Radiohead_2016.jpg"):
			_, _ = w.Write(radioheadPhoto)
		case strings.Contains(titles, "Radiohead_logo.png"):
			_, _ = w.Write(radioheadLogo)
		case strings.Contains(titles, "Artist_photo.jpg"):
			_, _ = w.Write(artistPhoto)
		case strings.Contains(titles, "Band_logo.png"):
			_, _ = w.Write(bandLogo)
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
		if strings.Contains(query, "00000000-0000-0000-0000-000000000000") {
			_, _ = w.Write([]byte(`{"results":{"bindings":[]}}`))
			return
		}
		_, _ = w.Write(sparqlData)
	}))

	return sparqlSrv, commonsSrv
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

	_, err := a.GetArtist(context.Background(), "00000000-0000-0000-0000-000000000000")
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

	images, err := a.GetImages(context.Background(), "11111111-1111-1111-1111-111111111111")
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

	images, err := a.GetImages(context.Background(), "22222222-2222-2222-2222-222222222222")
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

	_, err := a.GetImages(context.Background(), "33333333-3333-3333-3333-333333333333")
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

	_, err := a.GetImages(context.Background(), "00000000-0000-0000-0000-000000000000")
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
		{"http://commons.wikimedia.org/wiki/Special:FilePath/Band%20Logo.png", "Band Logo.png"},
		{"http://commons.wikimedia.org/wiki/Special:FilePath/File:SomeImage.jpg", "SomeImage.jpg"},
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

func TestGetArtistInvalidMBID(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoint(limiter, logger, "http://localhost")

	_, err := a.GetArtist(context.Background(), "not-a-uuid")
	if err == nil {
		t.Fatal("expected error for invalid MBID")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetImagesInvalidMBID(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoints(limiter, logger, "http://localhost", "http://localhost")

	_, err := a.GetImages(context.Background(), "not-a-uuid")
	if err == nil {
		t.Fatal("expected error for invalid MBID")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestWikidataLangParam(t *testing.T) {
	tests := []struct {
		name  string
		prefs []string
		want  string
	}{
		{
			name:  "empty prefs returns en",
			prefs: nil,
			want:  "en",
		},
		{
			name:  "single simple tag",
			prefs: []string{"ja"},
			want:  "ja,en",
		},
		{
			name:  "regional tag expands to base then en",
			prefs: []string{"en-GB"},
			want:  "en-gb,en",
		},
		{
			name:  "multiple tags with deduplication",
			prefs: []string{"ja", "en-GB", "fr"},
			want:  "ja,en-gb,en,fr",
		},
		{
			name:  "en already in prefs not duplicated",
			prefs: []string{"en"},
			want:  "en",
		},
		{
			name:  "en-GB expands base en, final en not duplicated",
			prefs: []string{"fr", "en-GB"},
			want:  "fr,en-gb,en",
		},
		{
			name:  "zh-Hant expands to zh",
			prefs: []string{"zh-Hant"},
			want:  "zh-hant,zh,en",
		},
		{
			name:  "duplicate prefs deduplicated",
			prefs: []string{"ja", "ja", "en"},
			want:  "ja,en",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wikidataLangParam(tt.prefs)
			if got != tt.want {
				t.Errorf("wikidataLangParam(%v) = %q, want %q", tt.prefs, got, tt.want)
			}
		})
	}
}

func TestBuildArtistQuery_LanguagePrefsIncluded(t *testing.T) {
	// When language preferences are set, the SPARQL query must include them in the
	// wikibase:language parameter. The MBID must not appear in the language string.
	mbid := "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	prefs := []string{"ja", "en-GB"}
	query := buildArtistQuery(mbid, prefs)
	// The query must reference the MBID.
	if !strings.Contains(query, mbid) {
		t.Errorf("query does not contain MBID %s", mbid)
	}
	// The wikibase:language param should include user prefs and fallback en.
	if !strings.Contains(query, `"ja,en-gb,en"`) {
		t.Errorf("query wikibase:language does not contain expected lang param; query:\n%s", query)
	}
}

func TestGetArtist_LangPrefPassedToSPARQL(t *testing.T) {
	// Verify that GetArtist passes language preferences into the SPARQL query
	// by capturing the raw query string the server receives.
	artistData := loadFixture(t, "artist_radiohead.json")
	var capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/sparql-results+json")
		capturedQuery = r.URL.Query().Get("query")
		_, _ = w.Write(artistData)
	}))
	defer srv.Close()

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoint(limiter, logger, srv.URL)

	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja", "fr"})
	_, err := a.GetArtist(ctx, "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	// The captured SPARQL query must include the user's language preferences.
	if !strings.Contains(capturedQuery, "ja") {
		t.Errorf("SPARQL query does not include 'ja' lang preference; query: %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "fr") {
		t.Errorf("SPARQL query does not include 'fr' lang preference; query: %s", capturedQuery)
	}
}

func TestGetImagesCommonsResolutionFailure(t *testing.T) {
	// SPARQL returns valid image filenames but the Commons endpoint returns
	// HTTP 500 for all requests. GetImages should return ErrProviderUnavailable.
	sparqlData := loadFixture(t, "images_p18_only.json")

	commonsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer commonsSrv.Close()

	sparqlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/sparql-results+json")
		query := r.URL.Query().Get("query")
		if query == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write(sparqlData)
	}))
	defer sparqlSrv.Close()

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoints(limiter, logger, sparqlSrv.URL, commonsSrv.URL)

	_, err := a.GetImages(context.Background(), "11111111-1111-1111-1111-111111111111")
	if err == nil {
		t.Fatal("expected error when all commons resolutions fail")
	}
	if _, ok := err.(*provider.ErrProviderUnavailable); !ok {
		t.Errorf("expected ErrProviderUnavailable, got %T: %v", err, err)
	}
}
