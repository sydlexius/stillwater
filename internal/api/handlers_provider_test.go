package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/musicbrainz"
)

func testRouterWithMirror(t *testing.T) *Router {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	rateLimiters := provider.NewRateLimiterMap()
	providerSettings := provider.NewSettingsService(db, enc)
	registry := provider.NewRegistry()
	registry.Register(musicbrainz.New(rateLimiters, logger))

	return NewRouter(RouterDeps{
		ProviderSettings: providerSettings,
		ProviderRegistry: registry,
		RateLimiters:     rateLimiters,
		DB:               db,
		Logger:           logger,
		StaticDir:        "../../web/static",
	})
}

func TestHandleSetMirror(t *testing.T) {
	// Start a local mirror server so the auto-test in the JSON path succeeds.
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"artists":[]}`))
	}))
	defer mirrorSrv.Close()

	r := testRouterWithMirror(t)

	mirrorURL := mirrorSrv.URL + "/ws/2"
	body := fmt.Sprintf(`{"base_url":%q,"rate_limit":10}`, mirrorURL)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/musicbrainz/mirror", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "saved" {
		t.Errorf("expected status 'saved', got %q", resp["status"])
	}

	// Verify settings were persisted.
	ctx := context.Background()
	url, err := r.providerSettings.GetBaseURL(ctx, provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetBaseURL: %v", err)
	}
	if url != mirrorURL {
		t.Errorf("expected persisted base URL %q, got %q", mirrorURL, url)
	}

	limit, err := r.providerSettings.GetRateLimit(ctx, provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetRateLimit: %v", err)
	}
	if limit != 10 {
		t.Errorf("expected persisted rate limit 10, got %v", limit)
	}

	// Verify the adapter was updated.
	p := r.providerRegistry.Get(provider.NameMusicBrainz)
	mirrorable := p.(provider.MirrorableProvider)
	if mirrorable.BaseURL() != mirrorURL {
		t.Errorf("expected adapter base URL updated, got %q", mirrorable.BaseURL())
	}
}

func TestHandleSetMirrorValidation(t *testing.T) {
	r := testRouterWithMirror(t)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty body", `{}`, http.StatusBadRequest},
		{"missing base_url", `{"rate_limit":10}`, http.StatusBadRequest},
		{"invalid URL", `{"base_url":"not-a-url"}`, http.StatusBadRequest},
		{"ftp scheme", `{"base_url":"ftp://mirror/ws/2"}`, http.StatusBadRequest},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/musicbrainz/mirror", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d; body: %s", w.Code, tt.want, w.Body.String())
			}
		})
	}
}

func TestHandleSetMirrorDefaultRateLimit(t *testing.T) {
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"artists":[]}`))
	}))
	defer mirrorSrv.Close()

	r := testRouterWithMirror(t)

	// Omit rate_limit; should default to 10.
	body := fmt.Sprintf(`{"base_url":%q}`, mirrorSrv.URL+"/ws/2")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/musicbrainz/mirror", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	limit, err := r.providerSettings.GetRateLimit(context.Background(), provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetRateLimit: %v", err)
	}
	if limit != 10 {
		t.Errorf("expected default rate limit 10, got %v", limit)
	}
}

func TestHandleSetMirrorRateLimitCap(t *testing.T) {
	mirrorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"artists":[]}`))
	}))
	defer mirrorSrv.Close()

	r := testRouterWithMirror(t)

	// Rate limit above 100 should be capped.
	body := fmt.Sprintf(`{"base_url":%q,"rate_limit":999}`, mirrorSrv.URL+"/ws/2")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/musicbrainz/mirror", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	limit, err := r.providerSettings.GetRateLimit(context.Background(), provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetRateLimit: %v", err)
	}
	if limit != 100 {
		t.Errorf("expected capped rate limit 100, got %v", limit)
	}
}

func TestHandleDeleteMirror(t *testing.T) {
	r := testRouterWithMirror(t)
	ctx := context.Background()

	// Set mirror first.
	if err := r.providerSettings.SetBaseURL(ctx, provider.NameMusicBrainz, "http://mirror:5000/ws/2"); err != nil {
		t.Fatalf("SetBaseURL: %v", err)
	}
	if err := r.providerSettings.SetRateLimit(ctx, provider.NameMusicBrainz, 20); err != nil {
		t.Fatalf("SetRateLimit: %v", err)
	}
	p := r.providerRegistry.Get(provider.NameMusicBrainz)
	p.(provider.MirrorableProvider).SetBaseURL("http://mirror:5000/ws/2")

	// Delete mirror.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/providers/musicbrainz/mirror", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v1/providers/{name}/mirror", r.handleDeleteMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify cleared.
	url, err := r.providerSettings.GetBaseURL(ctx, provider.NameMusicBrainz)
	if err != nil {
		t.Fatalf("GetBaseURL: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty base URL after delete, got %q", url)
	}

	// Verify adapter reverted to default.
	mirrorable := p.(provider.MirrorableProvider)
	if mirrorable.BaseURL() != mirrorable.DefaultBaseURL() {
		t.Errorf("expected adapter reverted to default %q, got %q",
			mirrorable.DefaultBaseURL(), mirrorable.BaseURL())
	}
}

func TestHandleSetMirrorUnsupportedProvider(t *testing.T) {
	r := testRouterWithMirror(t)

	body := `{"base_url":"http://mirror:5000/ws/2"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/fanarttv/mirror", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/providers/{name}/mirror", r.handleSetMirror)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}
