package rule

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/filesystem"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
)

// imageProvider is the subset of provider.Orchestrator used by ImageFixer.
type imageProvider interface {
	FetchImages(ctx context.Context, mbid string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error)
}

// imageCacheEntry holds a cached FetchImages result (or error) for one MBID.
type imageCacheEntry struct {
	result *provider.FetchResult
	err    error
}

const (
	fetchTimeout  = 30 * time.Second
	maxImageBytes = 25 << 20 // 25 MB
)

// NFOFixer creates missing artist.nfo files from the artist's current metadata.
type NFOFixer struct {
	SnapshotService *nfo.SnapshotService
}

// CanFix returns true for the nfo_exists rule.
func (f *NFOFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleNFOExists
}

// Fix creates an artist.nfo file in the artist's directory.
// If the file already exists and was modified externally, returns without overwriting.
func (f *NFOFixer) Fix(_ context.Context, a *artist.Artist, _ *Violation) (*FixResult, error) {
	target := filepath.Join(a.Path, "artist.nfo")

	// Check for external modifications before writing
	conflict := nfo.CheckFileConflict(target, a.UpdatedAt)
	if conflict.HasConflict {
		return &FixResult{
			RuleID:  RuleNFOExists,
			Fixed:   false,
			Message: fmt.Sprintf("NFO conflict for %s: %s", a.Name, conflict.Reason),
		}, nil
	}

	nfoData := nfo.FromArtist(a)
	var buf bytes.Buffer
	if err := nfo.Write(&buf, nfoData); err != nil {
		return nil, fmt.Errorf("generating nfo: %w", err)
	}

	if err := filesystem.WriteFileAtomic(target, buf.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("writing nfo: %w", err)
	}

	a.NFOExists = true

	return &FixResult{
		RuleID:  RuleNFOExists,
		Fixed:   true,
		Message: fmt.Sprintf("created artist.nfo for %s", a.Name),
	}, nil
}

// MetadataFixer populates missing metadata (MBID, biography) from providers.
type MetadataFixer struct {
	orchestrator    *provider.Orchestrator
	snapshotService *nfo.SnapshotService
}

// NewMetadataFixer creates a MetadataFixer.
func NewMetadataFixer(orchestrator *provider.Orchestrator, snapshotService *nfo.SnapshotService) *MetadataFixer {
	return &MetadataFixer{orchestrator: orchestrator, snapshotService: snapshotService}
}

// CanFix returns true for nfo_has_mbid and bio_exists rules.
func (f *MetadataFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleNFOHasMBID || v.RuleID == RuleBioExists
}

// Fix searches providers and populates the missing metadata.
func (f *MetadataFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	switch v.RuleID {
	case RuleNFOHasMBID:
		return f.fixMBID(ctx, a)
	case RuleBioExists:
		return f.fixBio(ctx, a)
	default:
		return nil, fmt.Errorf("unsupported rule: %s", v.RuleID)
	}
}

func (f *MetadataFixer) fixMBID(ctx context.Context, a *artist.Artist) (*FixResult, error) {
	results, err := f.orchestrator.Search(ctx, a.Name)
	if err != nil {
		return nil, fmt.Errorf("searching providers: %w", err)
	}

	if len(results) == 0 {
		return &FixResult{
			RuleID:  RuleNFOHasMBID,
			Fixed:   false,
			Message: fmt.Sprintf("no provider results for %s", a.Name),
		}, nil
	}

	// Pick the best match with an MBID
	var best *provider.ArtistSearchResult
	for i := range results {
		if results[i].MusicBrainzID == "" {
			continue
		}
		if best == nil || results[i].Score > best.Score {
			best = &results[i]
		}
	}

	if best == nil {
		return &FixResult{
			RuleID:  RuleNFOHasMBID,
			Fixed:   false,
			Message: "no results with MusicBrainz ID found",
		}, nil
	}

	a.MusicBrainzID = best.MusicBrainzID

	if a.NFOExists {
		writeArtistNFO(a, f.snapshotService)
	}

	return &FixResult{
		RuleID:  RuleNFOHasMBID,
		Fixed:   true,
		Message: fmt.Sprintf("set MBID to %s for %s", best.MusicBrainzID, a.Name),
	}, nil
}

