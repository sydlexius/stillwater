package musicbrainz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/httpsafe"
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
			// Member alias lookups return per-member fixtures so tests can
			// verify localization behavior without mocking a second server.
			switch mbid {
			case "8bfac288-ccc5-448d-9573-c33ea2aa5c30":
				w.Write(loadFixture(t, "member_thom_yorke.json"))
				return
			case "member-002":
				w.Write(loadFixture(t, "member_jonny_greenwood.json"))
				return
			case "member-003":
				w.Write(loadFixture(t, "member_no_aliases.json"))
				return
			case "member-error":
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
	// Tests use a high rate to avoid the real 1 req/sec MusicBrainz pacing.
	// Production uses the default limiter; see internal/provider/ratelimit.go.
	limiter.SetLimit(provider.NameMusicBrainz, 1000)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, baseURL)
	// Production wires a.client to httpsafe.SafeClient, which rejects the
	// loopback (127.0.0.1) addresses that httptest.NewServer binds to by
	// design. Tests override the client with a plain *http.Client so the
	// httptest fixture is reachable; the production SSRF guard is exercised
	// by internal/httpsafe's own test suite plus the AST regression guard
	// in no_raw_client_construction_test.go (which allowlists this kind of
	// post-construction override implicitly by skipping _test.go files).
	a.client = &http.Client{Timeout: 10 * time.Second}
	return a
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
	if r.Origin != "GB" {
		t.Errorf("expected origin GB, got %s", r.Origin)
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

	t.Run("scalar fields", func(t *testing.T) {
		if meta.Name != "Radiohead" {
			t.Errorf("expected name Radiohead, got %s", meta.Name)
		}
		if meta.MusicBrainzID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
			t.Errorf("unexpected MBID: %s", meta.MusicBrainzID)
		}
		if meta.Type != "group" {
			t.Errorf("expected type group, got %s", meta.Type)
		}
		if meta.Origin != "United Kingdom" {
			t.Errorf("expected origin United Kingdom, got %s", meta.Origin)
		}
		if meta.Formed != "1991" {
			t.Errorf("expected formed 1991, got %s", meta.Formed)
		}
	})

	t.Run("genres", func(t *testing.T) {
		if len(meta.Genres) != 3 {
			t.Fatalf("expected 3 genres, got %d", len(meta.Genres))
		}
		if meta.Genres[0] != "alternative rock" {
			t.Errorf("expected first genre 'alternative rock', got %s", meta.Genres[0])
		}
	})

	t.Run("aliases", func(t *testing.T) {
		if len(meta.Aliases) != 1 {
			t.Fatalf("expected 1 alias, got %d", len(meta.Aliases))
		}
	})

	t.Run("members", func(t *testing.T) {
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
	})

	t.Run("URLs", func(t *testing.T) {
		if meta.URLs["official"] != "https://www.radiohead.com/" {
			t.Errorf("unexpected official URL: %s", meta.URLs["official"])
		}
		if meta.URLs["wikipedia"] != "https://en.wikipedia.org/wiki/Radiohead" {
			t.Errorf("unexpected wikipedia URL: %s", meta.URLs["wikipedia"])
		}
	})
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

// TestDoRequestAuthError verifies that a 401/403 from a (self-hosted/private)
// mirror is classified as an auth failure, not transient unavailability, so the
// API layer can report a credentials problem rather than "cannot reach" (#2278).
func TestDoRequestAuthError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		a := newTestAdapter(t, srv.URL)
		err := a.TestConnection(context.Background())
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error, got nil", status)
		}
		var authErr *provider.ErrAuthRequired
		if !errors.As(err, &authErr) {
			t.Errorf("status %d: expected *provider.ErrAuthRequired, got %T: %v", status, err, err)
		}
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

// TestSetHTTPClient verifies that SetHTTPClient replaces the adapter's
// unexported client field. Same-package access lets the test read the
// field directly; a behavioral test that drives a request through the
// swapped client lives in handlers_provider_test.go (which is the
// cross-package use case the setter was added for).
func TestSetHTTPClient(t *testing.T) {
	a := newTestAdapter(t, "http://localhost")
	custom := &http.Client{Timeout: 99 * time.Second}
	a.SetHTTPClient(custom)
	if a.client != custom {
		t.Errorf("SetHTTPClient did not replace the client field")
	}
}

// TestSetHTTPClientNilPanics verifies the documented nil-input panic.
// A silent nil acceptance would crash inside doRequest with a nil
// dereference far from the misconfiguration site.
func TestSetHTTPClientNilPanics(t *testing.T) {
	a := newTestAdapter(t, "http://localhost")
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("SetHTTPClient(nil) did not panic")
		}
	}()
	a.SetHTTPClient(nil)
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

	// Portishead fixture has "country":"GB" but no "area" object.
	// Origin must fall back to the ISO country code rather than returning empty.
	if meta.Origin != "GB" {
		t.Errorf("expected origin GB (ISO fallback when area absent), got %q", meta.Origin)
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
	var nf *provider.ErrNotFound
	return errors.As(err, &nf)
}

func isErrUnavailable(err error) bool {
	var unavail *provider.ErrProviderUnavailable
	return errors.As(err, &unavail)
}

// captureHandler is a minimal slog.Handler that records log entries so tests
// can assert on log level and attributes without capturing stderr. All
// instances sharing the same *captureState write to the same slice, so
// assertions against the root handler see entries produced by child loggers
// created via logger.With(...) (which calls WithAttrs internally).
type captureState struct {
	mu      sync.Mutex
	entries []captureEntry
}

type captureEntry struct {
	level slog.Level
	msg   string
	attrs []slog.Attr
}

type captureHandler struct {
	state *captureState
}

func newCaptureState() (*captureHandler, *captureState) {
	s := &captureState{}
	return &captureHandler{state: s}, s
}

func (h *captureHandler) Enabled(_ context.Context, level slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	entry := captureEntry{level: r.Level, msg: r.Message}
	r.Attrs(func(a slog.Attr) bool {
		entry.attrs = append(entry.attrs, a)
		return true
	})
	h.state.mu.Lock()
	h.state.entries = append(h.state.entries, entry)
	h.state.mu.Unlock()
	return nil
}

// WithAttrs returns a child handler that shares the same captureState so
// entries from logger.With(...) are visible to the original captureState.
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler {
	return &captureHandler{state: h.state}
}
func (h *captureHandler) WithGroup(_ string) slog.Handler { return h }

// hasWarn returns true when at least one captured entry is at WARN level and
// contains the given substring in its message or attribute values.
func (s *captureState) hasWarn(substring string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.level != slog.LevelWarn {
			continue
		}
		if strings.Contains(e.msg, substring) {
			return true
		}
		for _, a := range e.attrs {
			if strings.Contains(fmt.Sprintf("%v", a.Value), substring) {
				return true
			}
		}
	}
	return false
}

// warnAttrValue returns the string value of the named attribute from the
// first WARN entry whose message contains msgSubstr, and whether it was found.
// Unlike hasWarn it pins a specific (message, attribute) pair so a test can
// distinguish, for example, the parse-failure WARN from the fallback-retry WARN.
func (s *captureState) warnAttrValue(msgSubstr, attrKey string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.level != slog.LevelWarn || !strings.Contains(e.msg, msgSubstr) {
			continue
		}
		for _, a := range e.attrs {
			if a.Key == attrKey {
				return a.Value.String(), true
			}
		}
	}
	return "", false
}

// hasWarnAttr reports whether any WARN entry whose message contains msgSubstr
// also carries an attribute attrKey whose value equals attrValue exactly.
func (s *captureState) hasWarnAttr(msgSubstr, attrKey, attrValue string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.level != slog.LevelWarn || !strings.Contains(e.msg, msgSubstr) {
			continue
		}
		for _, a := range e.attrs {
			if a.Key == attrKey && a.Value.String() == attrValue {
				return true
			}
		}
	}
	return false
}

// newCaptureAdapter creates an Adapter backed by a captureHandler logger.
// The returned *captureState can be inspected for logged entries.
func newCaptureAdapter(t *testing.T, baseURL string) (*Adapter, *captureState) {
	t.Helper()
	h, state := newCaptureState()
	logger := slog.New(h)
	limiter := provider.NewRateLimiterMap()
	limiter.SetLimit(provider.NameMusicBrainz, 1000)
	a := NewWithBaseURL(limiter, logger, baseURL)
	a.client = &http.Client{Timeout: 10 * time.Second}
	return a, state
}

// --- #1033: mirror health check and error visibility ---

// TestTestConnection_HTMLResponse verifies that a 200 OK response with an
// HTML body is detected as a failure. This is the core mirror bug: a broken
// mirror returns an HTML error page with HTTP 200, which previously passed.
func TestTestConnection_HTMLResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>404 Not Found</body></html>`))
	}))
	defer srv.Close()

	a, _ := newCaptureAdapter(t, srv.URL)
	err := a.TestConnection(context.Background())
	if err == nil {
		t.Fatal("TestConnection should return an error when server responds with HTML")
	}
	if !strings.Contains(err.Error(), "non-JSON") {
		t.Errorf("error should mention non-JSON response, got: %v", err)
	}
}

// TestTestConnection_ValidJSON verifies that a well-formed SearchResponse JSON
// body does not return an error.
func TestTestConnection_ValidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":"2024-01-01T00:00:00.000Z","count":0,"offset":0,"artists":[]}`))
	}))
	defer srv.Close()

	a, _ := newCaptureAdapter(t, srv.URL)
	if err := a.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection should succeed for valid JSON: %v", err)
	}
}

