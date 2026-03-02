package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// fakeSettings implements SettingsProvider for testing.
type fakeSettings struct {
	keys map[provider.ProviderName]string
}

func (f *fakeSettings) GetAPIKey(_ context.Context, name provider.ProviderName) (string, error) {
	return f.keys[name], nil
}

func validCredentialsJSON() string {
	data, _ := json.Marshal(map[string]string{
		"client_id":     "test-client-id",
		"client_secret": "test-client-secret",
	})
	return string(data)
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		// Token endpoint
		case r.URL.Path == "/token":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Basic ") {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"invalid_client"}`))
				return
			}
			w.Write(loadFixture(t, "token_response.json"))

		// Search endpoint
		case r.URL.Path == "/search":
			q := r.URL.Query().Get("q")
			if q == "no-results-query" {
				w.Write([]byte(`{"artists":{"items":[],"total":0}}`))
				return
			}
			w.Write(loadFixture(t, "search_radiohead.json"))

		// Artist endpoint
		case strings.HasPrefix(r.URL.Path, "/artists/"):
			id := strings.TrimPrefix(r.URL.Path, "/artists/")
			switch id {
			case "0OdUWJ0sBjDrqHygGUXeCF":
				w.Write(loadFixture(t, "artist_no_images.json"))
			case "4Z8W4fKeB5YxbusRsdQVPb":
				w.Write(loadFixture(t, "artist_radiohead.json"))
			default:
				w.WriteHeader(http.StatusNotFound)
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestAdapter(t *testing.T, baseURL, tokenURL string, keys map[provider.ProviderName]string) *Adapter {
	t.Helper()
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	settings := &fakeSettings{keys: keys}
	return NewWithBaseURL(limiter, settings, logger, baseURL, tokenURL)
}

func TestName(t *testing.T) {
	a := newTestAdapter(t, "http://localhost", "http://localhost/token", nil)
	if a.Name() != provider.NameSpotify {
		t.Errorf("expected %q, got %q", provider.NameSpotify, a.Name())
	}
}

func TestRequiresAuth(t *testing.T) {
	a := newTestAdapter(t, "http://localhost", "http://localhost/token", nil)
	if !a.RequiresAuth() {
		t.Error("expected RequiresAuth to return true")
	}
}

func TestSearchArtist(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

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
	if results[0].ProviderID != "4Z8W4fKeB5YxbusRsdQVPb" {
		t.Errorf("expected provider ID 4Z8W4fKeB5YxbusRsdQVPb, got %q", results[0].ProviderID)
	}
	if results[0].Source != string(provider.NameSpotify) {
		t.Errorf("expected source %q, got %q", provider.NameSpotify, results[0].Source)
	}
}

func TestSearchArtistEmpty(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

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
	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

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
	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

	meta, err := a.GetArtist(context.Background(), "4Z8W4fKeB5YxbusRsdQVPb")
	if err != nil {
		t.Fatalf("GetArtist: %v", err)
	}
	if meta == nil {
		t.Fatal("expected non-nil metadata")
	}
	if meta.Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %q", meta.Name)
	}
	if meta.ProviderID != "4Z8W4fKeB5YxbusRsdQVPb" {
		t.Errorf("expected ProviderID 4Z8W4fKeB5YxbusRsdQVPb, got %q", meta.ProviderID)
	}
	if meta.SpotifyID != "4Z8W4fKeB5YxbusRsdQVPb" {
		t.Errorf("expected SpotifyID 4Z8W4fKeB5YxbusRsdQVPb, got %q", meta.SpotifyID)
	}
	if len(meta.Genres) != 4 {
		t.Errorf("expected 4 genres, got %d", len(meta.Genres))
	}
	if meta.URLs["spotify"] == "" {
		t.Error("expected spotify URL to be set")
	}
}

func TestGetArtistRejectsNonSpotifyID(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

	// MusicBrainz UUID should be rejected without an HTTP call
	_, err := a.GetArtist(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error for non-Spotify ID")
	}
	if _, ok := err.(*provider.ErrNotFound); !ok {
		t.Errorf("expected *provider.ErrNotFound, got %T: %v", err, err)
	}

	// Deezer numeric ID should be rejected
	_, err = a.GetArtist(context.Background(), "4050205")
	if err == nil {
		t.Fatal("expected error for numeric ID")
	}
}

func TestGetImages(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

	images, err := a.GetImages(context.Background(), "4Z8W4fKeB5YxbusRsdQVPb")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(images) == 0 {
		t.Fatal("expected at least one image")
	}
	if images[0].Type != provider.ImageThumb {
		t.Errorf("expected thumb type, got %q", images[0].Type)
	}
	if images[0].Source != string(provider.NameSpotify) {
		t.Errorf("expected source %q, got %q", provider.NameSpotify, images[0].Source)
	}
	// Should pick the largest (640x640)
	if images[0].Width != 640 {
		t.Errorf("expected width 640, got %d", images[0].Width)
	}
}