func (f *MetadataFixer) fixBio(ctx context.Context, a *artist.Artist) (*FixResult, error) {
	result, err := f.orchestrator.FetchMetadata(ctx, a.MusicBrainzID, a.Name)
	if err != nil {
		return nil, fmt.Errorf("fetching metadata: %w", err)
	}

	if result.Metadata == nil || result.Metadata.Biography == "" {
		return &FixResult{
			RuleID:  RuleBioExists,
			Fixed:   false,
			Message: fmt.Sprintf("no biography found for %s", a.Name),
		}, nil
	}

	a.Biography = result.Metadata.Biography

	if a.NFOExists {
		writeArtistNFO(a, f.snapshotService)
	}

	return &FixResult{
		RuleID:  RuleBioExists,
		Fixed:   true,
		Message: fmt.Sprintf("populated biography for %s", a.Name),
	}, nil
}

// ImageFixer fetches missing or low-quality images from providers.
type ImageFixer struct {
	orchestrator    imageProvider
	platformService *platform.Service
	logger          *slog.Logger
	imageCache      sync.Map // keyed by MBID; value: *imageCacheEntry
}

// NewImageFixer creates an ImageFixer.
func NewImageFixer(orchestrator imageProvider, platformService *platform.Service, logger *slog.Logger) *ImageFixer {
	return &ImageFixer{
		orchestrator:    orchestrator,
		platformService: platformService,
		logger:          logger,
	}
}

// fetchImages returns provider images for the given MBID, using a per-instance
// cache to avoid duplicate provider calls when an artist has multiple violations.
func (f *ImageFixer) fetchImages(ctx context.Context, mbid, deezerID string) (*provider.FetchResult, error) {
	cacheKey := mbid + ":" + deezerID
	if entry, ok := f.imageCache.Load(cacheKey); ok {
		e := entry.(*imageCacheEntry)
		return e.result, e.err
	}
	result, err := f.orchestrator.FetchImages(ctx, mbid, map[provider.ProviderName]string{
		provider.NameDeezer: deezerID,
	})
	f.imageCache.Store(cacheKey, &imageCacheEntry{result: result, err: err})
	return result, err
}

// CanFix returns true for image-related rules.
func (f *ImageFixer) CanFix(v *Violation) bool {
	switch v.RuleID {
	case RuleThumbExists, RuleFanartExists, RuleLogoExists, RuleThumbSquare, RuleThumbMinRes,
		RuleFanartMinRes, RuleFanartAspect, RuleLogoMinRes, RuleBannerExists, RuleBannerMinRes:
		return true
	default:
		return false
	}
}

// SupportsCandidateDiscovery implements CandidateDiscoverer. ImageFixer can
// return candidate lists without writing to disk when Config.DiscoveryOnly is
// set (manual mode) or when multiple candidates exist without SelectBestCandidate.
func (f *ImageFixer) SupportsCandidateDiscovery() bool { return true }