// TestUnmarshalResponse_WarnOnParseError verifies that a JSON parse failure
// logs at WARN level and includes the base URL in the log entry. This covers
// enhancement 3 (promote mirror parse errors to WARN).
func TestUnmarshalResponse_WarnOnParseError(t *testing.T) {
	a, cap := newCaptureAdapter(t, "http://mirror.example.com/ws/2")
	body := []byte(`<!DOCTYPE html><html><body>Service Unavailable</body></html>`)
	var resp SearchResponse
	err := a.unmarshalResponse("http://mirror.example.com/ws/2", body, &resp)
	if err == nil {
		t.Fatal("unmarshalResponse should return error for HTML input")
	}
	if !cap.hasWarn("mirror.example.com") {
		t.Error("expected WARN log entry containing the base URL")
	}
}

// TestAutoFallback_ParseErrorTriggersFallback verifies that when the configured
// mirror returns an HTML response, the adapter retries against the fallback
// URL (enhancement 4). Two httptest.Servers are used: the broken mirror
// (returns HTML) and the good "official" server (returns valid JSON). The
// fallback URL is overridden via setFallbackURL so the test does not reach
// the real musicbrainz.org.
func TestAutoFallback_ParseErrorTriggersFallback(t *testing.T) {
	// Good server: returns a valid SearchResponse.
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":"2024-01-01T00:00:00.000Z","count":0,"offset":0,"artists":[]}`))
	}))
	defer goodSrv.Close()

	// Broken mirror: returns HTML with HTTP 200.
	brokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body>Mirror Error</body></html>`))
	}))
	defer brokenSrv.Close()

	a, cap := newCaptureAdapter(t, brokenSrv.URL)
	// Override the fallback to point at the good server instead of musicbrainz.org.
	a.setFallbackURL(goodSrv.URL)

	_, err := a.SearchArtist(context.Background(), "test")
	if err != nil {
		t.Fatalf("SearchArtist should succeed via fallback: %v", err)
	}
	// The adapter should have logged a WARN about retrying.
	if !cap.hasWarn("fallback") {
		t.Error("expected WARN log entry mentioning fallback retry")
	}
}

// TestAutoFallback_NetworkTimeoutDoesNotFallback verifies that a network
// timeout from the mirror does NOT trigger fallback -- only a successful 200
// with unparsable body does.
func TestAutoFallback_NetworkTimeoutDoesNotFallback(t *testing.T) {
	// Good server: should never be reached.
	var goodHit atomic.Bool
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHit.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":"","count":0,"offset":0,"artists":[]}`))
	}))
	defer goodSrv.Close()

	// Broken server: immediately closes the connection to simulate a network error.
	brokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hijack and close to simulate a connection drop.
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer brokenSrv.Close()

	a, cap := newCaptureAdapter(t, brokenSrv.URL)
	a.setFallbackURL(goodSrv.URL)

	_, err := a.SearchArtist(context.Background(), "test")
	// The request must fail (network error from broken server).
	if err == nil {
		t.Fatal("expected error from network-level failure")
	}
	// The good server must NOT have been contacted.
	if goodHit.Load() {
		t.Error("good server was contacted, but fallback should not trigger on network error")
	}
	// No "fallback" WARN should appear -- the network error is reported by doRequest
	// before the parse stage that triggers fallback.
	if cap.hasWarn("fallback") {
		t.Error("fallback WARN should not be logged for a network-level error")
	}
}

// htmlServer returns an httptest.Server that responds 200 OK with an HTML
// body for every request, simulating a misconfigured mirror.
func htmlServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSearchArtist_ParseErrorFallbackAlsoFails covers the case where the
// configured mirror returns HTML and the fallback URL is unreachable: the
// original mirror parse error is returned (not a fallback network error).
// The HTML body is deliberately longer than 200 bytes to exercise the
// body-preview truncation branch in unmarshalResponse.
func TestSearchArtist_ParseErrorFallbackAlsoFails(t *testing.T) {
	longHTML := "<!DOCTYPE html><html><body>" + strings.Repeat("mirror error ", 40) + "</body></html>"
	mirror := htmlServer(t, longHTML)

	// Fallback server drops the connection to simulate a network failure.
	badFallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer badFallback.Close()

	a, _ := newCaptureAdapter(t, mirror.URL)
	a.setFallbackURL(badFallback.URL)

	_, err := a.SearchArtist(context.Background(), "test")
	if err == nil {
		t.Fatal("SearchArtist should return the mirror parse error when the fallback is unreachable")
	}
	// The returned error must be the mirror JSON parse failure, not the
	// fallback's network error: the operator's actionable problem is the
	// broken mirror.
	if !strings.Contains(err.Error(), "invalid character '<'") {
		t.Fatalf("expected the mirror JSON parse error, got: %v", err)
	}
	if strings.Contains(err.Error(), badFallback.URL) {
		t.Fatalf("returned error leaked fallback-network detail, expected only the mirror parse error: %v", err)
	}
}

// TestGetArtist_ParseErrorNoFallback covers GetArtist returning a parse error
// when the configured base URL equals the fallback URL (no distinct mirror,
// so no fallback retry is warranted).
func TestGetArtist_ParseErrorNoFallback(t *testing.T) {
	srv := htmlServer(t, `<html><body>not json</body></html>`)
	a, _ := newCaptureAdapter(t, srv.URL)
	a.setFallbackURL(srv.URL) // base == fallback: no fallback retry

	if _, err := a.GetArtist(context.Background(), "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("GetArtist should return a parse error for an HTML response")
	}
}

// TestFetchMemberAliases_ParseError covers the parse-error path of the
// per-member alias lookup.
func TestFetchMemberAliases_ParseError(t *testing.T) {
	srv := htmlServer(t, `<html><body>not json</body></html>`)
	a, _ := newCaptureAdapter(t, srv.URL)
	a.setFallbackURL(srv.URL)

	if _, err := a.fetchMemberAliases(context.Background(), "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("fetchMemberAliases should return a parse error for an HTML response")
	}
}

// TestGetReleaseGroups_ParseError covers the parse-error path of the
// release-group fetch.
func TestGetReleaseGroups_ParseError(t *testing.T) {
	srv := htmlServer(t, `<html><body>not json</body></html>`)
	a, _ := newCaptureAdapter(t, srv.URL)
	a.setFallbackURL(srv.URL)

	if _, err := a.GetReleaseGroups(context.Background(), "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("GetReleaseGroups should return a parse error for an HTML response")
	}
}

// TestTestConnection_ServerError covers TestConnection surfacing the doRequest
// error when the endpoint responds with a non-2xx status (HTTP 503), distinct
// from the non-JSON-body failure path.
func TestTestConnection_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	a, _ := newCaptureAdapter(t, srv.URL)
	if err := a.TestConnection(context.Background()); err == nil {
		t.Fatal("TestConnection should return an error when the endpoint responds 503")
	}
}

// TestTestConnection_ValidJSONWrongShape covers the shape check: a 200 OK
// response whose body is well-formed JSON but is not a MusicBrainz search
// response (no "created" field) must still be reported as a failure. This is
// the false-positive the health check exists to prevent: a proxy health page
// or a JSON error body would otherwise pass JSON-syntax validation.
func TestTestConnection_ValidJSONWrongShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
	}))
	defer srv.Close()

	a, _ := newCaptureAdapter(t, srv.URL)
	err := a.TestConnection(context.Background())
	if err == nil {
		t.Fatal("TestConnection should reject valid JSON that is not a MusicBrainz search response")
	}
	if !strings.Contains(err.Error(), "not a MusicBrainz search response") {
		t.Errorf("error should mention the wrong-shape response, got: %v", err)
	}
}

// TestUnmarshalResponse_EmptyBody covers a 200 OK response with an empty body
// (a common proxy-truncation failure): json.Unmarshal reports "unexpected end
// of JSON input", which unmarshalResponse must surface as an error.
func TestUnmarshalResponse_EmptyBody(t *testing.T) {
	a, _ := newCaptureAdapter(t, "http://mirror.example.com/ws/2")
	var resp SearchResponse
	if err := a.unmarshalResponse("http://mirror.example.com/ws/2", []byte{}, &resp); err == nil {
		t.Fatal("unmarshalResponse should return an error for an empty body")
	}
}

