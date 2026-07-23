package scanner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/rule"
)

// LibraryLister retrieves the list of configured libraries.
type LibraryLister interface {
	List(ctx context.Context) ([]library.Library, error)
}

// ErrScanInProgress is returned by Run when a scan is already running.
var ErrScanInProgress = fmt.Errorf("scan already in progress")

// Image filename patterns to detect for each type.
var (
	thumbPatterns  = []string{"folder.jpg", "folder.png", "artist.jpg", "artist.png", "poster.jpg", "poster.png"}
	fanartPatterns = []string{"fanart.jpg", "fanart.png", "backdrop.jpg", "backdrop.png"}
	logoPatterns   = []string{"logo.png", "logo-white.png"}
	bannerPatterns = []string{"banner.jpg", "banner.png"}
)

// Service runs filesystem scans against the music library.
type Service struct {
	artistService *artist.Service
	ruleEngine    *rule.Engine
	ruleService   *rule.Service
	logger        *slog.Logger
	libraryPath   string
	// exclusions holds the lowercased artist-directory names the scanner
	// skips. Stored as an atomic.Pointer so a live update via SetExclusions
	// (from the settings handler) is visible to in-flight background scans
	// without a lock, mirroring the mtimeFastPath atomic rationale below.
	// The pointed-to map is never mutated in place after Store; SetExclusions
	// always swaps in a freshly built map.
	exclusions atomic.Pointer[map[string]bool]
	// exclusionsDisplay holds the same exclusion tokens in their ORIGINAL case
	// and input order, swapped in lockstep with exclusions. Matching uses the
	// lowercased map above; this slice exists purely so the settings UI round-
	// trips the operator's typed casing (e.g. "Various Artists") instead of the
	// lowercased lookup key ("various artists"). Stored as an atomic.Pointer for
	// the same lock-free live-update reason as exclusions.
	exclusionsDisplay atomic.Pointer[[]string]
	eventBus          *event.Bus
	defaultLibraryID  string
	libraryLister     LibraryLister

	// postScanHook, when set, runs once every scan finishes. Wired at
	// construction only (never mid-scan), like eventBus/libraryLister, so it
	// needs no synchronization. See SetPostScanHook.
	postScanHook PostScanHook

	// mtimeFastPath toggles the per-directory mtime check that skips the
	// inner ReadDir + image probe loop when an artist directory has not
	// been touched since the previous scan. Set via SetMtimeFastPath
	// (config-driven) so production gets the optimization by default and
	// operators on filesystems with unreliable mtimes can disable it.
	// Stored as atomic.Bool because background scans read this flag from
	// detectFilesWithFastPath while SetMtimeFastPath may be called from
	// the config-reload path concurrently.
	mtimeFastPath atomic.Bool

	mu          sync.Mutex
	currentScan *ScanResult

	// shutdownCtx is canceled when Shutdown is called, signaling background
	// scan goroutines to stop. This replaces context.WithoutCancel which
	// created goroutines that survived application shutdown.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	scanWg         sync.WaitGroup
}

// SetEventBus sets the event bus for publishing scan events.
func (s *Service) SetEventBus(bus *event.Bus) {
	s.eventBus = bus
}

// SetDefaultLibraryID sets the library ID assigned to newly discovered artists.
func (s *Service) SetDefaultLibraryID(id string) {
	s.defaultLibraryID = id
}

// SetLibraryLister sets the library lister used to discover all library paths.
func (s *Service) SetLibraryLister(ll LibraryLister) {
	s.libraryLister = ll
}

// PostScanHook runs once a scan's work is done, BEFORE the scan is stamped
// completed and ScanCompleted is published - so anything observing "scan
// completed" (UI, SSE, a test) can rely on the hook having already run. ctx is
// the scanner's shutdown-scoped context, so a hook doing I/O is canceled with the
// application.
type PostScanHook func(ctx context.Context)

// SetPostScanHook registers the post-scan hook. It exists because the SCAN is
// where several derived states first become computable: path-mapping inference
// (#2380) joins Stillwater artists to a peer's artists by MusicBrainz ID, and in
// the normal first-run order the operator adds the connection BEFORE the first
// scan - at which point the library has zero MBIDs and inference has nothing to
// infer from. Without a re-run at end-of-scan the connection stays permanently
// unmapped on a split mount (and the fail-closed root guard then refuses every
// push, forever), because nothing else ever revisits it.
//
// Wire it at construction, before the first Run; it is not safe to swap while a
// scan is in flight. Nil clears it. The hook is best-effort and must not panic
// on its own errors: a scan is never failed by it.
func (s *Service) SetPostScanHook(h PostScanHook) {
	s.postScanHook = h
}

// NewService creates a scanner service. The mtime fast-path is enabled by
// default so production deployments get the optimization without an
// explicit setter call; callers that need to disable it (FUSE mounts,
// restored backups with broken mtimes) toggle SetMtimeFastPath(false).
func NewService(artistService *artist.Service, ruleEngine *rule.Engine, ruleService *rule.Service, logger *slog.Logger, libraryPath string, exclusions []string) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		artistService:  artistService,
		ruleEngine:     ruleEngine,
		ruleService:    ruleService,
		logger:         logger,
		libraryPath:    libraryPath,
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
	}
	excMap, display := buildExclusions(exclusions)
	s.exclusions.Store(&excMap)
	s.exclusionsDisplay.Store(&display)
	s.mtimeFastPath.Store(true) // matches ScannerConfig.MtimeFastPath default
	return s
}

// buildExclusions normalises a slice of artist-directory names into the two
// representations the scanner keeps in lockstep: the lowercased lookup map used
// for case-insensitive matching, and a display slice that preserves the
// operator's ORIGINAL casing and input order for the settings UI. Empty/
// whitespace-only tokens are dropped (a trailing comma or blank CSV field must
// not create an exclusion for the empty directory name), and tokens that
// collapse to the same lowercased key are de-duplicated -- the first-seen
// casing wins -- so the UI never shows a redundant pair like "VA, va".
func buildExclusions(exclusions []string) (map[string]bool, []string) {
	excMap := make(map[string]bool, len(exclusions))
	display := make([]string, 0, len(exclusions))
	for _, e := range exclusions {
		name := strings.TrimSpace(e)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if excMap[lower] {
			continue // duplicate (case-insensitively); keep the first casing.
		}
		excMap[lower] = true
		display = append(display, name)
	}
	return excMap, display
}

// SetExclusions replaces the scanner's exclusion set at runtime. The next scan
// (and any directory not yet processed by an in-flight scan) honors the new
// set; tokens are trimmed and empties dropped to match the constructor and the
// SW_SCANNER_EXCLUSIONS CSV semantics. Wired from the settings handler so an
// operator can edit the skip list without a restart.
func (s *Service) SetExclusions(exclusions []string) {
	excMap, display := buildExclusions(exclusions)
	s.exclusions.Store(&excMap)
	s.exclusionsDisplay.Store(&display)
}

// Exclusions returns the current exclusion set in the operator's original
// casing and input order. Used by the settings UI to display the value
// actually in effect (which already reflects the SW_SCANNER_EXCLUSIONS env /
// YAML value applied at startup) without lowercasing the operator's typed
// casing on save+reload. Matching remains case-insensitive via the lowercased
// lookup map; this accessor is display-only. The returned slice is a copy, so
// callers cannot mutate the stored set.
func (s *Service) Exclusions() []string {
	d := s.exclusionsDisplay.Load()
	if d == nil {
		return nil
	}
	out := make([]string, len(*d))
	copy(out, *d)
	return out
}

// MtimeFastPath reports whether the per-directory mtime fast path is currently
// enabled. Used by the settings UI to render the toggle's initial state.
func (s *Service) MtimeFastPath() bool {
	return s.mtimeFastPath.Load()
}

// SetMtimeFastPath toggles the per-directory mtime-based fast path. Pass
// false from startup wiring on filesystems that do not maintain stable
// directory mtimes; otherwise leave it on the default-true value.
func (s *Service) SetMtimeFastPath(enabled bool) {
	s.mtimeFastPath.Store(enabled)
}

// Shutdown cancels any in-progress background scans and waits for them to
// complete. Call this during application shutdown.
func (s *Service) Shutdown() {
	s.shutdownCancel()
	s.scanWg.Wait()
}

