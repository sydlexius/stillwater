package deezer

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
		case r.URL.Path == "/search/artist":
			q := r.URL.Query().Get("q")
			if q == "no-results-query" {
				w.Write([]byte(`{"data":[],"total":0}`))
				return
			}
			w.Write(loadFixture(t, "search_radiohead.json"))

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
	return NewWithBaseURL(limiter, logger, baseURL)
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
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtistNotFound(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL)

	_, err := a.GetArtist(context.Background(), "not-found")
	if err == nil {
		t.Fatal("expected error for non-numeric ID")
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

func isErrNotFound(err error, target **provider.ErrNotFound) bool {
	if e, ok := err.(*provider.ErrNotFound); ok {
		*target = e
		return true
	}
	return false
}