// TestUnmarshalResponse_TruncatesBodyPreview verifies the body_preview log
// attribute is capped at 200 bytes for an oversized body and left intact for a
// body at or under the limit.
func TestUnmarshalResponse_TruncatesBodyPreview(t *testing.T) {
	long := []byte(strings.Repeat("x", 500))
	a, capLong := newCaptureAdapter(t, "http://mirror.example.com/ws/2")
	var resp SearchResponse
	_ = a.unmarshalResponse("http://mirror.example.com/ws/2", long, &resp)
	preview, ok := capLong.warnAttrValue("JSON parse failed", "body_preview")
	if !ok {
		t.Fatal("expected a JSON-parse-failed WARN with a body_preview attribute")
	}
	if len(preview) != 200 {
		t.Errorf("body_preview for a 500-byte body should be truncated to 200 bytes, got %d", len(preview))
	}

	short := []byte(strings.Repeat("y", 50))
	a2, capShort := newCaptureAdapter(t, "http://mirror.example.com/ws/2")
	_ = a2.unmarshalResponse("http://mirror.example.com/ws/2", short, &resp)
	previewShort, ok := capShort.warnAttrValue("JSON parse failed", "body_preview")
	if !ok {
		t.Fatal("expected a JSON-parse-failed WARN for the short body")
	}
	if len(previewShort) != 50 {
		t.Errorf("body_preview for a 50-byte body should be left intact, got %d", len(previewShort))
	}
}

// TestAutoFallback_FallbackBodyAlsoUnparsable covers the case where both the
// configured mirror and the fallback URL return HTML: SearchArtist returns an
// error, and a parse-failure WARN is logged against the fallback URL (in
// addition to the WARN against the mirror).
func TestAutoFallback_FallbackBodyAlsoUnparsable(t *testing.T) {
	mirror := htmlServer(t, `<html><body>mirror is broken</body></html>`)
	fallback := htmlServer(t, `<html><body>fallback is also broken</body></html>`)

	a, capState := newCaptureAdapter(t, mirror.URL)
	a.setFallbackURL(fallback.URL)

	if _, err := a.SearchArtist(context.Background(), "test"); err == nil {
		t.Fatal("SearchArtist should return an error when both mirror and fallback return HTML")
	}
	if !capState.hasWarnAttr("JSON parse failed", "base_url", fallback.URL) {
		t.Errorf("expected a parse-failure WARN against the fallback URL %q", fallback.URL)
	}
	if !capState.hasWarn("retrying against fallback") {
		t.Error("expected a WARN noting the fallback retry")
	}
}

// --- #973: YearsActive synthesis ---

func TestMapArtist_YearsActive(t *testing.T) {
	tests := []struct {
		name            string
		artistType      string
		artistName      string
		lifeSpan        MBLifeSpan
		wantYearsActive string
	}{
		{
			name:            "GroupFormedOnly",
			artistType:      "Group",
			artistName:      "Test Band",
			lifeSpan:        MBLifeSpan{Begin: "1990"},
			wantYearsActive: "1990-present",
		},
		{
			name:            "GroupFormedAndDisbanded",
			artistType:      "Group",
			artistName:      "Test Band",
			lifeSpan:        MBLifeSpan{Begin: "1990", End: "2005", Ended: true},
			wantYearsActive: "1990-2005",
		},
		{
			name:            "OrchestraFormedOnly",
			artistType:      "Orchestra",
			artistName:      "Berlin Philharmonic",
			lifeSpan:        MBLifeSpan{Begin: "1882"},
			wantYearsActive: "1882-present",
		},
		{
			name:            "ChoirFormedAndDisbanded",
			artistType:      "Choir",
			artistName:      "Test Choir",
			lifeSpan:        MBLifeSpan{Begin: "2000", End: "2020", Ended: true},
			wantYearsActive: "2000-2020",
		},
		{
			name:            "SoloArtistNotSynthesized",
			artistType:      "Person",
			artistName:      "Solo Person",
			lifeSpan:        MBLifeSpan{Begin: "1970"},
			wantYearsActive: "",
		},
		{
			name:            "GroupNoFormedDate",
			artistType:      "Group",
			artistName:      "Mystery Band",
			lifeSpan:        MBLifeSpan{},
			wantYearsActive: "",
		},
		{
			name:            "PartialDates",
			artistType:      "Group",
			artistName:      "Partial Date Band",
			lifeSpan:        MBLifeSpan{Begin: "1990-05-14", End: "2005-12", Ended: true},
			wantYearsActive: "1990-2005",
		},
		{
			name:            "PartialBeginNoEnd",
			artistType:      "Group",
			artistName:      "Active Band",
			lifeSpan:        MBLifeSpan{Begin: "1990-05", End: "", Ended: false},
			wantYearsActive: "1990-present",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAdapter(t, "http://localhost:0")
			mb := &MBArtist{
				ID:       "abc-123",
				Name:     tc.artistName,
				Type:     tc.artistType,
				LifeSpan: tc.lifeSpan,
			}
			meta := a.mapArtist(context.Background(), mb)
			if meta.YearsActive != tc.wantYearsActive {
				t.Errorf("expected YearsActive %q, got %q", tc.wantYearsActive, meta.YearsActive)
			}
		})
	}
}

// --- #974: Member deduplication ---

func TestMapArtist_DeduplicateMembers_MergesDateRanges(t *testing.T) {
	a := newTestAdapter(t, "http://localhost:0")
	mb := &MBArtist{
		ID:   "band-001",
		Name: "Test Band",
		Type: "Group",
		Relations: []MBRelation{
			{
				Type:      "member of band",
				Direction: "backward",
				Begin:     "1990",
				End:       "1995",
				Ended:     true,
				Artist: &MBArtist{
					ID:   "member-001",
					Name: "John Doe",
				},
				Attributes: []string{"guitar"},
			},
			{
				Type:      "member of band",
				Direction: "backward",
				Begin:     "2000",
				End:       "",
				Ended:     false,
				Artist: &MBArtist{
					ID:   "member-001",
					Name: "John Doe",
				},
				Attributes: []string{"vocals"},
			},
		},
	}

	meta := a.mapArtist(context.Background(), mb)

	if len(meta.Members) != 1 {
		t.Fatalf("expected 1 member after dedup, got %d", len(meta.Members))
	}
	m := meta.Members[0]
	if m.MBID != "member-001" {
		t.Errorf("expected MBID %q, got %q", "member-001", m.MBID)
	}
	if m.DateJoined != "1990" {
		t.Errorf("expected DateJoined %q, got %q", "1990", m.DateJoined)
	}
	// Second stint is open-ended, so DateLeft should be empty.
	if m.DateLeft != "" {
		t.Errorf("expected empty DateLeft (still active), got %q", m.DateLeft)
	}
	if !m.IsActive {
		t.Error("expected IsActive=true since second stint is not ended")
	}
	// Instruments should be merged.
	if len(m.Instruments) != 2 {
		t.Errorf("expected 2 instruments, got %d: %v", len(m.Instruments), m.Instruments)
	}
}

func TestMapArtist_DeduplicateMembers_UniqueMembers(t *testing.T) {
	a := newTestAdapter(t, "http://localhost:0")
	mb := &MBArtist{
		ID:   "band-002",
		Name: "Unique Band",
		Type: "Group",
		Relations: []MBRelation{
			{
				Type:      "member of band",
				Direction: "backward",
				Begin:     "1990",
				Artist:    &MBArtist{ID: "m1", Name: "Alice"},
			},
			{
				Type:      "member of band",
				Direction: "backward",
				Begin:     "1991",
				Artist:    &MBArtist{ID: "m2", Name: "Bob"},
			},
		},
	}

	meta := a.mapArtist(context.Background(), mb)

	if len(meta.Members) != 2 {
		t.Fatalf("expected 2 unique members, got %d", len(meta.Members))
	}
}

func TestMapArtist_DeduplicateMembers_BothPeriodsClosed(t *testing.T) {
	a := newTestAdapter(t, "http://localhost:0")
	mb := &MBArtist{
		ID:   "band-003",
		Name: "Former Band",
		Type: "Group",
		LifeSpan: MBLifeSpan{
			Begin: "1980",
			End:   "2010",
			Ended: true,
		},
		Relations: []MBRelation{
			{
				Type:      "member of band",
				Direction: "backward",
				Begin:     "1980",
				End:       "1990",
				Ended:     true,
				Artist:    &MBArtist{ID: "m1", Name: "Dave"},
			},
			{
				Type:      "member of band",
				Direction: "backward",
				Begin:     "2000",
				End:       "2010",
				Ended:     true,
				Artist:    &MBArtist{ID: "m1", Name: "Dave"},
			},
		},
	}

	meta := a.mapArtist(context.Background(), mb)

	if len(meta.Members) != 1 {
		t.Fatalf("expected 1 member after dedup, got %d", len(meta.Members))
	}
	m := meta.Members[0]
	if m.DateJoined != "1980" {
		t.Errorf("expected earliest DateJoined %q, got %q", "1980", m.DateJoined)
	}
	if m.DateLeft != "2010" {
		t.Errorf("expected latest DateLeft %q, got %q", "2010", m.DateLeft)
	}
}

// --- #908: "Also Performs As" aliases ---

