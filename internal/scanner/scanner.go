package scanner

import (
	"context"
	"fmt"
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
	return &Service{
		artistService: artistService,
		ruleEngine:    ruleEngine,
		ruleService:   ruleService,
		logger:        logger,
		libraryPath:   libraryPath,
		exclusions:    excMap,
	}
}

// Run starts a filesystem scan. Only one scan runs at a time.
// Returns a snapshot of the initial scan result (safe to read without synchronization).
func (s *Service) Run(ctx context.Context) (*ScanResult, error) {
	s.mu.Lock()
	if s.currentScan != nil && s.currentScan.Status == "running" {
		s.mu.Unlock()
		return nil, fmt.Errorf("scan already in progress")
	}

	result := &ScanResult{
		ID:        uuid.New().String(),
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	s.currentScan = result
	snapshot := *result
	s.mu.Unlock()

	// Use a detached context so the scan is not canceled when the HTTP
	// request that triggered it completes.
	go s.runScan(context.WithoutCancel(ctx), result)

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
			if lib.Path != "" {
				targets = append(targets, scanTarget{path: lib.Path, libraryID: lib.ID})
			}
		}
	}
	// Fallback: if no libraries found, use the legacy single path.
	if len(targets) == 0 && s.libraryPath != "" {
		targets = append(targets, scanTarget{path: s.libraryPath, libraryID: s.defaultLibraryID})
	}

	if len(targets) == 0 {
		s.mu.Lock()
		result.Status = "failed"
		result.Error = "no library paths configured"
		s.mu.Unlock()
		s.logger.Error("scan failed: no library paths configured")
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
	detected := detectFiles(dirPath)

	// Check if directory name matches exclusion list
	excluded := s.exclusions[strings.ToLower(name)]

	existing, err := s.artistService.GetByPath(ctx, dirPath)
	if err != nil {
		return fmt.Errorf("looking up artist by path: %w", err)
	}

	if existing == nil {
		// New artist
		a := &artist.Artist{
			Name:         name,
			SortName:     name,
			Path:         dirPath,
			LibraryID:    libraryID,
			NFOExists:    detected.NFOExists,
			ThumbExists:  detected.ThumbExists,
			FanartExists: detected.FanartExists,
			LogoExists:   detected.LogoExists,
			BannerExists: detected.BannerExists,
			ThumbLowRes:  detected.ThumbLowRes,
			FanartLowRes: detected.FanartLowRes,
			LogoLowRes:   detected.LogoLowRes,
			BannerLowRes: detected.BannerLowRes,
		}
		if excluded {
			a.IsExcluded = true
			a.ExclusionReason = "default exclusion list"
		}

		// Parse NFO if it exists for metadata
		if detected.NFOExists {
			s.populateFromNFO(dirPath, a)
		}

		now := time.Now().UTC()
		a.LastScannedAt = &now

		if err := s.artistService.Create(ctx, a); err != nil {
			return fmt.Errorf("creating artist: %w", err)
		}

		s.evaluateHealthScore(ctx, a)

		s.mu.Lock()
		result.NewArtists++
		s.mu.Unlock()
		s.logger.Debug("new artist discovered", "name", name, "path", dirPath)
	} else {
		// Update file existence and low-resolution flags
		changed := existing.NFOExists != detected.NFOExists ||
			existing.ThumbExists != detected.ThumbExists ||
			existing.FanartExists != detected.FanartExists ||
			existing.LogoExists != detected.LogoExists ||
			existing.BannerExists != detected.BannerExists ||
			existing.ThumbLowRes != detected.ThumbLowRes ||
			existing.FanartLowRes != detected.FanartLowRes ||
			existing.LogoLowRes != detected.LogoLowRes ||
			existing.BannerLowRes != detected.BannerLowRes ||
			existing.IsExcluded != excluded

		if changed || detected.NFOExists {
			existing.NFOExists = detected.NFOExists
			existing.ThumbExists = detected.ThumbExists
			existing.FanartExists = detected.FanartExists
			existing.LogoExists = detected.LogoExists
			existing.BannerExists = detected.BannerExists
			existing.ThumbLowRes = detected.ThumbLowRes
			existing.FanartLowRes = detected.FanartLowRes
			existing.LogoLowRes = detected.LogoLowRes
			existing.BannerLowRes = detected.BannerLowRes

			// Update exclusion status
			if excluded {
				existing.IsExcluded = true
				existing.ExclusionReason = "default exclusion list"
			} else {
				existing.IsExcluded = false
				existing.ExclusionReason = ""
			}

			// Re-parse NFO for updated metadata
			if detected.NFOExists {
				s.populateFromNFO(dirPath, existing)
			}

			now := time.Now().UTC()
			existing.LastScannedAt = &now

			if err := s.artistService.Update(ctx, existing); err != nil {
				return fmt.Errorf("updating artist: %w", err)
			}

			s.evaluateHealthScore(ctx, existing)

			s.mu.Lock()
			result.UpdatedArtists++
			s.mu.Unlock()
			s.logger.Debug("artist updated", "name", existing.Name, "path", dirPath)
		}
	}

	return nil
}

