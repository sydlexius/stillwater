package rule

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
)

// MetadataProvider abstracts the subset of provider.Orchestrator that the
// language-preference rule needs. It is used by the name_language_pref
// checker to fetch a localized name candidate for comparison against the
// stored artist Name and SortName. Wired by SetMetadataProvider; when nil
// the checker degrades to a no-op rather than failing evaluation.
type MetadataProvider interface {
	FetchMetadata(ctx context.Context, mbid, name string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error)
}

// ReleaseGroupFetcher abstracts fetching an artist's MusicBrainz release
// groups (albums, EPs, singles) by MBID. It is satisfied by the MusicBrainz
// provider adapter, which implements provider.ReleaseGroupFetcher with the
// same method. The discography_populated checker and the DiscographyFixer
// both use it. When nil the checker only flags artists with a completely
// empty discography (no MusicBrainz round-trip is attempted).
type ReleaseGroupFetcher interface {
	GetReleaseGroups(ctx context.Context, mbid string) ([]provider.ReleaseGroupInfo, error)
}

// ProviderAvailability abstracts the subset of provider.SettingsService that the
// provider_id_missing checker needs: the set of providers that are currently
// configured (no key required, or a key is stored). The checker requires an
// artist to carry a provider ID only for providers that are actually available,
// so a missing key never produces a violation the operator cannot act on. Wired
// by SetProviderAvailability; when nil the checker degrades to a no-op rather
// than failing evaluation.
type ProviderAvailability interface {
	AvailableProviderNames(ctx context.Context) (map[provider.ProviderName]bool, error)
}

// ruleCacheTTL is how long the in-memory rule list cache is considered fresh.
// A short TTL (5 s) eliminates the N+1 DB query pattern under concurrent load
// while ensuring that rule changes propagate within a few seconds.
const ruleCacheTTL = 5 * time.Second

// logoBoundsCacheKey identifies a cached logo content-bounds result.
// The modTime field ensures the entry is invalidated when the file changes.
type logoBoundsCacheKey struct {
	filePath string
	modTime  time.Time
}

// logoBoundsCacheEntry stores the content and original rectangles returned
// by ContentBounds for a given logo file.
type logoBoundsCacheEntry struct {
	content  image.Rectangle
	original image.Rectangle
}

// maxLogoBoundsCacheSize is the maximum number of entries retained in the
// logo bounds cache. When this limit is reached, the oldest entry is evicted.
const maxLogoBoundsCacheSize = 500

// PlatformImageFetcher abstracts fetching and uploading artist images through
// platform connections (Emby, Jellyfin). This allows the rule engine to check
// and fix images for artists that have no local filesystem path but are managed
// by a media server API.
type PlatformImageFetcher interface {
	// FetchArtistImage downloads the image bytes for the given Stillwater artist ID
	// and image type (e.g. "logo", "thumb") from a connected media platform.
	FetchArtistImage(ctx context.Context, artistID, imageType string) (data []byte, contentType string, err error)
	// UploadArtistImage pushes image bytes to the connected media platform(s)
	// for the given Stillwater artist ID and image type.
	UploadArtistImage(ctx context.Context, artistID, imageType string, data []byte, contentType string) error
	// ListArtistImageSlots returns a map of image types to their slot count
	// as reported by a connected platform. For example: {"thumb": 1, "fanart": 3, "logo": 1}.
	// Used by the extraneous images checker to detect platform-reported images
	// with no matching artist_images row.
	ListArtistImageSlots(ctx context.Context, artistID string) (map[string]int, error)
}

// apiImageCacheKey identifies a cached API-fetched image by artist and type.
type apiImageCacheKey struct {
	artistID  string
	imageType string
}