func TestMapArtist_AlsoPerformsAs_AddsAlias(t *testing.T) {
	a := newTestAdapter(t, "http://localhost:0")
	mb := &MBArtist{
		ID:   "person-001",
		Name: "Richard Melville Hall",
		Type: "Person",
		Relations: []MBRelation{
			{
				Type:      "is person",
				Direction: "forward",
				Artist: &MBArtist{
					ID:   "person-002",
					Name: "Moby",
				},
			},
		},
	}

	meta := a.mapArtist(context.Background(), mb)

	found := false
	for _, alias := range meta.Aliases {
		if alias == "Moby" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'Moby' in aliases, got %v", meta.Aliases)
	}
}

func TestMapArtist_AlsoPerformsAs_NoDuplicateWithExistingAlias(t *testing.T) {
	a := newTestAdapter(t, "http://localhost:0")
	mb := &MBArtist{
		ID:   "person-001",
		Name: "Main Name",
		Type: "Person",
		Aliases: []MBAlias{
			{Name: "Stage Name", Type: "artist name"},
		},
		Relations: []MBRelation{
			{
				Type:      "is person",
				Direction: "forward",
				Artist: &MBArtist{
					ID:   "person-002",
					Name: "Stage Name",
				},
			},
		},
	}

	meta := a.mapArtist(context.Background(), mb)

	count := 0
	for _, alias := range meta.Aliases {
		if alias == "Stage Name" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 'Stage Name' exactly once in aliases, found %d times in %v", count, meta.Aliases)
	}
}

func TestMapArtist_AlsoPerformsAs_DoesNotAddSelfName(t *testing.T) {
	a := newTestAdapter(t, "http://localhost:0")
	mb := &MBArtist{
		ID:   "person-001",
		Name: "Same Name",
		Type: "Person",
		Relations: []MBRelation{
			{
				Type:      "is person",
				Direction: "forward",
				Artist: &MBArtist{
					ID:   "person-002",
					Name: "Same Name",
				},
			},
		},
	}

	meta := a.mapArtist(context.Background(), mb)

	if len(meta.Aliases) != 0 {
		t.Errorf("expected no aliases when relation name matches artist name, got %v", meta.Aliases)
	}
}

// --- deduplicateMembers unit tests ---

func TestDeduplicateMembers_EmptySlice(t *testing.T) {
	result := deduplicateMembers(nil)
	if result != nil {
		t.Errorf("expected nil for nil input, got %v", result)
	}
}

func TestDeduplicateMembers_SingleMember(t *testing.T) {
	members := []provider.MemberInfo{{Name: "Solo", MBID: "m1"}}
	result := deduplicateMembers(members)
	if len(result) != 1 || result[0].Name != "Solo" {
		t.Errorf("expected single member unchanged, got %v", result)
	}
}

func TestDeduplicateMembers_NoMBID(t *testing.T) {
	// Members without MBIDs should not be merged with each other.
	members := []provider.MemberInfo{
		{Name: "Unknown A"},
		{Name: "Unknown B"},
	}
	result := deduplicateMembers(members)
	if len(result) != 2 {
		t.Errorf("expected 2 members without MBIDs kept separate, got %d", len(result))
	}
}

func TestDeduplicateMembers_NoMBIDIdenticalNameKeptSeparate(t *testing.T) {
	// Two members with the same name, empty join date, and no MBID should be
	// kept as separate entries (not merged), even though their names collide.
	members := []provider.MemberInfo{
		{
			Name:        "John Smith",
			MBID:        "",
			DateJoined:  "",
			Instruments: []string{"guitar"},
		},
		{
			Name:        "John Smith",
			MBID:        "",
			DateJoined:  "",
			Instruments: []string{"drums"},
		},
	}

	result := deduplicateMembers(members)

	if len(result) != 2 {
		t.Fatalf("expected 2 separate members, got %d", len(result))
	}
	if result[0].Instruments[0] != "guitar" {
		t.Errorf("expected first member to play guitar, got %q", result[0].Instruments[0])
	}
	if result[1].Instruments[0] != "drums" {
		t.Errorf("expected second member to play drums, got %q", result[1].Instruments[0])
	}
}

func TestDeduplicateMembers_DuplicateMBIDKeepsFirstName(t *testing.T) {
	// Two entries for the same member (same MBID) with different name variants.
	// MusicBrainz relation stubs carry no alias/locale data, so locale-aware
	// selection is not possible. The first canonical name seen is kept (#1020).
	members := []provider.MemberInfo{
		{
			Name:       "Taro Yamada",
			MBID:       "aaa-bbb-ccc",
			DateJoined: "2000",
			IsActive:   true,
		},
		{
			Name:       "Alternative Name",
			MBID:       "aaa-bbb-ccc",
			DateJoined: "2005",
			IsActive:   false,
		},
	}

	result := deduplicateMembers(members)

	if len(result) != 1 {
		t.Fatalf("expected 1 member, got %d", len(result))
	}
	if result[0].Name != "Taro Yamada" {
		t.Errorf("expected first canonical name %q to be kept, got %q", "Taro Yamada", result[0].Name)
	}
	// Verify merge still works: should be active since first entry was active.
	if !result[0].IsActive {
		t.Error("expected merged member to be active")
	}
}

func TestIsGroupType(t *testing.T) {
	tests := []struct {
		mbType string
		want   bool
	}{
		{"Group", true},
		{"Orchestra", true},
		{"Choir", true},
		{"Person", false},
		{"Character", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := isGroupType(tt.mbType); got != tt.want {
			t.Errorf("isGroupType(%q) = %v, want %v", tt.mbType, got, tt.want)
		}
	}
}

// --- mergeDateRanges unit tests ---

func TestMergeDateRanges_SinglePeriod(t *testing.T) {
	earliest, latest := mergeDateRanges([][2]string{{"1990", "2000"}})
	if earliest != "1990" || latest != "2000" {
		t.Errorf("expected 1990-2000, got %s-%s", earliest, latest)
	}
}

func TestMergeDateRanges_OpenEnded(t *testing.T) {
	earliest, latest := mergeDateRanges([][2]string{
		{"1990", "1995"},
		{"2000", ""},
	})
	if earliest != "1990" || latest != "" {
		t.Errorf("expected 1990-(open), got %s-%s", earliest, latest)
	}
}

func TestMergeDateRanges_MultipleClosed(t *testing.T) {
	earliest, latest := mergeDateRanges([][2]string{
		{"2000", "2005"},
		{"1980", "1990"},
	})
	if earliest != "1980" || latest != "2005" {
		t.Errorf("expected 1980-2005, got %s-%s", earliest, latest)
	}
}

func TestMergeDateRanges_NilAndEmpty(t *testing.T) {
	e1, l1 := mergeDateRanges(nil)
	if e1 != "" || l1 != "" {
		t.Errorf("nil input: expected both empty, got %q %q", e1, l1)
	}

	e2, l2 := mergeDateRanges([][2]string{})
	if e2 != "" || l2 != "" {
		t.Errorf("empty input: expected both empty, got %q %q", e2, l2)
	}
}

// --- #964: member name localization via MusicBrainz aliases ---

// TestGetArtist_MemberNamePromotion drives member alias localization through
// GetArtist, verifying each table case from the issue: language match,
// no-match fallback, cache-hit behavior, and fetch failure tolerance.
func TestGetArtist_MemberNamePromotion(t *testing.T) {
	tests := []struct {
		name      string
		langs     []string
		wantThom  string
		wantJonny string
		wantColin string
	}{
		{
			name:      "JapanesePreferenceLocalizesMembers",
			langs:     []string{"ja"},
			wantThom:  "トム・ヨーク",
			wantJonny: "ジョニー・グリーンウッド",
			// Colin has no aliases, so his canonical name is retained.
			wantColin: "Colin Greenwood",
		},
		{
			name:      "UnmatchedPreferenceKeepsCanonicalName",
			langs:     []string{"de"},
			wantThom:  "Thom Yorke",
			wantJonny: "Jonny Greenwood",
			wantColin: "Colin Greenwood",
		},
		{
			name:      "NoPreferencesSkipsMemberFetch",
			langs:     nil,
			wantThom:  "Thom Yorke",
			wantJonny: "Jonny Greenwood",
			wantColin: "Colin Greenwood",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			defer srv.Close()
			a := newTestAdapter(t, srv.URL)

			ctx := context.Background()
			if len(tc.langs) > 0 {
				ctx = provider.WithMetadataLanguages(ctx, tc.langs)
			}

			meta, err := a.GetArtist(ctx, "a74b1b7f-71a5-4011-9441-d0b5e4122711")
			if err != nil {
				t.Fatalf("GetArtist: %v", err)
			}

			byMBID := make(map[string]string, len(meta.Members))
			for _, m := range meta.Members {
				byMBID[m.MBID] = m.Name
			}

			if got := byMBID["8bfac288-ccc5-448d-9573-c33ea2aa5c30"]; got != tc.wantThom {
				t.Errorf("Thom Yorke: got %q, want %q", got, tc.wantThom)
			}
			if got := byMBID["member-002"]; got != tc.wantJonny {
				t.Errorf("Jonny Greenwood: got %q, want %q", got, tc.wantJonny)
			}
			if got := byMBID["member-003"]; got != tc.wantColin {
				t.Errorf("Colin Greenwood: got %q, want %q", got, tc.wantColin)
			}
		})
	}
}

