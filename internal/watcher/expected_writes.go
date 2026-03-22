package watcher

import (
	"sync"
	"time"
)

// ExpectedWrites tracks file paths that Stillwater is about to write.
// Callers register paths before writing and clear them after. The watcher
// uses this set to distinguish Stillwater's own writes from external ones.
type ExpectedWrites struct {
	mu    sync.RWMutex
	paths map[string]time.Time // path -> registration time
}

// NewExpectedWrites creates a new ExpectedWrites tracker.
func NewExpectedWrites() *ExpectedWrites {
	return &ExpectedWrites{
		paths: make(map[string]time.Time),
	}
}

// Add registers a path as an expected write. Call this before writing a file.
func (ew *ExpectedWrites) Add(path string) {
	ew.mu.Lock()
	defer ew.mu.Unlock()
	ew.paths[path] = time.Now()
}

// AddAll registers multiple paths as expected writes.
func (ew *ExpectedWrites) AddAll(paths []string) {
	ew.mu.Lock()
	defer ew.mu.Unlock()
	now := time.Now()
	for _, p := range paths {
		ew.paths[p] = now
	}
}

// Remove clears a path from the expected set. Call this after the write completes.
func (ew *ExpectedWrites) Remove(path string) {
	ew.mu.Lock()
	defer ew.mu.Unlock()
	delete(ew.paths, path)
}

// RemoveAll clears multiple paths from the expected set.
func (ew *ExpectedWrites) RemoveAll(paths []string) {
	ew.mu.Lock()
	defer ew.mu.Unlock()
	for _, p := range paths {
		delete(ew.paths, p)
	}
}

// IsExpected reports whether the given path is in the expected-writes set.
func (ew *ExpectedWrites) IsExpected(path string) bool {
	ew.mu.RLock()
	defer ew.mu.RUnlock()
	_, ok := ew.paths[path]
	return ok
}

// Prune removes entries older than maxAge. Call periodically to prevent leaks
// from writes that crashed before clearing. Returns the number of pruned entries.
func (ew *ExpectedWrites) Prune(maxAge time.Duration) int {
	ew.mu.Lock()
	defer ew.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	pruned := 0
	for path, registered := range ew.paths {
		if registered.Before(cutoff) {
			delete(ew.paths, path)
			pruned++
		}
	}
	return pruned
}
