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

	"golang.org/x/text/unicode/norm"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/httpsafe"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/watcher"
)

// imageProvider is the subset of provider.Orchestrator used by ImageFixer.
type imageProvider interface {
	FetchImages(ctx context.Context, mbid string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error)
}

// metadataOrchestrator is the subset of provider.Orchestrator used by
// MetadataFixer. Defining it as an interface keeps the fixer testable with a
// stub instead of requiring a full orchestrator and live provider chain.
type metadataOrchestrator interface {
	Search(ctx context.Context, name string) ([]provider.ArtistSearchResult, error)
	FetchMetadata(ctx context.Context, mbid, name string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error)
	FetchFieldFromProviders(ctx context.Context, mbid, name, field string, providerIDs map[provider.ProviderName]string) ([]provider.FieldProviderResult, error)
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
	SnapshotService    *nfo.SnapshotService
	nfoSettingsService *nfo.NFOSettingsService
	fsCheck            *SharedFSCheck
	expectedWrites     *watcher.ExpectedWrites
}

// NewNFOFixer creates an NFOFixer with an optional shared-filesystem guard.
// The nfoSettings parameter is used to read the current field map for
// platform-specific NFO element mapping; if nil, the default mapping is used.
func NewNFOFixer(snapshotService *nfo.SnapshotService, nfoSettings *nfo.NFOSettingsService, fsCheck *SharedFSCheck, expectedWrites *watcher.ExpectedWrites) *NFOFixer {
	return &NFOFixer{
		SnapshotService:    snapshotService,
		nfoSettingsService: nfoSettings,
		fsCheck:            fsCheck,
		expectedWrites:     expectedWrites,
	}
}

// CanFix returns true for the nfo_exists rule.
func (f *NFOFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleNFOExists
}