// Run starts a filesystem scan. Only one scan runs at a time.
// Returns a snapshot of the initial scan result (safe to read without synchronization).
func (s *Service) Run(ctx context.Context) (*ScanResult, error) {
	s.mu.Lock()
	if s.currentScan != nil && s.currentScan.Status == "running" {
		s.mu.Unlock()
		return nil, ErrScanInProgress
	}

	result := &ScanResult{
		ID:        uuid.New().String(),
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	s.currentScan = result
	snapshot := *result
	s.mu.Unlock()

	// Use the shutdown context so the scan outlives the HTTP request but
	// is still canceled on application shutdown.
	s.scanWg.Add(1)
	go s.runScan(s.shutdownCtx, result) //nolint:contextcheck // intentional -- scan goroutine must outlive request; scoped to shutdownCtx for app-level cancellation

	return &snapshot, nil
}

// Status returns a snapshot of the current or most recent scan result.
// The returned value is a copy and safe to read without synchronization.
func (s *Service) Status() *ScanResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentScan == nil {
		return nil
	}
	snapshot := *s.currentScan
	return &snapshot
}

// markScanFailed stamps result as terminally failed. Status, Error, and
// CompletedAt are all written inside one critical section so a concurrent
// reader (Status(), the completion event, a test) can never observe a
// terminal Status with a still-nil CompletedAt -- the exact ordering gap
// that made TestScan_RecoversFromPanic flake under -race (#2599): the old
// code set Status="failed" and released the lock, then relied on the
// unrelated completion defer -- which runs the (unlocked) post-scan hook
// first -- to fill in CompletedAt later, leaving a real window where the
// two fields disagreed.
func (s *Service) markScanFailed(result *ScanResult, reason string) {
	s.mu.Lock()
	now := time.Now().UTC()
	result.Status = "failed"
	result.Error = reason
	result.CompletedAt = &now
	s.mu.Unlock()
}