// Engine evaluates rules against artists.
type Engine struct {
	service         *Service
	db              *sql.DB
	platformService *platform.Service
	libraryService  *library.Service
	checkers        map[string]Checker
	logger          *slog.Logger

	// fsCache caches filesystem metadata (directory listings and stat results)
	// to reduce I/O during rule evaluation. When nil, checkers fall back to
	// direct os.ReadDir and os.Stat calls (backward compatible). Initialized
	// by SetFSCache after construction.
	fsCache *FSCache

	// sharedFSCache caches IsSharedFilesystem results by library ID during
	// a single evaluation run to avoid N+1 DB queries when multiple artists
	// share the same library. Cleared at the start of each Evaluate call.
	// Guarded by sharedFSMu because Evaluate is called from concurrent HTTP
	// handlers (net/http serves requests in separate goroutines).
	sharedFSMu    sync.Mutex
	sharedFSCache map[string]bool

	// imageDupCache caches one findImageDuplicates result per artist within a
	// single Evaluate call, so the exact and perceptual duplicate checkers --
	// which both call findImageDuplicates for the same artist -- share one
	// computation instead of each independently re-reading and re-hashing
	// every fanart slot (#2349's whole point, reintroduced by running the
	// same pass twice). Keyed by artist ID rather than a single slot because
	// Evaluate runs from concurrent HTTP handlers for different artists; an
	// unkeyed slot would let one artist's checker read another artist's
	// result mid-flight. Also keyed by tolerance: the perceptual grouping
	// depends on it, and the two checkers are not guaranteed to request the
	// same value (the exact checker always uses the default; the perceptual
	// checker honors a per-rule override), so a tolerance mismatch forces a
	// fresh, correct recompute rather than serving a result grouped at the
	// wrong threshold. Cleared at the start of each Evaluate call, same as
	// sharedFSCache; a clear racing a concurrent artist's in-flight populate
	// costs that artist a cache miss (one redundant recompute), never wrong
	// data -- the same safety envelope sharedFSCache already relies on.
	imageDupMu    sync.Mutex
	imageDupCache map[string]imageDupCacheEntry

	// capabilities holds the per-(rule, artist) eligibility predicates keyed by
	// rule ID. Consulted by eligibleRules before any checker runs, so that both
	// the live evaluator and the offline health recompute derive the same set of
	// applicable rules from one place. Populated in NewEngine; not mutated after.
	capabilities map[string]ruleCapability

	// imageCapCache memoizes the per-artist image-hash summary that the two
	// duplicate rules' capability predicates need, so a single evaluation runs
	// one query per artist instead of one per (rule, call-site). Cleared at the
	// top of EvaluateScoped, same as imageDupCache. Guarded because evaluation
	// runs from concurrent HTTP handlers.
	imageCapMu    sync.Mutex
	imageCapCache map[string]imageHashCapability

	// ruleCacheMu guards ruleList and ruleFetchedAt.
	ruleCacheMu   sync.RWMutex
	ruleList      []Rule
	ruleFetchedAt time.Time

	// logoBoundsCacheMu guards logoBoundsCache. The cache persists across
	// Evaluate calls and is keyed by (filePath, modTime) so entries are
	// automatically invalidated when the logo file changes on disk.
	logoBoundsCacheMu sync.Mutex
	// logoBoundsCache stores ContentBounds results for logo files to avoid
	// re-decoding the same PNG on every rule evaluation. Bounded to
	// maxLogoBoundsCacheSize entries; oldest entry is evicted when full.
	logoBoundsCache     map[logoBoundsCacheKey]logoBoundsCacheEntry
	logoBoundsCacheKeys []logoBoundsCacheKey // insertion-order list for eviction

	// imageFetcher provides platform API access for fetching and uploading
	// artist images. When set, checkers and fixers can handle artists that
	// have no local filesystem path (API-only imports from Emby/Jellyfin).
	imageFetcher PlatformImageFetcher

	// metadataProvider is used by the name_language_pref checker and fixer
	// to fetch the best-matching localized alias for an artist. When nil,
	// the checker silently no-ops (returns nil violations) so evaluation
	// remains stable in test or stripped-down environments.
	metadataProvider MetadataProvider

	// releaseGroupFetcher is used by the discography_populated checker to
	// count an artist's MusicBrainz release groups when measuring NFO
	// coverage. When nil the checker only flags an entirely empty
	// discography and skips the coverage comparison.
	releaseGroupFetcher ReleaseGroupFetcher

	// providerAvailability is used by the provider_id_missing checker to learn
	// which providers are configured, so it only requires a provider ID for
	// providers image search can actually reach. When nil the checker no-ops.
	providerAvailability ProviderAvailability

	// apiImageCacheMu guards apiImageCache.
	apiImageCacheMu sync.Mutex
	// apiImageCache stores raw image bytes fetched via the platform API. This
	// avoids double-fetching between the checker (which reads the image to
	// measure padding) and the fixer (which reads it again to trim). Entries
	// are consumed (deleted) by ConsumeAPIImage when the fixer reads them,
	// preventing unbounded growth without requiring a global clear.
	apiImageCache map[apiImageCacheKey][]byte

	// imageHashRecorder persists perceptual and content hashes computed during
	// duplicate detection. Without it the duplicate rules still return correct
	// results, but every evaluation re-reads and re-decodes each unhashed image
	// because nothing writes the result back; wiring it is what turns hashing
	// into a once-per-file cost. Set via SetImageHashRecorder.
	imageHashRecorder imageHashRecorder
}