// Fix creates an artist.nfo file in the artist's directory.
// If the file already exists and was modified externally, returns without overwriting.
func (f *NFOFixer) Fix(ctx context.Context, a *artist.Artist, _ *Violation) (*FixResult, error) {
	if f.fsCheck.IsShared(ctx, a) {
		return &FixResult{
			RuleID:  RuleNFOExists,
			Fixed:   false,
			Message: "skipped: NFO write disabled for shared-filesystem library; platform may overwrite",
		}, nil
	}

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

	// Read the current NFO field map for platform-specific element mapping.
	fm := nfo.DefaultFieldMap()
	if f.nfoSettingsService != nil {
		loaded, loadErr := f.nfoSettingsService.GetFieldMap(ctx)
		if loadErr != nil {
			slog.Default().Warn("reading NFO field map for fixer, using default",
				slog.String("error", loadErr.Error()))
		} else {
			fm = loaded
		}
	}
	nfoData := nfo.FromArtistWithFieldMap(a, fm)
	nfoData.LockData = true
	var buf bytes.Buffer
	if err := nfo.Write(&buf, nfoData); err != nil {
		return nil, fmt.Errorf("generating nfo: %w", err)
	}

	// Register expected write so the filesystem watcher does not treat
	// this fixer-created NFO as an external modification.
	if f.expectedWrites != nil {
		f.expectedWrites.Add(target)
		defer f.expectedWrites.Remove(target)
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

// MetadataFixer populates missing metadata (MBID, biography, origin) from providers.
type MetadataFixer struct {
	orchestrator metadataOrchestrator
	logger       *slog.Logger
}

// NewMetadataFixer creates a MetadataFixer. NFO write-back and platform push
// are handled by the Pipeline's publisher after all fixes complete, so the
// fixer no longer needs snapshot or expected-writes dependencies.
func NewMetadataFixer(orchestrator *provider.Orchestrator, logger *slog.Logger) *MetadataFixer {
	return &MetadataFixer{orchestrator: orchestrator, logger: logger}
}

// CanFix returns true for nfo_has_mbid, bio_exists, metadata_quality, and
// origin_missing rules.
func (f *MetadataFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleNFOHasMBID || v.RuleID == RuleBioExists ||
		v.RuleID == RuleMetadataQuality || v.RuleID == RuleOriginMissing
}

// Fix searches providers and populates the missing metadata.
func (f *MetadataFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	switch v.RuleID {
	case RuleNFOHasMBID:
		return f.fixMBID(ctx, a)
	case RuleBioExists:
		return f.fixBio(ctx, a)
	case RuleMetadataQuality:
		return f.fixJunkBio(ctx, a)
	case RuleOriginMissing:
		return f.fixOrigin(ctx, a)
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

	return &FixResult{
		RuleID:  RuleNFOHasMBID,
		Fixed:   true,
		Message: fmt.Sprintf("set MBID to %s for %s", best.MusicBrainzID, a.Name),
	}, nil
}

func (f *MetadataFixer) fixBio(ctx context.Context, a *artist.Artist) (*FixResult, error) {
	result, err := f.orchestrator.FetchMetadata(ctx, a.MusicBrainzID, a.Name, a.ProviderIDMap())
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

	return &FixResult{
		RuleID:  RuleBioExists,
		Fixed:   true,
		Message: fmt.Sprintf("populated biography for %s", a.Name),
	}, nil
}

// fixJunkBio clears a junk biography and re-fetches from providers.
// The orchestrator's IsJunkBiography filter ensures the replacement
// biography is not also junk.
func (f *MetadataFixer) fixJunkBio(ctx context.Context, a *artist.Artist) (*FixResult, error) {
	oldBio := a.Biography
	a.Biography = "" // clear junk so providers are queried fresh

	result, err := f.orchestrator.FetchMetadata(ctx, a.MusicBrainzID, a.Name, a.ProviderIDMap())
	if err != nil {
		a.Biography = oldBio // restore on error
		return nil, fmt.Errorf("fetching metadata: %w", err)
	}

	if result.Metadata == nil || result.Metadata.Biography == "" {
		a.Biography = oldBio // restore if no replacement found
		return &FixResult{
			RuleID:  RuleMetadataQuality,
			Fixed:   false,
			Message: fmt.Sprintf("no quality biography found for %s", a.Name),
		}, nil
	}

	a.Biography = result.Metadata.Biography

	return &FixResult{
		RuleID:  RuleMetadataQuality,
		Fixed:   true,
		Message: fmt.Sprintf("replaced junk biography for %s", a.Name),
	}, nil
}

// fixOrigin populates a missing origin field by querying providers in priority
// order for the "origin" field and applying the first non-empty value.
//
// FetchFieldFromProviders returns one result per configured provider in the
// field's priority order (wikipedia, audiodb, wikidata, musicbrainz), so the
// first result with data is the highest-priority non-empty value. This is the
// same first-non-empty-wins behavior used for other single-value fields.
func (f *MetadataFixer) fixOrigin(ctx context.Context, a *artist.Artist) (*FixResult, error) {
	results, err := f.orchestrator.FetchFieldFromProviders(ctx, a.MusicBrainzID, a.Name, "origin", a.ProviderIDMap())
	if err != nil {
		return nil, fmt.Errorf("fetching origin from providers: %w", err)
	}

	value, source := firstNonEmptyFieldValue(results)
	if value == "" {
		return &FixResult{
			RuleID:  RuleOriginMissing,
			Fixed:   false,
			Message: fmt.Sprintf("no origin found for %s", a.Name),
		}, nil
	}

	a.Origin = value

	return &FixResult{
		RuleID:  RuleOriginMissing,
		Fixed:   true,
		Message: fmt.Sprintf("populated origin '%s' from %s for %s", value, source, a.Name),
	}, nil
}

// firstNonEmptyFieldValue walks per-provider field results in priority order
// and returns the first value with data, along with the provider that supplied
// it. Returns empty strings when no provider returned a usable value.
func firstNonEmptyFieldValue(results []provider.FieldProviderResult) (value, source string) {
	for _, r := range results {
		if r.HasData {
			if trimmed := strings.TrimSpace(r.Value); trimmed != "" {
				return trimmed, string(r.Provider)
			}
		}
	}
	return "", ""
}

// provenanceRecorder records image provenance data (phash, source, file format,
// write timestamp) in the artist_images table after an image is saved to disk.
// Used by the fix pipeline and bulk executor after persisting the artist so
// that the artist_images row exists before UpdateImageProvenance is called.
type provenanceRecorder interface {
	UpdateImageProvenance(ctx context.Context, artistID, imageType string, slotIndex int, phash, source, fileFormat, lastWrittenAt string) error
}

// ImageFixer resolves image-related rule violations by fetching images from
// configured metadata providers.
type ImageFixer struct {
	orchestrator    imageProvider
	platformService *platform.Service
	fsCheck         *SharedFSCheck
	logger          *slog.Logger
	// httpClient is the HTTP client used to download image bytes from
	// provider-supplied URLs. It defaults to an SSRF-safe client backed by
	// httpsafe.SafeTransport, which blocks loopback/private/link-local
	// destinations so a malicious or compromised provider cannot coerce the
	// rule engine into fetching internal-network resources. Tests that exercise
	// the download path against httptest.NewServer (which binds to 127.0.0.1)
	// must override this field with a plain *http.Client after construction.
	httpClient *http.Client
	imageCache sync.Map // keyed by MBID; value: *imageCacheEntry
}

// NewImageFixer creates an ImageFixer.
func NewImageFixer(orchestrator imageProvider, platformService *platform.Service, fsCheck *SharedFSCheck, logger *slog.Logger) *ImageFixer {
	return &ImageFixer{
		orchestrator:    orchestrator,
		platformService: platformService,
		fsCheck:         fsCheck,
		logger:          logger,
		httpClient:      httpsafe.SafeClient(fetchTimeout),
	}
}

// fetchImages returns provider images for the given MBID and provider IDs,
// using a per-instance cache to avoid duplicate provider calls when an artist
// has multiple violations.
func (f *ImageFixer) fetchImages(ctx context.Context, mbid string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error) {
	cacheKey := fmt.Sprintf("%s|audiodb=%s|discogs=%s|deezer=%s|spotify=%s",
		mbid,
		providerIDs[provider.NameAudioDB],
		providerIDs[provider.NameDiscogs],
		providerIDs[provider.NameDeezer],
		providerIDs[provider.NameSpotify],
	)
	if entry, ok := f.imageCache.Load(cacheKey); ok {
		e := entry.(*imageCacheEntry)
		return e.result, e.err
	}
	result, err := f.orchestrator.FetchImages(ctx, mbid, providerIDs)
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

// imageFixContext bundles the per-call state ImageFixer.Fix derives from the
// (artist, violation) pair so the discover / filter / download stages can be
// invoked without re-deriving it. The type is local to the package and never
// crosses the API boundary.
type imageFixContext struct {
	imageType string
	minW      int
	minH      int
	existW    int
	existH    int
}

// imageFilterResult is the output of filterCandidatesByQuality. When the
// resolution gate eliminates every candidate, result is the user-visible
// FixResult to return and ok is false; otherwise candidates holds the
// quality-ordered survivors.
type imageFilterResult struct {
	candidates []provider.ImageResult
	result     *FixResult
	ok         bool
}

// Fix fetches the best available image from providers and saves it. The
// per-stage helpers (validatePreconditions, discoverCandidates,
// filterCandidatesByQuality, downloadAndPersist) own their own edge-case
// FixResults; Fix is a thin orchestrator that dispatches across the manual
// discovery, awaiting-user-selection, and auto-select paths.
func (f *ImageFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	fctx, pre, err := f.validatePreconditions(ctx, a, v)
	if err != nil {
		return nil, err
	}
	if pre != nil {
		return pre, nil
	}

	candidates, discoverResult, err := f.discoverCandidates(ctx, a, v, fctx)
	if err != nil {
		return nil, err
	}
	if discoverResult != nil {
		return discoverResult, nil
	}

	filtered := f.filterCandidatesByQuality(ctx, a, v, fctx, candidates)
	if !filtered.ok {
		return filtered.result, nil
	}

	// Discovery-only mode (manual automation): return all candidates as a list
	// without downloading or saving anything.
	if v.Config.DiscoveryOnly {
		return f.candidateListResult(v, fctx, filtered.candidates,
			fmt.Sprintf("found %d %s candidate(s) for user selection", len(filtered.candidates), fctx.imageType)), nil
	}

	// When multiple candidates exist and SelectBestCandidate is not set,
	// return the list for the user to choose from the Notifications inbox.
	if len(filtered.candidates) > 1 && !v.Config.SelectBestCandidate {
		return f.candidateListResult(v, fctx, filtered.candidates,
			fmt.Sprintf("found %d %s candidates; awaiting user selection", len(filtered.candidates), fctx.imageType)), nil
	}

	return f.downloadAndPersist(ctx, a, v, fctx, filtered.candidates), nil
}

// validatePreconditions guards the (artist, violation) pair against the
// states that make image fetching impossible: no MBID, shared-filesystem
// library, or a rule the fixer cannot map to an image type. Returns a
// populated imageFixContext when the call should proceed; otherwise returns
// a non-nil *FixResult that Fix surfaces verbatim.
func (f *ImageFixer) validatePreconditions(ctx context.Context, a *artist.Artist, v *Violation) (*imageFixContext, *FixResult, error) {
	if a.MusicBrainzID == "" {
		return nil, &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: "no MBID, cannot search image providers",
		}, nil
	}
	// API-only artists carry no on-disk path. Without it
	// readExistingImageDimensions would call os.ReadDir("") and read the
	// current working directory; downloadAndPersist would write to an
	// arbitrary CWD-relative location. Refuse early instead.
	if a.Path == "" {
		return nil, &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: "artist has no local path; image download requires filesystem access",
		}, nil
	}
	if f.fsCheck.IsShared(ctx, a) {
		return nil, &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: "skipped: image download disabled for shared-filesystem library",
		}, nil
	}
	imageType := ruleToImageType(v.RuleID)
	if imageType == "" {
		return nil, nil, fmt.Errorf("no image type for rule %s", v.RuleID)
	}
	existW, existH := readExistingImageDimensions(ctx, a.Path, imageType, f.platformService)
	return &imageFixContext{
		imageType: imageType,
		minW:      v.Config.MinWidth,
		minH:      v.Config.MinHeight,
		existW:    existW,
		existH:    existH,
	}, nil, nil
}

// discoverCandidates calls the image provider and returns the type-matched,
// quality-sorted candidate list. When the provider returns zero candidates
// of the requested type, returns a populated FixResult that Fix surfaces
// verbatim.
func (f *ImageFixer) discoverCandidates(ctx context.Context, a *artist.Artist, v *Violation, fctx *imageFixContext) ([]provider.ImageResult, *FixResult, error) {
	result, err := f.fetchImages(ctx, a.MusicBrainzID, a.ProviderIDMap())
	if err != nil {
		return nil, nil, fmt.Errorf("fetching images: %w", err)
	}
	if result == nil {
		return nil, nil, fmt.Errorf("fetching images: provider returned nil result")
	}
	var candidates []provider.ImageResult
	for _, im := range result.Images {
		if string(im.Type) == fctx.imageType {
			candidates = append(candidates, im)
		}
	}
	if len(candidates) == 0 {
		return nil, &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: fmt.Sprintf("no %s images found from providers", fctx.imageType),
		}, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Likes != candidates[j].Likes {
			return candidates[i].Likes > candidates[j].Likes
		}
		return (candidates[i].Width * candidates[i].Height) > (candidates[j].Width * candidates[j].Height)
	})
	return candidates, nil, nil
}