func TestGetImagesNoImages(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

	images, err := a.GetImages(context.Background(), "0OdUWJ0sBjDrqHygGUXeCF")
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("expected 0 images for artist without images, got %d", len(images))
	}
}

func TestGetImagesRejectsNonSpotifyID(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

	_, err := a.GetImages(context.Background(), "a74b1b7f-71a5-4011-9441-d0b5e4122711")
	if err == nil {
		t.Fatal("expected error for non-Spotify ID")
	}
}

func TestTestConnection(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

	if err := a.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
}

func TestTestConnectionBadCredentials(t *testing.T) {
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer badServer.Close()

	a := newTestAdapter(t, badServer.URL, badServer.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

	err := a.TestConnection(context.Background())
	if err == nil {
		t.Fatal("expected error for bad credentials")
	}
	// refreshToken now maps 401 to ErrAuthRequired
	var authErr *provider.ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Errorf("expected *provider.ErrAuthRequired in chain, got %T: %v", err, err)
	}
}

func TestNoCredentials(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{})

	_, err := a.SearchArtist(context.Background(), "radiohead")
	if err == nil {
		t.Fatal("expected error when no credentials configured")
	}
	if _, ok := err.(*provider.ErrAuthRequired); !ok {
		t.Errorf("expected *provider.ErrAuthRequired, got %T: %v", err, err)
	}
}

func TestCredentialsParsing(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"valid", validCredentialsJSON(), false},
		{"empty", "", true},
		{"invalid json", "not-json", true},
		{"missing client_id", `{"client_secret":"secret"}`, true},
		{"missing client_secret", `{"client_id":"id"}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
				provider.NameSpotify: tc.raw,
			})
			_, err := a.SearchArtist(context.Background(), "test")
			if tc.wantErr && err == nil {
				t.Error("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestTokenCaching(t *testing.T) {
	tokenRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			tokenRequests++
			w.Write(loadFixture(t, "token_response.json"))
		case "/search":
			w.Write([]byte(`{"artists":{"items":[],"total":0}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

	// First call should fetch a token
	a.SearchArtist(context.Background(), "test1")
	if tokenRequests != 1 {
		t.Fatalf("expected 1 token request, got %d", tokenRequests)
	}

	// Second call should reuse the cached token
	a.SearchArtist(context.Background(), "test2")
	if tokenRequests != 1 {
		t.Errorf("expected 1 token request (cached), got %d", tokenRequests)
	}
}

func TestTokenRefreshOnExpiry(t *testing.T) {
	tokenRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			tokenRequests++
			// Return a token that expires in 1 second
			w.Write([]byte(`{"access_token":"short-lived-token","token_type":"Bearer","expires_in":1}`))
		case "/search":
			w.Write([]byte(`{"artists":{"items":[],"total":0}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, srv.URL+"/token", map[provider.ProviderName]string{
		provider.NameSpotify: validCredentialsJSON(),
	})

	// First call fetches a token
	a.SearchArtist(context.Background(), "test1")
	if tokenRequests != 1 {
		t.Fatalf("expected 1 token request, got %d", tokenRequests)
	}

	// Manually expire the token (expiry within 60s buffer triggers refresh)
	a.mu.Lock()
	a.tokenExpiry = time.Now().Add(-1 * time.Second)
	a.mu.Unlock()

	// Second call should refresh the token
	a.SearchArtist(context.Background(), "test2")
	if tokenRequests != 2 {
		t.Errorf("expected 2 token requests after expiry, got %d", tokenRequests)
	}
}

func TestIsSpotifyID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"4Z8W4fKeB5YxbusRsdQVPb", true},                // Radiohead
		{"0OdUWJ0sBjDrqHygGUXeCF", true},                // Band of Horses
		{"", false},                                     // empty
		{"a74b1b7f-71a5-4011-9441-d0b5e4122711", false}, // UUID (MusicBrainz)
		{"4050205", false},                              // numeric (Deezer)
		{"radiohead", false},                            // too short
		{"4Z8W4fKeB5YxbusRsdQVP!", false},               // invalid char
		{"4Z8W4fKeB5YxbusRsdQVPbb", false},              // too long (23 chars)
		{"4Z8W4fKeB5YxbusRsdQVP", false},                // too short (21 chars)
	}
	for _, tc := range cases {
		if got := IsSpotifyID(tc.id); got != tc.want {
			t.Errorf("IsSpotifyID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