// NewEngine creates a rule evaluation engine with all built-in checkers registered.
func NewEngine(service *Service, db *sql.DB, platformService *platform.Service, libraryService *library.Service, logger *slog.Logger) *Engine {
	e := &Engine{
		service:         service,
		db:              db,
		platformService: platformService,
		libraryService:  libraryService,
		logger:          logger.With(slog.String("component", "rule-engine")),
		checkers: map[string]Checker{
			RuleNFOExists:             checkNFOExists,
			RuleNFOHasMBID:            checkNFOHasMBID,
			RuleThumbExists:           checkThumbExists,
			RuleFanartExists:          checkFanartExists,
			RuleLogoExists:            checkLogoExists,
			RuleBioExists:             checkBioExists,
			RuleBannerExists:          checkBannerExists,
			RuleArtistIDMismatch:      checkArtistIDMismatch,
			RuleDirectoryNameMismatch: checkDirectoryNameMismatch,
			RuleMetadataQuality:       checkMetadataQuality,
			RuleOriginMissing:         checkOriginMissing,
		},
	}
	// Register checkers that need the Engine's FSCache for cached filesystem
	// access. These use the e.makeXxxChecker() pattern so they capture the
	// Engine pointer and can call readDirCached / getImageDimensionsCached.
	e.checkers[RuleThumbSquare] = e.makeThumbSquareChecker()
	e.checkers[RuleThumbMinRes] = e.makeThumbMinResChecker()
	e.checkers[RuleFanartMinRes] = e.makeFanartMinResChecker()
	e.checkers[RuleFanartAspect] = e.makeFanartAspectChecker()
	e.checkers[RuleLogoMinRes] = e.makeLogoMinResChecker()
	e.checkers[RuleBannerMinRes] = e.makeBannerMinResChecker()
	e.checkers[RuleExtraneousImages] = e.makeExtraneousImagesChecker()
	e.checkers[RuleImageDuplicate] = e.makeImageDuplicateChecker()
	e.checkers[RuleImageDuplicateExact] = e.makeImageDuplicateExactChecker()
	e.checkers[RuleBackdropSequencing] = e.makeBackdropSequencingChecker()
	e.checkers[RuleBackdropMinCount] = e.makeBackdropMinCountChecker()
	e.checkers[RuleLogoPadding] = e.makeLogoPaddingChecker()
	e.checkers[RuleNameLanguagePref] = e.makeNameLanguagePrefChecker()
	e.checkers[RuleDiscographyPopulated] = e.makeDiscographyChecker()
	e.checkers[RuleProviderIDMissing] = e.makeProviderIDMissingChecker()

	// Register per-(rule, artist) capability predicates. A rule with no entry
	// here is always capable and is gated only by Enabled and, for the
	// filesystem-only rules, FilesystemDependent. See ruleCapability.
	e.capabilities = map[string]ruleCapability{
		RuleImageDuplicate:      e.capImageDuplicate,
		RuleImageDuplicateExact: e.capImageDuplicateExact,
	}
	return e
}

// SetImageHashRecorder attaches the store that duplicate detection writes
// computed image hashes back to (typically artist.Service). It is required for
// the duplicate rules to perform correctly at scale: detection still returns
// the right answer without it, but every evaluation re-reads and re-decodes
// each image whose hash is not yet stored, because nothing persists the result.
func (e *Engine) SetImageHashRecorder(r imageHashRecorder) {
	e.imageHashRecorder = r
}

// SetMetadataProvider attaches a metadata provider (typically the
// provider.Orchestrator) to the engine. The name_language_pref checker
// uses it to fetch localized aliases for comparison against the stored
// artist Name and SortName. Pass nil to disable the rule (its checker
// will return nil for every artist).
func (e *Engine) SetMetadataProvider(p MetadataProvider) {
	e.metadataProvider = p
}

// SetReleaseGroupFetcher attaches a MusicBrainz release-group fetcher to the
// engine. The discography_populated checker uses it to count an artist's
// release groups when measuring how much of the discography the NFO covers.
// Pass nil to disable coverage detection (the checker still flags artists
// whose NFO has zero album entries).
func (e *Engine) SetReleaseGroupFetcher(f ReleaseGroupFetcher) {
	e.releaseGroupFetcher = f
}

