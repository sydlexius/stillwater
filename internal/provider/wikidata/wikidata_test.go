package wikidata

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
			w.Write([]byte(`{"results":{"bindings":[]}}`))
			return
		}
		w.Write(loadFixture(t, "artist_radiohead.json"))
	}))
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

func TestGetImagesReturnsNil(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithEndpoint(limiter, logger, "http://localhost")

	images, err := a.GetImages(context.Background(), "any")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if images != nil {
		t.Errorf("expected nil, got %v", images)
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
