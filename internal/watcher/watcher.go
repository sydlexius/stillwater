package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/library"
)

// LibraryLister retrieves the list of configured libraries.
type LibraryLister interface {
	List(ctx context.Context) ([]library.Library, error)
}

// Service watches library root directories for subdirectory creation and
// removal, triggering scans and publishing events in response.
type Service struct {
	scanFn        func(ctx context.Context) error
	libraries     LibraryLister
	eventBus      *event.Bus
	logger        *slog.Logger
	debounce      time.Duration
	refreshPeriod time.Duration
	probeCache    *ProbeCache

	mu        sync.Mutex
	watcher   *fsnotify.Watcher
	watching  map[string]bool
	knownDirs map[string]map[string]struct{} // root -> set of known subdirectory names

	// Polling state.
	pollSnapshots map[string]map[string]struct{} // path -> set of dir entry names
	lastPollTime  map[string]time.Time           // path -> last poll time
	pollIntervals map[string]int                 // path -> poll interval in seconds
}

// NewService creates a new filesystem watcher service.
func NewService(scanFn func(ctx context.Context) error, libraries LibraryLister, eventBus *event.Bus, logger *slog.Logger, probeCache *ProbeCache) *Service {
	return &Service{
		scanFn:        scanFn,
		libraries:     libraries,
		eventBus:      eventBus,
		logger:        logger.With("component", "fs-watcher"),
		debounce:      1 * time.Second,
		refreshPeriod: 5 * time.Minute,
		probeCache:    probeCache,
		watching:      make(map[string]bool),
		knownDirs:     make(map[string]map[string]struct{}),
		pollSnapshots: make(map[string]map[string]struct{}),
		lastPollTime:  make(map[string]time.Time),
		pollIntervals: make(map[string]int),
	}
}

// SetDebounce overrides the default debounce interval (for testing).
func (s *Service) SetDebounce(d time.Duration) {
	s.debounce = d
}

// Start blocks until ctx is canceled. It creates an fsnotify watcher,
// watches library root directories, and dispatches events. If fsnotify
// is unavailable, the service still runs with poll-only support.
func (s *Service) Start(ctx context.Context) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		s.logger.Warn("fsnotify unavailable, running poll-only", "error", err)
	} else {
		defer w.Close() //nolint:errcheck
		s.mu.Lock()
		s.watcher = w
		s.mu.Unlock()
		s.refreshWatchPaths(ctx)
	}

	s.initPollSnapshots(ctx)
	s.logger.Info("filesystem watcher starting")

	refreshTicker := time.NewTicker(s.refreshPeriod)
	defer refreshTicker.Stop()

	// Poll ticker: base tick of 1 minute. Per-library intervals are checked
	// inside pollDirectories.
	pollTicker := time.NewTicker(1 * time.Minute)
	defer pollTicker.Stop()

	// Debounce timer for coalescing create events into a single scan.
	// Starts stopped; reset on each create event.
	debounceTimer := time.NewTimer(0)
	if !debounceTimer.Stop() {
		<-debounceTimer.C
	}
	scanPending := false

	// When fsnotify is unavailable, use nil channels (never receive).
	var eventCh <-chan fsnotify.Event
	var errCh <-chan error
	if w != nil {
		eventCh = w.Events
		errCh = w.Errors
	}

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("filesystem watcher stopping")
			return

		case ev, ok := <-eventCh:
			if !ok {
				return
			}
			s.handleFSEvent(ev, debounceTimer, &scanPending)

		case err, ok := <-errCh:
			if !ok {
				return
			}
			s.logger.Error("fsnotify error", "error", err)

		case <-debounceTimer.C:
			if scanPending {
				scanPending = false
				s.logger.Info("debounce elapsed, triggering scan")
				if err := s.scanFn(ctx); err != nil {
					s.logger.Error("scan triggered by fs watcher failed", "error", err)
				}
			}

		case <-pollTicker.C:
			changed := s.pollDirectories()
			if changed && !scanPending {
				// Reuse the debounce timer for scan coalescing.
				if !debounceTimer.Stop() {
					select {
					case <-debounceTimer.C:
					default:
					}
				}
				debounceTimer.Reset(s.debounce)
				scanPending = true
			}

		case <-refreshTicker.C:
			if w != nil {
				s.refreshWatchPaths(ctx)
			}
			s.refreshPollPaths(ctx)
		}
	}
}

