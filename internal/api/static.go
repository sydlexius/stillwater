package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"net/http"
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
	fsys   fs.FS
}

// NewStaticAssets creates a StaticAssets manager that scans the given filesystem.
func NewStaticAssets(fsys fs.FS, logger *slog.Logger) *StaticAssets {
	if fsys == nil {
		panic("api.NewStaticAssets: fsys must not be nil")
	}
	sa := &StaticAssets{
		hashes: make(map[string]string),
		fsys:   fsys,
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
	fileServer := http.FileServerFS(sa.fsys)
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

// Rescan rescans the static filesystem and updates hashes.
// Useful if files change at runtime (e.g., during development with os.DirFS).
func (sa *StaticAssets) Rescan(logger *slog.Logger) {
	sa.scan(logger)
}

func (sa *StaticAssets) scan(logger *slog.Logger) {
	hashes := make(map[string]string)

	if err := fs.WalkDir(sa.fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			logger.Warn("failed to walk static asset", "path", path, "error", err)
			return nil //nolint:nilerr // WalkDir callback: log and skip problem entries to continue walking
		}
		if d.IsDir() {
			return nil
		}

		data, err := fs.ReadFile(sa.fsys, path)
		if err != nil {
			logger.Warn("failed to hash static file", "path", path, "error", err)
			return nil
		}

		h := sha256.Sum256(data)
		relativePath := "/" + strings.ReplaceAll(path, "\\", "/")
		hashes[relativePath] = hex.EncodeToString(h[:])

		return nil
	}); err != nil {
		logger.Warn("failed to scan static filesystem", "error", err)
	}

	sa.mu.Lock()
	sa.hashes = hashes
	sa.mu.Unlock()

	logger.Info("static assets scanned", slog.Int("files", len(hashes)))
}
