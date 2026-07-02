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
	"github.com/sydlexius/stillwater/internal/library"
)

// newBrowseRouter returns a minimal Router sufficient for filesystem browse
// tests. allowedRoot becomes the configured music library path, i.e. the
// browse allowlist root (plus its parent, per the option-B confinement).
// A root of "/" is skipped as the filesystem root and grants nothing:
// isFilesystemRoot fails closed, so browsing "/" returns 403.
func newBrowseRouter(t *testing.T, allowedRoot string) *Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return &Router{
		logger:           logger,
		musicLibraryPath: allowedRoot,
	}
}

func TestHandleFilesystemBrowse_Valid(t *testing.T) {
	t.Parallel()
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

	r := newBrowseRouter(t, dir)
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
	t.Parallel()
	dir := t.TempDir()

	r := newBrowseRouter(t, dir)
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
	t.Parallel()
	r := newBrowseRouter(t, "/")
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse", nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleFilesystemBrowse_RelativePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
	}{
		{"relative path", "music/artists"},
		{"dot-dot traversal", "../etc"},
		{"just dot-dot", ".."},
	}

	r := newBrowseRouter(t, "/")
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
	t.Parallel()
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

	// The allowlist covers the symlink TARGET: confinement is checked on the
	// resolved path, so browsing via the link succeeds only because the
	// target itself is an allowed root.
	r := newBrowseRouter(t, outerDir)
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

func TestHandleFilesystemBrowse_NonExistentPathOutsideRootForbidden(t *testing.T) {
	t.Parallel()
	// Confinement is enforced BEFORE existence handling: a nonexistent path
	// outside every allowed root must return 403, never 404, so the endpoint
	// does not leak whether out-of-scope paths exist. Here the allowlist is
	// empty ("/" is skipped as the filesystem root), so any path is outside it.
	r := newBrowseRouter(t, "/")
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path=/nonexistent/path/that/does/not/exist", nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestHandleFilesystemBrowse_NonExistentPathInsideRootNotFound(t *testing.T) {
	t.Parallel()
	// The legitimate 404: a path that IS within a configured allowed root but
	// does not exist. Confinement passes, so returning 404 only reveals the
	// existence of in-scope paths -- which is expected behavior.
	root := t.TempDir()
	missing := filepath.Join(root, "does", "not", "exist")

	r := newBrowseRouter(t, root)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+missing, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleFilesystemBrowse_RequireAdmin(t *testing.T) {
	t.Parallel()
	// The RequireAdmin middleware is applied at route registration, but we
	// verify the pattern directly by wrapping the handler as the router does.
	dir := t.TempDir()
	r := newBrowseRouter(t, dir)

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
	t.Parallel()
	dir := t.TempDir()

	// Resolve the dir through any OS-level symlinks (e.g. /var -> /private/var on macOS).
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving dir symlinks: %v", err)
	}

	// Browse a subdirectory of the allowed root: its parent (the root
	// itself) is inside the allowlist and must be returned as-is.
	subDir := filepath.Join(dir, "artists")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}

	r := newBrowseRouter(t, dir)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+subDir, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp filesystemBrowseResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp.Parent != resolvedDir {
		t.Errorf("parent = %q, want %q", resp.Parent, resolvedDir)
	}

	// Browse the allowed root itself: its parent is ALSO an allowed browse
	// root (the parent rule), so it is returned rather than clamped.
	resolvedParent := filepath.Dir(resolvedDir)
	req2 := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+dir, nil)
	w2 := httptest.NewRecorder()

	r.handleFilesystemBrowse(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("root browse status = %d, want %d", w2.Code, http.StatusOK)
	}
	var resp2 filesystemBrowseResponse
	if err := json.NewDecoder(w2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decoding root browse response: %v", err)
	}
	if resp2.Parent != resolvedParent {
		t.Errorf("parent at allowlist root = %q, want %q (the allowed parent root)", resp2.Parent, resolvedParent)
	}

	// Browse the parent root itself: ITS parent lies OUTSIDE the allowlist,
	// so the handler must clamp it to "" (UI disables "go up").
	req3 := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+resolvedParent, nil)
	w3 := httptest.NewRecorder()

	r.handleFilesystemBrowse(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("parent-root browse status = %d, want %d", w3.Code, http.StatusOK)
	}
	var resp3 filesystemBrowseResponse
	if err := json.NewDecoder(w3.Body).Decode(&resp3); err != nil {
		t.Fatalf("decoding parent-root browse response: %v", err)
	}
	if resp3.Parent != "" {
		t.Errorf("parent at top of allowlist = %q, want empty string", resp3.Parent)
	}
}