// TestGetArtist_MemberAliasFetchFailureFallsBack verifies that a per-member
// upstream failure does not block the overall artist refresh and that the
// member retains the canonical (non-localized) name returned in the primary
// artist payload.
func TestGetArtist_MemberAliasFetchFailureFallsBack(t *testing.T) {
	// Serve a custom artist payload that includes a member whose MBID always
	// returns 503. The rest of the artist should still localize where it can.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mbid := strings.TrimPrefix(r.URL.Path, "/artist/")
		switch mbid {
		case "main-artist":
			w.Write([]byte(`{
				"id": "main-artist",
				"type": "Group",
				"name": "Test Band",
				"sort-name": "Test Band",
				"relations": [
					{"type": "member of band", "target-type": "artist", "direction": "backward",
					 "artist": {"id": "member-error", "name": "Broken Member", "sort-name": "Member, Broken", "type": "Person"}},
					{"type": "member of band", "target-type": "artist", "direction": "backward",
					 "artist": {"id": "8bfac288-ccc5-448d-9573-c33ea2aa5c30", "name": "Thom Yorke", "sort-name": "Yorke, Thom", "type": "Person"}}
				]
			}`))
		case "member-error":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "8bfac288-ccc5-448d-9573-c33ea2aa5c30":
			w.Write(loadFixture(t, "member_thom_yorke.json"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja"})

	meta, err := a.GetArtist(ctx, "main-artist")
	if err != nil {
		t.Fatalf("GetArtist should not fail because one member alias fetch failed: %v", err)
	}
	if len(meta.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(meta.Members))
	}

	var broken, thom string
	for _, m := range meta.Members {
		switch m.MBID {
		case "member-error":
			broken = m.Name
		case "8bfac288-ccc5-448d-9573-c33ea2aa5c30":
			thom = m.Name
		}
	}
	if broken != "Broken Member" {
		t.Errorf("broken member: expected canonical name retained, got %q", broken)
	}
	if thom != "トム・ヨーク" {
		t.Errorf("good member: expected Japanese promotion, got %q", thom)
	}
}

// TestLocalizeMembers_CacheAvoidsRefetch verifies the per-request cache
// prevents redundant upstream calls when the same member MBID appears more
// than once in a single refresh.
func TestLocalizeMembers_CacheAvoidsRefetch(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "member_thom_yorke.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja"})

	members := []provider.MemberInfo{
		{MBID: "8bfac288-ccc5-448d-9573-c33ea2aa5c30", Name: "Thom Yorke"},
		{MBID: "8bfac288-ccc5-448d-9573-c33ea2aa5c30", Name: "Thom Yorke"},
		{MBID: "8bfac288-ccc5-448d-9573-c33ea2aa5c30", Name: "Thom Yorke"},
	}
	a.localizeMembers(ctx, members, []string{"ja"}, newMemberAliasCache())

	if hits != 1 {
		t.Errorf("expected exactly 1 upstream fetch due to cache, got %d", hits)
	}
	for _, m := range members {
		if m.Name != "トム・ヨーク" {
			t.Errorf("expected all members localized, got %q", m.Name)
		}
	}
}

// TestLocalizeMembers_RateLimiterIsInvoked ensures the member-alias path
// routes through the shared rate limiter. We install a 1 req/sec limiter
// and verify two sequential lookups take at least ~1 second in aggregate.
func TestLocalizeMembers_RateLimiterIsInvoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "member_thom_yorke.json"))
	}))
	defer srv.Close()

	limiter := provider.NewRateLimiterMap()
	// Real MB production rate; burst 1 means the second call waits ~1 second.
	limiter.SetLimit(provider.NameMusicBrainz, 1)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL)
	// See newTestAdapter for why the SafeClient override is required.
	a.client = &http.Client{Timeout: 10 * time.Second}

	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja"})
	members := []provider.MemberInfo{
		{MBID: "mbid-a", Name: "A"},
		{MBID: "mbid-b", Name: "B"},
	}

	start := time.Now()
	a.localizeMembers(ctx, members, []string{"ja"}, newMemberAliasCache())
	elapsed := time.Since(start)

	// Two distinct MBIDs; burst=1 means the second Wait blocks ~1s. Allow
	// some slack for CI. If the limiter were bypassed this would be ms-scale.
	if elapsed < 800*time.Millisecond {
		t.Errorf("expected rate-limited sequential fetch (>=800ms), got %v", elapsed)
	}
}

// TestSelectMemberAlias_LegalNameFiltered verifies that aliases tagged
// Type="Legal name" are excluded at every tier. MusicBrainz uses this type
// to mark birth/legal names; surfacing them without consent is a privacy
// concern. See GAMO (3e959bbb) for the real-world case: the JA primary alias
// is the legal name (蒲生俊貴) and the JA non-primary alias is the stage name
// (ガモー). Selection must prefer the stage name even though it's non-primary.
func TestSelectMemberAlias_LegalNameFiltered(t *testing.T) {
	gamoAliases := []MBAlias{
		{Name: "\u30ac\u30e2\u30fc", Locale: "ja", Primary: false, Type: "Artist name"},
		{Name: "\u84b2\u751f\u4fca\u8cb4", Locale: "ja", Primary: true, Type: "Legal name"},
	}
	got, ok := selectMemberAlias(gamoAliases, []string{"ja", "fr", "en"})
	if !ok {
		t.Fatal("expected an alias to be selected")
	}
	if got.Name != "\u30ac\u30e2\u30fc" {
		t.Errorf("got %q, want JA stage name (\u30ac\u30e2\u30fc) -- legal name should be filtered", got.Name)
	}
}

// TestSelectMemberAlias_NonPrimaryFallback verifies tier 2: when no primary
// alias matches the language preferences, a non-primary locale-matched alias
// is used. See Terrassy (f5caaf2b): only JA alias is non-primary "Legal name"
// (filtered out), so this test uses a synthesized non-primary "Artist name"
// to verify the tier-2 path itself.
func TestSelectMemberAlias_NonPrimaryFallback(t *testing.T) {
	aliases := []MBAlias{
		// EN primary -- would win in old code.
		{Name: "Stage Name", Locale: "en", Primary: true, Type: "Artist name"},
		// JA non-primary -- preferred under new tier-2 logic.
		{Name: "\u30b9\u30c6\u30fc\u30b8", Locale: "ja", Primary: false, Type: "Artist name"},
	}
	got, ok := selectMemberAlias(aliases, []string{"ja", "en"})
	if !ok {
		t.Fatal("expected an alias to be selected")
	}
	if got.Name != "\u30b9\u30c6\u30fc\u30b8" {
		t.Errorf("got %q, want JA non-primary alias -- tier 2 should pick locale match over primary EN", got.Name)
	}
}

// TestSelectMemberAlias_PrimaryStillWinsWhenLocaleMatches verifies that when
// a primary alias matches the language preferences, tier 2 is not consulted.
// Tier 2 is strictly a fallback for the no-primary-match case.
func TestSelectMemberAlias_PrimaryStillWinsWhenLocaleMatches(t *testing.T) {
	aliases := []MBAlias{
		{Name: "JA Primary", Locale: "ja", Primary: true, Type: "Artist name"},
		{Name: "JA Non-Primary", Locale: "ja", Primary: false, Type: "Artist name"},
	}
	got, ok := selectMemberAlias(aliases, []string{"ja"})
	if !ok {
		t.Fatal("expected an alias to be selected")
	}
	if got.Name != "JA Primary" {
		t.Errorf("got %q, want JA Primary -- primary tier must win when it matches", got.Name)
	}
}

// TestSelectMemberAlias_NoMatch verifies that no alias is selected when
// nothing matches the preferences (after filtering out legal names).
func TestSelectMemberAlias_NoMatch(t *testing.T) {
	aliases := []MBAlias{
		{Name: "Birth Name", Locale: "ja", Primary: true, Type: "Legal name"},
		{Name: "EN Only", Locale: "en", Primary: true, Type: "Artist name"},
	}
	_, ok := selectMemberAlias(aliases, []string{"ja", "fr"})
	if ok {
		t.Error("expected no selection: legal name filtered, EN doesn't match [ja, fr]")
	}
}

// TestSelectMemberAlias_TiesPreserveFirstSeen verifies the deterministic
// tie-break for aliases at the same composite score: the first one
// encountered in the input slice wins. MB sometimes returns multiple
// JA-locale primary "Artist name" aliases for a single member; pinning
// "first wins" prevents a future refactor from silently flipping the
// behavior with a `<=` comparator change.
func TestSelectMemberAlias_TiesPreserveFirstSeen(t *testing.T) {
	aliases := []MBAlias{
		{Name: "First JA Primary", Locale: "ja", Primary: true, Type: "Artist name"},
		{Name: "Second JA Primary", Locale: "ja", Primary: true, Type: "Artist name"},
	}
	got, ok := selectMemberAlias(aliases, []string{"ja"})
	if !ok {
		t.Fatal("expected an alias to be selected")
	}
	if got.Name != "First JA Primary" {
		t.Errorf("got %q, want First JA Primary -- ties must resolve to first-seen", got.Name)
	}
}

