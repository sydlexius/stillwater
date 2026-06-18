package deezer

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
		case r.URL.Path == "/search/artist":
			q := r.URL.Query().Get("q")
			if q == "no-results-query" {
				w.Write([]byte(`{"data":[],"total":0}`))
				return
			}
			w.Write(loadFixture(t, "search_radiohead.json"))

		case strings.HasPrefix(r.URL.Path, "/artist/") && strings.HasSuffix(r.URL.Path, "/albums"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/artist/"), "/albums")
			switch id {
			case "not-found":
				w.WriteHeader(http.StatusNotFound)
			case "8888888":
				// Empty discography.
				w.Write([]byte(`{"data":[],"total":0}`))
			case "7777777":
				// Two-page discography to exercise pagination via index/limit.
				// Page size in the adapter is 50; the server returns exactly 50
				// on index=0 (with total=51) then the remainder on index=50.
				index := r.URL.Query().Get("index")
				if index == "0" {
					w.Write([]byte(paginatedAlbumsPage(0, 50, 51)))
				} else {
					w.Write([]byte(paginatedAlbumsPage(50, 1, 51)))
				}
			default:
				w.Write(loadFixture(t, "artist_albums_radiohead.json"))
			}

		case strings.HasPrefix(r.URL.Path, "/artist/"):
			id := strings.TrimPrefix(r.URL.Path, "/artist/")
			switch id {
			case "not-found":
				w.WriteHeader(http.StatusNotFound)
			case "9999999":
				w.Write(loadFixture(t, "artist_no_photo.json"))
			default:
				w.Write(loadFixture(t, "artist_radiohead.json"))
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, baseURL)
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	a.client = &http.Client{Timeout: 10 * time.Second}
	return a
}

func TestName(t *testing.T) {
	a := newTestAdapter(t, "http://localhost")
	if a.Name() != provider.NameDeezer {
		t.Errorf("expected %q, got %q", provider.NameDeezer, a.Name())
	}
}

func TestRequiresAuth(t *testing.T) {
	a := newTestAdapter(t, "http://localhost")
	if a.RequiresAuth() {
		t.Error("expected RequiresAuth to return false")
	}
}

func TestSearchArtist(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	results, err := a.SearchArtist(context.Background(), "radiohead")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %q", results[0].Name)
	}
	if results[0].ProviderID != "4050205" {
		t.Errorf("expected provider ID 4050205, got %q", results[0].ProviderID)
	}
	if results[0].Source != string(provider.NameDeezer) {
		t.Errorf("expected source %q, got %q", provider.NameDeezer, results[0].Source)
	}
	// Score should be computed via NameSimilarity, not hard-coded to 100.
	// "radiohead" vs "Radiohead" is a case-insensitive exact match = 100.
	if results[0].Score != 100 {
		t.Errorf("expected score 100 for exact match, got %d", results[0].Score)
	}
}

func TestSearchArtistFuzzyMatch(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	// Search for a misspelled name to prove scores are computed via
	// NameSimilarity rather than hard-coded to 100.
	results, err := a.SearchArtist(context.Background(), "radiohed")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	// "radiohed" vs "Radiohead": normalized distance=1, maxLen=9,
	// expected score = 100 - (1*100)/9 = 88. Bracket to catch both
	// hardcoding (100) and zero-score bugs.
	if results[0].Score < 80 || results[0].Score > 95 {
		t.Errorf("expected score in [80, 95] for fuzzy match, got %d", results[0].Score)
	}
}

func TestSearchArtistEmpty(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	results, err := a.SearchArtist(context.Background(), "no-results-query")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestSearchArtistEmptyName(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	results, err := a.SearchArtist(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results for empty name")
	}
}

func TestGetArtist(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	meta, err := a.GetArtist(context.Background(), "4050205")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta == nil {
		t.Fatal("expected non-nil metadata")
	}
	if meta.Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %q", meta.Name)
	}
	if meta.ProviderID != "4050205" {
		t.Errorf("expected ProviderID 4050205, got %q", meta.ProviderID)
	}
	if meta.URLs["deezer"] == "" {
		t.Error("expected deezer URL to be set")
	}
}