// filterCandidatesByQuality drops candidates below the configured minimum or
// the existing image's pixel count. Existing dimensions are read for all
// image rules, not only min-res ones, because rules like thumb_square can
// fire on a high-res image and must not replace it with a lower-res
// candidate. When every candidate is eliminated, the returned result
// contains the user-visible FixResult; otherwise ok is true and candidates
// holds the survivors.
func (f *ImageFixer) filterCandidatesByQuality(_ context.Context, _ *artist.Artist, v *Violation, fctx *imageFixContext, candidates []provider.ImageResult) imageFilterResult {
	survivors := filterCandidatesByResolution(candidates, fctx.minW, fctx.minH, fctx.existW, fctx.existH, f.logger)
	if len(survivors) > 0 {
		return imageFilterResult{candidates: survivors, ok: true}
	}
	return imageFilterResult{
		ok: false,
		result: &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: fmt.Sprintf("no %s candidates meet %s", fctx.imageType, resolutionConstraintDesc(fctx.minW, fctx.minH, fctx.existW, fctx.existH)),
		},
	}
}

// resolutionConstraintDesc renders the human-readable description of the
// resolution gate that eliminated every candidate. Pulled out so the
// switch does not inflate filterCandidatesByQuality's complexity.
func resolutionConstraintDesc(minW, minH, existW, existH int) string {
	hasMinConstraint := minW > 0 || minH > 0
	hasExistingConstraint := existW > 0 && existH > 0
	switch {
	case hasMinConstraint && hasExistingConstraint:
		return "minimum and existing image resolution requirements"
	case hasMinConstraint:
		return "minimum resolution requirements"
	case hasExistingConstraint:
		return "existing image resolution requirements"
	default:
		return "resolution requirements"
	}
}

// candidateListResult builds the FixResult used by the manual-discovery and
// awaiting-user-selection paths: a non-fixed result whose Candidates field
// carries the quality-ordered survivors for the UI to display.
func (f *ImageFixer) candidateListResult(v *Violation, fctx *imageFixContext, candidates []provider.ImageResult, message string) *FixResult {
	imageCandidates := make([]ImageCandidate, 0, len(candidates))
	for _, c := range candidates {
		imageCandidates = append(imageCandidates, ImageCandidate{
			URL:       c.URL,
			Width:     c.Width,
			Height:    c.Height,
			Source:    c.Source,
			ImageType: fctx.imageType,
		})
	}
	return &FixResult{
		RuleID:     v.RuleID,
		Fixed:      false,
		Message:    message,
		Candidates: imageCandidates,
	}
}

