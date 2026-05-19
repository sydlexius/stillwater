package scanner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	artistService    *artist.Service
	ruleEngine       *rule.Engine
	ruleService      *rule.Service
	logger           *slog.Logger
	libraryPath      string
	exclusions       map[string]bool
	eventBus         *event.Bus
	defaultLibraryID string
	libraryLister    LibraryLister

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

// NewService creates a scanner service. The mtime fast-path is enabled by
// default so production deployments get the optimization without an
// explicit setter call; callers that need to disable it (FUSE mounts,
// restored backups with broken mtimes) toggle SetMtimeFastPath(false).
func NewService(artistService *artist.Service, ruleEngine *rule.Engine, ruleService *rule.Service, logger *slog.Logger, libraryPath string, exclusions []string) *Service {
	excMap := make(map[string]bool, len(exclusions))
	for _, e := range exclusions {
		excMap[strings.ToLower(e)] = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		artistService:  artistService,
		ruleEngine:     ruleEngine,
		ruleService:    ruleService,
		logger:         logger,
		libraryPath:    libraryPath,
		exclusions:     excMap,
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
	}
	s.mtimeFastPath.Store(true) // matches ScannerConfig.MtimeFastPath default
	return s
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

func (s *Service) runScan(ctx context.Context, result *ScanResult) {
	defer s.scanWg.Done()
	defer func() {
		s.mu.Lock()
		now := time.Now().UTC()
		result.CompletedAt = &now
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
		for _, lib := range libs {
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
		s.mu.Lock()
		result.Status = "failed"
		result.Error = "no scannable libraries (all libraries are API-only or no paths configured)"
		s.mu.Unlock()
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
			}
		}

		discoveredPaths := make(map[string]bool, len(entries))
		for _, entry := range entries {
			if ctx.Err() != nil {
				s.mu.Lock()
				result.Status = "failed"
				result.Error = "scan canceled"
				s.mu.Unlock()
				return
			}

			if !entry.IsDir() {
				continue
			}
			// Skip hidden directories
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}

			s.mu.Lock()
			result.TotalDirectories++
			s.mu.Unlock()
			dirPath := filepath.Join(target.path, entry.Name())
			discoveredPaths[dirPath] = true

			if err := s.processDirectory(ctx, dirPath, entry.Name(), target.libraryID, libraryArtists, result); err != nil {
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

func (s *Service) processDirectory(ctx context.Context, dirPath, name, libraryID string, preloaded map[string]*artist.Artist, result *ScanResult) error {
	excluded := s.exclusions[strings.ToLower(name)]

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
		return s.processNewArtist(ctx, dirPath, name, libraryID, excluded, detected, result)
	}
	return s.processExistingArtist(ctx, dirPath, existing, excluded, detected, result)
}

// processNewArtist creates a new artist record for a directory not yet in the
// database. It applies file detection results, parses NFO metadata if present,
// and publishes an ArtistUpdated event.
func (s *Service) processNewArtist(ctx context.Context, dirPath, name, libraryID string, excluded bool, detected detectedFiles, result *ScanResult) error {
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
			// Import lockdata from NFO only for newly discovered artists.
			a.Locked = true
			a.LockSource = "imported"
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
	return nil
}

// processExistingArtist reconciles an artist already in the database against
// the current filesystem state. It preserves placeholders and dimensions on
// transient probe failures, applies NFO re-import for unlocked artists, and
// falls back to a lightweight ReconcileImages call when nothing has changed.
func (s *Service) processExistingArtist(ctx context.Context, dirPath string, existing *artist.Artist, excluded bool, detected detectedFiles, result *ScanResult) error {
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
		repaired, err := s.artistService.ReconcileImages(ctx, existing)
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
	if detected.NFOExists && !existing.Locked {
		if s.populateFromNFO(dirPath, existing) {
			// Mirror the new-artist path: an NFO carrying
			// <lockdata>true</lockdata> (set by Stillwater or another
			// tool) surfaces as an artist-level lock so the UI reflects
			// that the metadata is locked. One-way (NFO -> artist).
			existing.Locked = true
			existing.LockSource = "imported"
			lockedAt := time.Now().UTC()
			existing.LockedAt = &lockedAt
		}
	}

	now := time.Now().UTC()
	existing.LastScannedAt = &now

	if err := s.artistService.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating artist: %w", err)
	}

	s.publishArtistUpdated(existing.ID)

	s.mu.Lock()
	result.UpdatedArtists++
	s.mu.Unlock()
	s.logger.Debug("artist updated", "name", existing.Name, "path", dirPath)
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
		for _, a := range artists {
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
	for _, a := range allArtists {
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
		// for the cached-FanartCount invariant.
		base := strings.TrimSuffix(p, filepath.Ext(p))
		if strings.HasPrefix(lower, base) && lower != p {
			ext := filepath.Ext(lower)
			if ext == filepath.Ext(p) {
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
		if _, ok := files[strings.ToLower(p)]; ok {
			d.ThumbExists = true
			d.ThumbWidth = existing.ThumbWidth
			d.ThumbHeight = existing.ThumbHeight
			d.ThumbLowRes = existing.ThumbLowRes
			d.ThumbPlaceholder = existing.ThumbPlaceholder
			break
		}
	}
	for _, p := range fanartPatterns {
		if actual, ok := files[strings.ToLower(p)]; ok {
			d.FanartExists = true
			d.FanartWidth = existing.FanartWidth
			d.FanartHeight = existing.FanartHeight
			d.FanartLowRes = existing.FanartLowRes
			d.FanartPlaceholder = existing.FanartPlaceholder
			// Count fanart variants from the same ReadDir result the
			// full path uses via img.DiscoverFanart. Cheap: pure
			// string-pattern matching against the already-built map.
			fanartPaths, fanartErr := img.DiscoverFanart(dirPath, actual)
			if fanartErr != nil {
				slog.Warn("discovering fanart variants during scan",
					slog.String("dir", dirPath),
					slog.String("error", fanartErr.Error()))
			}
			if len(fanartPaths) > 0 {
				d.FanartCount = len(fanartPaths)
			} else {
				d.FanartCount = 1
			}
			break
		}
	}
	for _, p := range logoPatterns {
		if _, ok := files[strings.ToLower(p)]; ok {
			d.LogoExists = true
			d.LogoWidth = existing.LogoWidth
			d.LogoHeight = existing.LogoHeight
			d.LogoLowRes = existing.LogoLowRes
			d.LogoPlaceholder = existing.LogoPlaceholder
			break
		}
	}
	for _, p := range bannerPatterns {
		if _, ok := files[strings.ToLower(p)]; ok {
			d.BannerExists = true
			d.BannerWidth = existing.BannerWidth
			d.BannerHeight = existing.BannerHeight
			d.BannerLowRes = existing.BannerLowRes
			d.BannerPlaceholder = existing.BannerPlaceholder
			break
		}
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
	for _, p := range fanartPatterns {
		if actual, ok := files[strings.ToLower(p)]; ok {
			fp := filepath.Join(dirPath, actual)
			d.FanartExists = true
			d.FanartWidth, d.FanartHeight, d.FanartLowRes, d.FanartPlaceholder = probeImageFileFn(fp, "fanart", existFanartPH)
			// Count all fanart files (primary + numbered variants).
			fanartPaths, fanartErr := img.DiscoverFanart(dirPath, actual)
			if fanartErr != nil {
				slog.Warn("discovering fanart variants during scan",
					slog.String("dir", dirPath),
					slog.String("error", fanartErr.Error()))
			}
			if len(fanartPaths) > 0 {
				d.FanartCount = len(fanartPaths)
			} else {
				d.FanartCount = 1
			}
			break
		}
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
