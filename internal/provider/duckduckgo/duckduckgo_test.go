package duckduckgo

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

func TestSearchImages(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	// Mock server that serves VQD token page and image results
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/html/" && r.Method == http.MethodPost:
			// Return HTML containing VQD token
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><script>vqd=12345-67890&</script></html>`))
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
		switch r.URL.Path {
		case "/html/":
			w.Write([]byte(`<html><script>vqd=12345&</script></html>`))
		case "/i.js":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"results":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL, srv.URL)

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
		switch r.URL.Path {
		case "/html/":
			w.Write([]byte(`<html><script>vqd=12345&</script></html>`))
		case "/i.js":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL, srv.URL)

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

	_, err := a.SearchImages(context.Background(), "Radiohead", provider.ImageThumb)
	if err == nil {
		t.Fatal("expected error when VQD token not found")
	}
}

func TestSearchImagesUnsupportedType(t *testing.T) {
	limiter := provider.NewRateLimiterMap()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, "http://localhost", "http://localhost")

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

func TestSearchImagesContextCanceled(t *testing.T) {
	limiter := provider.NewRateLimiterMap()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><script>vqd=12345&</script></html>`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a := NewWithBaseURL(limiter, logger, srv.URL, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := a.SearchImages(ctx, "Radiohead", provider.ImageThumb)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}
