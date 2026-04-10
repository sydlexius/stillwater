package musicbrainz

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
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/artist" && r.URL.Query().Get("query") != "":
			query := r.URL.Query().Get("query")
			if query == "nonexistent-artist-xyz" {
				w.Write([]byte(`{"created":"","count":0,"offset":0,"artists":[]}`))
				return
			}
			w.Write(loadFixture(t, "search_radiohead.json"))

		case strings.HasPrefix(r.URL.Path, "/artist/"):
			mbid := strings.TrimPrefix(r.URL.Path, "/artist/")
			if mbid == "not-found-id" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if mbid == "server-error-id" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Write(loadFixture(t, "artist_radiohead.json"))

		case r.URL.Path == "/release-group" && r.URL.Query().Get("artist") != "":
			artistID := r.URL.Query().Get("artist")
			if artistID == "not-found-id" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(loadFixture(t, "release_groups_radiohead.json"))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewWithBaseURL(limiter, logger, baseURL)
}

func TestName(t *testing.T) {
	a := newTestAdapter(t, "http://localhost")
	if a.Name() != provider.NameMusicBrainz {
		t.Errorf("expected %s, got %s", provider.NameMusicBrainz, a.Name())
	}
}

func TestRequiresAuth(t *testing.T) {
	a := newTestAdapter(t, "http://localhost")
	if a.RequiresAuth() {
		t.Error("MusicBrainz should not require auth")
	}
}

func TestSearchArtist(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	results, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	r := results[0]
	if r.Name != "Radiohead" {
		t.Errorf("expected name Radiohead, got %s", r.Name)
	}
	if r.MusicBrainzID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("unexpected MBID: %s", r.MusicBrainzID)
	}
	// Score is max(apiScore=100, nameSimilarity=100) for exact match.
	if r.Score != 100 {
		t.Errorf("expected score 100, got %d", r.Score)
	}
	if r.Country != "GB" {
		t.Errorf("expected country GB, got %s", r.Country)
	}
	if r.Type != "Group" {
		t.Errorf("expected type Group, got %s", r.Type)
	}
	if r.Source != string(provider.NameMusicBrainz) {
		t.Errorf("expected source musicbrainz, got %s", r.Source)
	}

	// Second result ("Radiohead Tribute") has API score 45; name similarity
	// should be higher than 45 (partial match), so the final score uses
	// the name similarity value via max(apiScore, nameSimilarity).
	r2 := results[1]
	if r2.Score <= 45 {
		t.Errorf("expected score > 45 (name similarity should exceed API score), got %d", r2.Score)
	}
	if r2.Score >= 100 {
		t.Errorf("expected score < 100 for partial match, got %d", r2.Score)
	}

	// Results should be sorted by score descending.
	if results[0].Score < results[1].Score {
		t.Errorf("results not sorted by score descending: first=%d, second=%d",
			results[0].Score, results[1].Score)
	}
}

func TestSearchArtistEmpty(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	results, err := a.SearchArtist(context.Background(), "nonexistent-artist-xyz")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestGetArtist(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	meta, err := a.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	if meta.Name != "Radiohead" {
		t.Errorf("expected name Radiohead, got %s", meta.Name)
	}
	if meta.MusicBrainzID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("unexpected MBID: %s", meta.MusicBrainzID)
	}
	if meta.Type != "group" {
		t.Errorf("expected type group, got %s", meta.Type)
	}
	if meta.Country != "GB" {
		t.Errorf("expected country GB, got %s", meta.Country)
	}
	if meta.Formed != "1991" {
		t.Errorf("expected formed 1991, got %s", meta.Formed)
	}

	// Genres
	if len(meta.Genres) != 3 {
		t.Fatalf("expected 3 genres, got %d", len(meta.Genres))
	}
	if meta.Genres[0] != "alternative rock" {
		t.Errorf("expected first genre 'alternative rock', got %s", meta.Genres[0])
	}

	// Aliases
	if len(meta.Aliases) != 1 {
		t.Fatalf("expected 1 alias, got %d", len(meta.Aliases))
	}

	// Members
	if len(meta.Members) != 5 {
		t.Fatalf("expected 5 members, got %d", len(meta.Members))
	}
	thom := meta.Members[0]
	if thom.Name != "Thom Yorke" {
		t.Errorf("expected first member Thom Yorke, got %s", thom.Name)
	}
	if thom.MBID != "8bfac288-ccc5-448d-9573-c33ea2aa5c30" {
		t.Errorf("unexpected MBID for Thom Yorke: %s", thom.MBID)
	}
	if len(thom.Instruments) != 2 {
		t.Errorf("expected 2 instruments for Thom Yorke, got %d", len(thom.Instruments))
	}
	if !thom.IsActive {
		t.Error("expected Thom Yorke to be active")
	}

	// URLs
	if meta.URLs["official"] != "https://www.radiohead.com/" {
		t.Errorf("unexpected official URL: %s", meta.URLs["official"])
	}
	if meta.URLs["wikipedia"] != "https://en.wikipedia.org/wiki/Radiohead" {
		t.Errorf("unexpected wikipedia URL: %s", meta.URLs["wikipedia"])
	}
}