//nolint:gocognit // Scan worker: lock-protected status transitions, per-library file walk, cancellation checkpoints, error aggregation, progress publication, and completion sync; the lifecycle ordering (acquire -> walk -> publish -> release) and cancellation-aware control flow do not factor cleanly into helpers without sharing the result mutex across them.
func (s *Service) runScan(ctx context.Context, result *ScanResult) {
	defer s.scanWg.Done()
	defer func() {
		// Post-scan hook runs BEFORE the scan is marked finished, deliberately.
		// Two reasons, both learned the hard way:
		//   1. "Scan completed" must mean the post-scan work (path-mapping
		//      inference, #2380) is done too. Otherwise an observer that waits for
		//      the completed status - the UI, the SSE client, a test - races the
		//      hook and can read a connection that is not yet mapped.
		//   2. Shutdown cancels shutdownCtx and THEN waits on scanWg. A hook that
		//      ran after the status flip could still be mid-flight at that moment
		//      and have its context canceled out from under its HTTP calls.
		// The hook is best-effort: it never fails the scan, and its own timeouts
		// bound how long completion can be delayed. It runs in this defer, which
		// executes AFTER the recover() defer below (deferred functions run LIFO,
		// and that recover defer is registered later in this function), so a
		// panic here would NOT be caught by that recover -- it needs its own.
		if hook := s.postScanHook; hook != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						s.logger.Error("post-scan hook panicked",
							"panic", r,
							"stack", string(debug.Stack()),
						)
					}
				}()
				hook(ctx)
			}()
		}

		s.mu.Lock()
		// Only stamp CompletedAt here if an earlier terminal write (a
		// markScanFailed call, or the recover defer below) has not already
		// set it. Each terminal writer sets Status and CompletedAt together
		// under this same mutex, so "CompletedAt == nil" is equivalent to
		// "still running" and never observable as a lone stale field.
		if result.CompletedAt == nil {
			now := time.Now().UTC()
			result.CompletedAt = &now
		}
		if result.Status == "running" {
			result.Status = "completed"
		}
		s.mu.Unlock()

		if s.eventBus != nil {
			s.eventBus.Publish(event.Event{
				Type: event.ScanCompleted,
				Data: map[string]any{
					"scan_id":           result.ID,
					"status":            result.Status,
					"total_directories": result.TotalDirectories,
					"new_artists":       result.NewArtists,
				},
			})
		}
	}()
	// Deferred functions run LIFO, so this recover defer -- registered
	// last -- runs FIRST during a panic unwind, before the completion-status
	// defer above. Marking the result "failed" here (while status is still
	// "running") means the completion defer's `if result.Status == "running"`
	// check no longer matches, so it leaves "failed" alone instead of
	// overwriting it with "completed". scanWg.Done() (registered first, runs
	// last) still fires either way, so callers waiting on the WaitGroup are
	// never blocked by a panicked scan.
	defer func() {
		if r := recover(); r != nil {
			s.markScanFailed(result, fmt.Sprintf("scan panicked: %v", r))
			s.logger.Error("scan goroutine panicked",
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()

	// Build the list of libraries to scan.
	type scanTarget struct {
		path      string
		libraryID string
	}
	var targets []scanTarget

	if s.libraryLister != nil {
		libs, err := s.libraryLister.List(ctx)
		if err != nil {
			s.logger.Error("listing libraries for scan", "error", err)
		}
		for i := range libs {
			lib := &libs[i]
			if lib.Path == "" {
				s.logger.Info("skipping pathless library (no path configured)", "library_id", lib.ID, "name", lib.Name)
				continue
			}
			targets = append(targets, scanTarget{path: lib.Path, libraryID: lib.ID})
		}
	}
	// Fallback: if no library lister is configured, use the legacy single path.
	// When a lister IS set but returns empty, the user has no libraries -- do not fall back.
	if len(targets) == 0 && s.libraryLister == nil && s.libraryPath != "" {
		targets = append(targets, scanTarget{path: s.libraryPath, libraryID: s.defaultLibraryID})
	}

	if len(targets) == 0 {
		s.markScanFailed(result, "no scannable libraries (all libraries are API-only or no paths configured)")
		s.logger.Error("scan failed: no scannable libraries (all libraries are API-only or no paths configured)")
		return
	}

	// Collect discovered paths per library for removal detection. Keyed
	// by libraryID so detectRemoved can query only the artists belonging
	// to each scanned library (#1409) instead of paginating the entire
	// catalog.
	discoveredByLibrary := make(map[string]map[string]bool, len(targets))

	for _, target := range targets {
		s.logger.Info("scanning library", "path", target.path, "library_id", target.libraryID)

		entries, err := os.ReadDir(target.path)
		if err != nil {
			s.logger.Error("reading library directory", "error", err, "path", target.path)
			continue
		}

		// Pre-load every existing artist for this library so
		// processDirectory can look up the prior DB state from an in-
		// memory map instead of issuing one GetByPath round-trip per
		// directory entry (#1411).
		//
		// Hydration choice:
		//   Images:      required -- Thumb*/Fanart*/Logo*/Banner* fields
		//                live on artist_images and the scanner reads
		//                them in detectFilesWithFastPath and
		//                processExistingArtist.
		//   ProviderIDs: required -- preloaded artists flow into
		//                Update() (via processExistingArtist), which
		//                calls persistNormalized that re-inserts
		//                artist_provider_ids from the in-memory struct.
		//                Without hydration the struct's MBID / AudioDB /
		//                Discogs / etc. fields are zero, so Update would
		//                silently wipe the existing rows. Documented in
		//                TestRenameDirectory_PreservesProviderIDs.
		//   PrimaryLibrary: skipped -- persistNormalized does not write
		//                back artist_libraries on Update, so leaving
		//                LibraryID unhydrated is safe.
		// Cost: 3 queries per scanned library (artists + artist_images
		// + artist_provider_ids) instead of N round-trips per directory.
		var libraryArtists map[string]*artist.Artist
		// preloadedKeys maps normalized identity key -> existing artist name for
		// all artists already in this library.  processNewArtist uses this to
		// detect when a newly-created artist shares a key with an existing one
		// and logs a Warn + increments ScanResult.SuspectedDuplicates.
		// Built at preload time so the check is O(1) per new artist.
		var preloadedKeys map[string]string
		if target.libraryID != "" {
			pre, preErr := s.artistService.PreloadArtistsByLibrary(ctx, target.libraryID, artist.HydrateOpts{Images: true, ProviderIDs: true})
			if preErr != nil {
				// Falling back to the legacy GetByPath path costs one
				// round-trip per directory but is still correct, so we
				// log and continue rather than failing the scan.
				s.logger.Warn("preloading artists for library",
					"library_id", target.libraryID, "error", preErr)
			} else {
				libraryArtists = pre
				// Build the key map from the preloaded path-keyed map.
				// Each preloaded artist has a non-empty path (path-empty
				// rows are excluded by PreloadArtistsByLibrary).
				preloadedKeys = make(map[string]string, len(pre))
				for _, a := range pre {
					if k := artist.NormalizeIdentityKey(a.Name); k != "" {
						// Multiple existing artists can have the same key
						// (the very condition we are detecting). We store
						// at most one name per key here; the purpose is
						// only to know "some existing artist has this key"
						// so the exact name stored is informational.
						if _, exists := preloadedKeys[k]; !exists {
							preloadedKeys[k] = a.Name
						}
					}
				}
			}
		}

		discoveredPaths := make(map[string]bool, len(entries))
		for _, entry := range entries {
			if ctx.Err() != nil {
				s.markScanFailed(result, "scan canceled")
				return
			}

			if !entry.IsDir() {
				continue
			}
			// Skip hidden directories
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			// Skip OS/NAS junk directories ($RECYCLE.BIN, System Volume
			// Information, @eaDir, lost+found, ...) and compilation
			// placeholder buckets (Various Artists / Various / VA). These are
			// never real artists, so drop them before they become an artist
			// row or a scanned directory. The dot-prefixed junk (.Trash,
			// .DS_Store) is already caught by the hidden-dir skip above; this
			// covers the non-dot-prefixed names the built-in sets add. These
			// are distinct from the operator-editable exclusion list, which
			// still CREATES the artist and flags it IsExcluded (#30, #41).
			if artist.IsIgnoredSystemName(entry.Name()) || artist.IsNonArtistDirName(entry.Name()) {
				continue
			}

			s.mu.Lock()
			result.TotalDirectories++
			s.mu.Unlock()
			dirPath := filepath.Join(target.path, entry.Name())
			discoveredPaths[dirPath] = true

			if err := s.processDirectory(ctx, dirPath, entry.Name(), target.libraryID, libraryArtists, preloadedKeys, result); err != nil {
				s.logger.Warn("error processing directory",
					"path", dirPath, "error", err)
			}
		}
		discoveredByLibrary[target.libraryID] = discoveredPaths
	}

	// Detect removed artists. Per-library sweep so we issue one
	// ListRefsByLibrary query per scanned library instead of paginating
	// every artist in the catalog (#1409).
	s.detectRemoved(ctx, discoveredByLibrary, result)

	// Record health snapshot at end of scan
	s.recordHealthSnapshot(ctx)
}

func (s *Service) processDirectory(ctx context.Context, dirPath, name, libraryID string, preloaded map[string]*artist.Artist, preloadedKeys map[string]string, result *ScanResult) error {
	excluded := false
	if m := s.exclusions.Load(); m != nil {
		excluded = (*m)[strings.ToLower(name)]
	}

	// Look up existing artist before detectFiles so we can skip expensive
	// placeholder regeneration when one already exists. Prefer the
	// pre-loaded path-keyed map populated once per library above; fall
	// back to a per-directory GetByPath only on miss (new artist, or the
	// pre-load failed and we are running degraded). This bounds the
	// "do we already know this artist?" cost to one query per library
	// instead of one per directory entry (#1411).
	var existing *artist.Artist
	if preloaded != nil {
		if hit, ok := preloaded[dirPath]; ok {
			existing = hit
		} else {
			// Cache miss: the preload map can legitimately miss an
			// existing artist (path-empty rows are excluded; a row
			// added between preload and this iteration). Fall back to
			// GetByPath so the lookup remains authoritative and we
			// don't accidentally treat an existing artist as new.
			got, err := s.artistService.GetByPath(ctx, dirPath)
			if err != nil {
				return fmt.Errorf("looking up artist by path: %w", err)
			}
			existing = got
		}
	} else {
		got, err := s.artistService.GetByPath(ctx, dirPath)
		if err != nil {
			return fmt.Errorf("looking up artist by path: %w", err)
		}
		existing = got
	}

	detected, detectErr := s.detectFilesWithFastPath(dirPath, existing)
	if detectErr != nil {
		s.logger.Warn("reading artist directory", "path", dirPath, "error", detectErr)
		return nil // skip this directory entirely -- preserve existing DB state
	}

	if existing == nil {
		return s.processNewArtist(ctx, dirPath, name, libraryID, excluded, detected, preloadedKeys, result)
	}
	return s.processExistingArtist(ctx, dirPath, libraryID, existing, excluded, detected, result)
}

// processNewArtist creates a new artist record for a directory not yet in the
// database. It applies file detection results, parses NFO metadata if present,
// and publishes an ArtistUpdated event.
//
// preloadedKeys maps normalized identity keys -> existing artist names for the
// library.  When non-nil, processNewArtist checks whether the new artist's key
// collides with an existing one and logs a Warn + increments
// ScanResult.SuspectedDuplicates so operators know to review
// /settings/artist-duplicates.  A nil map skips the check (degraded mode).
func (s *Service) processNewArtist(ctx context.Context, dirPath, name, libraryID string, excluded bool, detected detectedFiles, preloadedKeys map[string]string, result *ScanResult) error {
	a := &artist.Artist{
		Name:              name,
		SortName:          name,
		Path:              dirPath,
		LibraryID:         libraryID,
		NFOExists:         detected.NFOExists,
		ThumbExists:       detected.ThumbExists,
		FanartExists:      detected.FanartExists,
		FanartCount:       detected.FanartCount,
		LogoExists:        detected.LogoExists,
		BannerExists:      detected.BannerExists,
		ThumbLowRes:       detected.ThumbLowRes,
		FanartLowRes:      detected.FanartLowRes,
		LogoLowRes:        detected.LogoLowRes,
		BannerLowRes:      detected.BannerLowRes,
		ThumbPlaceholder:  detected.ThumbPlaceholder,
		FanartPlaceholder: detected.FanartPlaceholder,
		LogoPlaceholder:   detected.LogoPlaceholder,
		BannerPlaceholder: detected.BannerPlaceholder,
		ThumbWidth:        detected.ThumbWidth,
		ThumbHeight:       detected.ThumbHeight,
		FanartWidth:       detected.FanartWidth,
		FanartHeight:      detected.FanartHeight,
		LogoWidth:         detected.LogoWidth,
		LogoHeight:        detected.LogoHeight,
		BannerWidth:       detected.BannerWidth,
		BannerHeight:      detected.BannerHeight,
	}
	if excluded {
		a.IsExcluded = true
		a.ExclusionReason = "default exclusion list"
	}

	// Parse NFO if it exists for metadata
	if detected.NFOExists {
		if s.populateFromNFO(dirPath, a) {
			// Import lockdata from NFO only on the new-artist path: this is
			// the first time Stillwater sees the artist, so the NFO's
			// <lockdata>true</lockdata> is the only available signal. Tagged
			// with the dedicated "initial_import" lock_source so the user can
			// distinguish first-discovery imports from explicit user locks or
			// platform-pulled locks (issue #1726).
			a.Locked = true
			a.LockSource = "initial_import"
			now := time.Now().UTC()
			a.LockedAt = &now
		}
	}

	now := time.Now().UTC()
	a.LastScannedAt = &now

	if err := s.artistService.Create(ctx, a); err != nil {
		return fmt.Errorf("creating artist: %w", err)
	}

	s.publishArtistUpdated(a.ID)

	s.mu.Lock()
	result.NewArtists++
	s.mu.Unlock()
	s.logger.Debug("new artist discovered", "name", name, "path", dirPath)

	// Near-duplicate check: if the new artist's normalized identity key
	// matches a key already in the preloaded map, a near-duplicate pair
	// exists in this library.  We cannot prevent the row from being created
	// (each directory is a distinct path), but we can flag it for operator
	// review.  The check runs after Create so the row is safely persisted
	// even when a duplicate is detected.
	if preloadedKeys != nil {
		// Use the persisted a.Name rather than the raw directory-name variable:
		// populateFromNFO may have updated a.Name from the NFO <title> element,
		// so normalizing name here could miss a collision against preloadedKeys
		// that was built from previously-persisted artist names.
		if k := artist.NormalizeIdentityKey(a.Name); k != "" {
			// processNewArtist runs concurrently across directories, and we
			// now insert into preloadedKeys as well as read it, so the whole
			// check-then-insert must hold s.mu. Inserting the new artist's
			// key lets a later directory in the same scan collide against it;
			// without this, two directories that are both new in one run
			// would only be flagged if a previously-persisted artist matched.
			s.mu.Lock()
			existingName, isDuplicate := preloadedKeys[k]
			if isDuplicate {
				result.SuspectedDuplicates++
			} else {
				preloadedKeys[k] = a.Name
			}
			s.mu.Unlock()

			if isDuplicate {
				s.logger.Warn("suspected duplicate artist detected during scan",
					"new_name", a.Name,
					"new_path", dirPath,
					"existing_name", existingName,
					"key", k,
				)
			}
		}
	}

	return nil
}

// processExistingArtist reconciles an artist already in the database against
// the current filesystem state. It preserves placeholders and dimensions on
// transient probe failures, applies NFO re-import for unlocked artists, and
// falls back to a lightweight ReconcileImages call when nothing has changed.
func (s *Service) processExistingArtist(ctx context.Context, dirPath, libraryID string, existing *artist.Artist, excluded bool, detected detectedFiles, result *ScanResult) error {
	// Heal the artist_libraries membership on EVERY visit, before any
	// early-return branch below. An artist first created by a connection
	// import (Emby/Jellyfin) and matched here by path would otherwise never
	// acquire the filesystem-library membership for the library being
	// scanned, so it would be absent from local-library filtering and show
	// no folder badge (issue #1780). EnsureLibraryMembership is idempotent
	// and derives the source from the library's connection (filesystem when
	// none), so re-running a scan does not duplicate or move the row. Placed
	// ahead of the quiet-rescan early return so steady-state libraries (which
	// take that early-return path) still self-heal.
	if libraryID != "" {
		if err := s.artistService.EnsureLibraryMembership(ctx, existing.ID, libraryID); err != nil {
			return fmt.Errorf("ensuring library membership: %w", err)
		}
	}

	// Tag the context so every mutation this function drives is attributed to
	// the scanner rather than falling back to the "manual" default. Two things
	// depend on it: the destructive-image records emitted by the artist_images
	// upsert (issue #2636), which are useless without a calling path, and the
	// metadata history rows written by Update, which previously recorded
	// scan-driven edits as manual ones. "scan" is one of the canonical source
	// values validated in artist.HistoryService.Record.
	scanCtx := artist.ContextWithSource(ctx, "scan")

	// Protect existing placeholders from transient I/O failures: if the image
	// file still exists on disk but placeholder generation failed (returned
	// empty), preserve the existing placeholder.
	preservePlaceholders(existing, &detected)

	// Preserve existing dimensions when a probe fails (returns 0,0).
	preserveDimensions(existing, &detected)

	// Update file existence, low-resolution, and placeholder flags
	changed := existing.NFOExists != detected.NFOExists ||
		existing.ThumbExists != detected.ThumbExists ||
		existing.FanartExists != detected.FanartExists ||
		existing.FanartCount != detected.FanartCount ||
		existing.LogoExists != detected.LogoExists ||
		existing.BannerExists != detected.BannerExists ||
		existing.ThumbLowRes != detected.ThumbLowRes ||
		existing.FanartLowRes != detected.FanartLowRes ||
		existing.LogoLowRes != detected.LogoLowRes ||
		existing.BannerLowRes != detected.BannerLowRes ||
		existing.ThumbPlaceholder != detected.ThumbPlaceholder ||
		existing.FanartPlaceholder != detected.FanartPlaceholder ||
		existing.LogoPlaceholder != detected.LogoPlaceholder ||
		existing.BannerPlaceholder != detected.BannerPlaceholder ||
		existing.ThumbWidth != detected.ThumbWidth ||
		existing.ThumbHeight != detected.ThumbHeight ||
		existing.FanartWidth != detected.FanartWidth ||
		existing.FanartHeight != detected.FanartHeight ||
		existing.LogoWidth != detected.LogoWidth ||
		existing.LogoHeight != detected.LogoHeight ||
		existing.BannerWidth != detected.BannerWidth ||
		existing.BannerHeight != detected.BannerHeight ||
		existing.IsExcluded != excluded

	if !changed && !detected.NFOExists {
		// No flag change, so Update() was skipped and the artist_images
		// registry was not refreshed. Converge it directly so a row that
		// was deleted out-of-band (#1225) heals on the next visit even
		// when nothing else about the artist looks different. Mirror the
		// Update() path's event fanout only when reconciliation actually
		// repaired drift, so quiet rescans do not flood subscribers.
		repaired, err := s.artistService.ReconcileImages(scanCtx, existing, imageEnumeration(detected))
		if err != nil {
			return fmt.Errorf("reconciling artist images: %w", err)
		}
		if repaired {
			s.publishArtistUpdated(existing.ID)
		}
		return nil
	}

	existing.NFOExists = detected.NFOExists
	existing.ThumbExists = detected.ThumbExists
	existing.FanartExists = detected.FanartExists
	existing.FanartCount = detected.FanartCount
	existing.LogoExists = detected.LogoExists
	existing.BannerExists = detected.BannerExists
	existing.ThumbLowRes = detected.ThumbLowRes
	existing.FanartLowRes = detected.FanartLowRes
	existing.LogoLowRes = detected.LogoLowRes
	existing.BannerLowRes = detected.BannerLowRes
	existing.ThumbPlaceholder = detected.ThumbPlaceholder
	existing.FanartPlaceholder = detected.FanartPlaceholder
	existing.LogoPlaceholder = detected.LogoPlaceholder
	existing.BannerPlaceholder = detected.BannerPlaceholder
	existing.ThumbWidth = detected.ThumbWidth
	existing.ThumbHeight = detected.ThumbHeight
	existing.FanartWidth = detected.FanartWidth
	existing.FanartHeight = detected.FanartHeight
	existing.LogoWidth = detected.LogoWidth
	existing.LogoHeight = detected.LogoHeight
	existing.BannerWidth = detected.BannerWidth
	existing.BannerHeight = detected.BannerHeight

	// Update exclusion status
	if excluded {
		existing.IsExcluded = true
		existing.ExclusionReason = "default exclusion list"
	} else {
		existing.IsExcluded = false
		existing.ExclusionReason = ""
	}

	// Re-parse NFO for updated metadata. Skip for locked artists
	// to avoid overwriting user-curated metadata.
	//
	// IMPORTANT (issue #1726): the re-scan path must NEVER write
	// artists.locked. Before the fix, populateFromNFO returning true (the
	// NFO carries <lockdata>true</lockdata>) re-locked the artist on every
	// rescan, silently undoing user unlocks. The canonical lock signal is
	// (per-artist UI toggle) || (per-library NFOLockData setting) ||
	// (scheduled platform pull). The NFO file is downstream of those, not
	// upstream, so the scanner ignores its lockdata bit on re-scan.
	if detected.NFOExists && !existing.Locked {
		s.populateFromNFO(dirPath, existing)
	}

	now := time.Now().UTC()
	existing.LastScannedAt = &now

	// scanCtx carries source="scan" (issue #2636). Beyond tagging the image
	// deletion / exists_flag records this call may emit, the tag also reaches
	// the history layer: metadata_changes rows written by this re-scan now
	// record source="scan" where they previously fell through to the "manual"
	// default. That is the accurate value and an accepted one (see the source
	// validation in artist/history.go), but it is operator-visible: the
	// Activity and artist History views render and filter on this column, so
	// an operator filtering for "manual" to isolate human edits will see
	// scan-driven rows move out of that bucket.
	if err := s.artistService.Update(scanCtx, existing); err != nil {
		return fmt.Errorf("updating artist: %w", err)
	}

	// Update is declarative: it writes the slots `existing` names and leaves
	// slots it does not mention alone (issue #2635). Converging the registry
	// with disk therefore needs an explicit reconcile, and this call site has
	// earned one -- every image field on `existing` was overwritten from
	// `detected` a few lines above, so it carries filesystem truth. The bound
	// comes from `detected` rather than from `existing` for the reason
	// imageEnumeration documents. Without this, deleting artwork on disk would
	// leave its registry row behind forever.
	_, reconcileErr := s.artistService.ReconcileImages(scanCtx, existing, imageEnumeration(detected))

	// The event and the counter are NOT gated on the reconcile succeeding.
	// Update() has already committed by this point, so the artist genuinely
	// was updated; returning early on a reconcile failure used to suppress the
	// SSE fanout and the UpdatedArtists tally for a write that landed, leaving
	// the UI showing stale data with no event ever coming to correct it and a
	// scan summary that under-counted its own work. A reconcile failure is
	// reported, but it does not retroactively un-update the artist.
	s.publishArtistUpdated(existing.ID)

	s.mu.Lock()
	result.UpdatedArtists++
	s.mu.Unlock()
	s.logger.Debug("artist updated", "name", existing.Name, "path", dirPath)

	if reconcileErr != nil {
		return fmt.Errorf("reconciling artist images after update: %w", reconcileErr)
	}
	return nil
}

// preservePlaceholders fills any empty placeholder in detected from existing
// when the corresponding image file is still present on disk. This guards
// against transient I/O failures during placeholder generation clobbering a
// previously stored placeholder.
func preservePlaceholders(existing *artist.Artist, detected *detectedFiles) {
	if detected.ThumbPlaceholder == "" && detected.ThumbExists {
		detected.ThumbPlaceholder = existing.ThumbPlaceholder
	}
	if detected.FanartPlaceholder == "" && detected.FanartExists {
		detected.FanartPlaceholder = existing.FanartPlaceholder
	}
	if detected.LogoPlaceholder == "" && detected.LogoExists {
		detected.LogoPlaceholder = existing.LogoPlaceholder
	}
	if detected.BannerPlaceholder == "" && detected.BannerExists {
		detected.BannerPlaceholder = existing.BannerPlaceholder
	}
}

// preserveDimensions copies stored dimensions from existing into detected for
// any image slot where the probe returned 0,0. A zero result signals a decode
// failure, not a truly zero-dimension image.
func preserveDimensions(existing *artist.Artist, detected *detectedFiles) {
	if detected.ThumbExists && detected.ThumbWidth == 0 && detected.ThumbHeight == 0 && existing.ThumbWidth > 0 && existing.ThumbHeight > 0 {
		detected.ThumbWidth = existing.ThumbWidth
		detected.ThumbHeight = existing.ThumbHeight
	}
	if detected.FanartExists && detected.FanartWidth == 0 && detected.FanartHeight == 0 && existing.FanartWidth > 0 && existing.FanartHeight > 0 {
		detected.FanartWidth = existing.FanartWidth
		detected.FanartHeight = existing.FanartHeight
	}
	if detected.LogoExists && detected.LogoWidth == 0 && detected.LogoHeight == 0 && existing.LogoWidth > 0 && existing.LogoHeight > 0 {
		detected.LogoWidth = existing.LogoWidth
		detected.LogoHeight = existing.LogoHeight
	}
	if detected.BannerExists && detected.BannerWidth == 0 && detected.BannerHeight == 0 && existing.BannerWidth > 0 && existing.BannerHeight > 0 {
		detected.BannerWidth = existing.BannerWidth
		detected.BannerHeight = existing.BannerHeight
	}
}

// hasNumericSuffix reports whether s is non-empty and consists entirely of ASCII digits.
func hasNumericSuffix(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// publishArtistUpdated publishes an ArtistUpdated event if the event bus is
// configured. It is a no-op when no bus is set.
func (s *Service) publishArtistUpdated(artistID string) {
	if s.eventBus != nil {
		s.eventBus.Publish(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": artistID},
		})
	}
}

// populateFromNFO parses the artist.nfo file and merges metadata into the artist.
// Returns true if the parsed NFO contains <lockdata>true</lockdata>.
func (s *Service) populateFromNFO(dirPath string, a *artist.Artist) bool {
	nfoPath := filepath.Join(dirPath, "artist.nfo")
	f, err := os.Open(nfoPath) //nolint:gosec // G304: path is constructed from trusted library root
	if err != nil {
		s.logger.Warn("failed to open artist.nfo", "path", nfoPath, "error", err)
		return false
	}
	defer f.Close() //nolint:errcheck // Close error not actionable on cleanup

	parsed, err := nfo.Parse(f)
	if err != nil {
		s.logger.Warn("failed to parse artist.nfo", "path", nfoPath, "error", err)
		return false
	}

	u := nfo.ToMetadataUpdate(parsed)
	// The operator's per-field locks are enforced here: ApplyMetadata reads
	// a.LockedFields off the artist itself, so a pinned field survives this
	// NFO import whether the incoming NFO omits the element or carries a
	// different value (issue #2749).
	artist.ApplyMetadata(a, u, artist.NFOImport, artist.MergeOptions{})
	return parsed.LockData
}

// recordHealthSnapshot computes the library-wide health score and records it.
func (s *Service) recordHealthSnapshot(ctx context.Context) {
	if s.ruleService == nil {
		return
	}
	const pageSize = 200
	params := artist.ListParams{Page: 1, PageSize: pageSize, Sort: "name"}

	firstPage, total, err := s.artistService.List(ctx, params)
	if err != nil {
		s.logger.Warn("listing artists for health snapshot", "error", err)
		return
	}
	if total == 0 {
		return
	}

	compliant := 0
	var scoreSum float64
	processed := 0

	processPage := func(artists []artist.Artist) {
		for i := range artists {
			a := &artists[i]
			if a.HealthScore >= 100.0 {
				compliant++
			}
			scoreSum += a.HealthScore
			processed++
		}
	}

	processPage(firstPage)
	for processed < total {
		if ctx.Err() != nil {
			s.logger.Warn("context canceled while listing artists for health snapshot", "page", params.Page, "error", ctx.Err())
			break
		}
		params.Page++
		more, _, err := s.artistService.List(ctx, params)
		if err != nil {
			s.logger.Warn("listing artists for health snapshot (page)", "page", params.Page, "error", err)
			break
		}
		if len(more) == 0 {
			break
		}
		processPage(more)
	}

	if processed == 0 {
		return
	}
	avgScore := scoreSum / float64(processed)

	if err := s.ruleService.RecordHealthSnapshot(ctx, total, compliant, avgScore); err != nil {
		s.logger.Warn("recording health snapshot", "error", err)
	}
}

// detectRemoved finds artists in the database whose paths no longer exist
// on disk. Iterates the per-library map produced by the scan loop above
// and queries each library's membership with ListRefsByLibrary -- a
// single lightweight query per scanned library, instead of paginating
// the full hydrated artist catalog (#1409). When discoveredByLibrary has
// N entries this issues exactly N queries (plus the per-artist Delete
// call when a removal is actually needed).
//
// Legacy single-path mode (no LibraryLister + empty defaultLibraryID)
// falls back to the paginated full-catalog sweep that the M:N era
// inherited; without a libraryID to scope against, ListRefsByLibrary
// would return zero rows and the removal pass would be a silent no-op.
// Tests still depend on this fallback (TestScan_RemovedArtist), and the
// legacy code path is hit only when an operator runs Stillwater without
// a configured LibraryService, which is itself unusual.
func (s *Service) detectRemoved(ctx context.Context, discoveredByLibrary map[string]map[string]bool, result *ScanResult) {
	for libraryID, discoveredPaths := range discoveredByLibrary {
		if ctx.Err() != nil {
			return
		}
		if libraryID == "" {
			s.detectRemovedLegacyFallback(ctx, discoveredPaths, result)
			continue
		}
		refs, err := s.artistService.ListRefsByLibrary(ctx, libraryID)
		if err != nil {
			s.logger.Warn("listing artist refs for removal check",
				"library_id", libraryID, "error", err)
			continue
		}
		for _, ref := range refs {
			if discoveredPaths[ref.Path] {
				continue
			}
			if err := s.artistService.Delete(ctx, ref.ID); err != nil {
				s.logger.Warn("failed to remove artist", "id", ref.ID, "error", err)
				continue
			}
			s.mu.Lock()
			result.RemovedArtists++
			s.mu.Unlock()
			s.logger.Debug("artist removed (directory gone)", "name", ref.Name, "path", ref.Path)
		}
	}
}

// detectRemovedLegacyFallback is the paginated full-catalog sweep used by
// the legacy single-path mode (no LibraryLister). It performs path-prefix
// matching against s.libraryPath so artists outside the scanned tree are
// left alone. Kept here as a back-compat shim; production deployments use
// the per-library path above.
func (s *Service) detectRemovedLegacyFallback(ctx context.Context, discoveredPaths map[string]bool, result *ScanResult) {
	const pageSize = 200
	params := artist.ListParams{Page: 1, PageSize: pageSize, Sort: "name"}

	firstPage, total, err := s.artistService.List(ctx, params)
	if err != nil {
		s.logger.Warn("failed to list artists for removal check", "error", err)
		return
	}

	var allArtists []artist.Artist
	allArtists = append(allArtists, firstPage...)
	for len(allArtists) < total {
		if ctx.Err() != nil {
			return
		}
		params.Page++
		more, _, err := s.artistService.List(ctx, params)
		if err != nil {
			s.logger.Warn("failed to list artists for removal check (page)", "page", params.Page, "error", err)
			break
		}
		if len(more) == 0 {
			break
		}
		allArtists = append(allArtists, more...)
	}

	libraryPath := s.libraryPath
	for i := range allArtists {
		a := &allArtists[i]
		if discoveredPaths[a.Path] {
			continue
		}
		if libraryPath != "" {
			aPath := filepath.Clean(a.Path)
			rel, err := filepath.Rel(filepath.Clean(libraryPath), aPath)
			if err != nil {
				continue
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
		}
		if err := s.artistService.Delete(ctx, a.ID); err != nil {
			s.logger.Warn("failed to remove artist", "id", a.ID, "error", err)
			continue
		}
		s.mu.Lock()
		result.RemovedArtists++
		s.mu.Unlock()
		s.logger.Debug("artist removed (directory gone)", "name", a.Name, "path", a.Path)
	}
}

// detectedFiles holds file existence, low-resolution, placeholder, and dimension
// (width/height) flags for an artist directory.
type detectedFiles struct {
	NFOExists         bool
	ThumbExists       bool
	FanartExists      bool
	FanartCount       int
	LogoExists        bool
	BannerExists      bool
	ThumbLowRes       bool
	FanartLowRes      bool
	LogoLowRes        bool
	BannerLowRes      bool
	ThumbPlaceholder  string
	FanartPlaceholder string
	LogoPlaceholder   string
	BannerPlaceholder string
	ThumbWidth        int
	ThumbHeight       int
	FanartWidth       int
	FanartHeight      int
	LogoWidth         int
	LogoHeight        int
	BannerWidth       int
	BannerHeight      int
}

// detectFilesWithFastPath wraps detectFiles with a per-directory mtime
// check (#1413). It is the scanner's entry point into file detection; the
// behavior depends on whether the per-directory mtime indicates the
// directory may have changed since the last scan:
//
//   - Fast path (mtime <= last scan, existing artist, fast-path enabled):
//     calls detectFilesExistenceOnly. That helper still issues one
//     os.ReadDir to learn which canonical filenames are on disk -- enough
//     to set NFOExists, ThumbExists, FanartExists, FanartCount,
//     LogoExists, BannerExists from disk reality. The expensive work is
//     skipped: no os.Open, no image-header decode for dimensions, no full
//     decode for perceptual-hash placeholder generation. Existing
//     dimensions / low-res flags / placeholders are reused for image
//     slots whose file is still present, and zeroed for slots whose file
//     has disappeared (so persistNormalized removes the stale row).
//
//     Because existence is still probed from disk, the issue #1225
//     contract -- "the registry must converge with disk on every visit"
//     -- continues to hold: if hydrateImages reads an empty registry and
//     sets existing.FanartExists=false but the file is still on disk,
//     the fast path sees the file and sets detected.FanartExists=true,
//     the mismatch lands in processExistingArtist's change branch, and
//     Update -> persistNormalized rebuilds the row.
//
//   - Full path (any of: fast-path disabled, no existing artist, no
//     prior LastScannedAt, mtime > last scan, future-dated mtime more
//     than one minute ahead of "now", os.Stat failure): calls detectFiles
//     which does the full per-image probe (dimensions + low-res +
//     placeholder generation). The future-mtime guard absorbs ordinary
//     clock drift while preventing a misbehaving filesystem from
//     starving the per-image rescan indefinitely.
//
// The perf win is preserved: on a steady-state library where nothing
// changes on disk, every recurring scan does one os.Stat + one
// os.ReadDir per artist directory and zero image decodes. Image decodes
// (dimension probe + placeholder generation) only run on first scan,
// after a file mutation that bumps the directory mtime, or when the
// fast path is disabled for an off-clock filesystem.
func (s *Service) detectFilesWithFastPath(dirPath string, existing *artist.Artist) (detectedFiles, error) {
	if !s.mtimeFastPath.Load() || existing == nil || existing.LastScannedAt == nil {
		return detectFiles(dirPath, existing)
	}
	info, err := os.Stat(dirPath)
	if err != nil {
		// Stat failure: fall through so detectFiles surfaces the real
		// error (or recovers via ReadDir behavior). Don't swallow it
		// here -- preserves the legacy diagnostic.
		return detectFiles(dirPath, existing)
	}
	mtime := info.ModTime()
	lastScan := *existing.LastScannedAt
	// Defensive against future-dated mtimes: a filesystem with a broken
	// clock (or a manually-touched directory) can produce mtimes after
	// "now". Re-probe in that case so a stuck future timestamp cannot
	// indefinitely starve the per-file rescan. One minute of slack
	// absorbs ordinary clock drift without disabling the fast path.
	if mtime.After(time.Now().Add(time.Minute)) {
		return detectFiles(dirPath, existing)
	}
	if mtime.After(lastScan) {
		return detectFiles(dirPath, existing)
	}
	// Directory unchanged since last scan: do the cheap existence-only
	// probe and reuse stored dimensions / placeholders / low-res flags
	// for slots whose file is still on disk. detectFilesExistenceOnly
	// returns errFastPathFileTouched when a canonical file's own mtime
	// is newer than lastScan -- on POSIX, overwriting a file in place
	// does NOT update the parent directory's mtime, so we have to check
	// each canonical file's mtime individually before trusting cached
	// dimensions / placeholders.
	d, err := detectFilesExistenceOnly(dirPath, existing)
	if errors.Is(err, errFastPathFileTouched) {
		return detectFiles(dirPath, existing)
	}
	return d, err
}

// errFastPathFileTouched signals that detectFilesExistenceOnly detected an
// in-place rewrite of a canonical image / NFO file (file mtime advanced
// past existing.LastScannedAt). The directory mtime won't reflect this on
// POSIX, so detectFilesWithFastPath must fall back to detectFiles to
// re-probe dimensions and placeholders.
var errFastPathFileTouched = fmt.Errorf("fast path: canonical file mtime advanced; falling back to full probe")

// isCanonicalArtistFile reports whether lower (a lowercase filename) matches
// any pattern the scanner reads on the artist hot path: NFO, thumb/folder,
// fanart (including numbered variants), logo, banner.
func isCanonicalArtistFile(lower string) bool {
	if lower == "artist.nfo" {
		return true
	}
	for _, p := range thumbPatterns {
		if lower == p {
			return true
		}
	}
	for _, p := range fanartPatterns {
		if lower == p {
			return true
		}
		// Numbered fanart variants ({base}{N}.{ext}) -- DiscoverFanart's
		// counting walks these on the full path, so their mtime matters
		// for the cached-FanartCount invariant. Require the suffix to be
		// purely numeric so unrelated files like fanart-old.jpg or
		// fanart_backup.png don't needlessly drop the directory out of
		// the fast path; DiscoverFanart only recognizes the numeric form.
		base := strings.TrimSuffix(p, filepath.Ext(p))
		if strings.HasPrefix(lower, base) && lower != p {
			ext := filepath.Ext(lower)
			if ext != filepath.Ext(p) {
				continue
			}
			suffix := strings.TrimSuffix(strings.TrimPrefix(lower, base), ext)
			if hasNumericSuffix(suffix) {
				return true
			}
		}
	}
	for _, p := range logoPatterns {
		if lower == p {
			return true
		}
	}
	for _, p := range bannerPatterns {
		if lower == p {
			return true
		}
	}
	return false
}

// detectFilesExistenceOnly probes an artist directory for which canonical
// image / NFO filenames are present, without running the expensive
// per-file image probe (dimension decode + perceptual-hash placeholder
// generation). It is the cheap half of detectFiles; the fast path in
// detectFilesWithFastPath uses it to honor the issue #1225 disk-vs-
// registry convergence contract while still skipping the work that
// dominates scan time on slow / networked filesystems.
//
// Field policy:
//
//   - *Exists, FanartCount: computed from the live os.ReadDir result.
//     These match what detectFiles would compute.
//   - *Width, *Height, *LowRes, *Placeholder: reused from `existing` for
//     image slots whose file is still on disk. Zeroed when the file has
//     disappeared so persistNormalized clears the stale row.
//
// `existing` must be non-nil; callers already guard on that in
// detectFilesWithFastPath.
func detectFilesExistenceOnly(dirPath string, existing *artist.Artist) (detectedFiles, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return detectedFiles{}, fmt.Errorf("reading directory: %w", err)
	}

	files := make(map[string]string, len(entries))
	// canonicalEntries indexes the DirEntry for each canonical filename we
	// care about so we can stat each one's mtime without re-walking the
	// directory. Required because POSIX does not update the parent
	// directory's mtime when a file is overwritten in place, so the
	// directory-mtime gate in detectFilesWithFastPath cannot detect an
	// in-place rewrite of fanart.jpg / folder.jpg / etc. by itself.
	canonicalEntries := make(map[string]os.DirEntry, 8)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(e.Name())
		files[lower] = e.Name()
		if isCanonicalArtistFile(lower) {
			canonicalEntries[lower] = e
		}
	}

	// existing.LastScannedAt is guaranteed non-nil by the caller. Walk the
	// canonical matches and verify each file's mtime has not advanced
	// since the last scan. If any has, signal the caller to bail out and
	// re-run the full detectFiles probe so dimensions and placeholders
	// reflect the new content.
	lastScan := *existing.LastScannedAt
	for _, e := range canonicalEntries {
		info, err := e.Info()
		if err != nil {
			// Stat racing with a delete is the common cause; treat it
			// like an in-place change and re-probe.
			return detectedFiles{}, errFastPathFileTouched
		}
		if info.ModTime().After(lastScan) {
			return detectedFiles{}, errFastPathFileTouched
		}
	}

	var d detectedFiles
	if _, ok := files["artist.nfo"]; ok {
		d.NFOExists = true
	}

	for _, p := range thumbPatterns {
		if _, ok := files[strings.ToLower(p)]; !ok {
			continue
		}
		d.ThumbExists = true
		d.ThumbWidth = existing.ThumbWidth
		d.ThumbHeight = existing.ThumbHeight
		d.ThumbLowRes = existing.ThumbLowRes
		d.ThumbPlaceholder = existing.ThumbPlaceholder
		break
	}
	// Same ordinal-based resolution as the full path, so the fast path can
	// never report a different fanart shape than a full re-probe would.
	if fanartPaths := discoverFanartFiles(dirPath, entries); len(fanartPaths) > 0 {
		d.FanartExists = true
		d.FanartCount = len(fanartPaths)
		d.FanartWidth = existing.FanartWidth
		d.FanartHeight = existing.FanartHeight
		d.FanartLowRes = existing.FanartLowRes
		d.FanartPlaceholder = existing.FanartPlaceholder
	}
	for _, p := range logoPatterns {
		if _, ok := files[strings.ToLower(p)]; !ok {
			continue
		}
		d.LogoExists = true
		d.LogoWidth = existing.LogoWidth
		d.LogoHeight = existing.LogoHeight
		d.LogoLowRes = existing.LogoLowRes
		d.LogoPlaceholder = existing.LogoPlaceholder
		break
	}
	for _, p := range bannerPatterns {
		if _, ok := files[strings.ToLower(p)]; !ok {
			continue
		}
		d.BannerExists = true
		d.BannerWidth = existing.BannerWidth
		d.BannerHeight = existing.BannerHeight
		d.BannerLowRes = existing.BannerLowRes
		d.BannerPlaceholder = existing.BannerPlaceholder
		break
	}

	return d, nil
}

// detectFiles checks for the presence of known image and NFO files in an artist
// directory and probes each found image for low-resolution status.
// When existing is non-nil, its placeholders are reused to skip expensive
// regeneration for images that already have one.
func detectFiles(dirPath string, existing *artist.Artist) (detectedFiles, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return detectedFiles{}, fmt.Errorf("reading directory: %w", err)
	}

	// Map lowercase filenames to actual on-disk names so that file opens
	// use the real casing (required on case-sensitive filesystems).
	files := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files[strings.ToLower(e.Name())] = e.Name()
		}
	}

	// Extract existing placeholders (nil-safe).
	var existThumbPH, existFanartPH, existLogoPH, existBannerPH string
	if existing != nil {
		existThumbPH = existing.ThumbPlaceholder
		existFanartPH = existing.FanartPlaceholder
		existLogoPH = existing.LogoPlaceholder
		existBannerPH = existing.BannerPlaceholder
	}

	var d detectedFiles
	if _, ok := files["artist.nfo"]; ok {
		d.NFOExists = true
	}

	for _, p := range thumbPatterns {
		if actual, ok := files[strings.ToLower(p)]; ok {
			fp := filepath.Join(dirPath, actual)
			d.ThumbExists = true
			d.ThumbWidth, d.ThumbHeight, d.ThumbLowRes, d.ThumbPlaceholder = probeImageFileFn(fp, "thumb", existThumbPH)
			break
		}
	}
	// Fanart is resolved by ORDINAL, not by primary filename: slot_index is a
	// DiscoverFanart position, so the file at ordinal 0 is whatever discovery
	// returns first, primary or not.
	if fanartPaths := discoverFanartFiles(dirPath, entries); len(fanartPaths) > 0 {
		d.FanartExists = true
		d.FanartCount = len(fanartPaths)
		d.FanartWidth, d.FanartHeight, d.FanartLowRes, d.FanartPlaceholder = probeImageFileFn(fanartPaths[0], "fanart", existFanartPH)
	}
	for _, p := range logoPatterns {
		if actual, ok := files[strings.ToLower(p)]; ok {
			fp := filepath.Join(dirPath, actual)
			d.LogoExists = true
			d.LogoWidth, d.LogoHeight, d.LogoLowRes, d.LogoPlaceholder = probeImageFileFn(fp, "logo", existLogoPH)
			break
		}
	}
	for _, p := range bannerPatterns {
		if actual, ok := files[strings.ToLower(p)]; ok {
			fp := filepath.Join(dirPath, actual)
			d.BannerExists = true
			d.BannerWidth, d.BannerHeight, d.BannerLowRes, d.BannerPlaceholder = probeImageFileFn(fp, "banner", existBannerPH)
			break
		}
	}

	return d, nil
}

