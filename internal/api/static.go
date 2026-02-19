package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// StaticAssets manages static file serving with content-hash cache busting.
// Files are served with a version query parameter (e.g., /static/css/styles.css?v=abc123).
// When the hash matches, responses include immutable cache headers for aggressive browser caching.
// When files change, the hash changes, forcing browsers to fetch the new version.
type StaticAssets struct {
	mu     sync.RWMutex
	hashes map[string]string // path -> content hash
	dir    string
}

// NewStaticAssets creates a StaticAssets manager that scans the given directory.
func NewStaticAssets(dir string, logger *slog.Logger) *StaticAssets {
	sa := &StaticAssets{
		hashes: make(map[string]string),
		dir:    dir,
	}
	sa.scan(logger)
	return sa
}

// Path returns a cache-busted URL for a static file.
// Example: Path("/css/styles.css") returns "/static/css/styles.css?v=a1b2c3d4"
func (sa *StaticAssets) Path(filePath string) string {
	sa.mu.RLock()
	hash, ok := sa.hashes[filePath]
	sa.mu.RUnlock()

	if !ok {
		return "/static" + filePath
	}
	return "/static" + filePath + "?v=" + hash[:12]
}

// Handler returns an HTTP handler that serves static files with appropriate cache headers.
func (sa *StaticAssets) Handler(basePath string) http.Handler {
	fileServer := http.FileServer(http.Dir(sa.dir))
	stripped := http.StripPrefix(basePath+"/static/", fileServer)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If the request includes a version hash, set aggressive caching
		if v := r.URL.Query().Get("v"); v != "" {
			relativePath := strings.TrimPrefix(r.URL.Path, basePath+"/static")
			sa.mu.RLock()
			expectedHash, exists := sa.hashes[relativePath]
			sa.mu.RUnlock()

			if exists && strings.HasPrefix(expectedHash, v) {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				// Hash mismatch - serve but don't cache aggressively
				w.Header().Set("Cache-Control", "public, max-age=3600")
			}
		} else {
			// No version parameter - short cache to allow updates
			w.Header().Set("Cache-Control", "public, max-age=300")
		}

		stripped.ServeHTTP(w, r)
	})
}

// Rescan rescans the static directory and updates hashes.
// Useful if files change at runtime (e.g., during development).
func (sa *StaticAssets) Rescan(logger *slog.Logger) {
	sa.scan(logger)
}

func (sa *StaticAssets) scan(logger *slog.Logger) {
	hashes := make(map[string]string)

	filepath.WalkDir(sa.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			logger.Warn("failed to hash static file", "path", path, "error", err)
			return nil
		}

		h := sha256.Sum256(data)
		relativePath := "/" + strings.ReplaceAll(strings.TrimPrefix(path, sa.dir), "\\", "/")
		relativePath = strings.TrimPrefix(relativePath, "/")
		relativePath = "/" + relativePath
		hashes[relativePath] = hex.EncodeToString(h[:])

		return nil
	})

	sa.mu.Lock()
	sa.hashes = hashes
	sa.mu.Unlock()

	logger.Info("static assets scanned", slog.Int("files", len(hashes)))
}