func TestGetArtistRejectsNonNumericID(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	// MusicBrainz UUIDs should be rejected without making an HTTP call
	_, err := a.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error for non-Deezer ID")
	}
	var notFoundUUID *provider.ErrNotFound
	if !errors.As(err, &notFoundUUID) {
		t.Errorf("expected *provider.ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtistNotFound(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	// "not-found" is non-numeric so the adapter rejects it immediately
	_, err := a.GetArtist(context.Background(), "not-found")
	if err == nil {
		t.Fatal("expected error for non-numeric ID")
	}
	var notFound *provider.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("expected *provider.ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetImages(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	images, err := a.GetImages(context.Background(), "4050205")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(images) == 0 {
		t.Fatal("expected at least one image")
	}
	if images[0].Type != provider.ImageThumb {
		t.Errorf("expected thumb type, got %q", images[0].Type)
	}
	if images[0].Source != string(provider.NameDeezer) {
		t.Errorf("expected source %q, got %q", provider.NameDeezer, images[0].Source)
	}
}

func TestGetImagesRejectsNonNumericID(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	_, err := a.GetImages(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error for non-Deezer ID")
	}
}

func TestGetImagesDefaultPhoto(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	// Artist 9999999 has the default placeholder picture (double slash in URL)
	images, err := a.GetImages(context.Background(), "9999999")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("expected 0 images for artist with default placeholder, got %d", len(images))
	}
}

func TestDoRequestRetriesOn429(t *testing.T) {
	// The server always returns 429 with "Retry-After: 0". The zero-second
	// Retry-After makes provider.DoWithRetry compute a zero wait between
	// attempts, so the real clock never actually sleeps and the test runs
	// instantly while still exercising the full retry path. Returning 429 on
	// every request forces DoWithRetry to exhaust its budget, proving the
	// retries are bounded (no retry storm) and surface as
	// *provider.ErrProviderUnavailable.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)

	// "27" is a valid numeric Deezer ID, so the adapter performs the HTTP call
	// instead of short-circuiting with ErrNotFound.
	_, err := a.GetArtist(context.Background(), "27")
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}

	var unavailable *provider.ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Errorf("expected *provider.ErrProviderUnavailable, got %T: %v", err, err)
	}

	// The adapter uses provider.DefaultRetryPolicy(), so the server must be hit
	// exactly MaxAttempts times: one initial attempt plus the bounded retries.
	want := provider.DefaultRetryPolicy().MaxAttempts
	if got := int(hits.Load()); got != want {
		t.Errorf("expected exactly %d requests (MaxAttempts), got %d", want, got)
	}
}

func TestDoRequestRecoversAfter429(t *testing.T) {
	// End-to-end recovery path: the first request is rate-limited (429 with a
	// zero-second Retry-After so the test stays fast), and the retry succeeds
	// with real artist JSON. This proves the adapter does not give up on a
	// transient 429 -- it backs off and then returns the data.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "artist_radiohead.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)

	meta, err := a.GetArtist(context.Background(), "27")
	if err != nil {
		t.Fatalf("expected success after one retry, got error: %v", err)
	}
	if meta == nil || meta.Name == "" {
		t.Fatalf("expected populated artist metadata, got %+v", meta)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("expected 2 requests (one 429, one success), got %d", got)
	}
}

func TestDoRequestWrapsTransportError(t *testing.T) {
	// A connection-level failure (the server is already closed) is not a 429/503,
	// so DoWithRetry returns it as a raw, untyped error. doRequest must wrap it in
	// *provider.ErrProviderUnavailable so callers still see a transient failure.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	a := newTestAdapter(t, url)

	_, err := a.GetArtist(context.Background(), "27")
	var unavailable *provider.ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected *provider.ErrProviderUnavailable, got %T: %v", err, err)
	}
}