// ReleaseGroupFetcher returns the engine's release-group fetcher, or nil if
// none is configured.
func (e *Engine) ReleaseGroupFetcher() ReleaseGroupFetcher {
	return e.releaseGroupFetcher
}

// SetProviderAvailability attaches a provider-availability source (typically
// the provider.SettingsService) to the engine. The provider_id_missing checker
// uses it to require a provider ID only for providers that are configured. Pass
// nil to disable the rule (its checker returns nil for every artist).
func (e *Engine) SetProviderAvailability(p ProviderAvailability) {
	e.providerAvailability = p
}

// cachedRules returns the rule list from the in-memory cache when it is still
// fresh, or fetches it from the database and refreshes the cache otherwise.
// This eliminates the N+1 DB query pattern when EvaluateAll iterates over many
// artists: the list is fetched at most once per TTL window across all callers.
func (e *Engine) cachedRules(ctx context.Context) ([]Rule, error) {
	// Fast path: read lock to check freshness.
	// time.Since is evaluated inside the lock to avoid stale timestamps when
	// lock contention causes a delay between capturing now and acquiring the lock.
	e.ruleCacheMu.RLock()
	if !e.ruleFetchedAt.IsZero() && time.Since(e.ruleFetchedAt) < ruleCacheTTL {
		rules := e.ruleList
		e.ruleCacheMu.RUnlock()
		return rules, nil
	}
	e.ruleCacheMu.RUnlock()

	// Slow path: upgrade to write lock and re-check (another goroutine may
	// have already refreshed the cache between the two lock acquisitions).
	e.ruleCacheMu.Lock()
	defer e.ruleCacheMu.Unlock()

	if !e.ruleFetchedAt.IsZero() && time.Since(e.ruleFetchedAt) < ruleCacheTTL {
		return e.ruleList, nil
	}

	rules, err := e.service.List(ctx)
	if err != nil {
		return nil, err
	}
	// Normalize nil to an empty slice so callers always receive a non-nil
	// slice. This does not affect cache freshness, which is based solely
	// on ruleFetchedAt and ruleCacheTTL.
	if rules == nil {
		rules = []Rule{}
	}
	e.ruleList = rules
	e.ruleFetchedAt = time.Now()
	return rules, nil
}

// SetFSCache attaches a filesystem metadata cache to the engine. When set,
// rule checkers use cached directory listings and stat results instead of
// hitting the filesystem on every evaluation. Pass nil to disable caching
// (all checkers fall back to direct OS calls).
func (e *Engine) SetFSCache(cache *FSCache) {
	e.fsCache = cache
}

// FSCache returns the engine's filesystem metadata cache, or nil if none
// is configured. This accessor allows external components (e.g., the watcher
// event handler) to invalidate specific paths when filesystem changes are
// detected.
func (e *Engine) FSCache() *FSCache {
	return e.fsCache
}

// SetImageFetcher attaches a platform image fetcher to the engine. When set,
// the logo_padding checker and fixer can operate on API-only artists that have
// no local filesystem path.
func (e *Engine) SetImageFetcher(f PlatformImageFetcher) {
	e.imageFetcher = f
}

// lookupAPIImage returns cached image bytes fetched via the platform API for
// the given artist and image type. Returns nil, false if no entry exists.
// This is a read-only lookup used by checkers during evaluation.
func (e *Engine) lookupAPIImage(artistID, imageType string) ([]byte, bool) {
	key := apiImageCacheKey{artistID: artistID, imageType: imageType}
	e.apiImageCacheMu.Lock()
	defer e.apiImageCacheMu.Unlock()
	data, ok := e.apiImageCache[key]
	return data, ok
}

// ConsumeAPIImage returns cached image bytes and removes the entry from the
// cache. This is the exported accessor used by fixers: the consume-on-read
// pattern prevents unbounded cache growth and avoids the need for a global
// cache clear in Evaluate (which would race with concurrent evaluations).
func (e *Engine) ConsumeAPIImage(artistID, imageType string) ([]byte, bool) {
	key := apiImageCacheKey{artistID: artistID, imageType: imageType}
	e.apiImageCacheMu.Lock()
	defer e.apiImageCacheMu.Unlock()
	data, ok := e.apiImageCache[key]
	if ok {
		delete(e.apiImageCache, key)
	}
	return data, ok
}

