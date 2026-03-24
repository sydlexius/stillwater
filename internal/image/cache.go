package image

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// CacheStats returns the total size in bytes, file count, and unique artist
// directory count for the image cache. Returns all zeros if the directory
// does not exist. Symlinks are skipped.
func CacheStats(cacheDir string) (sizeBytes int64, fileCount int, artistCount int, err error) {
	artists := make(map[string]struct{})

	walkErr := filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil //nolint:nilerr // skip unreadable files
		}
		sizeBytes += info.Size()
		fileCount++

		// Track unique artist directories (immediate parent of file).
		parent := filepath.Base(filepath.Dir(path))
		if parent != filepath.Base(cacheDir) {
			artists[parent] = struct{}{}
		}
		return nil
	})

	if walkErr != nil && !errors.Is(walkErr, os.ErrNotExist) {
		return 0, 0, 0, walkErr
	}
	artistCount = len(artists)
	return sizeBytes, fileCount, artistCount, nil
}

type cachedFile struct {
	path    string
	size    int64
	modTime time.Time
}

// EnforceCacheLimit checks the total size of the image cache directory and
// evicts the oldest files until the total is at or below maxBytes. If maxBytes
// is zero, no eviction is performed (unlimited mode). Tolerates concurrent
// callers: os.ErrNotExist on individual deletions is silently skipped.
func EnforceCacheLimit(cacheDir string, maxBytes int64, logger *slog.Logger) error {
	if maxBytes <= 0 {
		return nil
	}

	var files []cachedFile
	var total int64

	walkErr := filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil //nolint:nilerr // skip unreadable files
		}
		files = append(files, cachedFile{path: path, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, os.ErrNotExist) {
		return walkErr
	}

	if total <= maxBytes {
		return nil
	}

	// Sort oldest first.
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	dirs := make(map[string]struct{})
	for _, f := range files {
		if total <= maxBytes {
			break
		}
		if err := os.Remove(f.path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			logger.Warn("cache eviction: failed to remove file", "path", f.path, "error", err)
			continue
		}
		logger.Info("cache eviction: removed file", "path", f.path, "freed_bytes", f.size)
		total -= f.size
		dirs[filepath.Dir(f.path)] = struct{}{}
	}

	// Clean up empty subdirectories.
	for dir := range dirs {
		if dir == cacheDir {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			continue
		}
		os.Remove(dir) //nolint:errcheck // best-effort cleanup
	}

	return nil
}