// TestSelectMemberAlias_EmptyTypeIsEligible verifies that aliases with an
// empty/missing Type field are eligible for selection. MusicBrainz often
// emits aliases with no type (especially for older entries); the Legal name
// filter must use case-insensitive equality (not substring) so an empty
// Type doesn't accidentally match.
func TestSelectMemberAlias_EmptyTypeIsEligible(t *testing.T) {
	aliases := []MBAlias{
		{Name: "Untyped JA Alias", Locale: "ja", Primary: true, Type: ""},
	}
	got, ok := selectMemberAlias(aliases, []string{"ja"})
	if !ok {
		t.Fatal("expected the untyped alias to be selected")
	}
	if got.Name != "Untyped JA Alias" {
		t.Errorf("got %q, want the untyped alias", got.Name)
	}
}

// TestSelectMemberAlias_TopPreferenceRanking verifies that within a single
// tier, language preference rank determines the winner.
func TestSelectMemberAlias_TopPreferenceRanking(t *testing.T) {
	aliases := []MBAlias{
		{Name: "EN Name", Locale: "en", Primary: true, Type: "Artist name"},
		{Name: "FR Name", Locale: "fr", Primary: true, Type: "Artist name"},
		{Name: "JA Name", Locale: "ja", Primary: true, Type: "Artist name"},
	}
	got, ok := selectMemberAlias(aliases, []string{"ja", "fr", "en"})
	if !ok {
		t.Fatal("expected an alias to be selected")
	}
	if got.Name != "JA Name" {
		t.Errorf("got %q, want JA Name (top pref)", got.Name)
	}
}

// TestLocalizeMembers_GAMOScenario reproduces the GAMO (3e959bbb) case from
// the UAT log. Canonical name is Latin "GAMO"; MB has both a JA primary
// "Legal name" (蒲生俊貴) and a JA non-primary "Artist name" (ガモー). The fix
// must promote to the stage name, not the legal name, even though primary
// would otherwise win.
func TestLocalizeMembers_GAMOScenario(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "3e959bbb-5fe3-4599-9b78-d8e75ec8e10c",
			"name": "GAMO",
			"sort-name": "GAMO",
			"aliases": [
				{"name": "\u30ac\u30e2\u30fc", "locale": "ja", "primary": false, "type": "Artist name"},
				{"name": "\u84b2\u751f\u4fca\u8cb4", "locale": "ja", "primary": true, "type": "Legal name"}
			]
		}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja", "fr", "en"})
	members := []provider.MemberInfo{
		{MBID: "3e959bbb-5fe3-4599-9b78-d8e75ec8e10c", Name: "GAMO"},
	}

	a.localizeMembers(ctx, members, []string{"ja", "fr", "en"}, newMemberAliasCache())

	want := "\u30ac\u30e2\u30fc"
	if members[0].Name != want {
		t.Errorf("got %q, want %q (JA stage name, not the JA legal name)", members[0].Name, want)
	}
}

// TestLocalizeMembers_TerrassyScenario reproduces the Terrassy (f5caaf2b)
// case: the only JA alias is a non-primary "Legal name". The Legal name
// filter takes precedence over the non-primary fallback, so the canonical
// Latin name must be preserved (no false promotion to a legal name).
func TestLocalizeMembers_TerrassyScenario(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "f5caaf2b-b162-456d-a617-bf33260ee5ea",
			"name": "Terrassy",
			"sort-name": "Terrassy",
			"aliases": [
				{"name": "\u5bfa\u5e2b\u5fb9", "locale": "ja", "primary": false, "type": "Legal name"}
			]
		}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja", "fr", "en"})
	members := []provider.MemberInfo{
		{MBID: "f5caaf2b-b162-456d-a617-bf33260ee5ea", Name: "Terrassy"},
	}

	a.localizeMembers(ctx, members, []string{"ja", "fr", "en"}, newMemberAliasCache())

	if members[0].Name != "Terrassy" {
		t.Errorf("got %q, want %q -- the only JA alias is a Legal name, must not promote", members[0].Name, "Terrassy")
	}
}

// TestLocalizeMembers_AsakuraScenario reproduces the 朝倉弘一 (70e53eda) case.
// Canonical is kanji; only alias is EN primary "Hirokazu Asakura". The
// script-skip optimization preserves the kanji canonical so we never even
// reach the alias selection -- which would otherwise have erroneously
// promoted to the EN name (the bug observed in the original UAT log).
func TestLocalizeMembers_AsakuraScenario(t *testing.T) {
	// Server should NOT be hit -- script-skip must fire before any fetch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected alias fetch for kanji canonical: %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja", "fr", "en"})
	members := []provider.MemberInfo{
		{MBID: "70e53eda-175e-4005-80dc-e86a0d8132b9", Name: "\u671d\u5009\u5f18\u4e00"},
	}

	a.localizeMembers(ctx, members, []string{"ja", "fr", "en"}, newMemberAliasCache())

	if members[0].Name != "\u671d\u5009\u5f18\u4e00" {
		t.Errorf("kanji canonical should be preserved, got %q", members[0].Name)
	}
}

// TestLocalizeMembers_SkipsFetchWhenCanonicalInPreferredScript verifies the
// script-skip optimization: when a member's canonical name is already in a
// script that matches the user's top language preference, no alias fetch is
// performed. This is the fix for the 1 req/sec rate-limit penalty on
// Japanese bands where most members already have Japanese-script canonical
// names.
func TestLocalizeMembers_SkipsFetchWhenCanonicalInPreferredScript(t *testing.T) {
	// The test server must never be hit -- if it is, the optimization broke.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected alias fetch for member whose canonical name matches top pref script: %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja", "fr", "en"})

	members := []provider.MemberInfo{
		// Kanji -- dominant script Han, matches ja -> skip.
		{MBID: "mbid-kanji", Name: "\u671d\u5009\u5f18\u4e00"},
		// Hiragana -- matches ja -> skip.
		{MBID: "mbid-hiragana", Name: "\u3072\u3089\u304c\u306a"},
		// Katakana -- matches ja -> skip.
		{MBID: "mbid-katakana", Name: "\u30c8\u30e0\u30fb\u30e8\u30fc\u30af"},
	}

	a.localizeMembers(ctx, members, []string{"ja", "fr", "en"}, newMemberAliasCache())

	// Names should be untouched because we skipped the fetch.
	wants := []string{"\u671d\u5009\u5f18\u4e00", "\u3072\u3089\u304c\u306a", "\u30c8\u30e0\u30fb\u30e8\u30fc\u30af"}
	for i, m := range members {
		if m.Name != wants[i] {
			t.Errorf("member[%d] name = %q, want %q (should be preserved)", i, m.Name, wants[i])
		}
	}
}

// TestLocalizeMembers_DoesNotSkipWhenCanonicalMatchesOnlySecondaryPref
// verifies that the optimization keys on the TOP preference only. A Latin
// name under prefs [ja, fr, en] must still trigger a fetch: Latin matches en
// and fr, but the user preferred ja, and a JA alias might exist.
//
// Hit counting uses sync/atomic so the test goroutine can safely read the
// counter after localizeMembers returns even when the server handler runs on
// a different goroutine.
func TestLocalizeMembers_DoesNotSkipWhenCanonicalMatchesOnlySecondaryPref(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "member_thom_yorke.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja", "fr", "en"})

	// "Thom Yorke" is Latin. Under prefs [ja, fr, en], top pref is ja and
	// Latin does not match ja -- the fetch must proceed so a Japanese alias
	// can be discovered.
	members := []provider.MemberInfo{
		{MBID: "8bfac288-ccc5-448d-9573-c33ea2aa5c30", Name: "Thom Yorke"},
	}

	a.localizeMembers(ctx, members, []string{"ja", "fr", "en"}, newMemberAliasCache())

	if got := hits.Load(); got != 1 {
		t.Errorf("expected 1 fetch (top pref ja doesn't match Latin canonical), got %d", got)
	}
	if members[0].Name != "\u30c8\u30e0\u30fb\u30e8\u30fc\u30af" {
		t.Errorf("expected ja promotion after fetch, got %q", members[0].Name)
	}
}

// TestLocalizeMembers_PromotesLatinAliasUnderLatinPref is the #1137 regression
// witness. Before the fix, the script-skip optimization fired whenever the
// canonical name's script matched the top pref's expected script -- which
// incorrectly included Latin-canonical members under a Latin-family top pref
// (e.g. Tokyo Ska Paradise Orchestra's "NARGO" under ["en"]). The skip
// suppressed the alias fetch, so typography/spelling/capitalization
// refinements available in MusicBrainz en-locale aliases were never promoted.
//
// The fix gates the skip on LocaleExpectsOnlyNonLatinScript(topPref): under a
// Latin-family pref, a Latin alias can still meaningfully improve the name, so
// the fetch must proceed.
func TestLocalizeMembers_PromotesLatinAliasUnderLatinPref(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "member_nargo.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"en"})

	members := []provider.MemberInfo{
		{MBID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Name: "NARGO"},
	}

	a.localizeMembers(ctx, members, []string{"en"}, newMemberAliasCache())

	if got := hits.Load(); got != 1 {
		t.Errorf("expected 1 fetch (en pref must not skip Latin-canonical fetch), got %d", got)
	}
	if members[0].Name != "Nargo" {
		t.Errorf("expected en primary alias promotion, got %q", members[0].Name)
	}
}