func TestHandleFilesystemBrowse_SortOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create directories in non-alphabetical order to verify case-insensitive sort.
	for _, name := range []string{"Zebra", "apple", "Mango", "banana"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatalf("creating dir %q: %v", name, err)
		}
	}

	r := newBrowseRouter(t, dir)
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
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "afile.txt")
	if err := os.WriteFile(filePath, []byte("content"), 0o644); err != nil {
		t.Fatalf("creating file: %v", err)
	}

	r := newBrowseRouter(t, dir)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+filePath, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (path is a file, not a dir)", w.Code, http.StatusBadRequest)
	}
}

func TestHandleFilesystemBrowse_RootParentIsEmpty(t *testing.T) {
	t.Parallel()
	// filepath.Dir of the filesystem root returns the root itself; the
	// handler's parent-clamp logic (parent == resolved -> parent = "") exists
	// to handle that case defensively, so the UI could disable the "go up"
	// control at the root level. But a configured root of "/" is now (FIX-1,
	// isFilesystemRoot) skipped entirely when building the allowlist, so "/"
	// is fail-closed: this test asserts that a configured root of "/" never
	// resolves to an allowed browse of the filesystem root itself, rather
	// than asserting what the (now unreachable) parent-clamp would look like.
	r := newBrowseRouter(t, "/")
	ctx := middleware.WithTestRole(context.Background(), "administrator")

	// Use the filesystem root on the current OS.
	root := filepath.VolumeName("/") + string(filepath.Separator)

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+root, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	// The allowlist is empty (musicLibraryPath "/" is skipped as the
	// filesystem root), so browsing is rejected -- fail closed, never the
	// old fail-open "/" allows everything" behavior.
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestHandleFilesystemBrowse_ConfinementInsideAndOutsideRoot(t *testing.T) {
	t.Parallel()
	// allowedRoot is the configured library root; the allowlist also covers
	// its parent (base/media), so "outside" must live one level above the
	// parent to be genuinely unlistable.
	base := t.TempDir()
	allowedRoot := filepath.Join(base, "media", "music")
	outside := filepath.Join(base, "other")
	inside := filepath.Join(allowedRoot, "artists")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("creating dir %q: %v", d, err)
		}
	}

	r := newBrowseRouter(t, allowedRoot)
	ctx := middleware.WithTestRole(context.Background(), "administrator")

	// Inside the allowed root: permitted.
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+inside, nil)
	w := httptest.NewRecorder()
	r.handleFilesystemBrowse(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("inside root: status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Outside the allowed root: rejected with 403, no listing.
	req2 := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+outside, nil)
	w2 := httptest.NewRecorder()
	r.handleFilesystemBrowse(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Errorf("outside root: status = %d, want %d; body: %s", w2.Code, http.StatusForbidden, w2.Body.String())
	}
}

func TestHandleFilesystemBrowse_NoRootConfiguredRejects(t *testing.T) {
	t.Parallel()
	// No music library path and no library service: the allowlist is empty,
	// so every browse must be rejected rather than exposing the whole FS.
	dir := t.TempDir()
	r := newBrowseRouter(t, "")
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+dir, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no library root configured") {
		t.Errorf("body = %q, want a clear no-library-root error", w.Body.String())
	}
}

