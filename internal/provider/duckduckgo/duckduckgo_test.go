package duckduckgo

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

// useLoopbackTestClient swaps the SafeClient-backed default for a plain
// http.Client when tests need to reach an httptest.Server on 127.0.0.1.
// SafeTransport blocks loopback by design. Timeout matches the adapter
// default so timeout-sensitive behavior is exercised consistently.
func useLoopbackTestClient(a *Adapter) {
	a.client = &http.Client{Timeout: 15 * time.Second}
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("loading fixture %s: %v", name, err)
	}
	return data
}

func TestSearchImages(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	// Mock server that serves VQD token page and image results.
	// The main page (GET /) returns the VQD in a script tag, matching
	// the current DDG response format.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" && r.Method == http.MethodGet && r.URL.Query().Get("q") != "":
			// Main search page with VQD token in script tag
			w.Header().Set("Content-Type", "text/html")
			// Real vqd tokens are digits/dashes only (e.g. "4-123456789");
			// the tightened vqdRegex (#3273) no longer accepts letters here.
			w.Write([]byte(`<html><script>vqd='4-123456789'</script></html>`))
		case r.URL.Path == "/i.js":
			w.Header().Set("Content-Type", "application/json")
			w.Write(loadFixture(t, "search_radiohead_thumb.json"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL, srv.URL)
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	useLoopbackTestClient(a)

	images, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb)
	if err != nil {
		t.Fatalf("SearchImages: %v", err)
	}

	if len(images) != 3 {
		t.Fatalf("expected 3 images, got %d", len(images))
	}

	// Check first result
	if images[0].URL != "https://example.com/radiohead_photo1.jpg" {
		t.Errorf("expected first URL, got %s", images[0].URL)
	}
	if images[0].Width != 1200 || images[0].Height != 800 {
		t.Errorf("expected 1200x800, got %dx%d", images[0].Width, images[0].Height)
	}
	if images[0].Type != provider.ImageThumb {
		t.Errorf("expected type thumb, got %s", images[0].Type)
	}
	if images[0].Source != string(provider.NameDuckDuckGo) {
		t.Errorf("expected source duckduckgo, got %s", images[0].Source)
	}

	// Third result has 0x0 dimensions (unknown)
	if images[2].Width != 0 || images[2].Height != 0 {
		t.Errorf("expected 0x0 for unknown dimensions, got %dx%d", images[2].Width, images[2].Height)
	}
}

func TestSearchImagesEmpty(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" && r.URL.Query().Get("q") != "":
			w.Write([]byte(`<html><script>vqd='12345'</script></html>`))
		case r.URL.Path == "/i.js":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"results":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL, srv.URL)
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	useLoopbackTestClient(a)

	images, err := a.SearchImages(context.Background(), "Unknown Artist", provider.ImageThumb)
	if err != nil {
		t.Fatalf("SearchImages: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("expected 0 images, got %d", len(images))
	}
}

func TestSearchImagesServerError(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" && r.URL.Query().Get("q") != "":
			w.Write([]byte(`<html><script>vqd='12345'</script></html>`))
		case r.URL.Path == "/i.js":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL, srv.URL)
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	useLoopbackTestClient(a)

	_, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb)
	if err == nil {
		t.Fatal("expected error for server error response")
	}
}

func TestSearchImagesVQDFailure(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return HTML without VQD token
		w.Write([]byte(`<html>no token here</html>`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL, srv.URL)
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	useLoopbackTestClient(a)

	_, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb)
	if err == nil {
		t.Fatal("expected error when VQD token not found")
	}
}

func TestSearchImagesUnsupportedType(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, "http://localhost", "http://localhost")
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	useLoopbackTestClient(a)

	images, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageType("unknown"))
	if err != nil {
		t.Fatalf("expected nil error for unsupported type, got %v", err)
	}
	if images != nil {
		t.Errorf("expected nil images for unsupported type, got %v", images)
	}
}

