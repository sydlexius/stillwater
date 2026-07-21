package rule

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

const (
	fetchTimeout  = 30 * time.Second
	maxImageBytes = 25 << 20 // 25 MB
)

// coalescedSearch routes Search through the per-evaluation context when
// one is attached to ctx (the canonical pipeline path), or falls back to
// the raw orchestrator. The fallback path matters for callers like
// FixViolation that operate on a single violation outside an evaluation
// pass; they still benefit from the unified telemetry tag the helper
// applies but do not get coalescing for free.
func coalescedSearch(ctx context.Context, orch metadataOrchestrator, name string) ([]provider.ArtistSearchResult, error) {
	if ec := EvaluationContextFromContext(ctx); ec != nil {
		return ec.Search(ctx, name)
	}
	return orch.Search(ctx, name)
}

// coalescedFetchMetadata routes FetchMetadata through the per-evaluation
// context when one is attached to ctx, falling back to the raw
// orchestrator otherwise.
func coalescedFetchMetadata(ctx context.Context, orch metadataOrchestrator, mbid, name string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error) {
	if ec := EvaluationContextFromContext(ctx); ec != nil {
		return ec.FetchMetadata(ctx, mbid, name, providerIDs)
	}
	return orch.FetchMetadata(ctx, mbid, name, providerIDs)
}

// coalescedFetchField routes FetchFieldFromProviders through the
// per-evaluation context when one is attached to ctx, falling back to the
// raw orchestrator otherwise.
func coalescedFetchField(ctx context.Context, orch metadataOrchestrator, mbid, name, field string, providerIDs map[provider.ProviderName]string) ([]provider.FieldProviderResult, error) {
	if ec := EvaluationContextFromContext(ctx); ec != nil {
		return ec.FetchFieldFromProviders(ctx, mbid, name, field, providerIDs)
	}
	return orch.FetchFieldFromProviders(ctx, mbid, name, field, providerIDs)
}

// coalescedFetchImages routes FetchImages through the per-evaluation
// context when one is attached to ctx, falling back to the raw image
// orchestrator (or its test stub) otherwise.
func coalescedFetchImages(ctx context.Context, orch imageProvider, mbid string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error) {
	if ec := EvaluationContextFromContext(ctx); ec != nil {
		return ec.FetchImages(ctx, mbid, providerIDs)
	}
	return orch.FetchImages(ctx, mbid, providerIDs)
}

// LockNFOResolver returns whether the artist's NFO should carry
// <lockdata>true</lockdata>. The canonical implementation lives in
// publish.Publisher.ResolveLockNFO and returns
// artist.Locked || library.NFOLockData (issue #1726). Injected as a narrow
// interface so the rule package does not depend on the publish package.
type LockNFOResolver interface {
	ResolveLockNFO(ctx context.Context, a *artist.Artist) bool
}

// activeProfileProvider resolves the active platform profile. *platform.Service
// satisfies it in production; tests inject a stub. Used to gate NFO creation on
// the active profile's NFOEnabled flag (#2306: Plex does not use .nfo files).
type activeProfileProvider interface {
	GetActive(ctx context.Context) (*platform.Profile, error)
}

// NFOFixer creates missing artist.nfo files from the artist's current metadata.
type NFOFixer struct {
	SnapshotService    *nfo.SnapshotService
	nfoSettingsService *nfo.NFOSettingsService
	fsCheck            *SharedFSCheck
	expectedWrites     *watcher.ExpectedWrites
	lockResolver       LockNFOResolver
	platformService    activeProfileProvider
}