// imageEnumeration reports what this directory walk actually found, in the
// form ReconcileImages requires before it will delete anything.
//
// It is built from `detected` and never from the Artist struct, deliberately.
// The Artist's flat image fields are a lossy re-derivation of the same walk
// (extractImageMetadata gates the fanart tail behind slot 0), so deriving the
// delete bound from them would let a lossy read authorize destruction --
// exactly the coupling #2635 was about. `detected` is the walk's own output.
//
// The single-slot types report 1 or 0 because their registry holds at most
// ordinal 0; fanart reports the real file count from discoverFanartFiles.
func imageEnumeration(d detectedFiles) []artist.ImageEnumeration {
	count := func(exists bool) int {
		if exists {
			return 1
		}
		return 0
	}
	return []artist.ImageEnumeration{
		{ImageType: "thumb", FoundSlots: count(d.ThumbExists)},
		{ImageType: "fanart", FoundSlots: d.FanartCount},
		{ImageType: "logo", FoundSlots: count(d.LogoExists)},
		{ImageType: "banner", FoundSlots: count(d.BannerExists)},
	}
}

// discoverFanartFiles returns the artist directory's fanart files in
// DiscoverFanart ordinal order, or nil when there are none. `files` maps
// lowercased filenames to their on-disk casing, as both detection paths build.
//
// It resolves in two passes, and the second pass is the point of the function.
//
// Pass 1 reproduces the historical behavior exactly: the first fanart pattern
// whose PRIMARY file is on disk wins, and discovery runs from that name. Pass 2
// runs only when pass 1 found nothing, and looks for orphan numbered variants
// -- fanart1.jpg with no fanart.jpg.
//
// Without pass 2 the scanner reported FanartExists=false for such an artist,
// because existence was decided by the primary patterns alone. That was already
// wrong (the artwork is on disk and the UI hid it), but the destructive
// reconcile path turned it into data loss: a "no fanart here" detection became
// an enumeration count of zero, which is a positive claim that every stored
// fanart row is stale, and the rows for files still sitting in the directory
// were deleted. The state is not exotic -- a slot delete that fails partway
// skips renumbering and leaves exactly this shape (#2635, #2644).
//
// Pass 2 cannot change any artist that pass 1 resolves, so it is strictly
// additive: it only ever fires where the old code had already concluded there
// was no fanart at all.
// It takes the RAW os.ReadDir entries rather than the lowercased filename map
// the detection paths also build. That map keys on strings.ToLower(name), so on
// a case-sensitive filesystem Fanart1.jpg and fanart1.jpg collapse to a single
// entry while DiscoverFanart, walking entries directly, sees both. The COUNT
// survives that collapse (colliding names share a base, so they share an
// ordinal, and the same-ordinal dedupe below keeps one either way), but WHICH
// file wins does not: the map keeps whichever entry ReadDir happened to insert
// last, where DiscoverFanart breaks the tie deterministically on path. Since
// slot_index is a DiscoverFanart ordinal and ordinal 0's path is what gets
// probed for dimensions, taking the same input the reference implementation
// takes removes the divergence instead of arguing it is harmless. It cannot be
// reproduced on a case-insensitive filesystem such as macOS APFS, but Stillwater
// ships in a Linux container where it can.
func discoverFanartFiles(dirPath string, entries []os.DirEntry) []string {
	// Pass 1: a primary file on disk decides which naming convention applies.
	for _, p := range fanartPatterns {
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(e.Name(), p) {
				return fanartVariants(dirPath, entries, p)
			}
		}
	}
	// Pass 2: no primary anywhere, so numbered variants may still be present.
	for _, p := range fanartPatterns {
		if paths := fanartVariants(dirPath, entries, p); len(paths) > 0 {
			return paths
		}
	}
	return nil
}