func TestDoRequestHonorsCanceledContext(t *testing.T) {
	// A canceled context makes the in-closure limiter wait fail before any HTTP
	// call, surfacing as *provider.ErrProviderUnavailable from the rate-limiter
	// branch. The server should therefore never be contacted.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Write(loadFixture(t, "artist_radiohead.json"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := a.GetArtist(ctx, "27")
	var unavailable *provider.ErrProviderUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected *provider.ErrProviderUnavailable, got %T: %v", err, err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("expected 0 requests (limiter rejected before HTTP), got %d", got)
	}
}

// paginatedAlbumsPage builds an albums-endpoint JSON page with count entries
// whose IDs start at startID, reporting the given total. Used to exercise the
// adapter's index/limit pagination loop in GetReleaseGroups.
func paginatedAlbumsPage(startID, count, total int) string {
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	for i := 0; i < count; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		id := startID + i
		sb.WriteString(`{"id":`)
		sb.WriteString(strconv.Itoa(id))
		sb.WriteString(`,"title":"Album `)
		sb.WriteString(strconv.Itoa(id))
		sb.WriteString(`","release_date":"2000-01-01","record_type":"album"}`)
	}
	sb.WriteString(`],"total":`)
	sb.WriteString(strconv.Itoa(total))
	sb.WriteString(`}`)
	return sb.String()
}

func TestGetReleaseGroups(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	groups, err := a.GetReleaseGroups(context.Background(), "4050205")
	if err != nil {
		t.Fatalf("GetReleaseGroups: %v", err)
	}
	if len(groups) != 4 {
		t.Fatalf("expected 4 release groups, got %d", len(groups))
	}
	if groups[0].Title != "OK Computer" {
		t.Errorf("expected first title 'OK Computer', got %q", groups[0].Title)
	}
	// record_type maps to PrimaryType; the single must be preserved.
	if groups[0].PrimaryType != "album" {
		t.Errorf("expected PrimaryType 'album', got %q", groups[0].PrimaryType)
	}
	if groups[3].PrimaryType != "single" {
		t.Errorf("expected PrimaryType 'single' for Creep, got %q", groups[3].PrimaryType)
	}
	// release_date maps to FirstReleaseDate; ID is stringified.
	if groups[0].FirstReleaseDate != "1997-05-28" {
		t.Errorf("expected FirstReleaseDate '1997-05-28', got %q", groups[0].FirstReleaseDate)
	}
	if groups[0].ID != "302127" {
		t.Errorf("expected ID '302127', got %q", groups[0].ID)
	}
}

func TestGetReleaseGroupsEmpty(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	groups, err := a.GetReleaseGroups(context.Background(), "8888888")
	if err != nil {
		t.Fatalf("GetReleaseGroups: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("expected 0 release groups, got %d", len(groups))
	}
}

func TestGetReleaseGroupsPagination(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	// Artist 7777777 returns 50 on the first page and 1 on the second
	// (total=51), so the loop must follow the second page.
	groups, err := a.GetReleaseGroups(context.Background(), "7777777")
	if err != nil {
		t.Fatalf("GetReleaseGroups: %v", err)
	}
	if len(groups) != 51 {
		t.Fatalf("expected 51 release groups across two pages, got %d", len(groups))
	}
}

func TestGetReleaseGroupsRejectsNonNumericID(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	_, err := a.GetReleaseGroups(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error for non-Deezer ID")
	}
	var notFound *provider.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("expected *provider.ErrNotFound, got %T: %v", err, err)
	}
}

func TestIsDeezerID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"4050205", true},
		{"0", true},
		{"123456789", true},
		{"", false},
		{"a74b1b7f-71a5-4011-9441-d0b5e4122711", false},
		{"radiohead", false},
		{"123abc", false},
	}
	for _, tc := range cases {
		if got := isDeezerID(tc.id); got != tc.want {
			t.Errorf("isDeezerID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