func TestHandleFilesystemBrowse_NoPrefixFalseMatch(t *testing.T) {
	t.Parallel()
	// The widest allowed root is the library root's PARENT (base/lib); a
	// directory whose name shares that parent's string prefix (base/lib vs
	// base/library) must NOT pass the confinement check.
	base := t.TempDir()
	root := filepath.Join(base, "lib", "music")
	sibling := filepath.Join(base, "library")
	for _, d := range []string{root, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("creating dir %q: %v", d, err)
		}
	}

	r := newBrowseRouter(t, root)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+sibling, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("prefix sibling: status = %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestHandleFilesystemBrowse_SymlinkEscapeRejected(t *testing.T) {
	t.Parallel()
	// A symlink INSIDE the allowed root pointing OUTSIDE the allowlist must
	// be rejected: confinement is checked on the symlink-resolved path. The
	// target lives above the root's allowed parent (base/media) so the
	// parent rule cannot legitimize it.
	base := t.TempDir()
	allowedRoot := filepath.Join(base, "media", "music")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{allowedRoot, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("creating dir %q: %v", d, err)
		}
	}
	linkPath := filepath.Join(allowedRoot, "escape")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("symlink creation not supported on this platform: %v", err)
	}

	r := newBrowseRouter(t, allowedRoot)
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+linkPath, nil)
	w := httptest.NewRecorder()

	r.handleFilesystemBrowse(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("symlink escape: status = %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestHandleFilesystemBrowse_ParentAndSiblingBrowsable(t *testing.T) {
	t.Parallel()
	// The parent rule: with a deep library root, the picker may browse the
	// root's parent and any sibling under it (to add a new library there),
	// but nothing above the parent.
	base := t.TempDir()
	root := filepath.Join(base, "data", "media", "music")
	parent := filepath.Join(base, "data", "media")
	sibling := filepath.Join(base, "data", "media", "other")
	outsideParent := filepath.Join(base, "data", "other2") // sibling of "media", one level above the parent
	for _, d := range []string{root, sibling, outsideParent} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("creating dir %q: %v", d, err)
		}
	}

	r := newBrowseRouter(t, root)
	ctx := middleware.WithTestRole(context.Background(), "administrator")

	cases := []struct {
		name string
		path string
		want int
	}{
		{"parent of the library root is browsable", parent, http.StatusOK},
		{"sibling of the library root is browsable", sibling, http.StatusOK},
		{"outside the parent is rejected", outsideParent, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+tc.path, nil)
			w := httptest.NewRecorder()
			r.handleFilesystemBrowse(w, req)
			if w.Code != tc.want {
				t.Errorf("path %q: status = %d, want %d; body: %s", tc.path, w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestBrowseParentRoot(t *testing.T) {
	t.Parallel()
	sep := string(os.PathSeparator)
	cases := []struct {
		name string
		root string
		want string
	}{
		{"deep root gets its parent", filepath.Join(sep+"data", "media", "music"), filepath.Join(sep+"data", "media")},
		{"two-level root gets its parent", filepath.Join(sep+"data", "music"), sep + "data"},
		{"one-level root: parent would be the fs root, skipped", sep + "music", ""},
		{"filesystem root itself: skipped", sep, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := browseParentRoot(tc.root); got != tc.want {
				t.Errorf("browseParentRoot(%q) = %q, want %q", tc.root, got, tc.want)
			}
		})
	}
}

func TestBrowseAllowedRoots_ShallowRootDoesNotAddFilesystemRoot(t *testing.T) {
	t.Parallel()
	// A one-level-deep library root (e.g. "/music") must stay strict: its
	// parent is the filesystem root and must NOT be added to the allowlist.
	// The path must not exist on the host (a real "/music" symlink would
	// resolve elsewhere and change what is being tested).
	sep := string(os.PathSeparator)
	shallow := sep + "stillwater-test-nonexistent-music"
	if _, err := os.Lstat(shallow); err == nil {
		t.Skipf("host unexpectedly has %s; cannot test the shallow-root guard hermetically", shallow)
	}
	r := newBrowseRouter(t, shallow)

	roots := r.browseAllowedRoots(context.Background())
	if len(roots) != 1 {
		t.Fatalf("allowlist = %v, want exactly the shallow root %q", roots, shallow)
	}
	for _, root := range roots {
		if root == sep {
			t.Fatalf("allowlist %v contains the filesystem root", roots)
		}
	}

	// Handler-level: browsing the filesystem root (or any top-level path
	// outside the shallow root) must still be rejected.
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	forbidden := []string{sep}
	if _, err := os.Stat(sep + "etc"); err == nil {
		forbidden = append(forbidden, sep+"etc")
	}
	for _, path := range forbidden {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+path, nil)
		w := httptest.NewRecorder()
		r.handleFilesystemBrowse(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("path %q: status = %d, want %d; body: %s", path, w.Code, http.StatusForbidden, w.Body.String())
		}
	}
}

func TestIsFilesystemRoot(t *testing.T) {
	t.Parallel()
	sep := string(os.PathSeparator)
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"filesystem root", sep, true},
		{"one-level path", sep + "music", false},
		{"deep path", filepath.Join(sep+"data", "media", "music"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFilesystemRoot(tc.path); got != tc.want {
				t.Errorf("isFilesystemRoot(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestBrowseAllowedRoots_ConfiguredRootAtFilesystemRootIsSkipped covers FIX-1:
// a configured (or DB-registered) library root that resolves to the
// filesystem root must never be added to the allowlist -- otherwise
// pathWithinRoot("/", anything) is always true and the whole host tree
// becomes browsable, silently defeating the empty-allowlist 403.
func TestBrowseAllowedRoots_ConfiguredRootAtFilesystemRootIsSkipped(t *testing.T) {
	t.Parallel()
	sep := string(os.PathSeparator)
	r := newBrowseRouter(t, sep)

	roots := r.browseAllowedRoots(context.Background())
	for _, root := range roots {
		if root == sep {
			t.Fatalf("allowlist %v contains the filesystem root", roots)
		}
	}
	if len(roots) != 0 {
		t.Fatalf("allowlist = %v, want empty (the only configured root is the filesystem root)", roots)
	}

	// Handler-level: with the allowlist empty, browsing anything is 403 --
	// the fail-closed empty-allowlist path, never a fail-open "/" match.
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	forbidden := []string{sep}
	if _, err := os.Stat(sep + "etc"); err == nil {
		forbidden = append(forbidden, sep+"etc")
	}
	for _, path := range forbidden {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+path, nil)
		w := httptest.NewRecorder()
		r.handleFilesystemBrowse(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("path %q: status = %d, want %d; body: %s", path, w.Code, http.StatusForbidden, w.Body.String())
		}
	}
}

// TestBrowseAllowedRoots_LibraryServiceErrorShrinksAllowlist covers the
// fail-toward-smaller-allowlist branch: when the DB-backed library lookup
// errors, the allowlist must fall back to just the configured music root
// (never widen, never panic) and paths outside that root stay 403.
func TestBrowseAllowedRoots_LibraryServiceErrorShrinksAllowlist(t *testing.T) {
	t.Parallel()
	musicRoot := t.TempDir()

	db := newTestDB(t)
	libSvc := library.NewService(db)
	// Force List() to error deterministically by closing the DB out from
	// under the service -- the query fails, exercising the "fail toward the
	// smaller allowlist" branch in browseAllowedRoots without needing a
	// hand-rolled stub interface (libraryService is a concrete *library.Service).
	if err := db.Close(); err != nil {
		t.Fatalf("closing test db: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := &Router{
		logger:           logger,
		musicLibraryPath: musicRoot,
		libraryService:   libSvc,
	}

	resolvedMusicRoot, err := filepath.EvalSymlinks(musicRoot)
	if err != nil {
		t.Fatalf("resolving musicRoot symlinks: %v", err)
	}
	roots := r.browseAllowedRoots(context.Background())
	if len(roots) == 0 {
		t.Fatal("allowlist is empty, want at least the music root")
	}
	found := false
	for _, root := range roots {
		if root == resolvedMusicRoot {
			found = true
		}
	}
	if !found {
		t.Errorf("allowlist %v does not contain the music root %q", roots, resolvedMusicRoot)
	}

	// A path well outside the music root (and its allowed parent) is still 403;
	// the DB error must not have widened access.
	outside := filepath.Join(filepath.Dir(filepath.Dir(resolvedMusicRoot)), "definitely-outside")
	ctx := middleware.WithTestRole(context.Background(), "administrator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+outside, nil)
	w := httptest.NewRecorder()
	r.handleFilesystemBrowse(w, req)
	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Errorf("outside path: status = %d, want %d or %d; body: %s", w.Code, http.StatusForbidden, http.StatusNotFound, w.Body.String())
	}
}

// TestBrowseAllowedRoots_RegisteredLibraryParentBrowsable covers the
// DB-registered-library branch of browseAllowedRoots (option B: the
// registered library's parent is browsable, same as the configured music
// path) using a real SQLite-backed library.Service rather than a stub.
func TestBrowseAllowedRoots_RegisteredLibraryParentBrowsable(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	libPath := filepath.Join(base, "data", "media", "music2")
	parent := filepath.Join(base, "data", "media")
	sibling := filepath.Join(base, "data", "media", "other2")
	outsideParent := filepath.Join(base, "data", "other3")
	for _, d := range []string{libPath, sibling, outsideParent} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("creating dir %q: %v", d, err)
		}
	}

	db := newTestDB(t)
	libSvc := library.NewService(db)
	lib := &library.Library{
		Name: "registered",
		Path: libPath,
		Type: library.TypeRegular,
	}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := &Router{
		logger:         logger,
		libraryService: libSvc,
	}

	ctx := middleware.WithTestRole(context.Background(), "administrator")

	// The registered library's parent is browsable (option B parent widening
	// applies to DB-registered libraries, not just the configured music path).
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+parent, nil)
	w := httptest.NewRecorder()
	r.handleFilesystemBrowse(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("parent of registered library: status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// A sibling under that parent is also browsable.
	req2 := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+sibling, nil)
	w2 := httptest.NewRecorder()
	r.handleFilesystemBrowse(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("sibling of registered library: status = %d, want %d; body: %s", w2.Code, http.StatusOK, w2.Body.String())
	}

	// Above the parent is still rejected.
	req3 := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/filesystem/browse?path="+outsideParent, nil)
	w3 := httptest.NewRecorder()
	r.handleFilesystemBrowse(w3, req3)
	if w3.Code != http.StatusForbidden {
		t.Errorf("outside the parent: status = %d, want %d; body: %s", w3.Code, http.StatusForbidden, w3.Body.String())
	}
}

func TestPathWithinRoot(t *testing.T) {
	t.Parallel()
	sep := string(os.PathSeparator)
	cases := []struct {
		name string
		root string
		path string
		want bool
	}{
		{"equal", sep + "music", sep + "music", true},
		{"descendant", sep + "music", filepath.Join(sep+"music", "artists"), true},
		{"outside", sep + "music", sep + "etc", false},
		{"parent", sep + "music", sep, false},
		{"prefix false match", sep + "lib", sep + "library", false},
		{"fs root allows all", sep, sep + "anything", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathWithinRoot(tc.root, tc.path); got != tc.want {
				t.Errorf("pathWithinRoot(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
			}
		})
	}
}