// NewNFOFixer creates an NFOFixer with an optional shared-filesystem guard.
// The nfoSettings parameter is used to read the current field map for
// platform-specific NFO element mapping; if nil, the default mapping is used.
// lockResolver supplies the OR-of-knobs (artist.Locked || library.NFOLockData)
// per issue #1726; when nil the fixer falls back to artist.Locked only.
func NewNFOFixer(snapshotService *nfo.SnapshotService, nfoSettings *nfo.NFOSettingsService, fsCheck *SharedFSCheck, expectedWrites *watcher.ExpectedWrites, lockResolver LockNFOResolver, platformService activeProfileProvider) *NFOFixer {
	return &NFOFixer{
		SnapshotService:    snapshotService,
		nfoSettingsService: nfoSettings,
		fsCheck:            fsCheck,
		expectedWrites:     expectedWrites,
		lockResolver:       lockResolver,
		platformService:    platformService,
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

	// #2306: honor the active platform profile. Plex (nfo_enabled=0) does not use
	// .nfo files, so do not create one. Fail-open (write) when the profile can't
	// be resolved -- see platform.NFOWriteAllowed. Nil service also fails open so
	// existing callers/tests that omit it keep writing.
	if f.platformService != nil {
		prof, profErr := f.platformService.GetActive(ctx)
		if !platform.NFOWriteAllowed(prof, profErr) {
			// prof is non-nil here (NFOWriteAllowed fails open on a nil profile or
			// a lookup error), but guard defensively so the message never depends
			// on that non-local invariant.
			profileName := "unknown"
			if prof != nil {
				profileName = prof.Name
			}
			return &FixResult{
				RuleID:  RuleNFOExists,
				Fixed:   false,
				Message: fmt.Sprintf("skipped: NFO writing is disabled for the active platform profile %q", profileName),
			}, nil
		}
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
	// Stamp lockdata only when either the per-artist lock or the per-library
	// NFOLockData setting is on (issue #1726). Without this OR, the fixer
	// unconditionally stamped lockdata=true, which the scanner then
	// re-imported as artists.locked=true on every rescan, silently undoing
	// any user unlock.
	if f.lockResolver != nil {
		nfoData.LockData = f.lockResolver.ResolveLockNFO(ctx, a)
	} else {
		nfoData.LockData = a.Locked
	}
	// Stamp provenance so an external overwrite can be detected on read,
	// matching the write-back and discography paths (#2306).
	nfoData.Stillwater = &nfo.StillwaterMeta{
		Version: nfo.StillwaterVersion,
		Written: time.Now().UTC().Format(time.RFC3339),
	}
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
	results, err := coalescedSearch(ctx, f.orchestrator, a.Name)
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
	result, err := coalescedFetchMetadata(ctx, f.orchestrator, a.MusicBrainzID, a.Name, a.ProviderIDMap())
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

	result, err := coalescedFetchMetadata(ctx, f.orchestrator, a.MusicBrainzID, a.Name, a.ProviderIDMap())
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
	results, err := coalescedFetchField(ctx, f.orchestrator, a.MusicBrainzID, a.Name, "origin", a.ProviderIDMap())
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

// provenanceRecorder records image provenance data (phash, content hash,
// source, file format, write timestamp) in the artist_images table after an
// image is saved to disk. Used by the fix pipeline and bulk executor after
// persisting the artist so that the artist_images row exists before
// UpdateImageProvenance is called.
type provenanceRecorder interface {
	UpdateImageProvenance(ctx context.Context, artistID, imageType string, slotIndex int, phash, contentHash, source, fileFormat, lastWrittenAt string) error
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
	// collisionGuard is the #2540 cross-artist backdrop check for
	// downloadAndPersist. It is LATE-WIRED via SetCollisionGuard rather than
	// taken as a constructor argument because main.go builds the ImageFixer as
	// part of the fixer slice that the pipeline is built from, while the
	// collision.Notifier is constructed AFTER the pipeline (it needs the
	// pipeline-adjacent rule.Service). Nil here is a fully supported state: a
	// nil *collisionGuard is a no-op at every method, so every existing
	// construction (tests, headless) keeps its previous behavior.
	collision *collisionGuard
}

// SetCollisionGuard late-wires the #2540 cross-artist backdrop-collision seam.
// See the collision field comment for why this is a setter and not a
// constructor parameter. Passing a nil notifier or indexer disables the check.
func (f *ImageFixer) SetCollisionGuard(notifier backdropCollisionNotifier, indexer fanartIdentityIndexer) {
	f.collision = newCollisionGuard(notifier, indexer, f.logger)
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

// fetchImages returns provider images for the given MBID and provider IDs.
//
// Routes through the per-evaluation EvaluationContext when one is attached
// to ctx (the canonical pipeline path) so multiple image rules on the same
// artist coalesce into a single provider call -- the load-bearing
// behavior issue #1133 introduces. Without an EvaluationContext (e.g.
// direct unit-test invocation that does not seed one) it falls through to
// the raw orchestrator without caching. The historical per-fixer
// sync.Map cache was unbounded across passes (no eviction) and is
// superseded by the per-evaluation cache, which is bounded by the
// pass lifetime.
func (f *ImageFixer) fetchImages(ctx context.Context, mbid string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error) {
	return coalescedFetchImages(ctx, f.orchestrator, mbid, providerIDs)
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

	// #2540 (#2565): build the cross-artist fanart registry ONCE for this fix,
	// outside the candidate loop -- it is a whole-library read, so rebuilding it
	// per candidate would re-scan the library on every download attempt. This is
	// the "once per scope" contract; for a single-artist rule fix the scope is
	// this call. Nil when the guard is unwired or the type is not fanart, and
	// nil on a build failure (fail-open).
	var identityIdx []img.FanartIdentityEntry
	if f.collision.active(fctx.imageType) {
		identityIdx = f.collision.buildIndex(ctx)
	}

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

		// #2540 NOTIFY-ONLY: decide the collision verdict HERE, on the CONVERTED
		// bytes (what SaveImageFromData will actually put on disk -- WebP becomes
		// PNG), but HOLD it. The notification is emitted only after the save below
		// is confirmed, because its durable half is a fixable Action Queue entry
		// whose auto-fix backs artwork OUT of the artist: raising it for a save
		// that then failed would point a destructive remediation at a file that
		// was never created. A conversion failure here is not fatal to the fix --
		// SaveImageFromData will surface the same error a moment later -- so it
		// just skips the check, fail-open.
		//
		// The conversion is hoisted out of the guard's own branch and its result is
		// handed to the save below, so the bytes are converted ONCE rather than
		// here and again inside SaveImageFromData. ConvertFormat is idempotent (it
		// emits JPEG or PNG and passes both through untouched), so what lands on
		// disk is byte-for-byte identical -- a WebP source just stops being decoded
		// and re-encoded twice per candidate. On a conversion failure saveData
		// stays as the raw bytes, so SaveImageFromData still surfaces the same
		// error it always did.
		var collisionResult *img.IdentityResult
		saveData := data
		if converted, _, convErr := img.ConvertFormat(bytes.NewReader(data)); convErr == nil {
			saveData = converted
			if len(identityIdx) > 0 {
				collisionResult = f.collision.verdict(a.ID, converted, identityIdx)
			}
		} else {
			f.logger.Debug("converting image failed; skipping the collision check",
				"source", c.Source, "error", convErr)
		}

		saveMeta := &img.ExifMeta{
			Source:  c.Source,
			Fetched: time.Now().UTC(),
			URL:     c.URL,
			Rule:    v.RuleID,
			Mode:    "auto",
		}

		saved, err := SaveImageFromData(ctx, a, fctx.imageType, saveData, nil, useSymlinks, saveMeta, f.platformService, f.logger)
		if err != nil {
			// Source identifies the provider without leaking the signed URL.
			f.logger.Debug("image save failed", "source", c.Source, "error", err)
			saveFails++
			continue
		}

		// The save returned no error, so the image the verdict was computed on
		// genuinely exists on disk now. Only here is it correct to notify. The fix
		// itself is never blocked -- this runs after the write, not instead of it.
		f.collision.notify(ctx, a.ID, a.Name, collisionResult)

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
//
// It records against slot 0, and every caller of it writes the primary. That
// invariant is asserted by recordSavedImageProvenanceSlot0 below rather than
// assumed here; see its comment for why 0 is right and what would make it wrong.
func recordSavedImageProvenance(ctx context.Context, pr provenanceRecorder, artistID, imageType, filePath string, logger *slog.Logger) {
	recordSavedImageProvenanceSlot0(ctx, pr, artistID, imageType, filePath, logger)
}

// recordSavedImageProvenanceSlot0 records provenance for a file known to occupy
// slot 0, mirroring Router.recordImageProvenanceSlot0 in internal/api.
//
// #2564's acceptance criteria name this package's call site alongside the API's,
// because both passed a hard-coded 0. The API's was a genuine DEFECT: it had
// acquired callers -- a fanart append, the per-slot Crop/Fetch edit (#2281) --
// that write a NON-primary slot, so the literal aimed their UPDATE at another
// slot's row. It now takes a slotIndex parameter (#2574).
//
// This package's 0 is CORRECT, and unlike the API's it is correct for a
// structural reason rather than by luck. Both callers -- Pipeline.FixViolation
// via FixResult.SavedPath, and BulkExecutor via saveBestImage -- reach the disk
// through SaveImageFromData, which resolves filenames via existingImageFileNames.
// That returns names from ImageNaming.NamesForType / img.FileNamesForType, which
// are format ALIASES for the primary (fanart.jpg, backdrop.jpg) and never
// numbered variants; when none exist it falls back to all[:1], "create only the
// primary filename". So no path from this package can write fanart2.jpg or
// above, and DiscoverFanart sorts the primary to ordinal 0 by exact-base match.
// The written file is slot 0 in every reachable case.
//
// It is kept as a distinct named function rather than a bare literal because
// that argument is load-bearing and lives two files away: the moment a caller in
// this package learns to write a numbered slot -- a rule that appends a backdrop
// rather than replacing the primary -- the literal silently becomes the API's
// bug, stamping the appended file's phash onto slot 0's row and feeding a
// per-slot phash reader a hash belonging to a different picture. The name is
// where that tripwire lives. A caller that writes a non-primary slot must call a
// slot-aware recorder, not this.
func recordSavedImageProvenanceSlot0(ctx context.Context, pr provenanceRecorder, artistID, imageType, filePath string, logger *slog.Logger) {
	const primarySlotIndex = 0
	log := logger.With(
		slog.String("artist_id", artistID),
		slog.String("image_type", imageType),
		slog.String("path", filePath),
		slog.Int("slot_index", primarySlotIndex),
	)

	d := img.CollectProvenance(filePath, log)
	if d.IsEmpty() {
		log.Warn("no provenance data collected from saved image, skipping update")
		return
	}
	if err := pr.UpdateImageProvenance(ctx, artistID, imageType, primarySlotIndex, d.PHash, d.ContentHash, d.Source, d.FileFormat, d.LastWrittenAt); err != nil {
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

// FetchImageURL is the exported form of fetchImageURL, for callers that must
// hold the downloaded bytes before the save rather than let SaveImageFromURL do
// both in one step.
//
// It exists for the #2540 collision seam (#2626). The API's apply-candidate
// handler has to compute a cross-artist collision verdict over the CONVERTED
// bytes that will land on disk, which means it needs the download and the
// conversion as separate steps it can interpose on. Routing it through this
// wrapper rather than a Router-side downloader keeps that endpoint's fetch
// semantics -- SSRF-safe client, size limit, status handling -- byte-for-byte
// identical to what SaveImageFromURL did before.
func FetchImageURL(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	return fetchImageURL(ctx, client, rawURL)
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
//
// Fanart writes from here are DESTRUCTIVE and unattended. fanart_min_res and
// fanart_aspect fire precisely BECAUSE a fanart already exists (it is too small or the
// wrong shape), and the fix downloads a replacement -- so this function, reached from
// the image fixer, from BulkExecutor.saveBestImage (bulk Mode "auto") and from the
// apply-candidate API handler, overwrites the user's primary backdrop library-wide with
// no confirmation. It routes through saveImageToDisk for that reason (#2433).
func SaveImageFromData(ctx context.Context, a *artist.Artist, imageType string, data []byte, naming []string, useSymlinks bool, meta *img.ExifMeta, platformService *platform.Service, logger *slog.Logger) ([]string, error) {
	converted, _, err := img.ConvertFormat(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("converting image format: %w", err)
	}

	// Use platform-aware naming when no explicit naming is provided
	if len(naming) == 0 {
		naming = existingImageFileNames(ctx, a.Path, imageType, platformService)
	}

	saved, err := saveImageToDisk(a.Path, imageType, converted, naming, useSymlinks, meta, logger)
	if err != nil {
		return nil, fmt.Errorf("saving image: %w", err)
	}

	setImageFlag(a, imageType)
	return saved, nil
}

// saveImageToDisk is the rule engine's SINGLE image-write sink, and the only place in
// internal/rule allowed to reach img.Save (see TestFanartSaveHasASingleChokepoint in
// internal/api, which fails the build on any other direct img.Save in this package).
//
// FANART routes through img.SaveSlotProtected: back up the existing image, write, and
// put the original back if the write fails. Every rule-driven fanart write lands here,
// and every one of them is destructive -- ruleToImageType maps fanart_exists,
// fanart_min_res and fanart_aspect to "fanart", and the latter two only fire when a
// fanart is ALREADY on disk. Before #2433 this called img.Save directly: Save's
// CleanupConflictingFormats DELETES the slot's other-format file before writing, so a
// failed replacement left the user with no backdrop and nothing to restore -- running
// unattended, across the whole library, in bulk auto-fix mode.
//
// EVERY OTHER TYPE (thumb/logo/banner) still goes to a bare img.Save with NO backup and
// NO rollback. That is a REAL residual gap of the same bug class, deliberately left
// outside this change's scope rather than papered over: the single-slot types have their
// own backup mechanism (img.BackupSingleSlot, one-deep PER TYPE) whose prune semantics
// differ from the slot-scoped one used here, and mixing the two in one .sw-backup/<type>/
// directory would corrupt the Router's revert feature. Closing it means lifting the
// Router's single-slot backup+rollback policy into internal/image as a second shared
// chokepoint, which is its own change.
func saveImageToDisk(dir, imageType string, data []byte, naming []string, useSymlinks bool, meta *img.ExifMeta, logger *slog.Logger) ([]string, error) {
	if imageType == "fanart" {
		return img.SaveSlotProtected(dir, imageType, naming, data, useSymlinks, meta, logger)
	}
	return img.Save(dir, imageType, data, naming, useSymlinks, meta, logger)
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
	// Through the shared sink, not img.Save directly, so saveImageToDisk stays the ONE
	// place in this package that reaches the image-write primitives. A logo is
	// single-slot, so this still lands on a bare Save today (see saveImageToDisk); the
	// point is that a fanart write can never be added here without the guard seeing it.
	savedNames, err := saveImageToDisk(a.Path, "logo", trimmed, naming, useSymlinks, padMeta, f.logger)
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

// DirectoryRenamer is the GUARDED rename port: the one road a directory rename
// is allowed to take. It is satisfied by *artist.Service, whose RenameDirectory
// holds renameMu across the Lstat -> RenameDirAtomic -> UpdatePath sequence,
// rolls back on a DB failure, AND invokes the PlatformRenameSyncer so every
// connected peer (Emby / Jellyfin / Lidarr) is re-pointed at the new path -
// which is where publish.guardPlatformPath refuses an out-of-root push.
//
// It exists as an interface here, rather than the fixer holding *artist.Service
// directly, purely for the injection seam (tests substitute a spy). internal/rule
// already imports internal/artist, and internal/artist imports nothing from
// internal/rule, so there is no cycle either way.
//
// #1221 / #2380: before this port existed the fixer called
// filesystem.RenameDirAtomic itself and told no peer. That was a SECOND,
// UNGUARDED road to a rename, and it reproduced the exact #2380 duplicate-artist
// symptom - peers kept the old path, a peer's NFO saver re-created the directory
// that was just renamed away, and the next scan re-imported it as a duplicate.
// Do not reintroduce a direct filesystem rename here.
type DirectoryRenamer interface {
	RenameDirectory(ctx context.Context, artistID, newDirName string) (newPath string, platforms []artist.PlatformRemapResult, err error)
}

// DirectoryRenameFixer renames an artist's directory to match the canonical name.
// When the artist's library has a shared-filesystem status, the fixer declines to
// auto-fix and returns a warning message instead, because renaming a directory
// that a platform connection references can break the platform's metadata index.
type DirectoryRenameFixer struct {
	fsCheck *SharedFSCheck
	renamer DirectoryRenamer
	logger  *slog.Logger
}

// NewDirectoryRenameFixer creates a DirectoryRenameFixer. renamer is REQUIRED:
// with a nil renamer the fixer refuses every rename (loudly - see Fix) rather
// than falling back to an unguarded direct rename, because a rename that cannot
// notify the peers is the bug, not a degraded mode.
func NewDirectoryRenameFixer(fsCheck *SharedFSCheck, renamer DirectoryRenamer, logger *slog.Logger) *DirectoryRenameFixer {
	return &DirectoryRenameFixer{
		fsCheck: fsCheck,
		renamer: renamer,
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
		chosen = fallback
		usedFallback = true
	}

	// BOTH the canonical branch and the sort-name FALLBACK branch land here, and
	// both go through the guarded port (#1221 / #2380). `chosen` is the leaf name
	// either branch settled on; `target` is only used for the collision probes
	// above - the renamer recomputes the path from the artist's persisted row.
	oldPath := a.Path
	newPath, platforms, err := f.renameGuarded(ctx, a, chosen)
	if err != nil {
		// Refusals the renamer expresses as sentinel errors are outcomes, not
		// failures: report them as an unfixed violation with the reason.
		if res, ok := f.declineFor(err, v, chosen); ok {
			return res, nil
		}
		return nil, err
	}

	// The renamer persisted the new path; mirror it onto the caller's in-memory
	// artist exactly once. Assigning the RETURNED path (not the locally computed
	// target) keeps this from drifting if the renamer ever normalizes differently.
	a.Path = newPath

	failed := 0
	for _, p := range platforms {
		if p.Result == artist.PlatformRemapFailed {
			failed++
		}
	}

	f.logger.Info("renamed artist directory",
		"artist", a.Name,
		"old_path", oldPath,
		"new_path", newPath,
		"used_sort_name_fallback", usedFallback,
		"platforms_synced", len(platforms)-failed,
		"platforms_failed", failed)

	msg := fmt.Sprintf("renamed directory to canonical name '%s'", chosen)
	if usedFallback {
		msg = fmt.Sprintf("renamed directory to sort-name fallback '%s' (canonical name collided)", chosen)
	}
	// A per-peer push failure does NOT fail the fix - the on-disk + DB rename has
	// already committed and SyncRename is best-effort by contract (see
	// artist.PlatformRenameSyncer). It is surfaced in the message instead of being
	// swallowed, so the operator can see that a peer still points at the old path.
	if failed > 0 {
		msg = fmt.Sprintf("%s; %d platform path push(es) failed - see logs", msg, failed)
	}
	return &FixResult{
		RuleID:  v.RuleID,
		Fixed:   true,
		Message: msg,
	}, nil
}

// renameGuarded performs the rename through the injected DirectoryRenamer, the
// SAME chokepoint the user-driven rename and the merge flow use (and therefore
// the same one publish.guardPlatformPath sits on).
//
// It FAILS LOUDLY rather than silently degrading. A missing renamer or an artist
// with no persisted ID means the peers cannot be notified at all; renaming the
// directory anyway is precisely the #2380 duplicate-artist bug, so the fix is
// refused and the misconfiguration is logged at error level.
func (f *DirectoryRenameFixer) renameGuarded(ctx context.Context, a *artist.Artist, newDirName string) (string, []artist.PlatformRemapResult, error) {
	if f.renamer == nil {
		f.logger.Error("directory rename refused: no guarded renamer configured; " +
			"renaming without notifying the platforms would strand peers on the old path")
		return "", nil, errRenamerUnavailable
	}
	if strings.TrimSpace(a.ID) == "" {
		f.logger.Error("directory rename refused: artist has no persisted ID, so the guarded rename cannot load it",
			"artist", a.Name, "path", a.Path)
		return "", nil, errRenamerUnavailable
	}
	return f.renamer.RenameDirectory(ctx, a.ID, newDirName)
}

// errRenamerUnavailable marks a rename that could not even be ATTEMPTED through
// the guarded road. It is reported as an unfixed violation (never as Fixed), so
// no rename ever reports success without the platform-sync step having run.
var errRenamerUnavailable = errors.New("guarded directory rename is unavailable")

// declineFor maps the renamer's sentinel refusals onto an unfixed FixResult. Any
// error not listed here is a genuine failure and is returned to the pipeline.
func (f *DirectoryRenameFixer) declineFor(err error, v *Violation, chosen string) (*FixResult, bool) {
	var msg string
	switch {
	case errors.Is(err, errRenamerUnavailable):
		msg = "skipped: directory rename cannot notify the connected platforms (see logs)"
	case errors.Is(err, artist.ErrRenameLocked):
		msg = "skipped: artist is locked"
	case errors.Is(err, artist.ErrRenameDestExists):
		msg = fmt.Sprintf("destination '%s' already exists", chosen)
	case errors.Is(err, artist.ErrRenameNoChange):
		msg = "paths already match"
	case errors.Is(err, artist.ErrRenameNoPath):
		msg = "artist has no path"
	case errors.Is(err, artist.ErrRenameInvalidName):
		msg = fmt.Sprintf("canonical name '%s' is not a valid directory name", chosen)
	default:
		return nil, false
	}
	return &FixResult{RuleID: v.RuleID, Fixed: false, Message: msg}, true
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
	// hashRecorder is required, not optional: renumbering moves a different
	// file into a slot, which invalidates that slot's stored hashes, and
	// image.RenumberFanart will not renumber without somewhere to record that.
	hashRecorder imageHashRecorder
	logger       *slog.Logger
}

// NewBackdropSequencingFixer creates a BackdropSequencingFixer.
func NewBackdropSequencingFixer(platformService *platform.Service, fsCheck *SharedFSCheck, hashRecorder imageHashRecorder, logger *slog.Logger) *BackdropSequencingFixer {
	return &BackdropSequencingFixer{
		platformService: platformService,
		fsCheck:         fsCheck,
		hashRecorder:    hashRecorder,
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

		if err := img.RenumberFanart(ctx, f.hashRecorder, a.ID, a.Path, primaryName, discovered, kodiNumbering); err != nil {
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

// ImageDuplicateFixer removes redundant within-type fanart duplicates, keeping
// the lowest slot index in each duplicate group and renumbering survivors into
// a contiguous sequence.
//
// It serves both duplicate rules, because the destructive half of the work --
// stage, delete, renumber, roll back on failure -- is identical for each; only
// the choice of which slots are redundant differs, and that choice is made by
// deletionSetFor. The two rules differ in how much trust their answer earns,
// not in what is done with it:
//
//   - image_duplicate_exact matches on byte equality, which cannot be wrong --
//     but the rule still seeds to manual mode (a destructive rule should not
//     start deleting a user's files on upgrade without them asking; see the
//     rule's definition in service.go). An operator can opt it into auto.
//   - image_duplicate matches on perceptual similarity, which is a judgement,
//     so its rule seeds to manual mode and this fixer runs only when a user
//     triggers FixViolation. It implements no CandidateDiscoverer, so manual
//     mode never invokes it as a side effect of evaluation.
type ImageDuplicateFixer struct {
	db                *sql.DB
	platformService   *platform.Service
	fsCheck           *SharedFSCheck
	imageHashRecorder imageHashRecorder
	logger            *slog.Logger
}

// NewImageDuplicateFixer creates an ImageDuplicateFixer.
func NewImageDuplicateFixer(db *sql.DB, platformService *platform.Service, fsCheck *SharedFSCheck, hashRecorder imageHashRecorder, logger *slog.Logger) *ImageDuplicateFixer {
	return &ImageDuplicateFixer{
		db:                db,
		platformService:   platformService,
		fsCheck:           fsCheck,
		imageHashRecorder: hashRecorder,
		logger:            logger.With(slog.String("component", "image-duplicate-fixer")),
	}
}

// CanFix returns true for both the exact and perceptual duplicate rules.
func (f *ImageDuplicateFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleImageDuplicate || v.RuleID == RuleImageDuplicateExact
}

// deletionSetFor picks the redundant slots for the rule that raised the
// violation.
//
// For the exact rule this is pure byte-equality grouping: equality is
// transitive, so a group of identical files collapses onto its lowest slot with
// no risk of destroying distinct artwork.
//
// For the perceptual rule it must go through nonTransitiveFanartDeletionSet,
// because perceptual similarity is NOT transitive and naive grouping there can
// delete genuinely distinct images (see that function's comment).
func deletionSetFor(ruleID string, res imageDupResult) map[int]bool {
	if ruleID == RuleImageDuplicateExact {
		return res.exactFanartToDelete
	}
	return nonTransitiveFanartDeletionSet(res.perceptual)
}

// Fix re-detects current within-type fanart duplicates, deletes the
// higher-numbered file in each duplicate group, and renumbers the survivors
// to close the resulting gap. Detection is re-run rather than trusting the
// persisted violation message, because FixViolation reconstructs the
// violation from the DB row without re-running the checker, so the on-disk
// state may have drifted since the violation was recorded.
func (f *ImageDuplicateFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	ruleID := RuleImageDuplicate
	if v != nil && v.RuleID != "" {
		ruleID = v.RuleID
	}

	if f.fsCheck.IsShared(ctx, a) {
		return &FixResult{
			RuleID:  ruleID,
			Fixed:   false,
			Message: "skipped: shared-filesystem library",
		}, nil
	}

	if a.Path == "" {
		return &FixResult{RuleID: ruleID, Fixed: false, Message: "artist has no path"}, nil
	}
	if f.db == nil {
		return &FixResult{RuleID: ruleID, Fixed: false, Message: "no database connection"}, nil
	}

	var profile *platform.Profile
	if f.platformService != nil {
		var profErr error
		profile, profErr = f.platformService.GetActive(ctx)
		if profErr != nil {
			// Abort rather than falling back to the default naming
			// convention: deleting files under the wrong convention is
			// destructive and not safely reversible (mirrors
			// BackdropSequencingFixer).
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
	var primaryName string
	if len(fanartNames) > 0 {
		primaryName = fanartNames[0]
	}
	if primaryName == "" {
		return &FixResult{
			RuleID:  ruleID,
			Fixed:   false,
			Message: "skipped: no fanart naming convention available",
		}, nil
	}
	kodiNumbering := profile != nil && strings.EqualFold(profile.ID, "kodi")

	tolerance := defaultImageDupTolerance
	if v != nil && v.Config.Tolerance > 0 && v.Config.Tolerance <= 1.0 {
		tolerance = v.Config.Tolerance
	}

	// fresh=true: this pass decides which files to DELETE, so it re-hashes every
	// file from disk rather than trusting the stored hashes. Those are keyed by
	// slot, and anything that moves a file between slots -- a previous fix's
	// renumber, a reorder, a slot delete, a user replacing a file over a network
	// share -- leaves a slot describing a file it no longer holds. Acting on
	// that reads distinct artwork as a byte-identical copy and deletes it. The
	// re-read also re-persists the corrected hashes, so this pass repairs the
	// staleness it refuses to trust. See findImageDuplicates.
	res, err := findImageDuplicates(ctx, f.db, a, primaryName, tolerance, f.imageHashRecorder, true, f.logger)
	if err != nil {
		return nil, fmt.Errorf("re-detecting image duplicates for %s: %w", a.Name, err)
	}

	toDelete := deletionSetFor(ruleID, res)

	if len(toDelete) == 0 {
		return &FixResult{
			RuleID:  ruleID,
			Fixed:   false,
			Message: "no removable within-type fanart duplicates found",
		}, nil
	}

	// #2533 carve-out: never delete a fanart slot the operator set by hand.
	// This fixer deletes by slot index and so bypasses the ruleToImageType
	// guard in attemptFix; it must filter protected (locked or "user"-
	// provenance) slots itself. A lock-state read error aborts the fix (no
	// deletion) rather than risk destroying a protected image -- a wrong delete
	// is irreversible. It is returned as a hard error to match the identical
	// DB-error handling of findImageDuplicates above (both read the same
	// connection, so in practice this errors only if that one already did).
	protected, protErr := f.protectedFanartSlots(ctx, a.ID)
	if protErr != nil {
		return nil, fmt.Errorf("reading fanart lock state for %s: %w", a.Name, protErr)
	}
	for slot := range protected {
		delete(toDelete, slot)
	}
	if len(toDelete) == 0 {
		return &FixResult{
			RuleID:  ruleID,
			Fixed:   false,
			Message: "skipped: duplicate slots locked or user-set",
		}, nil
	}

	removedNames, delErr := f.deleteDuplicateFanartWithRollback(ctx, a, primaryName, kodiNumbering, toDelete)
	if delErr != nil {
		return nil, delErr
	}

	// Resync the artist's fanart fields from disk so the pipeline's
	// subsequent artistService.Update(ctx, a) call (in Pipeline.FixViolation)
	// writes matching artist_images rows, mirroring the discover/remove/
	// renumber/resync sequence used by Router.updateArtistFanartCount.
	resyncFanartFields(a, primaryName)

	return &FixResult{
		RuleID:       ruleID,
		Fixed:        true,
		SlotsRemoved: len(removedNames),
		RemovedFiles: true,
		Message:      fmt.Sprintf("removed %d duplicate fanart file(s) for %s: %s", len(removedNames), a.Name, strings.Join(removedNames, ", ")),
	}, nil
}

// protectedFanartSlots returns the set of fanart slot indices for an artist
// that must not be auto-deleted because the operator set them deliberately --
// the artist_images row is locked or carries "user" provenance (#2533). The
// caller filters the deletion set against this before removing anything.
func (f *ImageDuplicateFixer) protectedFanartSlots(ctx context.Context, artistID string) (map[int]bool, error) {
	rows, err := f.db.QueryContext(ctx,
		`SELECT slot_index FROM artist_images
		 WHERE artist_id = ? AND image_type = 'fanart' AND (locked = 1 OR source = ?)`,
		artistID, artist.ImageSourceUser)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	protected := make(map[int]bool)
	for rows.Next() {
		var slot int
		if scanErr := rows.Scan(&slot); scanErr != nil {
			return nil, scanErr
		}
		protected[slot] = true
	}
	return protected, rows.Err()
}

// deleteDuplicateFanartWithRollback discovers the artist's fanart files and
// removes the within-type duplicates named by toDelete (keyed by compacted slot
// position, matching DiscoverFanart's ordering; see resolveImageDupHash), then
// renumbers the survivors to close the resulting gap. It returns the base names
// of the removed files.
//
// Deletion is crash-safe, mirroring img.RenumberFanart's two-phase
// stage/rollback shape: each duplicate is first STAGED to a tomb (renamed to
// ".dup_pending_delete.tmp") rather than unlinked immediately; the tombs are
// permanently unlinked only AFTER RenumberFanart succeeds. On any failure before
// that commit point, every staged tomb is restored to its original path
// (best-effort) so no distinct artwork is lost on a partial failure. A
// post-commit tomb-unlink failure is logged, not rolled back -- the survivors
// are already renumbered, and a leftover tomb is ignored by discovery.
func (f *ImageDuplicateFixer) deleteDuplicateFanartWithRollback(ctx context.Context, a *artist.Artist, primaryName string, kodiNumbering bool, toDelete map[int]bool) ([]string, error) {
	paths, discErr := img.DiscoverFanart(a.Path, primaryName)
	if discErr != nil {
		return nil, fmt.Errorf("discovering fanart for %s: %w", a.Name, discErr)
	}

	const dupTombSuffix = ".dup_pending_delete.tmp"
	type stagedDup struct {
		origPath string // original file path (restore target)
		tombPath string // staged tomb path
	}
	var staged []stagedDup
	var removedNames []string
	survivors := make([]string, 0, len(paths))

	// restoreStaged rolls staged tombs back to their originals (best-effort),
	// returning any restore-error descriptions for inclusion in the wrapped err.
	//
	// Checks occupancy before every rename rather than clobbering blindly.
	// This restore only runs after img.RenumberFanart reports a failure, and
	// that failure can be img.renumberFanartFiles' OWN best-effort rollback
	// failing partway (a rename-back that errors on some, but not all, of the
	// files it already finalized) -- in which case a just-renumbered survivor
	// can be left sitting on exactly the path a tombed duplicate used to
	// occupy. Renaming the tomb over an occupied path in that state would
	// silently overwrite that survivor with the content that was supposed to
	// be deleted -- the same destructive-rollback shape F1 closed for the
	// invalidation-failure trigger, but via a different trigger (a rollback
	// failure inside RenumberFanart itself) that the F1 reorder does not
	// reach. Refusing and failing loudly, rather than overwriting, is what
	// "never destroy data to clean up after a failure" means in practice:
	// the tomb is left in place (recoverable, inert -- discovery ignores its
	// suffix) instead of the alternative of silently erasing distinct
	// artwork.
	restoreStaged := func() []string {
		var rollbackErrs []string
		for _, s := range staged {
			if _, statErr := os.Lstat(s.origPath); statErr == nil {
				f.logger.Error("refusing to restore a staged duplicate onto an occupied path",
					"artist", a.Name, "path", s.origPath, "tomb", filepath.Base(s.tombPath))
				rollbackErrs = append(rollbackErrs, fmt.Sprintf(
					"restore %s: refused -- path is occupied (tomb left at %s)",
					filepath.Base(s.origPath), filepath.Base(s.tombPath)))
				continue
			} else if !os.IsNotExist(statErr) {
				rollbackErrs = append(rollbackErrs, fmt.Sprintf("restore %s: checking occupancy: %v", filepath.Base(s.origPath), statErr))
				continue
			}
			if rbErr := os.Rename(s.tombPath, s.origPath); rbErr != nil {
				rollbackErrs = append(rollbackErrs, fmt.Sprintf("restore %s: %v", filepath.Base(s.origPath), rbErr))
			}
		}
		return rollbackErrs
	}

	for i, p := range paths {
		if !toDelete[i] {
			survivors = append(survivors, p)
			continue
		}
		tombPath := p + dupTombSuffix
		// Clear any leftover tomb from a previous crashed operation.
		if rmErr := os.Remove(tombPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, wrapWithRollbackErrs(restoreStaged(),
				fmt.Errorf("clearing stale tomb %s for %s: %w", filepath.Base(tombPath), a.Name, rmErr))
		}
		if stageErr := os.Rename(p, tombPath); stageErr != nil {
			return nil, wrapWithRollbackErrs(restoreStaged(),
				fmt.Errorf("staging duplicate fanart %s for deletion (already staged: %s) for %s: %w", filepath.Base(p), strings.Join(removedNames, ", "), a.Name, stageErr))
		}
		staged = append(staged, stagedDup{origPath: p, tombPath: tombPath})
		f.logger.Info("staged duplicate fanart slot for deletion",
			"artist", a.Name, "slot", i, "file", filepath.Base(p))
		removedNames = append(removedNames, filepath.Base(p))
	}

	if renumberErr := img.RenumberFanart(ctx, f.imageHashRecorder, a.ID, a.Path, primaryName, survivors, kodiNumbering); renumberErr != nil {
		return nil, wrapWithRollbackErrs(restoreStaged(),
			fmt.Errorf("renumbering fanart after removing %d duplicate(s) (%s) for %s: %w", len(removedNames), strings.Join(removedNames, ", "), a.Name, renumberErr))
	}

	// Committed: permanently unlink the tombs.
	for _, s := range staged {
		if rmErr := os.Remove(s.tombPath); rmErr != nil {
			f.logger.Warn("removing staged duplicate-fanart tomb after renumber",
				"artist", a.Name, "tomb", filepath.Base(s.tombPath), "error", rmErr)
		}
	}
	return removedNames, nil
}

// wrapWithRollbackErrs appends any best-effort rollback-error descriptions to
// err so a partial failure reports both the original cause and any staged files
// that could not be restored. Returns err unchanged when the rollback was clean.
func wrapWithRollbackErrs(rollbackErrs []string, err error) error {
	if len(rollbackErrs) == 0 {
		return err
	}
	return fmt.Errorf("%w (rollback errors: %s)", err, strings.Join(rollbackErrs, "; "))
}

// resyncFanartFields re-discovers fanart files on disk after a mutation and
// updates the artist's fanart fields in place, mirroring
// Router.updateArtistFanartCount. Fixers have no Router reference, so this
// is a self-contained copy limited to the fields extractImageMetadata reads
// for fanart (Exists, Count, LowRes); Width/Height are left untouched since
// slot 0 is never deleted and its dimensions do not change.
func resyncFanartFields(a *artist.Artist, primaryName string) {
	existing, err := img.DiscoverFanart(a.Path, primaryName)
	if err != nil {
		return
	}
	count := len(existing)
	a.FanartExists = count > 0
	a.FanartCount = count
	a.FanartLowRes = false
	if count == 0 {
		return
	}
	f, openErr := os.Open(existing[0])
	if openErr != nil {
		return
	}
	defer f.Close() //nolint:errcheck // best-effort close after read
	w, h, dimErr := img.GetDimensions(f)
	if dimErr == nil {
		a.FanartLowRes = img.IsLowResolution(w, h, "fanart")
	}
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