// Fix fetches the best available image from providers and saves it.
func (f *ImageFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	if a.MusicBrainzID == "" {
		return &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: "no MBID, cannot search image providers",
		}, nil
	}

	imageType := ruleToImageType(v.RuleID)
	if imageType == "" {
		return nil, fmt.Errorf("no image type for rule %s", v.RuleID)
	}

	result, err := f.fetchImages(ctx, a.MusicBrainzID, a.DeezerID)
	if err != nil {
		return nil, fmt.Errorf("fetching images: %w", err)
	}

	// Filter by image type and sort by quality
	var candidates []provider.ImageResult
	for _, im := range result.Images {
		if string(im.Type) == imageType {
			candidates = append(candidates, im)
		}
	}

	if len(candidates) == 0 {
		return &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: fmt.Sprintf("no %s images found from providers", imageType),
		}, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Likes != candidates[j].Likes {
			return candidates[i].Likes > candidates[j].Likes
		}
		return (candidates[i].Width * candidates[i].Height) > (candidates[j].Width * candidates[j].Height)
	})

	// Resolution gate: drop candidates below the configured minimum or below
	// the existing image's pixel count (to prevent accidental downgrades).
	// Existing dimensions are read for all image rules, not only min-res ones,
	// because rules like thumb_square can also fire on a high-res image and
	// must not replace it with a lower-res candidate.
	minW, minH := v.Config.MinWidth, v.Config.MinHeight
	existW, existH := readExistingImageDimensions(ctx, a.Path, imageType, f.platformService)
	candidates = filterCandidatesByResolution(candidates, minW, minH, existW, existH, f.logger)

	if len(candidates) == 0 {
		hasMinConstraint := minW > 0 || minH > 0
		hasExistingConstraint := existW > 0 && existH > 0

		var constraintDesc string
		switch {
		case hasMinConstraint && hasExistingConstraint:
			constraintDesc = "minimum and existing image resolution requirements"
		case hasMinConstraint:
			constraintDesc = "minimum resolution requirements"
		case hasExistingConstraint:
			constraintDesc = "existing image resolution requirements"
		default:
			constraintDesc = "resolution requirements"
		}

		return &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: fmt.Sprintf("no %s candidates meet %s", imageType, constraintDesc),
		}, nil
	}

	// Discovery-only mode (manual automation): return all candidates as a list
	// without downloading or saving anything.
	if v.Config.DiscoveryOnly {
		imageCandidates := make([]ImageCandidate, 0, len(candidates))
		for _, c := range candidates {
			imageCandidates = append(imageCandidates, ImageCandidate{
				URL:       c.URL,
				Width:     c.Width,
				Height:    c.Height,
				Source:    c.Source,
				ImageType: imageType,
			})
		}
		return &FixResult{
			RuleID:     v.RuleID,
			Fixed:      false,
			Message:    fmt.Sprintf("found %d %s candidate(s) for user selection", len(candidates), imageType),
			Candidates: imageCandidates,
		}, nil
	}

	// When multiple candidates exist and SelectBestCandidate is not set,
	// return the list for the user to choose from the Notifications inbox.
	if len(candidates) > 1 && !v.Config.SelectBestCandidate {
		imageCandidates := make([]ImageCandidate, 0, len(candidates))
		for _, c := range candidates {
			imageCandidates = append(imageCandidates, ImageCandidate{
				URL:       c.URL,
				Width:     c.Width,
				Height:    c.Height,
				Source:    c.Source,
				ImageType: imageType,
			})
		}
		return &FixResult{
			RuleID:     v.RuleID,
			Fixed:      false,
			Message:    fmt.Sprintf("found %d %s candidates; awaiting user selection", len(candidates), imageType),
			Candidates: imageCandidates,
		}, nil
	}

	// Try downloading candidates until one succeeds
	for _, c := range candidates {
		data, err := fetchImageURL(ctx, c.URL)
		if err != nil {
			f.logger.Debug("image download failed", "url", c.URL, "error", err)
			continue
		}

		// Verify actual image dimensions post-download. Providers (FanartTV, Deezer)
		// do not report dimensions in their API responses, so all candidates arrive
		// with Width=0/Height=0 and slip past the pre-filter above. Checking here
		// catches low-res downloads before they overwrite a better existing image.
		if minW > 0 || minH > 0 || (existW > 0 && existH > 0) {
			if dw, dh, dimErr := img.GetDimensions(bytes.NewReader(data)); dimErr == nil && dw > 0 && dh > 0 {
				if (minW > 0 && dw < minW) || (minH > 0 && dh < minH) {
					f.logger.Debug("skipping candidate below configured minimum (actual)",
						"url", c.URL, "actual_width", dw, "actual_height", dh,
						"min_width", minW, "min_height", minH)
					continue
				}
				if existW > 0 && existH > 0 && dw*dh < existW*existH {
					f.logger.Debug("skipping candidate below existing resolution (actual)",
						"url", c.URL, "actual_width", dw, "actual_height", dh,
						"existing_width", existW, "existing_height", existH)
					continue
				}
			}
		}

		resized, _, err := img.Resize(bytes.NewReader(data), 3000, 3000)
		if err != nil {
			f.logger.Debug("image resize failed", "url", c.URL, "error", err)
			continue
		}

		naming := existingImageFileNames(ctx, a.Path, imageType, f.platformService)
		useSymlinks := activeUseSymlinks(ctx, f.platformService)
		saved, err := img.Save(a.Path, imageType, resized, naming, useSymlinks, f.logger)
		if err != nil {
			f.logger.Debug("image save failed", "url", c.URL, "error", err)
			continue
		}

		setImageFlag(a, imageType)

		return &FixResult{
			RuleID:  v.RuleID,
			Fixed:   true,
			Message: fmt.Sprintf("saved %s from %s (%v)", imageType, c.Source, saved),
		}, nil
	}

	return &FixResult{
		RuleID:  v.RuleID,
		Fixed:   false,
		Message: fmt.Sprintf("all %d image downloads failed", len(candidates)),
	}, nil
}