func TestName(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := New(limiter, logger)

	if a.Name() != provider.NameDuckDuckGo {
		t.Errorf("expected name duckduckgo, got %s", a.Name())
	}
	if a.RequiresAuth() {
		t.Error("expected RequiresAuth to return false")
	}
}

func TestVQDFallbackToHTML(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	// Main page returns no VQD, but HTML endpoint does (fallback path)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" && r.URL.Query().Get("q") != "":
			// Main page without VQD token
			w.Write([]byte(`<html><body>no token here</body></html>`))
		case r.URL.Path == "/html/" && r.Method == http.MethodPost:
			// Legacy HTML endpoint with VQD, in the unquoted digits-dash
			// query-parameter style historically observed there. This
			// fallback path uses vqdFallbackRegex (looser than the
			// main-page-only vqdRegex) specifically so this unquoted form
			// still matches -- see TestExtractVQDFromHTMLFallback for the
			// dedicated coverage and vqdFallbackRegex's doc comment for why.
			w.Write([]byte(`<html><script>vqd=98765&</script></html>`))
		case r.URL.Path == "/i.js":
			w.Header().Set("Content-Type", "application/json")
			w.Write(loadFixture(t, "search_radiohead_thumb.json"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL, srv.URL)
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	useLoopbackTestClient(a)

	images, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb)
	if err != nil {
		t.Fatalf("SearchImages with HTML fallback: %v", err)
	}
	if len(images) != 3 {
		t.Fatalf("expected 3 images, got %d", len(images))
	}
}

// TestVQDTokenFormats exercises the tightened vqdRegex (#3273), scoped to
// the MAIN page (extractVQDFromBytes / getVQDFromMainPage): a live GET
// during hostile review confirmed the real main page only emits the quoted
// forms below. This does NOT apply to the /html/ fallback endpoint, which
// was never live-verified the same way and uses the separate, looser
// vqdFallbackRegex (see TestExtractVQDFromHTMLFallback).
func TestVQDTokenFormats(t *testing.T) {
	tests := []struct {
		name  string
		html  string
		token string
	}{
		{"single quoted with dash", `vqd='4-123456789'`, "4-123456789"},
		{"double quoted with dash", `vqd="4-123456789"`, "4-123456789"},
		{"single quoted no dash", `vqd='123456789'`, "123456789"},
		{"in script tag", `<script>vqd='4-42';</script>`, "4-42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := extractVQDFromBytes([]byte(tt.html))
			if err != nil {
				t.Fatalf("extractVQDFromBytes(%q): %v", tt.html, err)
			}
			if token != tt.token {
				t.Errorf("got token %q, want %q", token, tt.token)
			}
		})
	}
}

// TestMainPageVQDRegexRejectsUnquoted proves the MAIN-PAGE tightening
// (extractVQDFromBytes / vqdRegex) actually narrowed behavior: forms the
// old shared [0-9a-zA-Z_-]+ regex accepted must now fail on that path. This
// no longer claims anything about the /html/ fallback -- that path
// deliberately accepts the unquoted form via vqdFallbackRegex instead (see
// TestExtractVQDFromHTMLFallback), since only the main page's quoted-only
// behavior was live-verified.
func TestMainPageVQDRegexRejectsUnquoted(t *testing.T) {
	rejected := []string{
		`vqd=4-123456789`,  // unquoted query-parameter style (fallback-only now)
		`vqd=abc123_DEF`,   // bare alphanumeric, no quotes
		`vqd='abc123_DEF'`, // quoted but non-digit body
	}
	for _, html := range rejected {
		html := html
		t.Run(html, func(t *testing.T) {
			if _, err := extractVQDFromBytes([]byte(html)); err == nil {
				t.Errorf("extractVQDFromBytes(%q): expected error, got a match", html)
			}
		})
	}
}

func TestSearchImagesContextCanceled(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><script>vqd='12345'</script></html>`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL, srv.URL)
	// Override the SafeClient-backed default (which rejects httptest's loopback) with a plain client.
	useLoopbackTestClient(a)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := a.SearchImages(ctx, "Radiohead", provider.ImageThumb)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}
