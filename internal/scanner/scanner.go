package scanner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// NewService creates a scanner service.
func NewService(artistService *artist.Service, ruleEngine *rule.Engine, ruleService *rule.Service, logger *slog.Logger, libraryPath string, exclusions []string) *Service {
	excMap := make(map[string]bool, len(exclusions))
	for _, e := range exclusions {
		excMap[strings.ToLower(e)] = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		artistService:  artistService,
		ruleEngine:     ruleEngine,
		ruleService:    ruleService,
		logger:         logger,
		libraryPath:    libraryPath,
		exclusions:     excMap,
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
	}
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
	go s.runScan(s.shutdownCtx, result)

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

	// Collect discovered paths for removal detection
	discoveredPaths := make(map[string]bool)
	var allLibraryPaths []string

	for _, target := range targets {
		allLibraryPaths = append(allLibraryPaths, target.path)
		s.logger.Info("scanning library", "path", target.path, "library_id", target.libraryID)

		entries, err := os.ReadDir(target.path)
		if err != nil {
			s.logger.Error("reading library directory", "error", err, "path", target.path)
			continue
		}

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

			if err := s.processDirectory(ctx, dirPath, entry.Name(), target.libraryID, result); err != nil {
				s.logger.Warn("error processing directory",
					"path", dirPath, "error", err)
			}
		}
	}

	// Detect removed artists
	s.detectRemoved(ctx, discoveredPaths, allLibraryPaths, result)

	// Record health snapshot at end of scan
	s.recordHealthSnapshot(ctx)
}

func (s *Service) processDirectory(ctx context.Context, dirPath, name, libraryID string, result *ScanResult) error {
	// Check if directory name matches exclusion list
	excluded := s.exclusions[strings.ToLower(name)]

	// Look up existing artist before detectFiles so we can skip expensive
	// placeholder regeneration when one already exists.
	existing, err := s.artistService.GetByPath(ctx, dirPath)
	if err != nil {
		return fmt.Errorf("looking up artist by path: %w", err)
	}

	detected, detectErr := detectFiles(dirPath, existing)
	if detectErr != nil {
		s.logger.Warn("reading artist directory", "path", dirPath, "error", detectErr)
		return nil // skip this directory entirely -- preserve existing DB state
	}

	if existing == nil {
		// New artist
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

		rule.EvaluateAndPersistHealth(ctx, s.ruleEngine, s.artistService, a, s.logger)

		s.mu.Lock()
		result.NewArtists++
		s.mu.Unlock()
		s.logger.Debug("new artist discovered", "name", name, "path", dirPath)
	} else {
		// Protect existing placeholders from transient I/O failures:
		// if the image file still exists on disk but placeholder generation
		// failed (returned empty), preserve the existing placeholder.
		thumbPH := detected.ThumbPlaceholder
		if thumbPH == "" && detected.ThumbExists {
			thumbPH = existing.ThumbPlaceholder
		}
		fanartPH := detected.FanartPlaceholder
		if fanartPH == "" && detected.FanartExists {
			fanartPH = existing.FanartPlaceholder
		}
		logoPH := detected.LogoPlaceholder
		if logoPH == "" && detected.LogoExists {
			logoPH = existing.LogoPlaceholder
		}
		bannerPH := detected.BannerPlaceholder
		if bannerPH == "" && detected.BannerExists {
			bannerPH = existing.BannerPlaceholder
		}

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
			existing.ThumbPlaceholder != thumbPH ||
			existing.FanartPlaceholder != fanartPH ||
			existing.LogoPlaceholder != logoPH ||
			existing.BannerPlaceholder != bannerPH ||
			existing.IsExcluded != excluded

		if changed || detected.NFOExists {
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
			existing.ThumbPlaceholder = thumbPH
			existing.FanartPlaceholder = fanartPH
			existing.LogoPlaceholder = logoPH
			existing.BannerPlaceholder = bannerPH

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
				s.populateFromNFO(dirPath, existing)
			}

			now := time.Now().UTC()
			existing.LastScannedAt = &now

			if err := s.artistService.Update(ctx, existing); err != nil {
				return fmt.Errorf("updating artist: %w", err)
			}

			rule.EvaluateAndPersistHealth(ctx, s.ruleEngine, s.artistService, existing, s.logger)

			s.mu.Lock()
			result.UpdatedArtists++
			s.mu.Unlock()
			s.logger.Debug("artist updated", "name", existing.Name, "path", dirPath)
		}
	}

	return nil
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
	defer f.Close() //nolint:errcheck

	parsed, err := nfo.Parse(f)
	if err != nil {
		s.logger.Warn("failed to parse artist.nfo", "path", nfoPath, "error", err)
		return false
	}

	converted := nfo.ToArtist(parsed)

	// Merge NFO fields into artist (NFO data takes precedence for metadata)
	if converted.Name != "" {
		a.Name = converted.Name
	}
	if converted.SortName != "" {
		a.SortName = converted.SortName
	}
	a.Type = converted.Type
	a.Gender = converted.Gender
	a.Disambiguation = converted.Disambiguation
	if converted.MusicBrainzID != "" {
		a.MusicBrainzID = converted.MusicBrainzID
	}
	if converted.AudioDBID != "" {
		a.AudioDBID = converted.AudioDBID
	}
	a.Genres = converted.Genres
	a.Styles = converted.Styles
	a.Moods = converted.Moods
	a.YearsActive = converted.YearsActive
	a.Born = converted.Born
	a.Formed = converted.Formed
	a.Died = converted.Died
	a.Disbanded = converted.Disbanded
	if converted.Biography != "" {
		a.Biography = converted.Biography
	}
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