// populateFromNFO parses the artist.nfo file and merges metadata into the artist.
func (s *Service) populateFromNFO(dirPath string, a *artist.Artist) {
	nfoPath := filepath.Join(dirPath, "artist.nfo")
	f, err := os.Open(nfoPath) //nolint:gosec // G304: path is constructed from trusted library root
	if err != nil {
		s.logger.Warn("failed to open artist.nfo", "path", nfoPath, "error", err)
		return
	}
	defer f.Close() //nolint:errcheck

	parsed, err := nfo.Parse(f)
	if err != nil {
		s.logger.Warn("failed to parse artist.nfo", "path", nfoPath, "error", err)
		return
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
}

// evaluateHealthScore runs the rule engine against an artist and persists the score.
func (s *Service) evaluateHealthScore(ctx context.Context, a *artist.Artist) {
	if s.ruleEngine == nil {
		return
	}
	eval, err := s.ruleEngine.Evaluate(ctx, a)
	if err != nil {
		s.logger.Warn("evaluating health score", "artist", a.Name, "error", err)
		return
	}
	a.HealthScore = eval.HealthScore
	if err := s.artistService.Update(ctx, a); err != nil {
		s.logger.Warn("persisting health score", "artist", a.Name, "error", err)
	}
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
	// List all known artists
	allArtists, _, err := s.artistService.List(ctx, artist.ListParams{
		Page:     1,
		PageSize: 200,
		Sort:     "name",
	})
	if err != nil {
		s.logger.Warn("failed to list artists for removal check", "error", err)
		return
	}

	for _, a := range allArtists {
		if discoveredPaths[a.Path] {
			continue
		}
		// Only remove artists whose path falls under a scanned library
		underScannedLib := false
		for _, lp := range libraryPaths {
			if strings.HasPrefix(a.Path, lp) {
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

// detectedFiles holds file existence and low-resolution flags for an artist directory.
type detectedFiles struct {
	NFOExists    bool
	ThumbExists  bool
	FanartExists bool
	LogoExists   bool
	BannerExists bool
	ThumbLowRes  bool
	FanartLowRes bool
	LogoLowRes   bool
	BannerLowRes bool
}

// detectFiles checks for the presence of known image and NFO files in an artist
// directory and probes each found image for low-resolution status.
func detectFiles(dirPath string) detectedFiles {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return detectedFiles{}
	}

	// Build a set of lowercase filenames for efficient lookup.
	files := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files[strings.ToLower(e.Name())] = true
		}
	}

	var d detectedFiles
	d.NFOExists = files["artist.nfo"]

	for _, p := range thumbPatterns {
		if files[strings.ToLower(p)] {
			d.ThumbExists = true
			d.ThumbLowRes = probeLowRes(filepath.Join(dirPath, p), "thumb")
			break
		}
	}
	for _, p := range fanartPatterns {
		if files[strings.ToLower(p)] {
			d.FanartExists = true
			d.FanartLowRes = probeLowRes(filepath.Join(dirPath, p), "fanart")
			break
		}
	}
	for _, p := range logoPatterns {
		if files[strings.ToLower(p)] {
			d.LogoExists = true
			d.LogoLowRes = probeLowRes(filepath.Join(dirPath, p), "logo")
			break
		}
	}
	for _, p := range bannerPatterns {
		if files[strings.ToLower(p)] {
			d.BannerExists = true
			d.BannerLowRes = probeLowRes(filepath.Join(dirPath, p), "banner")
			break
		}
	}

	return d
}

// probeLowRes opens a file, decodes its dimensions, and reports whether those
// dimensions fall below the threshold for the given image type.
// Returns false on any read or decode error.
func probeLowRes(filePath, imageType string) bool {
	f, err := os.Open(filePath) //nolint:gosec // path built from trusted naming patterns
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	w, h, err := img.GetDimensions(f)
	if err != nil {
		return false
	}
	return img.IsLowResolution(w, h, imageType)
}
