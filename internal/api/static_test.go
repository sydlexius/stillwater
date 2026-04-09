package api

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestStaticAssets_Path(t *testing.T) {
	fsys := fstest.MapFS{
		"css/styles.css": &fstest.MapFile{Data: []byte("body{}")},
		"js/htmx.min.js": &fstest.MapFile{Data: []byte("htmx")},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	sa := NewStaticAssets(fsys, logger)

	t.Run("known file returns versioned path", func(t *testing.T) {
		p := sa.Path("/css/styles.css")
		if p == "/static/css/styles.css" {
			t.Fatal("expected versioned path with ?v= parameter")
		}
		if len(p) < len("/static/css/styles.css?v=") {
			t.Fatalf("path too short: %s", p)
		}
	})

	t.Run("unknown file returns unversioned path", func(t *testing.T) {
		p := sa.Path("/css/nonexistent.css")
		if p != "/static/css/nonexistent.css" {
			t.Fatalf("expected unversioned path, got %s", p)
		}
	})
}

func TestStaticAssets_Handler(t *testing.T) {
	fsys := fstest.MapFS{
		"css/styles.css": &fstest.MapFile{Data: []byte("body{}")},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	sa := NewStaticAssets(fsys, logger)

	handler := sa.Handler("")

	t.Run("serves file without version param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/static/css/styles.css", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if w.Body.String() != "body{}" {
			t.Fatalf("unexpected body: %s", w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
			t.Fatalf("expected short cache, got %s", cc)
		}
	})

	t.Run("serves file with matching version param", func(t *testing.T) {
		// Get the versioned path to extract the hash
		path := sa.Path("/css/styles.css")
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Fatalf("expected immutable cache, got %s", cc)
		}
	})

	t.Run("serves file with mismatched version param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/static/css/styles.css?v=wronghash", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
			t.Fatalf("expected medium cache, got %s", cc)
		}
	})
}

func TestStaticAssets_DirFS(t *testing.T) {
	// Verify StaticAssets works with an on-disk fs.FS rooted at web/static.
	// This test uses os.DirFS, not the production embedded filesystem.
	fsys := os.DirFS("../../web/static")

	// Verify the on-disk FS is available by checking a known file exists.
	if _, err := fs.Stat(fsys, "js/htmx.min.js"); err != nil {
		t.Skipf("static assets not available: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	sa := NewStaticAssets(fsys, logger)

	// Verify some known assets are hashed
	for _, path := range []string{
		"/js/htmx.min.js",
		"/css/cropper.min.css",
		"/site.webmanifest",
	} {
		p := sa.Path(path)
		if p == "/static"+path {
			t.Errorf("expected versioned path for %s, got unversioned", path)
		}
	}

	// Verify the handler serves a file
	handler := sa.Handler("")
	req := httptest.NewRequest(http.MethodGet, "/static/js/htmx.min.js", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Fatal("expected non-empty response body")
	}
}

func TestStaticAssets_NilFS(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil fsys")
		}
	}()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	NewStaticAssets(nil, logger)
}

func TestStaticAssets_WithBasePath(t *testing.T) {
	fsys := fstest.MapFS{
		"css/styles.css": &fstest.MapFile{Data: []byte("body{}")},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	sa := NewStaticAssets(fsys, logger)

	handler := sa.Handler("/app")

	req := httptest.NewRequest(http.MethodGet, "/app/static/css/styles.css", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "body{}" {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// TestStaticAssetsHandlerStripPrefix verifies that the Handler correctly strips
// the base path prefix when serving files across multiple base path configurations.
func TestStaticAssetsHandlerStripPrefix(t *testing.T) {
	basePaths := []string{"", "/stillwater", "/foo/bar"}
	for _, bp := range basePaths {
		t.Run("basePath="+bp, func(t *testing.T) {
			dir := t.TempDir()
			cssDir := filepath.Join(dir, "css")
			if err := os.MkdirAll(cssDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cssDir, "test.css"), []byte("body{}"), 0o644); err != nil {
				t.Fatal(err)
			}

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			sa := NewStaticAssets(dir, logger)
			handler := sa.Handler(bp)

			// Build request path: basePath + /static/css/test.css
			reqPath := bp + "/static/css/test.css"
			req := httptest.NewRequest(http.MethodGet, reqPath, nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Errorf("Handler(%q) status = %d, want %d", reqPath, rr.Code, http.StatusOK)
			}

			// Verify requests without the base path prefix fail when one is configured.
			if bp != "" {
				rrWrong := httptest.NewRecorder()
				reqWrong := httptest.NewRequest(http.MethodGet, "/static/css/test.css", nil)
				handler.ServeHTTP(rrWrong, reqWrong)
				if rrWrong.Code == http.StatusOK {
					t.Errorf("Handler(%q) without prefix: status = %d, want non-200", bp, rrWrong.Code)
				}
			}
		})
	}
}