// detectRemoved finds artists in the database whose paths no longer exist on disk.
func (s *Service) detectRemoved(ctx context.Context, discoveredPaths map[string]bool, libraryPaths []string, result *ScanResult) {
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

	for _, a := range allArtists {
		if discoveredPaths[a.Path] {
			continue
		}
		// Only remove artists whose path falls under a scanned library.
		// Use filepath.Rel to avoid false positives (e.g. /music matching /music2).
		underScannedLib := false
		aPath := filepath.Clean(a.Path)
		for _, lp := range libraryPaths {
			rel, err := filepath.Rel(filepath.Clean(lp), aPath)
			if err != nil {
				continue
			}
			if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				underScannedLib = true
				break
			}
		}
		if !underScannedLib {
			continue
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

// detectedFiles holds file existence, low-resolution, and placeholder flags for an artist directory.
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
			d.ThumbLowRes, d.ThumbPlaceholder = probeImageFile(fp, "thumb", existThumbPH)
			break
		}
	}
	for _, p := range fanartPatterns {
		if actual, ok := files[strings.ToLower(p)]; ok {
			fp := filepath.Join(dirPath, actual)
			d.FanartExists = true
			d.FanartLowRes, d.FanartPlaceholder = probeImageFile(fp, "fanart", existFanartPH)
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
			d.LogoLowRes, d.LogoPlaceholder = probeImageFile(fp, "logo", existLogoPH)
			break
		}
	}
	for _, p := range bannerPatterns {
		if actual, ok := files[strings.ToLower(p)]; ok {
			fp := filepath.Join(dirPath, actual)
			d.BannerExists = true
			d.BannerLowRes, d.BannerPlaceholder = probeImageFile(fp, "banner", existBannerPH)
			break
		}
	}

	return d, nil
}

// probeImageFile opens a file once, probes dimensions for low-resolution
// detection, and generates a placeholder. If existingPlaceholder is non-empty
// the expensive full decode for placeholder generation is skipped.
// Returns (lowRes, placeholder); errors are silently swallowed (non-fatal).
func probeImageFile(filePath, imageType, existingPlaceholder string) (lowRes bool, placeholder string) {
	f, err := os.Open(filePath) //nolint:gosec // path built from trusted naming patterns
	if err != nil {
		return false, ""
	}
	defer func() { _ = f.Close() }()

	w, h, err := img.GetDimensions(f)
	if err != nil {
		return false, ""
	}
	lowRes = img.IsLowResolution(w, h, imageType)

	if existingPlaceholder != "" {
		return lowRes, existingPlaceholder
	}

	if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
		return lowRes, ""
	}
	placeholder, _ = img.GeneratePlaceholder(f, imageType)
	return lowRes, placeholder
}
