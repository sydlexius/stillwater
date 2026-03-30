package api

import (
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

// handleFilesystemBrowse lists direct subdirectories under a given path.
// GET /api/v1/filesystem/browse?path=/some/dir
//
// Security: admin-only (RequireAdmin applied at route registration).
// The path must be an absolute path. Symlinks are resolved before listing;
// the resolved path must still be absolute to prevent any escape attempts.
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

	// Normalise the path and reject any remaining ".." segments as a defense-in-depth check.
	cleaned := filepath.Clean(rawPath)
	if strings.Contains(cleaned, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path must not contain traversal segments"})
		return
	}

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

	writeJSON(w, http.StatusOK, filesystemBrowseResponse{
		Path:    resolved,
		Entries: dirs,
		Parent:  parent,
	})
}
