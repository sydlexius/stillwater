package rule

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/image"
)

// fsCacheTTL is the default time-to-live for cached filesystem metadata.
// Entries older than this are considered stale and re-fetched on next access.
// The 60-second default matches the watcher poll interval, providing a safety
// net for missed fsnotify events.
const fsCacheTTL = 60 * time.Second

// maxFSCacheSize is the maximum number of entries in each cache map (dirs and
// stats). When this limit is reached, the oldest entry is evicted to make
// room. At ~800 artists with ~20 files each, the stat cache needs at most
// ~4000 entries; 5000 provides headroom for larger libraries.
const maxFSCacheSize = 5000

// DirEntry is a lightweight, serializable representation of os.DirEntry
// suitable for caching. It stores only the fields that rule checkers need
// (name and whether the entry is a directory), avoiding retention of the
// underlying os.FileInfo handle.
type DirEntry struct {
	Name  string
	IsDir bool
}

// FileStat is a lightweight representation of os.FileInfo for caching.
// It captures the subset of metadata that rule checkers actually use:
// modification time (for logo bounds cache keys), size, and directory flag.
type FileStat struct {
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// dirCacheEntry holds a cached directory listing along with the time it was
// fetched, so the cache can enforce TTL-based expiry.
type dirCacheEntry struct {
	entries   []DirEntry
	fetchedAt time.Time
}

// statCacheEntry holds a cached os.Stat result (or error) along with the
// fetch timestamp. Caching errors (e.g., file not found) prevents repeated
// stat calls for paths that consistently fail.
type statCacheEntry struct {
	info      FileStat
	fetchedAt time.Time
	err       error
}

// FSCache caches filesystem metadata (directory listings and stat results)
// to reduce I/O during rule evaluation. Entries are invalidated explicitly
// by path (for watcher events) or expire after a configurable TTL as a
// safety net. The cache is bounded in size to prevent unbounded memory
// growth in large libraries.
//
// All methods are safe for concurrent access from multiple goroutines.
type FSCache struct {
	mu      sync.RWMutex
	dirs    map[string]*dirCacheEntry
	stats   map[string]*statCacheEntry
	ttl     time.Duration
	maxSize int
	logger  *slog.Logger

	// dirKeys and statKeys track insertion order for FIFO eviction when
	// the cache reaches maxSize. The oldest entry is evicted first.
	dirKeys  []string
	statKeys []string
}

// NewFSCache creates a new filesystem metadata cache. The ttl parameter
// controls how long entries remain fresh (use 0 for the default 60s).
// The maxSize parameter bounds total entries per map (use 0 for the
// default 5000).
func NewFSCache(ttl time.Duration, maxSize int, logger *slog.Logger) *FSCache {
	if ttl <= 0 {
		ttl = fsCacheTTL
	}
	if maxSize <= 0 {
		maxSize = maxFSCacheSize
	}
	return &FSCache{
		dirs:    make(map[string]*dirCacheEntry),
		stats:   make(map[string]*statCacheEntry),
		ttl:     ttl,
		maxSize: maxSize,
		logger:  logger,
	}
}

// ReadDir returns a cached directory listing for the given path, or fetches
// it from the filesystem and caches the result. The returned DirEntry slice
// is a lightweight copy; callers may iterate freely without holding a lock.
func (c *FSCache) ReadDir(path string) ([]DirEntry, error) {
	// Fast path: check cache under read lock.
	c.mu.RLock()
	if entry, ok := c.dirs[path]; ok && time.Since(entry.fetchedAt) < c.ttl {
		// Return a clone so callers cannot mutate the cached slice.
		clone := make([]DirEntry, len(entry.entries))
		copy(clone, entry.entries)
		c.mu.RUnlock()
		return clone, nil
	}
	c.mu.RUnlock()

	// Slow path: fetch from filesystem and store under write lock.
	osEntries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	// Convert os.DirEntry slice to lightweight DirEntry slice. This avoids
	// retaining file handles or OS-specific state in the cache.
	entries := make([]DirEntry, len(osEntries))
	for i, e := range osEntries {
		entries[i] = DirEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// If the key already exists, update in place without growing the key list.
	if _, exists := c.dirs[path]; exists {
		c.dirs[path] = &dirCacheEntry{entries: entries, fetchedAt: time.Now()}
		// Return a clone so callers cannot mutate the cached slice.
		clone := make([]DirEntry, len(entries))
		copy(clone, entries)
		return clone, nil
	}

	// Evict the oldest entry when at capacity.
	if len(c.dirs) >= c.maxSize && len(c.dirKeys) > 0 {
		oldest := c.dirKeys[0]
		c.dirKeys[0] = "" // clear reference so evicted path can be GC'd
		c.dirKeys = c.dirKeys[1:]
		delete(c.dirs, oldest)
	}

	c.dirs[path] = &dirCacheEntry{entries: entries, fetchedAt: time.Now()}
	c.dirKeys = append(c.dirKeys, path)
	// Return a clone so callers cannot mutate the cached slice.
	clone := make([]DirEntry, len(entries))
	copy(clone, entries)
	return clone, nil
}

// Stat returns cached file metadata for the given path, or fetches it from
// the filesystem and caches the result. Errors (e.g., file not found) are
// also cached to prevent repeated failing stat calls for the same path.
func (c *FSCache) Stat(path string) (FileStat, error) {
	// Fast path: check cache under read lock.
	c.mu.RLock()
	if entry, ok := c.stats[path]; ok && time.Since(entry.fetchedAt) < c.ttl {
		info, statErr := entry.info, entry.err
		c.mu.RUnlock()
		return info, statErr
	}
	c.mu.RUnlock()

	// Slow path: fetch from filesystem and store under write lock.
	fi, err := os.Stat(path)

	var info FileStat
	if err == nil {
		info = FileStat{
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
			IsDir:   fi.IsDir(),
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// If the key already exists, update in place without growing the key list.
	if _, exists := c.stats[path]; exists {
		c.stats[path] = &statCacheEntry{info: info, fetchedAt: time.Now(), err: err}
		if err != nil {
			return FileStat{}, err
		}
		return info, nil
	}

	// Evict the oldest entry when at capacity.
	if len(c.stats) >= c.maxSize && len(c.statKeys) > 0 {
		oldest := c.statKeys[0]
		c.statKeys[0] = "" // clear reference so evicted path can be GC'd
		c.statKeys = c.statKeys[1:]
		delete(c.stats, oldest)
	}

	c.stats[path] = &statCacheEntry{info: info, fetchedAt: time.Now(), err: err}
	c.statKeys = append(c.statKeys, path)

	if err != nil {
		return FileStat{}, err
	}
	return info, nil
}

// InvalidatePath removes cache entries for a specific file or directory path.
// It also invalidates the parent directory's listing, since a file change
// within a directory means the listing is stale. This is the primary
// invalidation path for fsnotify events.
func (c *FSCache) InvalidatePath(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove the exact path from both caches.
	delete(c.dirs, path)
	delete(c.stats, path)

	// Remove key tracking entries for the evicted paths.
	c.dirKeys = removeFromSlice(c.dirKeys, path)
	c.statKeys = removeFromSlice(c.statKeys, path)

	// Invalidate all descendant entries. When a directory is deleted and
	// recreated, cached stat entries for files inside it would have stale
	// ModTimes, causing the logo bounds cache to serve outdated results.
	prefix := path + string(os.PathSeparator)
	for key := range c.dirs {
		if strings.HasPrefix(key, prefix) {
			delete(c.dirs, key)
			c.dirKeys = removeFromSlice(c.dirKeys, key)
		}
	}
	for key := range c.stats {
		if strings.HasPrefix(key, prefix) {
			delete(c.stats, key)
			c.statKeys = removeFromSlice(c.statKeys, key)
		}
	}

	// Also invalidate the parent directory listing, since adding or removing
	// a file changes the ReadDir result for the parent.
	parent := filepath.Dir(path)
	if parent != path { // avoid infinite loop on root
		delete(c.dirs, parent)
		c.dirKeys = removeFromSlice(c.dirKeys, parent)
	}
}

// InvalidateAll clears the entire cache. Use this as a safety-net reset
// (e.g., after a full library rescan or when the watcher restarts).
func (c *FSCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirs = make(map[string]*dirCacheEntry)
	c.stats = make(map[string]*statCacheEntry)
	// Nil out the key slices so the backing arrays (and the path strings
	// they reference) become eligible for garbage collection.
	c.dirKeys = nil
	c.statKeys = nil
}

// Len returns the total number of entries across both the directory and stat
// caches. Useful for monitoring and tests.
func (c *FSCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.dirs) + len(c.stats)
}

// removeFromSlice returns a new slice with the first occurrence of target
// removed. If target is not found, the original slice is returned unchanged.
func removeFromSlice(s []string, target string) []string {
	for i, v := range s {
		if v == target {
			copy(s[i:], s[i+1:])
			s[len(s)-1] = "" // clear trailing slot to avoid retaining references
			return s[:len(s)-1]
		}
	}
	return s
}

// readDirCached returns a cached directory listing when the Engine has an
// FSCache configured, or falls back to a direct os.ReadDir call otherwise.
// This method is the single entry point for all directory reads within rule
// checkers, ensuring consistent caching behavior.
func (e *Engine) readDirCached(path string) ([]DirEntry, error) {
	if e.fsCache != nil {
		return e.fsCache.ReadDir(path)
	}
	// Fallback: no cache, read directly from filesystem.
	osEntries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries := make([]DirEntry, len(osEntries))
	for i, oe := range osEntries {
		entries[i] = DirEntry{
			Name:  oe.Name(),
			IsDir: oe.IsDir(),
		}
	}
	return entries, nil
}

// fileModTimeCached returns the modification time of a file using the
// FSCache when available, or falls back to a direct os.Stat call.
func (e *Engine) fileModTimeCached(path string) (time.Time, error) {
	if e.fsCache != nil {
		info, err := e.fsCache.Stat(path)
		if err != nil {
			return time.Time{}, err
		}
		return info.ModTime, nil
	}
	// Fallback: no cache, stat directly.
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// buildLowerToActual builds a case-insensitive filename lookup map from a
// DirEntry slice. Only non-directory entries are included. This is a common
// operation used by multiple checkers to find image files by pattern.
func buildLowerToActual(entries []DirEntry) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			m[strings.ToLower(e.Name)] = e.Name
		}
	}
	return m
}

// getImageDimensionsCached finds the first matching image file in the given
// directory using the Engine's cached directory listing and returns its
// pixel dimensions. The directory listing is cached but the file itself is
// opened directly (image content may have changed independently of the
// directory listing).
func (e *Engine) getImageDimensionsCached(dirPath string, patterns []string) (int, int, error) {
	entries, err := e.readDirCached(dirPath)
	if err != nil {
		return 0, 0, fmt.Errorf("reading directory: %w", err)
	}

	lowerToActual := buildLowerToActual(entries)

	for _, pattern := range patterns {
		if actual, ok := lowerToActual[strings.ToLower(pattern)]; ok {
			p := filepath.Join(dirPath, actual)
			f, openErr := os.Open(p) //nolint:gosec // G304: path from trusted library root
			if openErr != nil {
				continue
			}
			w, h, dimErr := image.GetDimensions(f)
			f.Close() //nolint:errcheck
			if dimErr != nil {
				continue
			}
			return w, h, nil
		}
	}

	return 0, 0, fmt.Errorf("no matching image in %s", dirPath)
}