// storeAPIImage caches image bytes fetched via the platform API for the given
// artist and image type. Entries are consumed (deleted) by ConsumeAPIImage when
// the fixer reads them, preventing unbounded growth.
func (e *Engine) storeAPIImage(artistID, imageType string, data []byte) {
	key := apiImageCacheKey{artistID: artistID, imageType: imageType}
	e.apiImageCacheMu.Lock()
	defer e.apiImageCacheMu.Unlock()
	if e.apiImageCache == nil {
		e.apiImageCache = make(map[apiImageCacheKey][]byte)
	}
	e.apiImageCache[key] = data
}

// InvalidateRuleCache drops the cached rule list so the next Evaluate call
// fetches fresh data from the database. Call this after any rule mutation
// (create, update, delete) to ensure the engine sees the change within the
// next evaluation cycle rather than waiting for the TTL to expire.
func (e *Engine) InvalidateRuleCache() {
	e.ruleCacheMu.Lock()
	e.ruleList = nil
	e.ruleFetchedAt = time.Time{}
	e.ruleCacheMu.Unlock()
}

// Evaluate runs all enabled rules against an artist and returns the results.
func (e *Engine) Evaluate(ctx context.Context, a *artist.Artist) (*EvaluationResult, error) {
	return e.EvaluateScoped(ctx, a, nil)
}

// EvaluateScoped runs only the rules whose IDs appear in only.
//
// A NIL set means "no scoping": every eligible rule runs, which is exactly what
// Evaluate does. A non-nil set means "only these", and an EMPTY non-nil set
// therefore means "evaluate nothing". That distinction is load-bearing: a
// category that happens to match no eligible rules must evaluate nothing, and
// if emptiness were treated as "unscoped" it would instead run every rule and
// silently reintroduce the very bug this scoping exists to fix.
//
// Scoping is a correctness concern, not an optimization. Some checkers reach an
// external provider (discography_populated queries MusicBrainz; name_language_pref
// fetches metadata), so evaluating rules the operator never asked for turns a
// purely local operation -- de-duplicating images by file hash, say -- into
// unrequested outbound traffic against a third party, once per artist. See #2476.
//
// The returned HealthScore is only meaningful for an unscoped call, because
// health is defined as passed/total across ALL eligible rules. A scoped result
// therefore leaves HealthScore at zero and sets Scoped, so a caller cannot
// mistake a subset score for the artist's real one. Callers that need to refresh
// the persisted score after a scoped run must use the pipeline's offline
// recompute rather than reading HealthScore from here.
func (e *Engine) EvaluateScoped(ctx context.Context, a *artist.Artist, only map[string]bool) (*EvaluationResult, error) {
	// Clear the shared-filesystem cache so each top-level evaluation gets
	// fresh data while avoiding N+1 queries within the same run. The API image
	// cache is NOT cleared here: it is keyed by (artistID, imageType) so
	// entries from different evaluations do not conflict, and the fixer consumes
	// entries via ConsumeAPIImage (delete-on-read) to prevent unbounded growth.
	e.sharedFSMu.Lock()
	e.sharedFSCache = nil
	e.sharedFSMu.Unlock()

	// Clear the image-duplicate-detection cache for the same reason: it is
	// scoped to one evaluation so the exact and perceptual checkers below
	// share one computation for THIS artist, without carrying a stale result
	// into the next evaluation for a different artist.
	e.imageDupMu.Lock()
	e.imageDupCache = nil
	e.imageDupMu.Unlock()

	// Same lifetime for the rule-eligibility image-hash summary: one query per
	// artist per evaluation, shared by both duplicate rules' capability checks
	// and by the EligibleRuleIDs call the offline health recompute makes right
	// after this evaluation.
	e.imageCapMu.Lock()
	e.imageCapCache = nil
	e.imageCapMu.Unlock()

	rules, skipped, err := e.eligibleRules(ctx, a)
	if err != nil {
		return nil, err
	}

	scoped := only != nil
	result := &EvaluationResult{
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Scoped:     scoped,
	}

	// A scoped run only speaks to the rules it was asked about, so it reports
	// only the skips within that scope. Reporting a skip for a rule the caller
	// never asked to evaluate would be noise, not information.
	for _, s := range skipped {
		if scoped && !only[s.RuleID] {
			continue
		}
		result.RulesSkipped = append(result.RulesSkipped, s)
	}

	if len(result.RulesSkipped) > 0 {
		// Debug rather than Info: this fires once per artist per evaluation, and
		// on a library with many API-only artists that is a line per artist per
		// pass. The operator-facing surface is RulesSkipped on the health API
		// response; this line exists so the same information is recoverable from
		// the logs when debugging a health score.
		ids := make([]string, 0, len(result.RulesSkipped))
		for _, s := range result.RulesSkipped {
			ids = append(ids, s.RuleID+"="+s.Reason)
		}
		e.logger.Debug("rules skipped: not applicable to this artist",
			slog.String("artist_id", a.ID),
			slog.String("artist", a.Name),
			slog.Int("skipped_count", len(result.RulesSkipped)),
			slog.String("skipped_rules", strings.Join(ids, "; ")),
		)
	}

	for i := range rules {
		r := &rules[i]
		if scoped && !only[r.ID] {
			continue
		}

		// eligibleRules already guaranteed a checker is registered.
		checker := e.checkers[r.ID]

		result.RulesTotal++
		result.RulesConsidered = append(result.RulesConsidered, r.ID)

		v := checker(ctx, a, r.Config)
		if v != nil {
			// Use severity from rule config if the checker did not set it
			if v.Severity == "" {
				v.Severity = r.Config.Severity
			}
			v.Config = r.Config
			result.Violations = append(result.Violations, *v)
		} else {
			result.RulesPassed++
		}
	}

	if !scoped {
		result.HealthScore = calculateHealthScore(result.RulesPassed, result.RulesTotal)
	}

	return result, nil
}

