package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sydlexius/stillwater/internal/library"
)

// ProbeCache caches the results of fsnotify support probes for library paths.
type ProbeCache struct {
	mu      sync.RWMutex
	results map[string]bool
}

// NewProbeCache creates an empty probe cache.
func NewProbeCache() *ProbeCache {
	return &ProbeCache{
		results: make(map[string]bool),
	}
}

// Get returns whether fsnotify is supported for the given path.
// The second return value is false if the path has not been probed.
func (pc *ProbeCache) Get(path string) (supported bool, ok bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	supported, ok = pc.results[path]
	return
}

// Set stores a probe result for the given path.
func (pc *ProbeCache) Set(path string, supported bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.results[path] = supported
}

// ProbeFSNotify tests whether fsnotify delivers events for the given path.
// It creates a temporary directory inside path, watches for the Create event,
// and returns true if the event arrives within the timeout.
func ProbeFSNotify(path string, timeout time.Duration) bool {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return false
	}
	defer w.Close() //nolint:errcheck

	if err := w.Add(path); err != nil {
		return false
	}

	// Create a probe directory with a random suffix.
	probeName := fmt.Sprintf(".stillwater_probe_%d", rand.Int63()) //nolint:gosec // G404: not security-sensitive
	probeDir := filepath.Join(path, probeName)

	if err := os.Mkdir(probeDir, 0o750); err != nil { //nolint:gosec // G301: probe dir is temporary
		return false
	}
	defer os.Remove(probeDir) //nolint:errcheck

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return false
			}
			if ev.Has(fsnotify.Create) && filepath.Base(ev.Name) == probeName {
				return true
			}
		case <-w.Errors:
			return false
		case <-timer.C:
			return false
		}
	}
}

// ProbeAll probes all non-degraded libraries and populates the cache.
// Called synchronously at startup before the watcher goroutine starts.
func (pc *ProbeCache) ProbeAll(ctx context.Context, libs []library.Library, logger *slog.Logger) {
	for _, lib := range libs {
		if lib.IsDegraded() {
			continue
		}
		// Verify path exists.
		info, err := os.Stat(lib.Path)
		if err != nil || !info.IsDir() {
			pc.Set(lib.Path, false)
			logger.Warn("library path not accessible for probe",
				"library", lib.Name, "path", lib.Path)
			continue
		}

		supported := ProbeFSNotify(lib.Path, 2*time.Second)
		pc.Set(lib.Path, supported)
		logger.Info("fsnotify probe result",
			"library", lib.Name,
			"path", lib.Path,
			"supported", supported,
		)

		// Check for context cancellation between probes.
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}
