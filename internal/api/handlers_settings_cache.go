package api

import (
	"net/http"
	"os"
	"path/filepath"

	img "github.com/sydlexius/stillwater/internal/image"
)

// handleCacheStats returns size, file count, and artist count for the image cache.
// GET /api/v1/settings/cache/stats
func (r *Router) handleCacheStats(w http.ResponseWriter, req *http.Request) {
	if r.imageCacheDir == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"size_bytes":   0,
			"file_count":   0,
			"artist_count": 0,
		})
		return
	}

	size, files, artists, err := img.CacheStats(r.imageCacheDir)
	if err != nil {
		r.logger.Error("computing cache stats", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"size_bytes":   size,
		"file_count":   files,
		"artist_count": artists,
	})
}

// handleCacheClear deletes all files in the image cache directory and resets
// exists_flag on artist_images for pathless artists. Uses best-effort deletion.
// DELETE /api/v1/settings/cache
func (r *Router) handleCacheClear(w http.ResponseWriter, req *http.Request) {
	if r.imageCacheDir == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"files_deleted": 0,
			"bytes_freed":   int64(0),
		})
		return
	}

	// Best-effort deletion: walk and remove all files, then empty dirs.
	var deleted int
	var freed int64
	filepath.WalkDir(r.imageCacheDir, func(path string, d os.DirEntry, err error) error { //nolint:errcheck
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // skip errors during best-effort deletion
		}
		info, _ := d.Info()
		if removeErr := os.Remove(path); removeErr != nil { //nolint:gosec // cache dir is internal, not user-controlled
			r.logger.Warn("cache clear: failed to remove file", "path", path, "error", removeErr)
			return nil
		}
		deleted++
		if info != nil {
			freed += info.Size()
		}
		return nil
	})

	// Remove empty subdirectories.
	entries, _ := os.ReadDir(r.imageCacheDir)
	for _, e := range entries {
		if e.IsDir() {
			subdir := filepath.Join(r.imageCacheDir, e.Name())
			sub, _ := os.ReadDir(subdir)
			if len(sub) == 0 {
				os.Remove(subdir) //nolint:errcheck
			}
		}
	}

	// Reset exists_flag for pathless artists whose images were in the cache.
	_, dbErr := r.db.ExecContext(req.Context(),
		`UPDATE artist_images SET exists_flag = 0
		 WHERE artist_id IN (SELECT id FROM artists WHERE path = '')`)
	if dbErr != nil {
		r.logger.Error("resetting exists_flag for pathless artists", "error", dbErr)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"files_deleted": deleted,
		"bytes_freed":   freed,
	})
}