// eligibleRules returns the rules a full evaluation would consider for this
// artist, in evaluation order: enabled, not skipped for want of a local path,
// and backed by a registered checker.
//
// EvaluateScoped and EligibleRuleIDs both derive from this, deliberately. The
// offline health recompute needs the same denominator a real evaluation would
// use, and computing that set twice in two places is how the two silently drift
// apart and start writing a wrong score.
// It also returns the rules that were SKIPPED because they cannot apply to this
// artist, each with a reason. A skipped rule is neither passed nor failed: it is
// out of the denominator entirely. Reporting them is what stops an inapplicable
// rule from being silently recorded as a pass, which is what #2509 fixed for the
// duplicate rules and what the filesystem-dependent skip had been doing all along.
//
// A rule that is merely disabled, or has no registered checker, is NOT reported
// as skipped: those are engine/config states, not statements about this artist.
func (e *Engine) eligibleRules(ctx context.Context, a *artist.Artist) ([]Rule, []SkippedRule, error) {
	rules, err := e.cachedRules(ctx)
	if err != nil {
		return nil, nil, err
	}

	eligible := make([]Rule, 0, len(rules))
	var skipped []SkippedRule
	for i := range rules {
		r := &rules[i]
		if !r.Enabled {
			continue
		}

		if _, ok := e.checkers[r.ID]; !ok {
			e.logger.Debug("no checker registered for rule", slog.String("rule_id", r.ID))
			continue
		}

		// Skip filesystem-dependent rules for artists without a local path.
		// API-imported artists (Emby/Jellyfin) have no filesystem directory and
		// cannot have NFO files; evaluating these rules against them produces
		// false violations.
		if r.FilesystemDependent && a.Path == "" {
			skipped = append(skipped, SkippedRule{RuleID: r.ID, RuleName: r.Name, Reason: SkipReasonNoLocalPath})
			continue
		}

		// The general per-(rule, artist) capability gate. Unlike the flag above,
		// it may consult the database: a rule can be inapplicable to this artist
		// because the data it compares does not exist, not just because a
		// directory does not.
		if capFn, ok := e.capabilities[r.ID]; ok {
			capable, reason, capErr := capFn(ctx, a)
			if capErr != nil {
				return nil, nil, fmt.Errorf("checking capability for rule %s: %w", r.ID, capErr)
			}
			if !capable {
				skipped = append(skipped, SkippedRule{RuleID: r.ID, RuleName: r.Name, Reason: reason})
				continue
			}
		}

		eligible = append(eligible, *r)
	}
	return eligible, skipped, nil
}