// fanartExtensions is the extension allowlist fanart discovery accepts. It
// mirrors image.DiscoverFanart's, and TestDiscoverFanartFiles_MatchesDiscoverFanart
// fails if the two ever drift.
var fanartExtensions = map[string]bool{".jpg": true, ".jpeg": true, ".png": true}

// fanartVariants resolves the fanart files matching primaryName and its
// numbered variants from an ALREADY-READ directory listing, returning absolute
// paths in DiscoverFanart ordinal order.
//
// This is image.DiscoverFanart's algorithm answered from memory, and answering
// it from memory is the entire point. DiscoverFanart re-reads the directory the
// caller has already read successfully, which made discovery a second,
// independent chance to fail -- and its failure had nowhere to go. The old code
// logged the error and returned nil, which is indistinguishable from "this
// artist has no fanart", so the caller set FanartCount=0 and imageEnumeration
// published {fanart, FoundSlots: 0}: a positive claim that zero fanart files
// exist. deleteStaleSlots keeps a row only when slotIndex < found, so a
// fabricated zero licensed deleting EVERY fanart row for the artist while the
// files sat untouched on disk. An SMB blip, fd exhaustion, or a permission
// change between the two reads was enough to trigger it (#2635).
//
// Sourcing the answer from the listing detectFiles already holds removes the
// failure rather than routing it: there is no second read, so there is no error
// to swallow and no path that can report a count it did not measure. If the one
// remaining read fails, its caller returns an error and the artist is skipped
// with its registry untouched. It also drops up to 4 redundant os.ReadDir calls
// per artist from the mtime fast path.
//
// Every rule below is load-bearing and each is a potential under-count if
// dropped, so each is pinned by a case in
// TestDiscoverFanartFiles_MatchesDiscoverFanart: the extension allowlist
// (.jpeg is accepted by DiscoverFanart but absent from fanartPatterns, so
// driving the allowlist off the patterns would silently drop fanart.jpeg), the
// Atoi-must-succeed-and-be-positive variant parse, the keep-first-per-ordinal
// dedupe, and the extension preference that stops backdrop.jpg and
// backdrop.png both claiming ordinal 0.
func fanartVariants(dirPath string, entries []os.DirEntry, primaryName string) []string {
	if primaryName == "" {
		return nil
	}
	primaryExt := strings.ToLower(filepath.Ext(primaryName))
	baseLower := strings.ToLower(strings.TrimSuffix(primaryName, filepath.Ext(primaryName)))

	type indexedFile struct {
		index int
		path  string
	}
	var found []indexedFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if !fanartExtensions[ext] {
			continue
		}
		nameBase := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
		switch {
		case nameBase == baseLower:
			// Primary: exact base match, ordinal 0.
			found = append(found, indexedFile{0, filepath.Join(dirPath, name)})
		case strings.HasPrefix(nameBase, baseLower):
			// Numbered variant: {base}{N} for a positive integer N.
			n, err := strconv.Atoi(nameBase[len(baseLower):])
			if err != nil || n <= 0 {
				continue
			}
			found = append(found, indexedFile{n, filepath.Join(dirPath, name)})
		}
	}

	// Sort by ordinal, then prefer the extension matching primaryName so that
	// when both backdrop.jpg and backdrop.png sit at ordinal 0, only one is
	// returned. The final path comparison breaks the remaining ties the same way
	// DiscoverFanart does, so two files differing only in case resolve
	// identically here and there.
	sort.Slice(found, func(i, j int) bool {
		if found[i].index != found[j].index {
			return found[i].index < found[j].index
		}
		ei := strings.ToLower(filepath.Ext(found[i].path))
		ej := strings.ToLower(filepath.Ext(found[j].path))
		if (ei == primaryExt) != (ej == primaryExt) {
			return ei == primaryExt
		}
		return found[i].path < found[j].path
	})

	// Deduplicate: keep only the first entry per ordinal.
	paths := make([]string, 0, len(found))
	lastIdx := -1
	for _, f := range found {
		if f.index == lastIdx {
			continue
		}
		lastIdx = f.index
		paths = append(paths, f.path)
	}
	if len(paths) == 0 {
		return nil
	}
	return paths
}

// probeImageFileFn is the package-level seam through which detectFiles calls
// probeImageFile. Tests swap it to count or short-circuit the expensive
// image-probe work; production code never reassigns it.
var probeImageFileFn = probeImageFile

// probeImageFile opens a file once, probes dimensions for low-resolution
// detection, and generates a placeholder. If existingPlaceholder is non-empty
// the expensive full decode for placeholder generation is skipped.
// Returns (width, height, lowRes, placeholder); errors are silently swallowed
// (non-fatal).
func probeImageFile(filePath, imageType, existingPlaceholder string) (width, height int, lowRes bool, placeholder string) {
	f, err := os.Open(filePath) //nolint:gosec // path built from trusted naming patterns
	if err != nil {
		return 0, 0, false, ""
	}
	defer func() { _ = f.Close() }()

	w, h, err := img.GetDimensions(f)
	if err != nil {
		return 0, 0, false, ""
	}
	lowRes = img.IsLowResolution(w, h, imageType)

	if existingPlaceholder != "" {
		return w, h, lowRes, existingPlaceholder
	}

	if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
		return w, h, lowRes, ""
	}
	placeholder, _ = img.GeneratePlaceholder(f, imageType)
	return w, h, lowRes, placeholder
}
