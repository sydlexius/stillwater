package rule

import (
	"context"
	"database/sql"
	"image"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
)

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

	// apiImageCacheMu guards apiImageCache.
	apiImageCacheMu sync.Mutex
	// apiImageCache stores raw image bytes fetched via the platform API. This
	// avoids double-fetching between the checker (which reads the image to
	// measure padding) and the fixer (which reads it again to trim). Entries
	// are consumed (deleted) by ConsumeAPIImage when the fixer reads them,
	// preventing unbounded growth without requiring a global clear.
	apiImageCache map[apiImageCacheKey][]byte
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
	e.checkers[RuleBackdropSequencing] = e.makeBackdropSequencingChecker()
	e.checkers[RuleLogoPadding] = e.makeLogoPaddingChecker()
	return e
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

// ImageFetcher returns the engine's platform image fetcher, or nil if none
// is configured. Fixers use this accessor to fetch and upload images through
// platform APIs.
func (e *Engine) ImageFetcher() PlatformImageFetcher {
	return e.imageFetcher
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
	// Clear the shared-filesystem cache so each top-level Evaluate call gets
	// fresh data while avoiding N+1 queries within the same run. The API image
	// cache is NOT cleared here: it is keyed by (artistID, imageType) so
	// entries from different evaluations do not conflict, and the fixer consumes
	// entries via ConsumeAPIImage (delete-on-read) to prevent unbounded growth.
	e.sharedFSMu.Lock()
	e.sharedFSCache = nil
	e.sharedFSMu.Unlock()

	// Classical artists in skip mode get a perfect score with no evaluation
	if a.IsClassical && GetClassicalMode(ctx, e.db) == ClassicalModeSkip {
		return &EvaluationResult{
			ArtistID:    a.ID,
			ArtistName:  a.Name,
			HealthScore: 100.0,
		}, nil
	}

	rules, err := e.cachedRules(ctx)
	if err != nil {
		return nil, err
	}

	result := &EvaluationResult{
		ArtistID:   a.ID,
		ArtistName: a.Name,
	}

	for _, r := range rules {
		if !r.Enabled {
			continue
		}

		checker, ok := e.checkers[r.ID]
		if !ok {
			e.logger.Debug("no checker registered for rule", slog.String("rule_id", r.ID))
			continue
		}

		result.RulesTotal++

		v := checker(a, r.Config)
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

	result.HealthScore = calculateHealthScore(result.RulesPassed, result.RulesTotal)

	return result, nil
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