// ScopeForCategory resolves a rule category into the evaluation scope that
// EvaluateScoped expects: every rule in that category, whether or not the artist
// is currently capable of being evaluated against it.
//
// An empty category returns a nil scope, meaning "evaluate everything" -- that is
// the whole-artist run. A category that matches no rule at all returns an EMPTY,
// NON-NIL scope, meaning "evaluate nothing", which is the honest answer and not
// an invitation to run every rule.
//
// The scope deliberately includes the category's SKIPPED rules (#2509). Adding an
// ineligible rule ID to the scope evaluates nothing extra: EvaluateScoped iterates
// only the ELIGIBLE rules and intersects them with the scope, so an ID that is not
// eligible can never reach a checker (and therefore never issues an unrequested
// provider call, the #2476 constraint). What the ID does buy is recognition: the
// skipped-set filter in EvaluateScoped keeps a SkippedRule only when it is in
// scope, so without the ID a category run would report RulesSkipped as empty and
// the pipeline's retraction would silently have nothing to retract -- leaving the
// stale pass row for a rule that never examined the artist exactly where it was.
//
// It runs no checkers and makes no provider calls, so the scope can be computed
// before deciding what to evaluate.
func (e *Engine) ScopeForCategory(ctx context.Context, a *artist.Artist, category string) (map[string]bool, error) {
	if category == "" {
		return nil, nil
	}
	eligible, skipped, err := e.eligibleRules(ctx, a)
	if err != nil {
		return nil, err
	}
	scope := make(map[string]bool, len(eligible)+len(skipped))
	for i := range eligible {
		if string(eligible[i].Category) == category {
			scope[eligible[i].ID] = true
		}
	}
	if len(skipped) == 0 {
		return scope, nil
	}
	// eligibleRules strips the Rule bodies from the skipped set (it carries only
	// id/name/reason), so the category has to come from the rule cache. This is
	// the same in-memory snapshot eligibleRules just read; it costs no query.
	all, err := e.cachedRules(ctx)
	if err != nil {
		return nil, err
	}
	categoryByID := make(map[string]string, len(all))
	for i := range all {
		categoryByID[all[i].ID] = string(all[i].Category)
	}
	for _, s := range skipped {
		if categoryByID[s.RuleID] == category {
			scope[s.RuleID] = true
		}
	}
	return scope, nil
}

// EligibleRuleIDs returns the IDs of the rules a full evaluation would consider
// for this artist right now. It runs no checkers and makes no provider calls.
//
// This is the denominator for the offline health recompute: health must be
// passed/total over the rules that are currently eligible, never over whatever
// rule_results rows happen to exist, because a rule enabled or disabled since
// the last full pass would otherwise skew the score.
func (e *Engine) EligibleRuleIDs(ctx context.Context, a *artist.Artist) ([]string, error) {
	rules, _, err := e.eligibleRules(ctx, a)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rules))
	for i := range rules {
		ids = append(ids, rules[i].ID)
	}
	return ids, nil
}

// EvaluateAll runs all enabled rules against multiple artists.
func (e *Engine) EvaluateAll(ctx context.Context, artists []artist.Artist) ([]EvaluationResult, error) {
	var results []EvaluationResult
	for i := range artists {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		r, err := e.Evaluate(ctx, &artists[i])
		if err != nil {
			return nil, err
		}
		results = append(results, *r)
	}
	return results, nil
}

// IsSharedFilesystem reports whether the given artist's library has a
// suspected or confirmed shared-filesystem status. Returns false if the
// library service is nil or the artist has no library ID. Returns true
// (fail closed) on DB errors to prevent destructive operations when the
// database is unavailable.
//
// Results are cached per library ID for the duration of a single evaluation
// run (cache is cleared at the start of each Evaluate call) to avoid N+1
// DB queries when multiple checkers call this for the same artist.
func (e *Engine) IsSharedFilesystem(ctx context.Context, a *artist.Artist) bool {
	if e.libraryService == nil || a.LibraryID == "" {
		return false
	}

	// Check the per-evaluation cache first.
	e.sharedFSMu.Lock()
	if e.sharedFSCache != nil {
		if cached, ok := e.sharedFSCache[a.LibraryID]; ok {
			e.sharedFSMu.Unlock()
			return cached
		}
	}
	e.sharedFSMu.Unlock()

	lib, err := e.libraryService.GetByID(ctx, a.LibraryID)
	if err != nil {
		// Fail closed: assume shared filesystem when the DB is unavailable
		// to prevent destructive operations (e.g. deleting "extraneous" images
		// that a platform actually owns).
		e.logger.Warn("library lookup failed; assuming shared filesystem",
			slog.String("library_id", a.LibraryID),
			slog.String("error", err.Error()))
		e.cacheSharedFS(a.LibraryID, true)
		return true
	}

	shared := lib.IsSharedFS()
	e.cacheSharedFS(a.LibraryID, shared)
	return shared
}