// ruleToImageType maps a rule ID to a provider image type string.
func ruleToImageType(ruleID string) string {
	switch ruleID {
	case RuleThumbExists, RuleThumbSquare, RuleThumbMinRes:
		return "thumb"
	case RuleFanartExists, RuleFanartMinRes, RuleFanartAspect:
		return "fanart"
	case RuleLogoExists, RuleLogoMinRes:
		return "logo"
	case RuleBannerExists, RuleBannerMinRes:
		return "banner"
	default:
		return ""
	}
}

// setImageFlag updates the appropriate image flag on the artist.
func setImageFlag(a *artist.Artist, imageType string) {
	switch imageType {
	case "thumb":
		a.ThumbExists = true
	case "fanart":
		a.FanartExists = true
	case "logo":
		a.LogoExists = true
	case "banner":
		a.BannerExists = true
	}
}

// ApplyImageCandidate downloads a candidate URL and saves it as an image in the
// artist directory. The naming slice controls which filenames are written; pass
// nil to fall back to DefaultFileNames. Used by the apply-candidate API handler.
func ApplyImageCandidate(ctx context.Context, a *artist.Artist, imageType, rawURL string, naming []string, useSymlinks bool, logger *slog.Logger) error {
	data, err := fetchImageURL(ctx, rawURL)
	if err != nil {
		return fmt.Errorf("downloading image: %w", err)
	}

	resized, _, err := img.Resize(bytes.NewReader(data), 3000, 3000)
	if err != nil {
		return fmt.Errorf("resizing image: %w", err)
	}

	if len(naming) == 0 {
		naming = img.FileNamesForType(img.DefaultFileNames, imageType)
	}
	if _, err := img.Save(a.Path, imageType, resized, naming, useSymlinks, logger); err != nil {
		return fmt.Errorf("saving image: %w", err)
	}

	setImageFlag(a, imageType)
	return nil
}

// writeArtistNFO writes the artist's current metadata to an artist.nfo file (best effort).
// If a SnapshotService is provided, saves a snapshot of the existing NFO before overwriting.
func writeArtistNFO(a *artist.Artist, ss *nfo.SnapshotService) {
	target := filepath.Join(a.Path, "artist.nfo")

	// Save a snapshot of the existing NFO before overwriting
	if ss != nil {
		if existing, err := os.ReadFile(target); err == nil && len(existing) > 0 { //nolint:gosec // G304: path from trusted artist.Path
			_, _ = ss.Save(context.Background(), a.ID, string(existing))
		}
	}

	nfoData := nfo.FromArtist(a)
	var buf bytes.Buffer
	if err := nfo.Write(&buf, nfoData); err != nil {
		return
	}
	_ = filesystem.WriteFileAtomic(target, buf.Bytes(), 0o644)
}

// existingImageFileNames returns the subset of canonical filenames for imageType
// that actually exist in dir. If none exist, returns a single-element slice with
// the primary filename so that img.Save creates it fresh.
// This prevents img.Save from writing to every canonical filename (e.g.
// folder.jpg, artist.jpg, poster.jpg) when only one of them exists, which would
// otherwise clobber high-res files that are not causing the violation.
// When platformService is non-nil, uses the active profile's names instead of
// the hardcoded defaults.
func existingImageFileNames(ctx context.Context, dir, imageType string, platformService *platform.Service) []string {
	var all []string
	if platformService != nil {
		if profile, err := platformService.GetActive(ctx); err == nil && profile != nil {
			all = profile.ImageNaming.NamesForType(imageType)
		}
	}
	if len(all) == 0 {
		all = img.FileNamesForType(img.DefaultFileNames, imageType)
	}
	var found []string
	for _, name := range all {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			found = append(found, name)
		}
	}
	if len(found) == 0 && len(all) > 0 {
		return all[:1] // create only the primary filename
	}
	return found
}

