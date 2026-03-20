package wikipedia

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestIsUUID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "valid lowercase", id: "a74b1b7f-71a5-4011-9441-d0b5e4122711", want: true},
		{name: "valid uppercase", id: "A74B1B7F-71A5-4011-9441-D0B5E4122711", want: true},
		{name: "valid mixed case", id: "a74B1b7F-71a5-4011-9441-d0b5E4122711", want: true},
		{name: "empty string", id: "", want: false},
		{name: "too short", id: "a74b1b7f-71a5-4011-9441", want: false},
		{name: "too long", id: "a74b1b7f-71a5-4011-9441-d0b5e41227110", want: false},
		{name: "missing hyphens", id: "a74b1b7f71a540119441d0b5e4122711xxxx", want: false},
		{name: "wrong hyphen positions", id: "a74b1b7f071a5-4011-9441-d0b5e412271", want: false},
		{name: "non-hex characters", id: "g74b1b7f-71a5-4011-9441-d0b5e4122711", want: false},
		{name: "plain artist name", id: "Radiohead", want: false},
		{name: "numeric ID", id: "12345", want: false},
		{name: "36 chars but not UUID", id: "not-a-uuid-at-all-but-right-length!!", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := provider.IsUUID(tt.id)
			if got != tt.want {
				t.Errorf("provider.IsUUID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestGetArtist_ValidMBID(t *testing.T) {
	// Mock Wikidata SPARQL endpoint
	sparqlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := sparqlResponse{}
		resp.Results.Bindings = append(resp.Results.Bindings, struct {
			Article struct {
				Value string `json:"value"`
			} `json:"article"`
		}{
			Article: struct {
				Value string `json:"value"`
			}{Value: "https://en.wikipedia.org/wiki/Radiohead"},
		})
		w.Header().Set("Content-Type", "application/sparql-results+json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer sparqlServer.Close()

	// Mock Wikipedia REST API
	wikiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := summaryResponse{
			Title:       "Radiohead",
			DisplayName: "Radiohead",
			Extract:     "Radiohead are an English rock band formed in Abingdon, Oxfordshire, in 1985.",
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer wikiServer.Close()

	limiter := provider.NewRateLimiterMap()
	adapter := NewWithEndpoints(limiter, silentLogger(), wikiServer.URL, sparqlServer.URL)

	meta, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta.Name != "Radiohead" {
		t.Errorf("Name = %q, want Radiohead", meta.Name)
	}
	if meta.Biography != "Radiohead are an English rock band formed in Abingdon, Oxfordshire, in 1985." {
		t.Errorf("Biography = %q, want Radiohead bio", meta.Biography)
	}
	if meta.URLs["wikipedia"] == "" {
		t.Error("expected wikipedia URL in metadata")
	}
}

func TestGetArtist_NonMBID(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	adapter := NewWithEndpoints(limiter, silentLogger(), "http://unused", "http://unused")

	_, err := adapter.GetArtist(context.Background(), "Radiohead")
	if err == nil {
		t.Fatal("expected error for non-MBID input")
	}
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtist_NotFoundInWikidata(t *testing.T) {
	// SPARQL returns empty bindings
	sparqlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := sparqlResponse{}
		w.Header().Set("Content-Type", "application/sparql-results+json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer sparqlServer.Close()

	limiter := provider.NewRateLimiterMap()
	adapter := NewWithEndpoints(limiter, silentLogger(), "http://unused", sparqlServer.URL)

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error when MBID not found in Wikidata")
	}
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtist_WikidataServerError(t *testing.T) {
	sparqlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sparqlServer.Close()

	limiter := provider.NewRateLimiterMap()
	adapter := NewWithEndpoints(limiter, silentLogger(), "http://unused", sparqlServer.URL)

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error on Wikidata server error")
	}
	var unavail *provider.ErrProviderUnavailable
	if !isErrUnavailable(err, &unavail) {
		t.Errorf("expected ErrProviderUnavailable, got %T: %v", err, err)
	}
}

func TestGetArtist_SummaryNotFound(t *testing.T) {
	// SPARQL succeeds
	sparqlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := sparqlResponse{}
		resp.Results.Bindings = append(resp.Results.Bindings, struct {
			Article struct {
				Value string `json:"value"`
			} `json:"article"`
		}{
			Article: struct {
				Value string `json:"value"`
			}{Value: "https://en.wikipedia.org/wiki/Test_Artist"},
		})
		w.Header().Set("Content-Type", "application/sparql-results+json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer sparqlServer.Close()

	// Wikipedia returns 404
	wikiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer wikiServer.Close()

	limiter := provider.NewRateLimiterMap()
	adapter := NewWithEndpoints(limiter, silentLogger(), wikiServer.URL, sparqlServer.URL)

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error when Wikipedia article not found")
	}
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtist_EmptyExtract(t *testing.T) {
	sparqlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := sparqlResponse{}
		resp.Results.Bindings = append(resp.Results.Bindings, struct {
			Article struct {
				Value string `json:"value"`
			} `json:"article"`
		}{
			Article: struct {
				Value string `json:"value"`
			}{Value: "https://en.wikipedia.org/wiki/Empty_Article"},
		})
		w.Header().Set("Content-Type", "application/sparql-results+json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer sparqlServer.Close()

	// Wikipedia returns a valid response but with an empty extract
	wikiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := summaryResponse{
			Title:   "Empty Article",
			Extract: "",
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer wikiServer.Close()

	limiter := provider.NewRateLimiterMap()
	adapter := NewWithEndpoints(limiter, silentLogger(), wikiServer.URL, sparqlServer.URL)

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error when extract is empty")
	}
	var notFound *provider.ErrNotFound
	if !isErrNotFound(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestGetArtist_UnexpectedArticleURL(t *testing.T) {
	// SPARQL returns a URL without /wiki/ prefix
	sparqlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := sparqlResponse{}
		resp.Results.Bindings = append(resp.Results.Bindings, struct {
			Article struct {
				Value string `json:"value"`
			} `json:"article"`
		}{
			Article: struct {
				Value string `json:"value"`
			}{Value: "https://www.wikidata.org/entity/Q44190"},
		})
		w.Header().Set("Content-Type", "application/sparql-results+json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer sparqlServer.Close()

	limiter := provider.NewRateLimiterMap()
	adapter := NewWithEndpoints(limiter, silentLogger(), "http://unused", sparqlServer.URL)

	_, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error for unexpected article URL format")
	}
	// Should be ErrProviderUnavailable (not ErrNotFound) since the data exists
	// but the URL format is unexpected.
	var unavail *provider.ErrProviderUnavailable
	if !isErrUnavailable(err, &unavail) {
		t.Errorf("expected ErrProviderUnavailable for unexpected URL, got %T: %v", err, err)
	}
}

func TestGetArtist_DisplayNameFallback(t *testing.T) {
	sparqlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := sparqlResponse{}
		resp.Results.Bindings = append(resp.Results.Bindings, struct {
			Article struct {
				Value string `json:"value"`
			} `json:"article"`
		}{
			Article: struct {
				Value string `json:"value"`
			}{Value: "https://en.wikipedia.org/wiki/Some_Band"},
		})
		w.Header().Set("Content-Type", "application/sparql-results+json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer sparqlServer.Close()

	// Return summary with no DisplayName; should fall back to Title
	wikiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := summaryResponse{
			Title:       "Some_Band",
			DisplayName: "",
			Extract:     "Some Band is a fictional music group used for testing purposes in this codebase.",
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer wikiServer.Close()

	limiter := provider.NewRateLimiterMap()
	adapter := NewWithEndpoints(limiter, silentLogger(), wikiServer.URL, sparqlServer.URL)

	meta, err := adapter.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	// Title "Some_Band" should have underscores replaced with spaces
	if meta.Name != "Some Band" {
		t.Errorf("Name = %q, want %q", meta.Name, "Some Band")
	}
}

func TestTestConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	limiter := provider.NewRateLimiterMap()
	adapter := NewWithEndpoints(limiter, silentLogger(), server.URL, "http://unused")

	if err := adapter.TestConnection(context.Background()); err != nil {
		t.Errorf("TestConnection: %v", err)
	}
}

func TestName(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	adapter := New(limiter, silentLogger())
	if adapter.Name() != provider.NameWikipedia {
		t.Errorf("Name() = %q, want %q", adapter.Name(), provider.NameWikipedia)
	}
}

func TestRequiresAuth(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	adapter := New(limiter, silentLogger())
	if adapter.RequiresAuth() {
		t.Error("RequiresAuth() should be false")
	}
}

func TestSupportsNameLookup(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	adapter := New(limiter, silentLogger())
	if adapter.SupportsNameLookup() {
		t.Error("SupportsNameLookup() should be false")
	}
}

// isErrNotFound checks if err (or any wrapped error) is an *provider.ErrNotFound.
func isErrNotFound(err error, target **provider.ErrNotFound) bool {
	return errors.As(err, target)
}

// isErrUnavailable checks if err (or any wrapped error) is an *provider.ErrProviderUnavailable.
func isErrUnavailable(err error, target **provider.ErrProviderUnavailable) bool {
	return errors.As(err, target)
}