// cacheSharedFS stores a shared-filesystem lookup result in the per-evaluation
// cache, lazily initializing the map on first use.
func (e *Engine) cacheSharedFS(libraryID string, shared bool) {
	e.sharedFSMu.Lock()
	defer e.sharedFSMu.Unlock()
	if e.sharedFSCache == nil {
		e.sharedFSCache = make(map[string]bool)
	}
	e.sharedFSCache[libraryID] = shared
}

// imageDupCacheEntry holds a findImageDuplicates result alongside the
// tolerance it was computed with, so a cache lookup can tell a genuine hit
// apart from a stale result computed for a different threshold.
type imageDupCacheEntry struct {
	tolerance float64
	result    imageDupResult
}

// getCachedImageDuplicates returns findImageDuplicates' result for this
// artist and tolerance, computing it at most once per Evaluate call.
//
// The exact and perceptual duplicate checkers (makeImageDuplicateExactChecker,
// makeImageDuplicateChecker) both call this for the same artist during the
// same evaluation pass; without it, each independently re-reads and re-hashes
// every fanart slot findImageDuplicates' own doc comment promises is shared
// work -- true within one call, false across the two rules that actually
// exercise it. See that comment for why sharing is safe (detection only,
// never the destructive path) and the imageDupCache field doc for why the
// cache is keyed by artist ID and tolerance rather than a single slot.
func (e *Engine) getCachedImageDuplicates(
	ctx context.Context,
	a *artist.Artist,
	primaryName string,
	tolerance float64,
	logger *slog.Logger,
) (imageDupResult, error) {
	e.imageDupMu.Lock()
	if cached, ok := e.imageDupCache[a.ID]; ok && cached.tolerance == tolerance {
		e.imageDupMu.Unlock()
		return cached.result, nil
	}
	e.imageDupMu.Unlock()

	res, err := findImageDuplicates(ctx, e.db, a, primaryName, tolerance, e.imageHashRecorder, false, logger)
	if err != nil {
		return imageDupResult{}, err
	}

	e.imageDupMu.Lock()
	if e.imageDupCache == nil {
		e.imageDupCache = make(map[string]imageDupCacheEntry)
	}
	e.imageDupCache[a.ID] = imageDupCacheEntry{tolerance: tolerance, result: res}
	e.imageDupMu.Unlock()

	return res, nil
}

// lookupLogoBounds returns the cached ContentBounds result for the given logo
// file path and modification time. Returns false if no cached entry exists.
func (e *Engine) lookupLogoBounds(filePath string, modTime time.Time) (logoBoundsCacheEntry, bool) {
	key := logoBoundsCacheKey{filePath: filePath, modTime: modTime}
	e.logoBoundsCacheMu.Lock()
	defer e.logoBoundsCacheMu.Unlock()
	entry, ok := e.logoBoundsCache[key]
	return entry, ok
}

// storeLogoBounds saves a ContentBounds result in the bounded cache. When the
// cache is at capacity, the oldest insertion is evicted to make room.
func (e *Engine) storeLogoBounds(filePath string, modTime time.Time, entry logoBoundsCacheEntry) {
	key := logoBoundsCacheKey{filePath: filePath, modTime: modTime}
	e.logoBoundsCacheMu.Lock()
	defer e.logoBoundsCacheMu.Unlock()
	if e.logoBoundsCache == nil {
		e.logoBoundsCache = make(map[logoBoundsCacheKey]logoBoundsCacheEntry)
	}
	// If the key already exists, update in place without growing the key list.
	if _, exists := e.logoBoundsCache[key]; exists {
		e.logoBoundsCache[key] = entry
		return
	}
	// Evict the oldest entry when at capacity.
	if len(e.logoBoundsCache) >= maxLogoBoundsCacheSize {
		oldest := e.logoBoundsCacheKeys[0]
		e.logoBoundsCacheKeys = e.logoBoundsCacheKeys[1:]
		delete(e.logoBoundsCache, oldest)
	}
	e.logoBoundsCache[key] = entry
	e.logoBoundsCacheKeys = append(e.logoBoundsCacheKeys, key)
}

// calculateHealthScore returns the percentage of rules passed, rounded to 1 decimal.
func calculateHealthScore(passed, total int) float64 {
	if total == 0 {
		return 100.0
	}
	score := (float64(passed) / float64(total)) * 100.0
	return math.Round(score*10) / 10
}