// TestLocalizeMembers_EmptyLangPrefsStillFetches verifies that an empty
// langPrefs slice keeps localizeMembers on the safe path: topPref defaults to
// "", the skip guard's topPref != "" check fails, and the alias fetch
// proceeds. This protects against a future refactor that replaces the guarded
// init with an unguarded langPrefs[0] index, which would panic on an empty
// slice. selectMemberAlias returns false under nil prefs so no promotion
// occurs, but the fetch still runs to exercise the alias-cache plumbing.
func TestLocalizeMembers_EmptyLangPrefsStillFetches(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "member_nargo.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	members := []provider.MemberInfo{
		{MBID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Name: "NARGO"},
	}

	a.localizeMembers(context.Background(), members, []string{}, newMemberAliasCache())

	if got := hits.Load(); got != 1 {
		t.Errorf("expected 1 fetch (empty prefs must not trigger skip), got %d", got)
	}
	if members[0].Name != "NARGO" {
		t.Errorf("expected canonical preserved (no promotion under empty prefs), got %q", members[0].Name)
	}
}

// TestLocalizeMembers_SortNameFallback exercises the romanization fallback:
// when no tagged alias wins AND the canonical is in a non-Latin script AND
// the MB sort-name is Latin AND the user's top pref accepts Latin, the
// promoted display name is derived from the sort-name (reversed from MB's
// "Family, Given" convention to "Given Family" display form). This covers
// the common TSPO-style case where MB has a romanization in sort-name but
// no dedicated en alias.
func TestLocalizeMembers_SortNameFallback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "member_aoki.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"en"})

	members := []provider.MemberInfo{
		{MBID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Name: "\u9752\u6728\u9054\u4e4b"},
	}

	a.localizeMembers(ctx, members, []string{"en"}, newMemberAliasCache())

	if got := hits.Load(); got != 1 {
		t.Errorf("expected 1 fetch, got %d", got)
	}
	if members[0].Name != "Tatsuyuki Aoki" {
		t.Errorf("expected sort-name reversal to \"Tatsuyuki Aoki\", got %q", members[0].Name)
	}
}

// TestLocalizeMembers_SortNameFallbackSkippedUnderNonLatinPref verifies the
// inverse gate: when the user's top pref is non-Latin (ja, zh, ko, ...), a
// Latin sort-name must NOT be promoted over a non-Latin canonical. The user
// asked for Japanese, so Japanese is what they get even if MB has a
// romanization they could read.
func TestLocalizeMembers_SortNameFallbackSkippedUnderNonLatinPref(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "member_aoki.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"ja"})

	canonical := "\u9752\u6728\u9054\u4e4b"
	members := []provider.MemberInfo{
		{MBID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Name: canonical},
	}

	a.localizeMembers(ctx, members, []string{"ja"}, newMemberAliasCache())

	if members[0].Name != canonical {
		t.Errorf("expected canonical preserved under ja pref, got %q", members[0].Name)
	}
}

// TestLocalizeMembers_SortNameFallbackFiresUnderMixedScriptPref witnesses
// the sr (Serbian) mixed-script case: Serbian allows both Cyrillic and Latin
// by default (localeScripts["sr"] = {Cyrillic, Latin}), so a Cyrillic
// canonical + Latin sort-name under [sr] pref satisfies all six gating
// conditions and promotes via reversal. This is the intended behavior --
// under a mixed-script pref the user has indicated they accept Latin output.
// Locking it in protects against a future LocaleExpectsOnlyNonLatinScript or
// ScriptSatisfiesLocale change silently flipping semantics for mixed-script
// locales.
func TestLocalizeMembers_SortNameFallbackFiresUnderMixedScriptPref(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "member_cyrillic.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"sr"})

	canonical := "\u041f\u0435\u0442\u0440\u043e\u0432\u0438\u045b"
	members := []provider.MemberInfo{
		{MBID: "dddddddd-dddd-dddd-dddd-dddddddddddd", Name: canonical},
	}

	a.localizeMembers(ctx, members, []string{"sr"}, newMemberAliasCache())

	if members[0].Name != "Marko Petrovic" {
		t.Errorf("expected sr mixed-script pref to promote Latin sort-name, got %q", members[0].Name)
	}
}