func TestGetArtistNotFound(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	_, err := a.GetArtist(context.Background(), "not-found-id")
	if err == nil {
		t.Fatal("expected error for not-found ID")
	}
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
	_ = notFound
}

func TestGetArtistServerError(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	_, err := a.GetArtist(context.Background(), "server-error-id")
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !isErrUnavailable(err) {
		t.Errorf("expected ErrProviderUnavailable, got %T: %v", err, err)
	}
}

func TestGetImagesReturnsNil(t *testing.T) {
	a := newTestAdapter(t, "http://localhost")
	images, err := a.GetImages(context.Background(), "any-id")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if images != nil {
		t.Errorf("expected nil images, got %v", images)
	}
}

func TestTestConnection(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	err := a.TestConnection(context.Background())
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := a.SearchArtist(ctx, "Radiohead")
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"created":"","count":0,"offset":0,"artists":[]}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	_, _ = a.SearchArtist(context.Background(), "test")

	if !strings.HasPrefix(gotUA, "Stillwater/") {
		t.Errorf("expected User-Agent starting with Stillwater/, got %s", gotUA)
	}
}

func TestGetReleaseGroups(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	groups, err := a.GetReleaseGroups(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetReleaseGroups: %v", err)
	}
	if len(groups) != 5 {
		t.Fatalf("expected 5 release groups, got %d", len(groups))
	}

	first := groups[0]
	if first.Title != "Pablo Honey" {
		t.Errorf("expected first title Pablo Honey, got %s", first.Title)
	}
	if first.PrimaryType != "Album" {
		t.Errorf("expected primary type Album, got %s", first.PrimaryType)
	}
	if first.FirstReleaseDate != "1993-02-22" {
		t.Errorf("expected first release date 1993-02-22, got %s", first.FirstReleaseDate)
	}
}