// filterCandidatesByResolution removes candidates whose declared dimensions are
// below the configured minimum or below the existing image's pixel count.
// Candidates with unknown dimensions (0x0) are kept.
func filterCandidatesByResolution(
	candidates []provider.ImageResult,
	minW, minH, existingW, existingH int,
	logger *slog.Logger,
) []provider.ImageResult {
	filtered := candidates[:0]
	for _, c := range candidates {
		if c.Width > 0 && c.Height > 0 {
			if (minW > 0 && c.Width < minW) || (minH > 0 && c.Height < minH) {
				logger.Debug("skipping candidate below configured minimum",
					"url", c.URL, "width", c.Width, "height", c.Height,
					"min_width", minW, "min_height", minH)
				continue
			}
			if existingW > 0 && existingH > 0 && c.Width*c.Height < existingW*existingH {
				logger.Debug("skipping candidate below existing resolution",
					"url", c.URL, "width", c.Width, "height", c.Height,
					"existing_width", existingW, "existing_height", existingH)
				continue
			}
		}
		filtered = append(filtered, c)
	}
	return filtered
}

// readExistingImageDimensions returns the pixel dimensions of the highest
// resolution recognizable image found in dir for the given image type.
// When platformService is non-nil, uses the active profile's names.
func readExistingImageDimensions(ctx context.Context, dir, imageType string, platformService *platform.Service) (int, int) {
	var names []string
	if platformService != nil {
		if profile, err := platformService.GetActive(ctx); err == nil && profile != nil {
			names = profile.ImageNaming.NamesForType(imageType)
		}
	}
	if len(names) == 0 {
		names = img.FileNamesForType(img.DefaultFileNames, imageType)
	}

	maxW, maxH := 0, 0
	maxPixels := 0

	for _, name := range names {
		if w, h, ok := readFileDimensions(filepath.Join(dir, name)); ok {
			if pixels := w * h; pixels > maxPixels {
				maxPixels = pixels
				maxW, maxH = w, h
			}
		}
	}

	return maxW, maxH
}

func readFileDimensions(path string) (int, int, bool) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return 0, 0, false
	}
	defer f.Close() //nolint:errcheck
	w, h, err := img.GetDimensions(f)
	if err != nil || w == 0 || h == 0 {
		return 0, 0, false
	}
	return w, h, true
}

// ExtraneousImagesFixer deletes non-canonical image files from artist directories.
type ExtraneousImagesFixer struct {
	platformService *platform.Service
	logger          *slog.Logger
}

// NewExtraneousImagesFixer creates an ExtraneousImagesFixer.
func NewExtraneousImagesFixer(platformService *platform.Service, logger *slog.Logger) *ExtraneousImagesFixer {
	return &ExtraneousImagesFixer{
		platformService: platformService,
		logger:          logger,
	}
}

// CanFix returns true for the extraneous_images rule.
func (f *ExtraneousImagesFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleExtraneousImages
}

// Fix deletes all extraneous image files from the artist directory.
func (f *ExtraneousImagesFixer) Fix(ctx context.Context, a *artist.Artist, _ *Violation) (*FixResult, error) {
	if a.Path == "" {
		return &FixResult{
			RuleID:  RuleExtraneousImages,
			Fixed:   false,
			Message: "artist has no path",
		}, nil
	}

	var profile *platform.Profile
	if f.platformService != nil {
		profile, _ = f.platformService.GetActive(ctx)
	}
	expected := expectedImageFiles(profile, a.Path)

	entries, readErr := os.ReadDir(a.Path)
	if readErr != nil {
		return nil, fmt.Errorf("reading directory: %w", readErr)
	}

	var deleted []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if !imageExtensions[ext] {
			continue
		}
		if expected[strings.ToLower(name)] {
			continue
		}
		target := filepath.Join(a.Path, name)
		if rmErr := os.Remove(target); rmErr != nil {
			f.logger.Warn("failed to delete extraneous image",
				slog.String("path", target), slog.String("error", rmErr.Error()))
			continue
		}
		f.logger.Info("deleted extraneous image", slog.String("path", target))
		deleted = append(deleted, name)
	}

	if len(deleted) == 0 {
		return &FixResult{
			RuleID:  RuleExtraneousImages,
			Fixed:   false,
			Message: "no extraneous files to delete",
		}, nil
	}

	return &FixResult{
		RuleID:  RuleExtraneousImages,
		Fixed:   true,
		Message: fmt.Sprintf("deleted %d extraneous file(s): %s", len(deleted), strings.Join(deleted, ", ")),
	}, nil
}

// LogoTrimFixer trims transparent padding from logo PNG files.
type LogoTrimFixer struct {
	platformService *platform.Service
	logger          *slog.Logger
}

// NewLogoTrimFixer creates a LogoTrimFixer.
func NewLogoTrimFixer(platformService *platform.Service, logger *slog.Logger) *LogoTrimFixer {
	return &LogoTrimFixer{
		platformService: platformService,
		logger:          logger,
	}
}