func (s *Service) handleFSEvent(ev fsnotify.Event, debounceTimer *time.Timer, scanPending *bool) {
	// Only handle create, remove, and rename operations.
	if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Remove) && !ev.Has(fsnotify.Rename) {
		return
	}

	// Only react to direct children of watched library roots.
	parent := filepath.Dir(ev.Name)
	s.mu.Lock()
	watched := s.watching[parent]
	s.mu.Unlock()
	if !watched {
		return
	}

	dirName := filepath.Base(ev.Name)

	if ev.Has(fsnotify.Create) {
		// Verify the created entry is a directory.
		info, err := os.Stat(ev.Name)
		if err != nil || !info.IsDir() {
			return
		}

		// Track the new directory so Remove events can be verified.
		s.mu.Lock()
		if s.knownDirs[parent] == nil {
			s.knownDirs[parent] = make(map[string]struct{})
		}
		s.knownDirs[parent][dirName] = struct{}{}
		s.mu.Unlock()

		s.logger.Info("directory created in library",
			"path", ev.Name,
			"name", dirName,
			"library_root", parent,
		)

		s.eventBus.Publish(event.Event{
			Type: event.FSDirCreated,
			Data: map[string]any{
				"path":         ev.Name,
				"name":         dirName,
				"library_root": parent,
			},
		})

		// Reset debounce timer to coalesce rapid creates.
		if !debounceTimer.Stop() {
			select {
			case <-debounceTimer.C:
			default:
			}
		}
		debounceTimer.Reset(s.debounce)
		*scanPending = true
		return
	}

	// Remove or Rename: only emit if the entry was a known directory.
	s.mu.Lock()
	_, wasDir := s.knownDirs[parent][dirName]
	if wasDir {
		delete(s.knownDirs[parent], dirName)
	}
	s.mu.Unlock()

	if !wasDir {
		return
	}

	s.logger.Warn("directory removed from library",
		"path", ev.Name,
		"name", dirName,
		"library_root", parent,
	)

	s.eventBus.Publish(event.Event{
		Type: event.FSDirRemoved,
		Data: map[string]any{
			"path":         ev.Name,
			"name":         dirName,
			"library_root": parent,
		},
	})
}

// refreshWatchPaths synchronizes the set of watched directories with the
// current list of libraries that have the watch bit enabled and where
// fsnotify is supported.
func (s *Service) refreshWatchPaths(ctx context.Context) {
	libs, err := s.libraries.List(ctx)
	if err != nil {
		s.logger.Error("failed to list libraries for watch refresh", "error", err)
		return
	}

	wanted := make(map[string]bool)
	for _, lib := range libs {
		if !lib.FSWatchEnabled() || lib.IsPathless() {
			continue
		}
		// Check probe cache: only watch if fsnotify is supported.
		if s.probeCache != nil {
			if supported, ok := s.probeCache.Get(lib.Path); ok && !supported {
				continue
			}
		}
		// Verify the path exists and is a directory.
		info, err := os.Stat(lib.Path)
		if err != nil || !info.IsDir() {
			s.logger.Warn("library path not watchable",
				"library", lib.Name,
				"path", lib.Path,
				"error", err,
			)
			continue
		}
		wanted[lib.Path] = true
	}

	// Determine which paths to add and remove under the lock, but perform
	// the blocking fsnotify and filesystem I/O outside the lock to avoid
	// holding the mutex during potentially slow operations.
	s.mu.Lock()
	var toRemove []string
	for path := range s.watching {
		if !wanted[path] {
			toRemove = append(toRemove, path)
		}
	}
	var toAdd []string
	for path := range wanted {
		if !s.watching[path] {
			toAdd = append(toAdd, path)
		}
	}
	s.mu.Unlock()

	// Remove watches (fsnotify I/O outside the lock).
	// Track which removes succeeded so we only update internal state for those.
	var removed []string
	for _, path := range toRemove {
		if err := s.watcher.Remove(path); err != nil {
			s.logger.Warn("failed to remove watch", "path", path, "error", err)
			continue
		}
		s.logger.Info("stopped watching library path", "path", path)
		removed = append(removed, path)
	}

	// Add watches and snapshot directories (fsnotify + filesystem I/O outside the lock).
	type addResult struct {
		path string
		snap map[string]struct{}
	}
	var added []addResult
	for _, path := range toAdd {
		if err := s.watcher.Add(path); err != nil {
			s.logger.Error("failed to watch library path", "path", path, "error", err)
			continue
		}
		snap := readDirSnapshot(path)
		added = append(added, addResult{path: path, snap: snap})
		s.logger.Info("watching library path", "path", path)
	}

	// Update internal state under the lock -- only for paths that actually changed.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, path := range removed {
		delete(s.watching, path)
		delete(s.knownDirs, path)
	}
	for _, ar := range added {
		s.watching[ar.path] = true
		s.knownDirs[ar.path] = ar.snap
	}
}

