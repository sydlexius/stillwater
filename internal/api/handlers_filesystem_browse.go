package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filesystemBrowseResponse is the JSON response for the browse endpoint.
type filesystemBrowseResponse struct {
	// Path is the resolved absolute path that was listed.
	Path string `json:"path"`
	// Entries contains the names of direct subdirectories within Path.
	Entries []string `json:"entries"`
	// Parent is the parent directory of Path, or an empty string when Path is the filesystem root.
	Parent string `json:"parent"`
}

// browseAllowedRoots returns the allowlist of directory roots the filesystem
// browse endpoint may serve. The rule: existing roots + each root's parent
// (so the add-library picker can browse siblings to add a new library),
// never the filesystem root. Allowed are exactly:
//   - the configured default music library path (SW_MUSIC_PATH / music.library_path)
//   - the path of every library registered through the UI (pathless
//     API-only libraries contribute nothing)
//   - the PARENT directory of each of the above, so an operator can
//     navigate to a sibling directory of an existing library to add a new
//     one -- EXCEPT when that parent is the filesystem root ("/") or a
//     volume root, which would open the whole host tree; such shallow
//     roots stay strict (the root itself only). See browseParentRoot.
//
// Each root (and each added parent) is resolved through symlinks when
// possible so it compares correctly against the symlink-resolved request
// path; a root that does not exist yet falls back to its cleaned form.
// The result is de-duplicated.
func (r *Router) browseAllowedRoots(ctx context.Context) []string {
	var raw []string
	if r.musicLibraryPath != "" {
		raw = append(raw, r.musicLibraryPath)
	}
	if r.libraryService != nil {
		libs, err := r.libraryService.List(ctx)
		if err != nil {
			// Fail toward the smaller allowlist: browsing library paths the
			// lookup would have permitted returns 403 until the DB recovers.
			r.logger.Error("listing libraries for filesystem browse allowlist", "error", err)
		}
		for i := range libs {
			if libs[i].Path != "" {
				raw = append(raw, libs[i].Path)
			}
		}
	}

	roots := make([]string, 0, len(raw)*2)
	seen := make(map[string]struct{}, len(raw)*2)
	add := func(path string) {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		if _, dup := seen[path]; dup {
			return
		}
		seen[path] = struct{}{}
		roots = append(roots, path)
	}
	for _, root := range raw {
		cleaned := filepath.Clean(root)
		// Resolve the root first so the parent is computed from the real
		// location of the library, not from a symlink alias of it.
		resolved := cleaned
		if res, err := filepath.EvalSymlinks(cleaned); err == nil {
			resolved = res
		}
		// Never add the filesystem root itself as a browse root: if a
		// configured/registered library path resolves to "/" (or a volume
		// root), adding it here would make the whole host tree browsable
		// and the empty-allowlist 403 below would never trip. Skip it
		// instead, so browse fails closed (403) for that root.
		if isFilesystemRoot(resolved) {
			r.logger.Warn("configured library root resolves to the filesystem root; filesystem browse is disabled for it", "root", root)
			continue
		}
		add(resolved)
		if parent := browseParentRoot(resolved); parent != "" {
			add(parent)
		}
	}
	return roots
}

// isFilesystemRoot reports whether path is the filesystem root ("/") or a
// volume root (e.g. Windows "C:\"), i.e. a path with no further parent.
func isFilesystemRoot(path string) bool {
	return path == "/" || filepath.Dir(path) == path
}

// browseParentRoot returns the parent directory of a (cleaned, absolute)
// library root to include as an additional allowed browse root, or "" when
// the root is too shallow to have a sane parent. The hard guard: never widen
// the allowlist to the filesystem root ("/") or a volume root, so a
// one-level-deep library like "/music" stays strict (the root itself only)
// instead of exposing the entire host tree.
func browseParentRoot(root string) string {
	parent := filepath.Dir(root)
	if parent == root || parent == "." || isFilesystemRoot(parent) {
		return ""
	}
	return parent
}

// pathWithinRoot reports whether path is root itself or a descendant of root.
// Both arguments must be absolute and symlink-resolved. The comparison is on
// path boundaries (via filepath.Rel), so /library is NOT within /lib.
func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// handleFilesystemBrowse lists direct subdirectories under a given path.
// GET /api/v1/filesystem/browse?path=/some/dir
//
// Security: admin-only (RequireAdmin applied at route registration).
// The path must be an absolute path. Symlinks are resolved before listing;
// the resolved path must still be absolute to prevent any escape attempts.
// The resolved path must also fall inside one of the allowed browse roots
// (browseAllowedRoots: the configured library roots plus each root's parent
// directory, never the filesystem root) -- anything else is rejected with
// 403 so an admin cannot enumerate the whole host filesystem.
// Only directories are returned; files are excluded.
func (r *Router) handleFilesystemBrowse(w http.ResponseWriter, req *http.Request) {
	rawPath := req.URL.Query().Get("path")
	if rawPath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path query parameter is required"})
		return
	}

	// Reject non-absolute paths immediately, before any resolution.
	if !filepath.IsAbs(rawPath) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path must be absolute"})
		return
	}

	// Normalise the path. After filepath.Clean all ".." sequences are resolved
	// into the canonical path; no further traversal check is needed here.
	// filepath.EvalSymlinks below provides the authoritative absolute-path check.
	cleaned := filepath.Clean(rawPath)

	// Resolve symlinks and verify the resolved path is still absolute.
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "path not found"})
			return
		}
		r.logger.Error("resolving symlinks for filesystem browse", "path", cleaned, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path could not be resolved"})
		return
	}

	if !filepath.IsAbs(resolved) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "resolved path is not absolute"})
		return
	}

	// Confine browsing to the allowed browse roots (library roots plus
	// their parents). Without this an admin could enumerate the entire
	// host filesystem tree.
	roots := r.browseAllowedRoots(req.Context())
	if len(roots) == 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no library root configured; filesystem browse is disabled"})
		return
	}
	allowed := false
	for _, root := range roots {
		if pathWithinRoot(root, resolved) {
			allowed = true
			break
		}
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "path is outside the allowed browse roots"})
		return
	}

	// Verify the target is a directory.
	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "path not found"})
			return
		}
		r.logger.Error("stat for filesystem browse", "path", resolved, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to stat path"})
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is not a directory"})
		return
	}

	// List directory entries and collect subdirectory names only.
	entries, err := os.ReadDir(resolved)
	if err != nil {
		r.logger.Error("reading directory for filesystem browse", "path", resolved, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read directory"})
		return
	}

	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip hidden directories (those starting with ".").
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dirs = append(dirs, e.Name())
	}

	// Case-insensitive alphabetical sort.
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j])
	})

	// Determine the parent path. At the filesystem root, parent is empty.
	parent := filepath.Dir(resolved)
	if parent == resolved {
		// filepath.Dir("/") == "/" -- we are at the root.
		parent = ""
	}
	// Clamp the parent to the allowlist: at the top of an allowed root the
	// parent lies outside it and browsing there would be rejected, so return
	// "" to let the UI disable its "go up" control.
	if parent != "" {
		parentAllowed := false
		for _, root := range roots {
			if pathWithinRoot(root, parent) {
				parentAllowed = true
				break
			}
		}
		if !parentAllowed {
			parent = ""
		}
	}

	writeJSON(w, http.StatusOK, filesystemBrowseResponse{
		Path:    resolved,
		Entries: dirs,
		Parent:  parent,
	})
}