func TestGetReleaseGroupsNotFound(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	_, err := a.GetReleaseGroups(context.Background(), "not-found-id")
	if err == nil {
		t.Fatal("expected error for not-found ID")
	}
	if !isErrNotFound(err) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestSetBaseURL(t *testing.T) {
	a := newTestAdapter(t, "http://localhost")

	// Initially the default.
	if a.BaseURL() != "http://localhost" {
		t.Errorf("expected http://localhost, got %s", a.BaseURL())
	}

	// Set a custom URL.
	a.SetBaseURL("http://mirror:5000/ws/2")
	if a.BaseURL() != "http://mirror:5000/ws/2" {
		t.Errorf("expected http://mirror:5000/ws/2, got %s", a.BaseURL())
	}

	// Trailing slash is stripped.
	a.SetBaseURL("http://mirror:5000/ws/2/")
	if a.BaseURL() != "http://mirror:5000/ws/2" {
		t.Errorf("expected trailing slash stripped, got %s", a.BaseURL())
	}
}

func TestDefaultBaseURL(t *testing.T) {
	a := newTestAdapter(t, "http://localhost")
	if a.DefaultBaseURL() != "https://musicbrainz.org/ws/2" {
		t.Errorf("unexpected default base URL: %s", a.DefaultBaseURL())
	}
}

func TestMirrorableSearchArtist(t *testing.T) {
	// Start a test server, create adapter pointing elsewhere, then redirect via SetBaseURL.
	srv := newTestServer(t)
	defer srv.Close()

	a := newTestAdapter(t, "http://will-not-work:9999")
	a.SetBaseURL(srv.URL)

	results, err := a.SearchArtist(context.Background(), "Radiohead")
	if err != nil {
		t.Fatalf("SearchArtist after SetBaseURL: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestGetArtistStyleExtraction(t *testing.T) {
	// Use a custom server that returns the Portishead fixture.
	// Portishead has genres: [electronic, trip hop]
	// and tags: [downtempo, dark, post-punk, experimental]
	// Expected: styles extracted from tag classification minus genres.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/artist/") {
			_, _ = w.Write(loadFixture(t, "artist_portishead.json"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	meta, err := a.GetArtist(context.Background(), "8f6bd1e4-fbe1-4f50-aa9b-94c450ec0f11")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	// Genres should come from the genres array: electronic, trip hop
	if len(meta.Genres) != 2 {
		t.Fatalf("expected 2 genres, got %d: %v", len(meta.Genres), meta.Genres)
	}

	// Styles should be tag-classified styles minus those already in genres.
	// "trip hop" is in genres, so it should not appear in styles.
	// "downtempo" and "post-punk" are style-classified tags not in genres.
	if len(meta.Styles) != 2 {
		t.Fatalf("expected 2 styles, got %d: %v", len(meta.Styles), meta.Styles)
	}
	// Check that the deduplicated styles contain downtempo and post-punk.
	styleSet := make(map[string]bool, len(meta.Styles))
	for _, s := range meta.Styles {
		styleSet[s] = true
	}
	if !styleSet["downtempo"] {
		t.Errorf("expected 'downtempo' in styles, got %v", meta.Styles)
	}
	if !styleSet["post-punk"] {
		t.Errorf("expected 'post-punk' in styles, got %v", meta.Styles)
	}
}

func TestGetArtistTagOnlyFallback(t *testing.T) {
	// When an artist has no structured genres (genres array is empty),
	// tags should be classified into genres/styles/moods instead of being
	// dumped wholesale into genres (which would make styles always empty).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/artist/") {
			_, _ = w.Write(loadFixture(t, "artist_tagonly.json"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	meta, err := a.GetArtist(context.Background(), "tag-only-artist-id")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}

	// Tags: rock(genre), shoegaze(style), dream pop(style),
	//       melancholic(mood), seen live(ignore)
	// Genres should contain only genre-classified tags.
	if len(meta.Genres) != 1 {
		t.Fatalf("expected 1 genre, got %d: %v", len(meta.Genres), meta.Genres)
	}
	if meta.Genres[0] != "rock" {
		t.Errorf("expected genre 'rock', got %q", meta.Genres[0])
	}

	// Styles should contain style-classified tags, not be empty.
	if len(meta.Styles) != 2 {
		t.Fatalf("expected 2 styles, got %d: %v", len(meta.Styles), meta.Styles)
	}
	styleSet := make(map[string]bool, len(meta.Styles))
	for _, s := range meta.Styles {
		styleSet[s] = true
	}
	if !styleSet["shoegaze"] {
		t.Errorf("expected 'shoegaze' in styles, got %v", meta.Styles)
	}
	if !styleSet["dream pop"] {
		t.Errorf("expected 'dream pop' in styles, got %v", meta.Styles)
	}

	// Moods should contain mood-classified tags.
	if len(meta.Moods) != 1 {
		t.Fatalf("expected 1 mood, got %d: %v", len(meta.Moods), meta.Moods)
	}
	if meta.Moods[0] != "melancholic" {
		t.Errorf("expected mood 'melancholic', got %q", meta.Moods[0])
	}
}

func TestDeduplicateStyles(t *testing.T) {
	tests := []struct {
		name   string
		styles []string
		genres []string
		want   int
	}{
		{"no overlap", []string{"shoegaze", "dream pop"}, []string{"rock"}, 2},
		{"full overlap", []string{"rock"}, []string{"rock"}, 0},
		{"partial overlap", []string{"art rock", "shoegaze"}, []string{"art rock", "electronic"}, 1},
		{"case insensitive", []string{"Art Rock"}, []string{"art rock"}, 0},
		{"empty styles", nil, []string{"rock"}, 0},
		{"empty genres", []string{"shoegaze"}, nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deduplicateStyles(tt.styles, tt.genres)
			if len(got) != tt.want {
				t.Errorf("deduplicateStyles(%v, %v) returned %d items, want %d: %v",
					tt.styles, tt.genres, len(got), tt.want, got)
			}
		})
	}
}

func TestNormalizeHyphens(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"a\u2010ha", "a-ha"},                    // U+2010 HYPHEN
		{"a\u2011ha", "a-ha"},                    // U+2011 NON-BREAKING HYPHEN
		{"a-ha", "a-ha"},                         // already ASCII, unchanged
		{"Sigur \u2013 Ros", "Sigur \u2013 Ros"}, // en-dash left as-is
		{"", ""},
	}
	for _, c := range cases {
		got := normalizeHyphens(c.input)
		if got != c.want {
			t.Errorf("normalizeHyphens(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestGetArtist_NamePromotion(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	// Without language preferences: canonical name is used.
	meta, err := a.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("without prefs: expected name Radiohead, got %s", meta.Name)
	}

	// With Japanese preference: the Japanese primary alias should be promoted.
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja"})
	meta, err = a.GetArtist(ctx, "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist with ja pref: %v", err)
	}
	if meta.Name == "Radiohead" {
		t.Error("with ja pref: expected promoted Japanese name, still got Radiohead")
	}
	if meta.SortName == "" {
		t.Error("with ja pref: expected a sort name after promotion")
	}
	// The canonical name should appear in aliases after promotion.
	found := false
	for _, alias := range meta.Aliases {
		if alias == "Radiohead" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("with ja pref: canonical name 'Radiohead' should appear in aliases, got %v", meta.Aliases)
	}

	// With a non-matching preference (German): no promotion, canonical retained.
	ctx = provider.WithMetadataLanguages(context.Background(), []string{"de"})
	meta, err = a.GetArtist(ctx, "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist with de pref: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("with de pref: expected name Radiohead (no promotion), got %s", meta.Name)
	}
}

func isErrNotFound(err error) bool {
	_, ok := err.(*provider.ErrNotFound)
	return ok
}

func isErrUnavailable(err error) bool {
	_, ok := err.(*provider.ErrProviderUnavailable)
	return ok
}
