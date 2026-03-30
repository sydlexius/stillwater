package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

// newBrowseRouter returns a minimal Router sufficient for filesystem browse tests.
func newBrowseRouter(t *testing.T) *Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return &Router{
		logger: logger,
	}
}

func TestHandleFilesystemBrowse_Valid(t *testing.T) {
	// Create a temp directory with some subdirectories and a file.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "albums"), 0o755); err != nil {
		t.Fatalf("creating albums dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "artists"), 0o755); err != nil {
		t.Fatalf("creating artists dir: %v", err)
	}
	// A file that must not appear in the listing.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("test"), 0o644); err != nil {
		t.Fatalf("creating test file: %v", err)
	}
	// A hidden directory that must not appear in the listing.
	if err := os.MkdirAll(filepath.Join(dir, ".hidden"), 0o755); err != nil {
		t.Fatalf("creating hidden dir: %v", err)
	}

	r := newBrowseRouter(t)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+dir, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp filesystemBrowseResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Entries must contain only subdirectories (no files, no hidden dirs).
	if len(resp.Entries) != 2 {
		t.Errorf("entries = %v, want [albums artists]", resp.Entries)
	}

	for _, e := range resp.Entries {
		if e == "readme.txt" {
			t.Error("listing must not include files")
		}
		if e == ".hidden" {
			t.Error("listing must not include hidden directories")
		}
	}

	if resp.Path == "" {
		t.Error("response path must not be empty")
	}
}

func TestHandleFilesystemBrowse_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	r := newBrowseRouter(t)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+dir, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp filesystemBrowseResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Errorf("expected 0 entries for empty dir, got %v", resp.Entries)
	}
}

func TestHandleFilesystemBrowse_MissingPath(t *testing.T) {
	r := newBrowseRouter(t)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse", nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleFilesystemBrowse_RelativePath(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"relative path", "music/artists"},
		{"dot-dot traversal", "../etc"},
		{"just dot-dot", ".."},
	}

	r := newBrowseRouter(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := middleware.WithTestRole(context.Background(), "administrator")
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+tc.path, nil)
			w := httptest.NewRecorder()

			r.handleFilesystemBrowse(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("path %q: status = %d, want %d", tc.path, w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleFilesystemBrowse_SymlinkResolvesToValidPath(t *testing.T) {
	// Create a symlink inside innerDir that points to outerDir.
	// When browsing the symlink path directly, it must resolve to outerDir.
	outerDir := t.TempDir()
	innerDir := t.TempDir()

	linkPath := filepath.Join(innerDir, "escape")
	if err := os.Symlink(outerDir, linkPath); err != nil {
		t.Skipf("symlink creation not supported on this platform: %v", err)
	}

	// Resolve the expected outerDir ourselves to handle OS-level symlinks
	// (e.g. /var -> /private/var on macOS).
	resolvedOuter, err := filepath.EvalSymlinks(outerDir)
	if err != nil {
		t.Fatalf("resolving outerDir symlinks: %v", err)
	}

	r := newBrowseRouter(t)
	ctx := middleware.WithTestRole(context.Background(), "administrator")

	// Navigate into the symlink -- it must resolve to outerDir and succeed.
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+linkPath, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp filesystemBrowseResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// The resolved path must be the symlink target, not the symlink path itself.
	if resp.Path != resolvedOuter {
		t.Errorf("resolved path = %q, want %q", resp.Path, resolvedOuter)
	}
}

func TestHandleFilesystemBrowse_NonExistentPath(t *testing.T) {
	r := newBrowseRouter(t)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path=/nonexistent/path/that/does/not/exist", nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleFilesystemBrowse_RequireAdmin(t *testing.T) {
	// The RequireAdmin middleware is applied at route registration, but we
	// verify the pattern directly by wrapping the handler as the router does.
	r := newBrowseRouter(t)
	dir := t.TempDir()

	// Operator role -- should be rejected by RequireAdmin middleware.
	ctx := middleware.WithTestRole(context.Background(), "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+dir, nil)
	w := httptest.NewRecorder()

	middleware.RequireAdmin(r.handleFilesystemBrowse)(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("operator: status = %d, want %d", w.Code, http.StatusForbidden)
	}

	// Unauthenticated (empty role) -- also rejected.
	ctx2 := context.Background() // no role set
	req2 := httptest.NewRequestWithContext(ctx2, http.MethodGet, "/api/v1/filesystem/browse?path="+dir, nil)
	w2 := httptest.NewRecorder()

	middleware.RequireAdmin(r.handleFilesystemBrowse)(w2, req2)

	if w2.Code != http.StatusForbidden {
		t.Errorf("unauthenticated: status = %d, want %d", w2.Code, http.StatusForbidden)
	}
}

func TestHandleFilesystemBrowse_ParentPath(t *testing.T) {
	dir := t.TempDir()

	// Resolve the dir through any OS-level symlinks (e.g. /var -> /private/var on macOS).
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving dir symlinks: %v", err)
	}

	r := newBrowseRouter(t)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+dir, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp filesystemBrowseResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	expectedParent := filepath.Dir(resolvedDir)
	if resp.Parent != expectedParent {
		t.Errorf("parent = %q, want %q", resp.Parent, expectedParent)
	}
}

func TestHandleFilesystemBrowse_SortOrder(t *testing.T) {
	dir := t.TempDir()
	// Create directories in non-alphabetical order to verify case-insensitive sort.
	for _, name := range []string{"Zebra", "apple", "Mango", "banana"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatalf("creating dir %q: %v", name, err)
		}
	}

	r := newBrowseRouter(t)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+dir, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp filesystemBrowseResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp.Entries) != 4 {
		t.Fatalf("entries = %v, want 4 entries", resp.Entries)
	}

	// Verify case-insensitive sort order: apple, banana, Mango, Zebra.
	for i := 1; i < len(resp.Entries); i++ {
		prev := strings.ToLower(resp.Entries[i-1])
		curr := strings.ToLower(resp.Entries[i])
		if prev > curr {
			t.Errorf("entries not sorted case-insensitively at index %d: %q > %q",
				i, resp.Entries[i-1], resp.Entries[i])
		}
	}
}

func TestHandleFilesystemBrowse_PathIsFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "afile.txt")
	if err := os.WriteFile(filePath, []byte("content"), 0o644); err != nil {
		t.Fatalf("creating file: %v", err)
	}

	r := newBrowseRouter(t)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+filePath, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (path is a file, not a dir)", w.Code, http.StatusBadRequest)
	}
}

func TestHandleFilesystemBrowse_RootParentIsEmpty(t *testing.T) {
	// filepath.Dir of the filesystem root returns the root itself.
	// The handler must detect this and return parent="" so the UI
	// can disable the "go up" control at the root level.
	r := newBrowseRouter(t)
	ctx := middleware.WithTestRole(context.Background(), "administrator")

	// Use the filesystem root on the current OS.
	root := filepath.VolumeName("/") + string(filepath.Separator)

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+root, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	// Root should return 200 with parent == "".
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp struct {
		Parent string `json:"parent"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Parent != "" {
		t.Errorf("parent = %q at root, want empty string", resp.Parent)
	}
}