// downloadAndPersist walks the candidate list in priority order and
// downloads the first one whose post-download dimensions clear the
// resolution gate, then saves it via the shared save pipeline.
// Provenance recording is intentionally NOT performed here: the pipeline
// calls Update() after Fix returns to create the artist_images row, so the
// SavedPath/ImageType fields on the returned FixResult drive the
// downstream UpdateImageProvenance call.
func (f *ImageFixer) downloadAndPersist(ctx context.Context, a *artist.Artist, v *Violation, fctx *imageFixContext, candidates []provider.ImageResult) *FixResult {
	useSymlinks := activeUseSymlinks(ctx, f.platformService)
	var downloadFails, dimGateFails, saveFails int
	for _, c := range candidates {
		data, err := fetchImageURL(ctx, f.httpClient, c.URL)
		if err != nil {
			// Source identifies the provider without leaking the signed URL.
			f.logger.Debug("image download failed", "source", c.Source, "error", err)
			downloadFails++
			continue
		}
		if !passesPostDownloadDimensionGate(data, fctx, f.logger) {
			dimGateFails++
			continue
		}

		saveMeta := &img.ExifMeta{
			Source:  c.Source,
			Fetched: time.Now().UTC(),
			URL:     c.URL,
			Rule:    v.RuleID,
			Mode:    "auto",
		}

		saved, err := SaveImageFromData(ctx, a, fctx.imageType, data, nil, useSymlinks, saveMeta, f.platformService, f.logger)
		if err != nil {
			// Source identifies the provider without leaking the signed URL.
			f.logger.Debug("image save failed", "source", c.Source, "error", err)
			saveFails++
			continue
		}

		savedPath := ""
		if len(saved) > 0 && a.Path != "" {
			savedPath = filepath.Join(a.Path, saved[0])
		}
		return &FixResult{
			RuleID:    v.RuleID,
			Fixed:     true,
			Message:   fmt.Sprintf("saved %s from %s (%v)", fctx.imageType, c.Source, saved),
			SavedPath: savedPath,
			ImageType: fctx.imageType,
		}
	}
	return &FixResult{
		RuleID: v.RuleID,
		Fixed:  false,
		Message: fmt.Sprintf("no suitable image saved from %d candidates: %d download failures, %d below resolution gate, %d save failures",
			len(candidates), downloadFails, dimGateFails, saveFails),
	}
}