// TestLocalizeMembers_SortNameFallbackNotAppliedToLatinCanonical verifies
// the canonical-script gate: the reversal is a no-op for Latin canonicals
// (where it would just flip first/last name for Western artists). Skipping
// this case is the reason the fallback only fires for non-Latin canonicals.
func TestLocalizeMembers_SortNameFallbackNotAppliedToLatinCanonical(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"cccccccc-cccc-cccc-cccc-cccccccccccc","type":"Person","name":"Chris Martin","sort-name":"Martin, Chris","aliases":[]}`))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"en"})

	members := []provider.MemberInfo{
		{MBID: "cccccccc-cccc-cccc-cccc-cccccccccccc", Name: "Chris Martin"},
	}

	a.localizeMembers(ctx, members, []string{"en"}, newMemberAliasCache())

	if members[0].Name != "Chris Martin" {
		t.Errorf("expected Latin canonical untouched by sort-name fallback, got %q", members[0].Name)
	}
}

// TestLocalizeMembers_RomanizationFallbackDisabled verifies that when the
// metadata_name_romanization_fallback preference is explicitly set to false the
// sort-name romanization path in localizeMembers is skipped entirely, leaving
// the canonical non-Latin name unchanged even when all other gating conditions
// (non-Latin canonical, Latin sort-name, Latin-accepting top pref) would
// normally trigger the promotion.
func TestLocalizeMembers_RomanizationFallbackDisabled(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Return the member_aoki fixture: non-Latin canonical, Latin sort-name,
		// no aliases -- this is the exact scenario that triggers the fallback
		// when the preference is enabled.
		w.Write(loadFixture(t, "member_aoki.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	// Top pref is "en" (Latin-accepting), romanization fallback explicitly disabled.
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"en"})
	ctx = provider.WithNameRomanizationFallback(ctx, false)

	canonical := "青木達之" // 青木達之
	members := []provider.MemberInfo{
		{MBID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Name: canonical},
	}

	a.localizeMembers(ctx, members, []string{"en"}, newMemberAliasCache())

	// The alias fetch still runs (no alias won), but the sort-name reversal
	// must not fire because the preference gate blocks it.
	if got := hits.Load(); got != 1 {
		t.Errorf("expected 1 alias fetch, got %d", got)
	}
	if members[0].Name != canonical {
		t.Errorf("expected canonical preserved when romanization fallback disabled, got %q", members[0].Name)
	}
}

// TestLocalizeMembers_RomanizationFallbackEnabledExplicitly verifies that
// setting the preference to true explicitly (as opposed to the default
// unset-means-true path) still fires the sort-name promotion. This tests
// provider.WithNameRomanizationFallback(ctx, true) directly.
func TestLocalizeMembers_RomanizationFallbackEnabledExplicitly(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "member_aoki.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx := provider.WithMetadataLanguages(context.Background(), []string{"en"})
	ctx = provider.WithNameRomanizationFallback(ctx, true)

	members := []provider.MemberInfo{
		{MBID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Name: "青木達之"},
	}

	a.localizeMembers(ctx, members, []string{"en"}, newMemberAliasCache())

	if members[0].Name != "Tatsuyuki Aoki" {
		t.Errorf("expected sort-name reversal with explicit true pref, got %q", members[0].Name)
	}
}

// TestRomanizeFromSortName exercises the MusicBrainz sort-name reversal
// helper. MB stores sort-name as "Family, Given"; display form is
// "Given Family". Only well-formed two-part sort-names return ok=true;
// empty, whitespace-only, single-token, multi-comma, and partial inputs
// return ("", false) so the caller can treat them as "no fallback
// available" instead of promoting a raw sort-form like "Family," or
// "Smith, Jr., John" into a display name.
func TestRomanizeFromSortName(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"standard two-part", "Aoki, Tatsuyuki", "Tatsuyuki Aoki", true},
		{"whitespace around tokens", "  Yorke,   Thom  ", "Thom Yorke", true},
		{"single token rejected", "Madonna", "", false},
		{"empty rejected", "", "", false},
		{"whitespace only rejected", "   ", "", false},
		{"multi-comma rejected", "Smith, Jr., John", "", false},
		{"empty family rejected", ", Given", "", false},
		{"empty given rejected", "Family,", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := romanizeFromSortName(tt.in)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("romanizeFromSortName(%q) = (%q, %v), want (%q, %v)",
					tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestLocalizeMembers_PreservesNonMBIDMembers asserts that members without
// an MBID (user-added entries that have not been matched to MusicBrainz)
// keep whatever name they arrived with. Localization only runs for members
// whose MBID can be resolved upstream.
func TestLocalizeMembers_PreservesNonMBIDMembers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no upstream fetch expected for member without MBID, but got request %q", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	members := []provider.MemberInfo{
		{MBID: "", Name: "Custom Manual Entry"},
	}
	a.localizeMembers(context.Background(), members, []string{"ja"}, newMemberAliasCache())

	if members[0].Name != "Custom Manual Entry" {
		t.Errorf("expected user-added member name preserved, got %q", members[0].Name)
	}
}

// --- #1038: MembersAuthoritative wiring ---

// TestMapArtist_MembersAuthoritative verifies that mapArtist sets
// MembersAuthoritative only for confirmed individual artist types (Person,
// Character) and never for group types or unknown/empty types.
//
// Rationale: an individual artist cannot have band members by definition, so
// an empty member list is authoritatively complete. A real band (Type="Group")
// may have sparse relation data in MusicBrainz, so an empty list there means
// "data unavailable", not "no members".
func TestMapArtist_MembersAuthoritative(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		mbType   string
		wantTrue bool
		comment  string
	}{
		{
			// Person type: individual artist, no members possible by definition.
			// An empty member list is complete and authoritative.
			name:     "person_type_is_authoritative",
			mbType:   "Person",
			wantTrue: true,
			comment:  "Person type must set MembersAuthoritative=true",
		},
		{
			// Character type: treated identically to Person for this purpose
			// (e.g. a fictional artist identity). Empty roster is authoritative.
			name:     "character_type_is_authoritative",
			mbType:   "Character",
			wantTrue: true,
			comment:  "Character type must set MembersAuthoritative=true",
		},
		{
			// Group type: real bands have members, but MusicBrainz relation data
			// is often sparse. An empty list for a Group means data is unavailable,
			// not that the band has no members.
			name:     "group_type_is_not_authoritative",
			mbType:   "Group",
			wantTrue: false,
			comment:  "Group type must NOT set MembersAuthoritative",
		},
		{
			// Orchestra: same rationale as Group.
			name:     "orchestra_type_is_not_authoritative",
			mbType:   "Orchestra",
			wantTrue: false,
			comment:  "Orchestra type must NOT set MembersAuthoritative",
		},
		{
			// Choir: same rationale as Group.
			name:     "choir_type_is_not_authoritative",
			mbType:   "Choir",
			wantTrue: false,
			comment:  "Choir type must NOT set MembersAuthoritative",
		},
		{
			// Empty type: unknown, cannot assert completeness.
			name:     "empty_type_is_not_authoritative",
			mbType:   "",
			wantTrue: false,
			comment:  "Empty/unknown type must NOT set MembersAuthoritative",
		},
	}

	for _, tc := range cases {
		tc := tc // capture for parallel sub-test
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newTestAdapter(t, "http://localhost:0")
			mb := &MBArtist{
				ID:   "test-mbid",
				Name: "Test Artist",
				Type: tc.mbType,
			}
			meta := a.mapArtist(context.Background(), mb)
			if meta.MembersAuthoritative != tc.wantTrue {
				t.Errorf("%s: MembersAuthoritative = %v, want %v", tc.comment, meta.MembersAuthoritative, tc.wantTrue)
			}
		})
	}
}

// TestMapArtist_YearsActive_PersonBornAndDied verifies the synthesis path for a
// Person-type artist with both lifespan begin and end dates. This is Finding 7:
// an individual with a closed lifespan should produce a "YYYY-YYYY" years_active.
// The test also confirms MembersAuthoritative is set for the Person type.
func TestMapArtist_YearsActive_PersonBornAndDied(t *testing.T) {
	t.Parallel()
	a := newTestAdapter(t, "http://localhost:0")
	mb := &MBArtist{
		ID:   "person-born-died",
		Name: "Deceased Solo Artist",
		Type: "Person",
		LifeSpan: MBLifeSpan{
			Begin: "1942-08-01",
			End:   "2018-03-14",
			Ended: true,
		},
	}
	meta := a.mapArtist(context.Background(), mb)

	// A Person with begin + end dates should synthesize "YYYY-YYYY".
	if meta.YearsActive != "1942-2018" {
		t.Errorf("YearsActive = %q, want %q", meta.YearsActive, "1942-2018")
	}
	// Person type must also set MembersAuthoritative.
	if !meta.MembersAuthoritative {
		t.Error("Person type must set MembersAuthoritative=true")
	}
}

// --- SSRF mirror-allowlist wiring (#2090) ---

// TestMirrorAllowedHost verifies host extraction for the SSRF exemption: the
// default endpoint and unparsable URLs return "" (stay guarded), while a custom
// mirror returns its lowercased hostname (the single exempted host).
func TestMirrorAllowedHost(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"default endpoint", defaultBaseURL, ""},
		{"default with trailing slash", defaultBaseURL + "/", ""},
		{"unparsable URL", "http://[::1", ""},
		{"private mirror", "http://192.0.2.10:5000/ws/2", "192.0.2.10"},
		{"loopback mirror", "http://127.0.0.1:5000", "127.0.0.1"},
		{"uppercase host lowercased", "http://Mirror.LAN:5000/ws/2", "mirror.lan"},
		{"public custom mirror", "https://mb.example.com/ws/2", "mb.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mirrorAllowedHost(tc.baseURL); got != tc.want {
				t.Errorf("mirrorAllowedHost(%q) = %q, want %q", tc.baseURL, got, tc.want)
			}
		})
	}
}

// TestDefaultConstructorGuardsPrivateTargets confirms a default-constructed
// adapter wires the plain SSRF-guarded client: it rejects a loopback target
// with ErrPrivateAddress, exactly as before this change. The default endpoint
// host is never in any allow-set.
func TestDefaultConstructorGuardsPrivateTargets(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := New(limiter, logger)

	// Drive the adapter's own guarded client against the loopback fixture; the
	// SSRF guard must reject it. (We bypass doRequest's baseURL so the test does
	// not depend on reaching the real musicbrainz.org.)
	resp, err := a.client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("default adapter client reached loopback; want ErrPrivateAddress (guarded)")
	}
	if !errors.Is(err, httpsafe.ErrPrivateAddress) {
		t.Fatalf("default adapter client.Get(loopback): err = %v; want ErrPrivateAddress (guarded)", err)
	}
}

// TestCustomMirrorConstructorReachesLoopback confirms the headline fix: an
// adapter constructed with a custom (loopback) mirror base URL reaches that
// mirror through its allowlisted httpsafe client -- WITHOUT the test overriding
// a.client. TestConnection performs a real request end-to-end.
func TestCustomMirrorConstructorReachesLoopback(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	limiter := provider.NewRateLimiterMap()
	limiter.SetLimit(provider.NameMusicBrainz, 1000)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL)

	if err := a.TestConnection(context.Background()); err != nil {
		t.Fatalf("custom-mirror TestConnection against loopback: err = %v; want success (host allowlisted)", err)
	}
}

// TestSetBaseURLRebuildsClient verifies SetBaseURL swaps the client to match the
// new base: switching to a custom loopback mirror makes it reachable, and
// reverting to the default re-installs the guarded client that re-blocks the
// same loopback target.
func TestSetBaseURLRebuildsClient(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	limiter := provider.NewRateLimiterMap()
	limiter.SetLimit(provider.NameMusicBrainz, 1000)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Start at the default (guarded), then point at the loopback mirror.
	a := New(limiter, logger)
	a.SetBaseURL(srv.URL)
	if err := a.TestConnection(context.Background()); err != nil {
		t.Fatalf("after SetBaseURL(loopback mirror): TestConnection err = %v; want success", err)
	}

	// Revert to default: the client must be guarded again and re-block loopback.
	a.SetBaseURL(a.DefaultBaseURL())
	resp, err := a.client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("after revert-to-default: client reached loopback; want ErrPrivateAddress (re-guarded)")
	}
	if !errors.Is(err, httpsafe.ErrPrivateAddress) {
		t.Fatalf("after revert-to-default: client.Get(loopback) err = %v; want ErrPrivateAddress (re-guarded)", err)
	}
}

// TestSetBaseURLClientReadRaceSafe exercises the concurrency contract: SetBaseURL
// reassigns a.client under a.mu while doRequest reads it under a.mu.RLock.
// Running both concurrently under -race must stay clean. Each request targets
// the loopback fixture via an allowlisted mirror URL, so the read path executes
// fully rather than short-circuiting on a guard rejection.
func TestSetBaseURLClientReadRaceSafe(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	limiter := provider.NewRateLimiterMap()
	limiter.SetLimit(provider.NameMusicBrainz, 100000)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			a.SetBaseURL(srv.URL)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			// Ignore the result; we only care that the concurrent a.client read
			// inside doRequest is race-clean.
			_, _ = a.SearchArtist(context.Background(), "radiohead")
		}
	}()
	wg.Wait()
}
