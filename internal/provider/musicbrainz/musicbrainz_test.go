package musicbrainz

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	result := deduplicateMembers(nil, nil)
	if result != nil {
		t.Errorf("expected nil for nil input, got %v", result)
	}
}

func TestDeduplicateMembers_SingleMember(t *testing.T) {
	members := []provider.MemberInfo{{Name: "Solo", MBID: "m1"}}
	result := deduplicateMembers(members, nil)
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
	result := deduplicateMembers(members, nil)
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

	result := deduplicateMembers(members, nil)

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

func TestDeduplicateMembers_LocalePreference(t *testing.T) {
	// Two entries for the same member (same MBID) with different name variants.
	// When language preferences favor "ja", the Japanese name should be selected.
	members := []provider.MemberInfo{
		{
			Name:       "Taro Yamada",
			MBID:       "aaa-bbb-ccc",
			DateJoined: "2000",
			IsActive:   true,
		},
		{
			Name:       "ja",
			MBID:       "aaa-bbb-ccc",
			DateJoined: "2005",
			IsActive:   false,
		},
	}

	// With "ja" preference, the name "ja" scores 0 (exact match at index 0)
	// while "Taro Yamada" scores -1 (no match). So "ja" wins.
	langPrefs := []string{"ja"}
	result := deduplicateMembers(members, langPrefs)

	if len(result) != 1 {
		t.Fatalf("expected 1 member, got %d", len(result))
	}
	if result[0].Name != "ja" {
		t.Errorf("expected locale-preferred name %q, got %q", "ja", result[0].Name)
	}
	// Verify merge still works: should be active since first entry was active.
	if !result[0].IsActive {
		t.Error("expected merged member to be active")
	}
}

func TestDeduplicateMembers_LocalePreference_NoPrefs(t *testing.T) {
	// Without language preferences, the first name should be retained.
	members := []provider.MemberInfo{
		{
			Name: "First Name",
			MBID: "aaa-bbb-ccc",
		},
		{
			Name: "Second Name",
			MBID: "aaa-bbb-ccc",
		},
	}

	result := deduplicateMembers(members, nil)

	if len(result) != 1 {
		t.Fatalf("expected 1 member, got %d", len(result))
	}
	if result[0].Name != "First Name" {
		t.Errorf("expected first name to be retained, got %q", result[0].Name)
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