// passesPostDownloadDimensionGate re-checks dimensions against the resolution
// gate using the actual decoded image bytes. Providers (FanartTV, Deezer)
// do not report dimensions in their API responses, so candidates arrive
// with Width=0/Height=0 and slip past the pre-download filter. Returns
// true when the candidate is unknown-sized (skip the gate) or clears both
// the minimum and existing constraints. The URL is intentionally omitted
// from the debug-log fields: provider URLs frequently carry signed query
// parameters or short-lived tokens that should not surface in operator
// logs (per CLAUDE.md "Scrub sensitive values from logs").
func passesPostDownloadDimensionGate(data []byte, fctx *imageFixContext, logger *slog.Logger) bool {
	if fctx.minW == 0 && fctx.minH == 0 && (fctx.existW == 0 || fctx.existH == 0) {
		return true
	}
	dw, dh, dimErr := img.GetDimensions(bytes.NewReader(data))
	if dimErr != nil || dw == 0 || dh == 0 {
		return true
	}
	if (fctx.minW > 0 && dw < fctx.minW) || (fctx.minH > 0 && dh < fctx.minH) {
		logger.Debug("skipping candidate below configured minimum (actual)",
			"actual_width", dw, "actual_height", dh,
			"min_width", fctx.minW, "min_height", fctx.minH)
		return false
	}
	if fctx.existW > 0 && fctx.existH > 0 && dw*dh < fctx.existW*fctx.existH {
		logger.Debug("skipping candidate below existing resolution (actual)",
			"actual_width", dw, "actual_height", dh,
			"existing_width", fctx.existW, "existing_height", fctx.existH)
		return false
	}
	return true
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

// recordSavedImageProvenance collects provenance data from a saved image file
// and records it in the database. Errors are logged as warnings; provenance
// recording is supplementary and must not fail the image save operation.
func recordSavedImageProvenance(ctx context.Context, pr provenanceRecorder, artistID, imageType, filePath string, logger *slog.Logger) {
	log := logger.With(
		slog.String("artist_id", artistID),
		slog.String("image_type", imageType),
		slog.String("path", filePath),
	)

	d := img.CollectProvenance(filePath, log)
	if d.IsEmpty() {
		log.Warn("no provenance data collected from saved image, skipping update")
		return
	}
	if err := pr.UpdateImageProvenance(ctx, artistID, imageType, 0, d.PHash, d.Source, d.FileFormat, d.LastWrittenAt); err != nil {
		log.Warn("recording image provenance after save",
			slog.String("error", err.Error()))
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

// SaveImageFromURL downloads an image from rawURL and saves it to the artist's
// directory using platform-aware naming. It handles the full pipeline:
//
//  1. fetchImageURL -- download the image bytes
//  2. img.ConvertFormat -- normalize to JPG/PNG
//  3. existingImageFileNames -- resolve platform-aware filenames
//  4. img.Save -- atomic write to disk
//  5. setImageFlag -- update the artist's in-memory image flag
//
// When naming is non-nil, it overrides the platform-aware resolution (used by
// the apply-candidate API handler which resolves naming at the call site).
// Returns the list of saved filenames on success.
//
// The client parameter must be an SSRF-safe HTTP client (e.g.
// httpsafe.SafeClient) in production paths. The rule engine threads its
// BulkExecutor.httpClient through here, and the apply-candidate API handler
// threads its Router.ssrfClient. Tests that exercise the download path against
// an httptest server can pass a plain *http.Client because SafeTransport
// blocks loopback destinations.
func SaveImageFromURL(ctx context.Context, client *http.Client, a *artist.Artist, imageType, rawURL string, naming []string, useSymlinks bool, meta *img.ExifMeta, platformService *platform.Service, logger *slog.Logger) ([]string, error) {
	data, err := fetchImageURL(ctx, client, rawURL)
	if err != nil {
		return nil, fmt.Errorf("downloading image: %w", err)
	}

	return SaveImageFromData(ctx, a, imageType, data, naming, useSymlinks, meta, platformService, logger)
}

// SaveImageFromData saves already-downloaded image bytes to the artist's
// directory using platform-aware naming. This is the core of the image save
// pipeline, separated from SaveImageFromURL so callers that need to inspect
// the downloaded data (e.g. post-download dimension checks) can do so before
// committing the save.
//
// When naming is non-empty, it overrides the platform-aware resolution.
// Returns the list of saved filenames on success.
func SaveImageFromData(ctx context.Context, a *artist.Artist, imageType string, data []byte, naming []string, useSymlinks bool, meta *img.ExifMeta, platformService *platform.Service, logger *slog.Logger) ([]string, error) {
	converted, _, err := img.ConvertFormat(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("converting image format: %w", err)
	}

	// Use platform-aware naming when no explicit naming is provided
	if len(naming) == 0 {
		naming = existingImageFileNames(ctx, a.Path, imageType, platformService)
	}

	saved, err := img.Save(a.Path, imageType, converted, naming, useSymlinks, meta, logger)
	if err != nil {
		return nil, fmt.Errorf("saving image: %w", err)
	}

	setImageFlag(a, imageType)
	return saved, nil
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		if len(all) > 0 {
			return all[:1]
		}
		return nil
	}
	lowerNames := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			lowerNames[strings.ToLower(e.Name())] = struct{}{}
		}
	}
	var found []string
	for _, name := range all {
		if _, ok := lowerNames[strings.ToLower(name)]; ok {
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
					"source", c.Source, "width", c.Width, "height", c.Height,
					"min_width", minW, "min_height", minH)
				continue
			}
			if existingW > 0 && existingH > 0 && c.Width*c.Height < existingW*existingH {
				logger.Debug("skipping candidate below existing resolution",
					"source", c.Source, "width", c.Width, "height", c.Height,
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
	f, err := os.Open(path) //nolint:gosec // G304: path is from validated library scanner output
	if err != nil {
		return 0, 0, false
	}
	defer f.Close() //nolint:errcheck // Close error not actionable on cleanup
	w, h, err := img.GetDimensions(f)
	if err != nil || w == 0 || h == 0 {
		return 0, 0, false
	}
	return w, h, true
}

// ExtraneousImagesFixer deletes non-canonical image files from artist directories.
type ExtraneousImagesFixer struct {
	platformService *platform.Service
	fsCheck         *SharedFSCheck
	logger          *slog.Logger
}

// NewExtraneousImagesFixer creates an ExtraneousImagesFixer.
func NewExtraneousImagesFixer(platformService *platform.Service, fsCheck *SharedFSCheck, logger *slog.Logger) *ExtraneousImagesFixer {
	return &ExtraneousImagesFixer{
		platformService: platformService,
		fsCheck:         fsCheck,
		logger:          logger,
	}
}

// CanFix returns true for the extraneous_images rule.
func (f *ExtraneousImagesFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleExtraneousImages
}

// Fix deletes all extraneous image files from the artist directory.
//
// When the artist's library has a shared-filesystem status, the expected set is
// expanded to include image filenames from ALL platform profiles, matching the
// same logic used by the checker. This prevents the fixer from deleting files
// that were legitimately written by another connected platform.
func (f *ExtraneousImagesFixer) Fix(ctx context.Context, a *artist.Artist, _ *Violation) (*FixResult, error) {
	if a.Path == "" {
		return &FixResult{
			RuleID:  RuleExtraneousImages,
			Fixed:   false,
			Message: "artist has no path",
		}, nil
	}

	// When shared filesystem is detected, union expected files from all
	// profiles so we do not delete images owned by another platform.
	var expected map[string]bool
	if f.fsCheck.IsShared(ctx, a) {
		if f.platformService == nil {
			return &FixResult{
				RuleID:  RuleExtraneousImages,
				Fixed:   false,
				Message: "skipped: cannot determine safe deletion set for shared-filesystem library without platform service",
			}, nil
		}
		expected = expectedImageFilesAllProfiles(ctx, f.platformService, f.logger, a.Path)
	}
	if expected == nil {
		var profile *platform.Profile
		if f.platformService != nil {
			profile, _ = f.platformService.GetActive(ctx)
		}
		expected = expectedImageFiles(profile, a.Path)
	}

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

// LogoPaddingFixer trims excessive padding from logos, preserving a configurable
// margin around the content. It uses area-based content detection that supports
// both alpha transparency (PNG) and whitespace borders (JPG).
//
// For API-only artists (no local filesystem path), the fixer reads the logo
// bytes from the engine's API image cache (populated by the checker) and
// uploads the trimmed result back via the platform image fetcher.
type LogoPaddingFixer struct {
	platformService *platform.Service
	fsCheck         *SharedFSCheck
	imageFetcher    PlatformImageFetcher
	apiImageLookup  func(artistID, imageType string) ([]byte, bool)
	logger          *slog.Logger
}

// NewLogoPaddingFixer creates a LogoPaddingFixer.
func NewLogoPaddingFixer(platformService *platform.Service, fsCheck *SharedFSCheck, logger *slog.Logger) *LogoPaddingFixer {
	return &LogoPaddingFixer{
		platformService: platformService,
		fsCheck:         fsCheck,
		logger:          logger,
	}
}

// SetImageFetcher attaches a platform image fetcher to the fixer. When set,
// the fixer can trim logos for API-only artists that have no local path.
func (f *LogoPaddingFixer) SetImageFetcher(fetcher PlatformImageFetcher, lookupFn func(artistID, imageType string) ([]byte, bool)) {
	f.imageFetcher = fetcher
	f.apiImageLookup = lookupFn
}

// CanFix returns true for the logo_padding rule.
func (f *LogoPaddingFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleLogoPadding
}

// Fix trims padding from the logo, keeping TrimMargin pixels around the content.
// For filesystem-backed artists, reads from disk and saves back. For API-only
// artists (no local path), fetches from the platform API cache and uploads the
// trimmed result back to the connected platform(s).
func (f *LogoPaddingFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	if f.fsCheck.IsShared(ctx, a) {
		return &FixResult{
			RuleID:  RuleLogoPadding,
			Fixed:   false,
			Message: "skipped: logo padding disabled for shared-filesystem library",
		}, nil
	}

	// API-only path: no local directory, use platform API.
	if a.Path == "" {
		return f.fixViaAPI(ctx, a, v)
	}

	return f.fixViaDisk(ctx, a, v)
}

// fixViaDisk handles the filesystem-based logo trim (original behavior).
//
//nolint:gocognit // Linear filesystem pipeline: case-insensitive logo discovery, trim with margin, dimension-unchanged short-circuit (avoid fix/reappear cycle), provenance preservation across the trimmed bytes, atomic save, then case-mismatched-duplicate cleanup. Each step depends on the previous output (path -> bytes -> trimmed -> dimensions -> save -> dedup) and the linearity is essential to the on-disk semantics.
func (f *LogoPaddingFixer) fixViaDisk(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
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
			RuleID:  RuleLogoPadding,
			Fixed:   false,
			Message: "no logo file found on disk",
		}, nil
	}

	data, err := os.ReadFile(logoPath) //nolint:gosec // G304: path from trusted artist directory
	if err != nil {
		return nil, fmt.Errorf("reading logo: %w", err)
	}

	origW, origH, origErr := img.GetDimensions(bytes.NewReader(data))

	// Determine the trim margin from the violation's config. The pipeline
	// attaches the rule config to the violation, so we read it from there.
	margin := max(v.Config.TrimMargin, 0)

	trimmed, _, err := img.TrimWithMargin(bytes.NewReader(data), margin)
	if err != nil {
		return nil, fmt.Errorf("trimming logo: %w", err)
	}

	newW, newH, newErr := img.GetDimensions(bytes.NewReader(trimmed))

	// If dimensions are unchanged, the trim had no effect (content + margin
	// fills the full image). Report as not fixed to avoid a fix/reappear cycle.
	if origErr == nil && newErr == nil && origW == newW && origH == newH {
		return &FixResult{
			RuleID:  RuleLogoPadding,
			Fixed:   false,
			Message: fmt.Sprintf("logo is already %dx%d after applying margin; no change needed", origW, origH),
		}, nil
	}

	// Preserve existing provenance metadata, updating only the rule field.
	var padMeta *img.ExifMeta
	existingPad, readPadErr := img.ReadProvenance(logoPath)
	if readPadErr != nil {
		f.logger.Debug("could not read existing provenance; creating fresh metadata",
			slog.String("path", logoPath),
			slog.String("error", readPadErr.Error()))
	}
	if readPadErr == nil && existingPad != nil {
		padMeta = existingPad
		padMeta.Rule = RuleLogoPadding
	} else {
		padMeta = &img.ExifMeta{Rule: RuleLogoPadding}
	}
	padMeta.Fetched = time.Now().UTC()
	// Recompute dhash from trimmed data.
	if hash, hashErr := img.PerceptualHash(bytes.NewReader(trimmed)); hashErr == nil {
		padMeta.DHash = img.HashHex(hash)
	}

	naming := []string{filepath.Base(logoPath)}
	useSymlinks := activeUseSymlinks(ctx, f.platformService)
	savedNames, err := img.Save(a.Path, "logo", trimmed, naming, useSymlinks, padMeta, f.logger)
	if err != nil {
		return nil, fmt.Errorf("saving trimmed logo: %w", err)
	}

	// Clean up case-mismatched duplicates.
	if len(savedNames) > 0 {
		oldBase := filepath.Base(logoPath)
		newBase := savedNames[0]
		if strings.EqualFold(oldBase, newBase) && oldBase != newBase {
			newPath := filepath.Join(a.Path, newBase)
			oldInfo, errOld := os.Stat(logoPath)
			newInfo, errNew := os.Stat(newPath) //nolint:gosec // G703: newPath is filepath.Join(a.Path, savedNames[0]); a.Path is the scanner-validated artist directory and newBase comes from the platform image-write result, both trusted
			if errOld == nil && errNew == nil && !os.SameFile(oldInfo, newInfo) {
				if rmErr := os.Remove(logoPath); rmErr != nil {
					f.logger.Warn("failed to remove case-mismatched logo duplicate",
						slog.String("path", logoPath), slog.String("error", rmErr.Error()))
				}
			}
		}
	}

	msg := fmt.Sprintf("trimmed logo padding (margin %dpx)", margin)
	if origErr == nil && newErr == nil {
		msg = fmt.Sprintf("trimmed logo from %dx%d to %dx%d (margin %dpx)", origW, origH, newW, newH, margin)
	}

	return &FixResult{
		RuleID:  RuleLogoPadding,
		Fixed:   true,
		Message: msg,
	}, nil
}

// fixViaAPI handles the API-based logo trim for artists with no local path.
// It reads from the engine's API image cache (populated by the checker), trims
// the padding, and uploads the result to the connected platform(s).
func (f *LogoPaddingFixer) fixViaAPI(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	if f.imageFetcher == nil {
		return &FixResult{
			RuleID:  RuleLogoPadding,
			Fixed:   false,
			Message: "artist has no path and no platform image fetcher configured",
		}, nil
	}

	// Try the API image cache first (populated during checker evaluation),
	// then fall back to a fresh fetch.
	var data []byte
	if f.apiImageLookup != nil {
		data, _ = f.apiImageLookup(a.ID, "logo")
	}
	if len(data) == 0 {
		var fetchErr error
		data, _, fetchErr = f.imageFetcher.FetchArtistImage(ctx, a.ID, "logo")
		if fetchErr != nil {
			return nil, fmt.Errorf("fetching logo from platform API: %w", fetchErr)
		}
	}
	if len(data) == 0 {
		return &FixResult{
			RuleID:  RuleLogoPadding,
			Fixed:   false,
			Message: "no logo data available from platform API",
		}, nil
	}

	origW, origH, origErr := img.GetDimensions(bytes.NewReader(data))

	margin := max(v.Config.TrimMargin, 0)

	trimmed, contentType, trimErr := img.TrimWithMargin(bytes.NewReader(data), margin)
	if trimErr != nil {
		return nil, fmt.Errorf("trimming logo: %w", trimErr)
	}

	newW, newH, newErr := img.GetDimensions(bytes.NewReader(trimmed))

	if origErr == nil && newErr == nil && origW == newW && origH == newH {
		return &FixResult{
			RuleID:  RuleLogoPadding,
			Fixed:   false,
			Message: fmt.Sprintf("logo is already %dx%d after applying margin; no change needed", origW, origH),
		}, nil
	}

	// Map the detected format to a MIME content type for the upload.
	mimeType := "image/png"
	if contentType == "jpeg" || contentType == "jpg" {
		mimeType = "image/jpeg"
	}

	if uploadErr := f.imageFetcher.UploadArtistImage(ctx, a.ID, "logo", trimmed, mimeType); uploadErr != nil {
		return nil, fmt.Errorf("uploading trimmed logo to platform: %w", uploadErr)
	}

	msg := fmt.Sprintf("trimmed logo padding via API (margin %dpx)", margin)
	if origErr == nil && newErr == nil {
		msg = fmt.Sprintf("trimmed logo from %dx%d to %dx%d via API (margin %dpx)", origW, origH, newW, newH, margin)
	}

	return &FixResult{
		RuleID:  RuleLogoPadding,
		Fixed:   true,
		Message: msg,
	}, nil
}

// fetchImageURL downloads image data from a URL with timeout and size limits.
//
// The client parameter is required and must be an SSRF-safe HTTP client (e.g.
// httpsafe.SafeClient) for production callers; the function does not construct
// its own client so that all outbound rule-engine fetches share the same
// SSRF-protected transport. Tests may inject a plain *http.Client when
// targeting httptest servers on the loopback interface.
func fetchImageURL(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

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

// DirectoryRenameFixer renames an artist's directory to match the canonical name.
// When the artist's library has a shared-filesystem status, the fixer declines to
// auto-fix and returns a warning message instead, because renaming a directory
// that a platform connection references can break the platform's metadata index.
type DirectoryRenameFixer struct {
	fsCheck *SharedFSCheck
	logger  *slog.Logger
}

// NewDirectoryRenameFixer creates a DirectoryRenameFixer.
func NewDirectoryRenameFixer(fsCheck *SharedFSCheck, logger *slog.Logger) *DirectoryRenameFixer {
	return &DirectoryRenameFixer{
		fsCheck: fsCheck,
		logger:  logger.With(slog.String("component", "directory-rename-fixer")),
	}
}

// CanFix returns true for the directory_name_mismatch rule.
func (f *DirectoryRenameFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleDirectoryNameMismatch
}

// Fix renames the artist directory to the canonical name derived from the
// artist's name and the rule's article mode configuration. When the artist's
// library has a shared-filesystem status, the fix is skipped to avoid breaking
// platform metadata indexes that reference the current directory path.
func (f *DirectoryRenameFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	if a.Path == "" {
		return &FixResult{RuleID: v.RuleID, Fixed: false, Message: "artist has no path"}, nil
	}

	// Decline to auto-fix when a platform connection shares the filesystem.
	if f.fsCheck.IsShared(ctx, a) {
		return &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: "skipped: directory rename disabled for shared-filesystem library",
		}, nil
	}

	canonical := canonicalDirName(a.Name, v.Config.ArticleMode)
	if canonical == "" {
		return &FixResult{RuleID: v.RuleID, Fixed: false, Message: "canonical name is empty or unsafe"}, nil
	}

	// Short-circuit when the existing directory name is Unicode-equivalent
	// (NFC vs NFD) to the canonical form. macOS filesystems store paths in
	// decomposed form, so an incoming NFC name and on-disk NFD name can
	// mismatch byte-for-byte while being the same text. In that case any
	// rename would be a no-op and must report Fixed to clear the violation.
	dirName := filepath.Base(a.Path)
	if norm.NFC.String(dirName) == norm.NFC.String(canonical) {
		return &FixResult{
			RuleID:  v.RuleID,
			Fixed:   true,
			Message: "directory name is Unicode-equivalent; no rename needed",
		}, nil
	}

	parentDir := filepath.Dir(a.Path)
	newPath := filepath.Join(parentDir, canonical)

	if a.Path == newPath {
		return &FixResult{RuleID: v.RuleID, Fixed: false, Message: "paths already match"}, nil
	}

	// Probe the canonical destination. ENOENT means the path is free; any
	// other error is a real filesystem failure surfaced to the caller. When
	// the canonical target already exists, fall back to a sort-name-derived
	// secondary target before refusing -- this matches the disambiguation
	// many users encode into Artist.SortName (e.g. "Carter Family, The
	// (later generations of the family after 1943)") so the auto-fixer can
	// still rename without forcing a manual collision-resolution step.
	canonicalFree, err := pathIsFree(newPath)
	if err != nil {
		return nil, fmt.Errorf("checking destination %q: %w", newPath, err)
	}

	target := newPath
	chosen := canonical
	usedFallback := false
	if !canonicalFree {
		fallback := canonicalDirName(a.SortName, v.Config.ArticleMode)
		if fallback == "" || fallback == canonical {
			// No usable sort-name-derived alternative: refuse as before.
			return &FixResult{
				RuleID:  v.RuleID,
				Fixed:   false,
				Message: fmt.Sprintf("destination '%s' already exists", canonical),
			}, nil
		}
		fallbackPath := filepath.Join(parentDir, fallback)
		// Idempotency: if a prior run already renamed a.Path to fallbackPath,
		// pathIsFree would return false (the current directory occupies the
		// target) and bounce the artist back into a "destination collides"
		// state on every rescan. Treat "fallback target equals current path"
		// as already-fixed.
		if fallbackPath == a.Path {
			return &FixResult{
				RuleID:  v.RuleID,
				Fixed:   true,
				Message: fmt.Sprintf("directory already uses sort-name fallback '%s' (canonical name collided)", fallback),
			}, nil
		}
		fallbackFree, err := pathIsFree(fallbackPath)
		if err != nil {
			return nil, fmt.Errorf("checking fallback destination %q: %w", fallbackPath, err)
		}
		if !fallbackFree {
			// Both canonical and sort-name targets collide; refuse.
			return &FixResult{
				RuleID: v.RuleID,
				Fixed:  false,
				Message: fmt.Sprintf(
					"destination '%s' already exists and sort-name fallback '%s' also collides",
					canonical, fallback,
				),
			}, nil
		}
		target = fallbackPath
		chosen = fallback
		usedFallback = true
	}

	// Does NOT invoke PlatformRenameSyncer; tracked separately by #1221.
	if err := filesystem.RenameDirAtomic(a.Path, target); err != nil {
		return nil, fmt.Errorf("renaming %q to %q: %w", a.Path, target, err)
	}

	f.logger.Info("renamed artist directory",
		"artist", a.Name,
		"old_path", a.Path,
		"new_path", target,
		"used_sort_name_fallback", usedFallback)

	a.Path = target

	msg := fmt.Sprintf("renamed directory to canonical name '%s'", chosen)
	if usedFallback {
		msg = fmt.Sprintf("renamed directory to sort-name fallback '%s' (canonical name collided)", chosen)
	}
	return &FixResult{
		RuleID:  v.RuleID,
		Fixed:   true,
		Message: msg,
	}, nil
}

// pathIsFree reports whether the given path is available for use as a rename
// target. Returns true when the path does not exist (ENOENT). Any other stat
// error is surfaced so the caller can refuse rather than guess.
//
// Uses Lstat so a dangling symlink at the target counts as occupied; Stat
// would follow the broken link and report ENOENT, classifying the path
// as free even though it is occupied, and the subsequent rename then
// fails mid-flight instead of being rejected upfront.
func pathIsFree(p string) (bool, error) {
	_, err := os.Lstat(p)
	if err == nil {
		return false, nil
	}
	if os.IsNotExist(err) {
		return true, nil
	}
	return false, err
}

// BackdropSequencingFixer renames fanart files to fill gaps and create a
// contiguous sequence. Uses image.RenumberFanart for safe two-phase rename.
type BackdropSequencingFixer struct {
	platformService *platform.Service
	fsCheck         *SharedFSCheck
	logger          *slog.Logger
}

// NewBackdropSequencingFixer creates a BackdropSequencingFixer.
func NewBackdropSequencingFixer(platformService *platform.Service, fsCheck *SharedFSCheck, logger *slog.Logger) *BackdropSequencingFixer {
	return &BackdropSequencingFixer{
		platformService: platformService,
		fsCheck:         fsCheck,
		logger:          logger.With(slog.String("component", "backdrop-sequencing-fixer")),
	}
}

// CanFix returns true for the backdrop_sequencing rule.
func (f *BackdropSequencingFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleBackdropSequencing
}

// Fix renumbers fanart files to occupy contiguous indices.
func (f *BackdropSequencingFixer) Fix(ctx context.Context, a *artist.Artist, _ *Violation) (*FixResult, error) {
	if f.fsCheck.IsShared(ctx, a) {
		return &FixResult{
			RuleID:  RuleBackdropSequencing,
			Fixed:   false,
			Message: "skipped: backdrop renumbering disabled for shared-filesystem library",
		}, nil
	}

	if a.Path == "" {
		return &FixResult{RuleID: RuleBackdropSequencing, Fixed: false, Message: "artist has no path"}, nil
	}

	var profile *platform.Profile
	if f.platformService != nil {
		var profErr error
		profile, profErr = f.platformService.GetActive(ctx)
		if profErr != nil {
			// Abort rather than falling back to default naming convention.
			// Renaming files with the wrong convention (e.g., non-Kodi on a
			// Kodi library) is destructive and not safely reversible.
			return nil, fmt.Errorf("loading active platform profile: %w", profErr)
		}
	}

	var fanartNames []string
	if profile != nil {
		fanartNames = profile.ImageNaming.NamesForType("fanart")
	}
	if len(fanartNames) == 0 {
		fanartNames = img.FileNamesForType(img.DefaultFileNames, "fanart")
	}
	kodiNumbering := profile != nil && strings.EqualFold(profile.ID, "kodi")

	for _, primaryName := range fanartNames {
		discovered, err := img.DiscoverFanart(a.Path, primaryName)
		if err != nil {
			f.logger.Warn("discovering fanart for sequencing fix",
				"artist", a.Name, "primary", primaryName, "error", err)
			continue
		}
		if len(discovered) == 0 {
			continue
		}

		// Check if already contiguous before renumbering.
		needsRenumber := false
		for i, path := range discovered {
			expected := img.FanartFilename(primaryName, i, kodiNumbering)
			expectedBase := strings.TrimSuffix(expected, filepath.Ext(expected))
			actualBase := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			if !strings.EqualFold(expectedBase, actualBase) {
				needsRenumber = true
				break
			}
		}
		if !needsRenumber {
			continue
		}

		if err := img.RenumberFanart(a.Path, primaryName, discovered, kodiNumbering); err != nil {
			return nil, fmt.Errorf("renumbering fanart for %s: %w", a.Name, err)
		}

		f.logger.Info("renumbered image sequence",
			"artist", a.Name,
			"primary", primaryName,
			"count", len(discovered))

		return &FixResult{
			RuleID:  RuleBackdropSequencing,
			Fixed:   true,
			Message: fmt.Sprintf("renumbered %d %s files for %s", len(discovered), strings.TrimSuffix(primaryName, filepath.Ext(primaryName)), a.Name),
		}, nil
	}

	return &FixResult{
		RuleID:  RuleBackdropSequencing,
		Fixed:   false,
		Message: "no fanart files needing renumbering",
	}, nil
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