// initPollSnapshots takes an initial snapshot of all poll-enabled library
// directories so the first poll tick only reports actual changes.
func (s *Service) initPollSnapshots(ctx context.Context) {
	libs, err := s.libraries.List(ctx)
	if err != nil {
		s.logger.Error("failed to list libraries for poll init", "error", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, lib := range libs {
		if !lib.FSPollEnabled() || lib.IsPathless() {
			continue
		}
		snap := readDirSnapshot(lib.Path)
		if snap != nil {
			s.pollSnapshots[lib.Path] = snap
			s.lastPollTime[lib.Path] = time.Now()
			interval := lib.FSPollInterval
			if interval <= 0 {
				interval = 60
			}
			s.pollIntervals[lib.Path] = interval
			s.logger.Info("initialized poll snapshot",
				"library", lib.Name,
				"path", lib.Path,
				"entries", len(snap),
				"interval_s", interval,
			)
		}
	}
}

// refreshPollPaths updates the poll path set from the current library list.
func (s *Service) refreshPollPaths(ctx context.Context) {
	libs, err := s.libraries.List(ctx)
	if err != nil {
		s.logger.Error("failed to list libraries for poll refresh", "error", err)
		return
	}

	wanted := make(map[string]int) // path -> interval
	for _, lib := range libs {
		if !lib.FSPollEnabled() || lib.IsPathless() {
			continue
		}
		interval := lib.FSPollInterval
		if interval <= 0 {
			interval = 60
		}
		wanted[lib.Path] = interval
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove paths no longer poll-enabled.
	for path := range s.pollSnapshots {
		if _, ok := wanted[path]; !ok {
			delete(s.pollSnapshots, path)
			delete(s.lastPollTime, path)
			delete(s.pollIntervals, path)
		}
	}

	// Add new paths.
	for path, interval := range wanted {
		if _, exists := s.pollSnapshots[path]; !exists {
			snap := readDirSnapshot(path)
			if snap != nil {
				s.pollSnapshots[path] = snap
				s.lastPollTime[path] = time.Now()
				s.pollIntervals[path] = interval
			}
		} else {
			// Update interval if changed.
			s.pollIntervals[path] = interval
		}
	}
}

// pollDirectories checks all poll-enabled libraries for directory changes.
// Returns true if any changes were detected.
func (s *Service) pollDirectories() bool {
	now := time.Now()

	// Collect all state under a single lock to avoid read-check-act races.
	type pollEntry struct {
		path    string
		oldSnap map[string]struct{}
	}

	s.mu.Lock()
	entries := make([]pollEntry, 0, len(s.pollSnapshots))
	for path, snap := range s.pollSnapshots {
		interval := s.pollIntervals[path]
		if interval <= 0 {
			interval = 60
		}
		lastPoll := s.lastPollTime[path]
		if now.Sub(lastPoll) < time.Duration(interval)*time.Second {
			continue
		}
		// Copy the snapshot so we can safely read it outside the lock.
		snapCopy := make(map[string]struct{}, len(snap))
		for k, v := range snap {
			snapCopy[k] = v
		}
		entries = append(entries, pollEntry{path: path, oldSnap: snapCopy})
	}
	s.mu.Unlock()

	changed := false

	for _, entry := range entries {
		// Filesystem I/O outside the lock.
		newSnap := readDirSnapshot(entry.path)
		if newSnap == nil {
			continue
		}

		// Detect new directories.
		for name := range newSnap {
			if _, existed := entry.oldSnap[name]; !existed {
				s.logger.Info("poll: directory created in library",
					"path", filepath.Join(entry.path, name),
					"name", name,
					"library_root", entry.path,
				)
				s.eventBus.Publish(event.Event{
					Type: event.FSDirCreated,
					Data: map[string]any{
						"path":         filepath.Join(entry.path, name),
						"name":         name,
						"library_root": entry.path,
					},
				})
				changed = true
			}
		}

		// Detect removed directories.
		for name := range entry.oldSnap {
			if _, exists := newSnap[name]; !exists {
				s.logger.Warn("poll: directory removed from library",
					"path", filepath.Join(entry.path, name),
					"name", name,
					"library_root", entry.path,
				)
				s.eventBus.Publish(event.Event{
					Type: event.FSDirRemoved,
					Data: map[string]any{
						"path":         filepath.Join(entry.path, name),
						"name":         name,
						"library_root": entry.path,
					},
				})
				changed = true
			}
		}

		// Update state under the lock.
		s.mu.Lock()
		// Only update if the path is still tracked (may have been removed
		// by refreshPollPaths while we were doing I/O).
		if _, stillTracked := s.pollSnapshots[entry.path]; stillTracked {
			s.pollSnapshots[entry.path] = newSnap
			s.lastPollTime[entry.path] = now
		}
		s.mu.Unlock()
	}

	return changed
}

// readDirSnapshot reads directory entries and returns a set of subdirectory names.
func readDirSnapshot(path string) map[string]struct{} {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	snap := make(map[string]struct{})
	for _, e := range entries {
		if e.IsDir() {
			snap[e.Name()] = struct{}{}
		}
	}
	return snap
}