// CanFix returns true for the logo_trimmable rule.
func (f *LogoTrimFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleLogoTrimmable
}

// Fix trims transparent padding from the logo and saves the result.
func (f *LogoTrimFixer) Fix(ctx context.Context, a *artist.Artist, _ *Violation) (*FixResult, error) {
	if a.Path == "" {
		return &FixResult{
			RuleID:  RuleLogoTrimmable,
			Fixed:   false,
			Message: "artist has no path",
		}, nil
	}

	// Find the existing logo file on disk using case-insensitive matching.
	entries, readErr := os.ReadDir(a.Path)
	if readErr != nil {
		return nil, fmt.Errorf("reading artist directory: %w", readErr)
	}
	lowerToActual := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			lowerToActual[strings.ToLower(e.Name())] = e.Name()
		}
	}

	var logoPath string
	for _, pattern := range logoPatterns {
		if actual, ok := lowerToActual[strings.ToLower(pattern)]; ok {
			logoPath = filepath.Join(a.Path, actual)
			break
		}
	}
	if logoPath == "" {
		return &FixResult{
			RuleID:  RuleLogoTrimmable,
			Fixed:   false,
			Message: "no logo file found on disk",
		}, nil
	}

	data, err := os.ReadFile(logoPath) //nolint:gosec // G304: path from trusted artist directory
	if err != nil {
		return nil, fmt.Errorf("reading logo: %w", err)
	}

	// Read original dimensions before trimming.
	origW, origH, origErr := img.GetDimensions(bytes.NewReader(data))

	trimmed, _, err := img.TrimAlpha(bytes.NewReader(data), 128)
	if err != nil {
		return nil, fmt.Errorf("trimming logo: %w", err)
	}

	newW, newH, newErr := img.GetDimensions(bytes.NewReader(trimmed))

	naming := []string{filepath.Base(logoPath)}
	useSymlinks := activeUseSymlinks(ctx, f.platformService)
	savedNames, err := img.Save(a.Path, "logo", trimmed, naming, useSymlinks, f.logger)
	if err != nil {
		return nil, fmt.Errorf("saving trimmed logo: %w", err)
	}

	// Remove original if only extension case changed (e.g., Logo.PNG -> Logo.png)
	// to avoid duplicates on case-sensitive filesystems, but only when the old
	// and new paths are distinct files. On case-insensitive filesystems they may
	// refer to the same file, in which case we must not remove the new logo.
	if len(savedNames) > 0 {
		oldBase := filepath.Base(logoPath)
		newBase := savedNames[0]
		if strings.EqualFold(oldBase, newBase) && oldBase != newBase {
			newPath := filepath.Join(a.Path, newBase)

			oldInfo, errOld := os.Stat(logoPath)
			newInfo, errNew := os.Stat(newPath)
			if errOld == nil && errNew == nil && !os.SameFile(oldInfo, newInfo) {
				if rmErr := os.Remove(logoPath); rmErr != nil {
					f.logger.Warn("failed to remove case-mismatched logo duplicate",
						slog.String("path", logoPath), slog.String("error", rmErr.Error()))
				}
			}
		}
	}

	msg := "trimmed logo padding"
	if origErr == nil && newErr == nil {
		msg = fmt.Sprintf("trimmed logo from %dx%d to %dx%d", origW, origH, newW, newH)
	}

	return &FixResult{
		RuleID:  RuleLogoTrimmable,
		Fixed:   true,
		Message: msg,
	}, nil
}

// fetchImageURL downloads image data from a URL with timeout and size limits.
func fetchImageURL(ctx context.Context, rawURL string) ([]byte, error) {
	client := &http.Client{Timeout: fetchTimeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil) //nolint:gosec // URL from trusted provider results
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req) //nolint:gosec // G704: URL validated against stored provider results before reaching this point
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("image exceeds 25MB limit")
	}

	return data, nil
}

// activeUseSymlinks returns the UseSymlinks flag from the active platform profile.
// Returns false if the service is nil or the profile cannot be fetched.
func activeUseSymlinks(ctx context.Context, svc *platform.Service) bool {
	if svc == nil {
		return false
	}
	p, err := svc.GetActive(ctx)
	if err != nil || p == nil {
		return false
	}
	return p.UseSymlinks
}
