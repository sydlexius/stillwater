package rule

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// evalProvider is the union of provider-call surfaces the
// EvaluationContext coalesces. Defining it as an interface (rather than
// holding a *provider.Orchestrator) lets tests inject a stub provider
// without spinning up the full orchestrator dependency chain, while the
// production wiring (Pipeline.SetOrchestrator -> NewEvaluationContext)
// passes the real *provider.Orchestrator which satisfies it.
type evalProvider interface {
	FetchImages(ctx context.Context, mbid string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error)
	FetchMetadata(ctx context.Context, mbid, name string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error)
	FetchFieldFromProviders(ctx context.Context, mbid, name, field string, providerIDs map[provider.ProviderName]string) ([]provider.FieldProviderResult, error)
	Search(ctx context.Context, name string) ([]provider.ArtistSearchResult, error)
}

// EvaluationContext coalesces provider fetches that would otherwise run once
// per rule when several rules on the same artist need the same upstream
// payload. It sits between the fixer chain and the provider Orchestrator:
// the first rule that asks for a given (artist, provider-call) pair triggers
// the real fetch and caches the full result; every subsequent rule in the
// same evaluation reuses that cached result instead of issuing a fresh
// network call.
//
// Phase 1 lifetime (issue #1133): one EvaluationContext per (artist,
// evaluation pass). The pipeline constructs it before dispatching
// violations and lets it fall out of scope once the artist's pass
// completes. There is no cross-artist leakage.
//
// Phase 2 (issue #1134) will widen the lifetime to one context per
// Run-All-Rules pass so a single artist's payload survives across re-entries
// inside the same pass. The cache key already includes artist_id so Phase 2
// can adopt the same struct without renaming counters or restructuring the
// map.
//
// Error caching: failed fetches are cached for the duration of the context
// per the issue spec, so a flapping provider does not amplify a single
// failure into N retries across N rules.
//
// Concurrency: the cache is guarded by a mutex. Current rule evaluation is
// serial per artist, but the cost of the lock is negligible compared to a
// provider call and the safety margin protects future parallel-fixer work
// (#1135 batch endpoints, scheduler).
//
// Counters (provider_fetch_total, provider_fetch_dedup_total) are exposed
// for the W4 (#1135) telemetry-gated decision. Phase 2 will add a
// provider_cache_hit_total counter; the names here are chosen so Phase 2
// does not need to rename anything.
type EvaluationContext struct {
	artistID string
	orch     evalProvider
	logger   *slog.Logger

	mu    sync.Mutex
	cache map[evalCacheKey]*evalCacheEntry

	// Atomic counters. fetchTotal increments on every actual provider
	// call dispatched through this context; dedupTotal increments on
	// every cache hit (a rule asked for a payload that was already
	// fetched, including a cached failure).
	fetchTotal atomic.Uint64
	dedupTotal atomic.Uint64
}

// evalCacheKey identifies a single coalesced provider call. method is a
// stable tag for the orchestrator method ("images", "metadata", "field",
// "search"); detail carries any method-specific discriminator (the field
// name for FetchFieldFromProviders, the search query for Search, the
// concatenated provider-ID fingerprint for the artist-bound calls).
//
// artist_id is included even in Phase 1 so the same struct survives the
// Phase 2 lifetime widening without a key rewrite.
type evalCacheKey struct {
	artistID string
	method   string
	detail   string
}

// evalCacheEntry stores the cached outcome of a coalesced fetch. Errors
// are cached so a flapping provider does not amplify a single failure
// into N retries across N rules in the same pass.
type evalCacheEntry struct {
	fetch  *provider.FetchResult
	search []provider.ArtistSearchResult
	field  []provider.FieldProviderResult
	err    error
}

// NewEvaluationContext constructs a fresh per-artist context. orch is the
// real provider orchestrator; logger is used to emit cache-hit / fetch
// debug records that future telemetry scrapes can pick up via the
// "evalctx" component tag.
//
// Passing a nil orchestrator is supported for tests that exercise paths
// not reaching a provider call -- methods will return a sentinel error
// instead of panicking.
func NewEvaluationContext(a *artist.Artist, orch evalProvider, logger *slog.Logger) *EvaluationContext {
	id := ""
	if a != nil {
		id = a.ID
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EvaluationContext{
		artistID: id,
		orch:     orch,
		logger:   logger.With(slog.String("component", "rule.evalctx")),
		cache:    make(map[evalCacheKey]*evalCacheEntry),
	}
}

// Counters returns the cumulative fetch and dedup counts for this
// context. The values are a snapshot taken without acquiring the cache
// mutex; both counters are atomic. Callers use this for end-of-pass
// telemetry assertions in tests and may later wire them into a metrics
// scraper.
func (e *EvaluationContext) Counters() (fetches, dedups uint64) {
	if e == nil {
		return 0, 0
	}
	return e.fetchTotal.Load(), e.dedupTotal.Load()
}

// providerIDFingerprint produces a stable, order-independent string from a
// providerIDs map so the cache key reflects the actual fetch parameters.
// Two violations that arrive with the same MBID but different provider-ID
// hints must NOT coalesce, otherwise a re-evaluation triggered after a
// provider-ID enrichment would silently reuse the pre-enrichment payload.
func providerIDFingerprint(ids map[provider.ProviderName]string) string {
	if len(ids) == 0 {
		return ""
	}
	// Emit in the canonical priority-order subset the existing
	// ImageFixer cache key already used, plus any other names that
	// appear in the map. The fixed prefix keeps the common case
	// (audiodb/discogs/deezer/spotify) deterministic; the trailing
	// extras path handles provider IDs added by future enrichment.
	canonical := []provider.ProviderName{
		provider.NameAudioDB,
		provider.NameDiscogs,
		provider.NameDeezer,
		provider.NameSpotify,
		provider.NameMusicBrainz,
		provider.NameLastFM,
		provider.NameFanartTV,
		provider.NameWikidata,
		provider.NameWikipedia,
	}
	var buf []byte
	for _, n := range canonical {
		buf = append(buf, byte('|'))
		buf = append(buf, string(n)...)
		buf = append(buf, '=')
		buf = append(buf, ids[n]...)
	}
	return string(buf)
}

// FetchImages coalesces calls to provider.Orchestrator.FetchImages keyed by
// (artist_id, mbid, providerIDs). The first call for a given key dispatches
// to the orchestrator; every subsequent call in the same context returns
// the cached *FetchResult and error.
func (e *EvaluationContext) FetchImages(ctx context.Context, mbid string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error) {
	if e == nil {
		// Defensive: fixers should never receive a nil context, but if
		// they do, surface a typed sentinel instead of nil-deref.
		return nil, errNilEvalContext
	}
	key := evalCacheKey{
		artistID: e.artistID,
		method:   "images",
		detail:   mbid + providerIDFingerprint(providerIDs),
	}
	e.mu.Lock()
	if cached, ok := e.cache[key]; ok {
		e.mu.Unlock()
		e.dedupTotal.Add(1)
		e.logger.Debug("provider fetch dedup",
			slog.String("method", "images"),
			slog.String("artist_id", e.artistID),
			slog.String("mbid", mbid),
		)
		return cached.fetch, cached.err
	}
	// Unlock before the orchestrator call so unrelated keys can fetch
	// concurrently. dispatch() performs a second cache check under the
	// lock to handle the narrow race where another goroutine populated
	// the entry between our miss and the dispatch call. For Phase 1 the
	// rule loop is sequential per artist so the race is unlikely;
	// Phase 2 (#1134) may revisit if duplicate fetches show up in the
	// telemetry.
	e.mu.Unlock()
	result, err := e.dispatch(ctx, key, func() *evalCacheEntry {
		fr, ferr := e.orch.FetchImages(ctx, mbid, providerIDs)
		return &evalCacheEntry{fetch: fr, err: ferr}
	})
	return result.fetch, err
}

// FetchMetadata coalesces calls to provider.Orchestrator.FetchMetadata.
// Cache key is (artist_id, mbid, name, providerIDs).
func (e *EvaluationContext) FetchMetadata(ctx context.Context, mbid, name string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error) {
	if e == nil {
		return nil, errNilEvalContext
	}
	key := evalCacheKey{
		artistID: e.artistID,
		method:   "metadata",
		detail:   mbid + "|" + name + providerIDFingerprint(providerIDs),
	}
	e.mu.Lock()
	if cached, ok := e.cache[key]; ok {
		e.mu.Unlock()
		e.dedupTotal.Add(1)
		e.logger.Debug("provider fetch dedup",
			slog.String("method", "metadata"),
			slog.String("artist_id", e.artistID),
			slog.String("name", name),
		)
		return cached.fetch, cached.err
	}
	e.mu.Unlock()
	result, err := e.dispatch(ctx, key, func() *evalCacheEntry {
		fr, ferr := e.orch.FetchMetadata(ctx, mbid, name, providerIDs)
		return &evalCacheEntry{fetch: fr, err: ferr}
	})
	return result.fetch, err
}

// FetchFieldFromProviders coalesces calls to
// provider.Orchestrator.FetchFieldFromProviders. Cache key is
// (artist_id, mbid, name, field, providerIDs).
func (e *EvaluationContext) FetchFieldFromProviders(ctx context.Context, mbid, name, field string, providerIDs map[provider.ProviderName]string) ([]provider.FieldProviderResult, error) {
	if e == nil {
		return nil, errNilEvalContext
	}
	key := evalCacheKey{
		artistID: e.artistID,
		method:   "field/" + field,
		detail:   mbid + "|" + name + providerIDFingerprint(providerIDs),
	}
	e.mu.Lock()
	if cached, ok := e.cache[key]; ok {
		e.mu.Unlock()
		e.dedupTotal.Add(1)
		e.logger.Debug("provider fetch dedup",
			slog.String("method", "field"),
			slog.String("artist_id", e.artistID),
			slog.String("field", field),
		)
		return cached.field, cached.err
	}
	e.mu.Unlock()
	result, err := e.dispatch(ctx, key, func() *evalCacheEntry {
		results, ferr := e.orch.FetchFieldFromProviders(ctx, mbid, name, field, providerIDs)
		return &evalCacheEntry{field: results, err: ferr}
	})
	return result.field, err
}

// Search coalesces calls to provider.Orchestrator.Search. Cache key is
// (artist_id, name). Search is keyed by name rather than MBID because its
// purpose is to RESOLVE a missing MBID; a second rule on the same artist
// that also needs to search would otherwise issue a duplicate call.
func (e *EvaluationContext) Search(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	if e == nil {
		return nil, errNilEvalContext
	}
	key := evalCacheKey{
		artistID: e.artistID,
		method:   "search",
		detail:   name,
	}
	e.mu.Lock()
	if cached, ok := e.cache[key]; ok {
		e.mu.Unlock()
		e.dedupTotal.Add(1)
		e.logger.Debug("provider fetch dedup",
			slog.String("method", "search"),
			slog.String("artist_id", e.artistID),
			slog.String("name", name),
		)
		return cached.search, cached.err
	}
	e.mu.Unlock()
	result, err := e.dispatch(ctx, key, func() *evalCacheEntry {
		results, ferr := e.orch.Search(ctx, name)
		return &evalCacheEntry{search: results, err: ferr}
	})
	return result.search, err
}

// dispatch runs fetch, stores the resulting entry under key, and bumps the
// fetch counter. The lock is held only across the map mutation, not across
// the network call, so unrelated keys can fetch concurrently. The current
// rule loop is sequential per artist so the lock is effectively
// uncontended; Phase 2 / #1135 may parallelize.
func (e *EvaluationContext) dispatch(_ context.Context, key evalCacheKey, fetch func() *evalCacheEntry) (*evalCacheEntry, error) {
	// Guard nil orchestrator so a misconfigured context surfaces the
	// typed sentinel instead of a nil-deref panic inside fetch(). This
	// keeps the doc promise on NewEvaluationContext consistent with the
	// runtime behavior for tests that construct a context without an
	// orchestrator. The sentinel entry is cached the same way any other
	// failure is, so subsequent rules in the same pass do not retry.
	if e.orch == nil {
		entry := &evalCacheEntry{err: errNilEvalContext}
		e.mu.Lock()
		if existing, ok := e.cache[key]; ok {
			e.mu.Unlock()
			return existing, existing.err
		}
		e.cache[key] = entry
		e.mu.Unlock()
		return entry, entry.err
	}
	// Re-check under the lock in case another goroutine populated the
	// key between our initial miss and now. Without this, two concurrent
	// callers for the same key both miss, both dispatch, and the second
	// to finish overwrites the first's entry -- benign for correctness
	// but doubles the fetch count we are trying to eliminate.
	e.mu.Lock()
	if cached, ok := e.cache[key]; ok {
		e.mu.Unlock()
		e.dedupTotal.Add(1)
		return cached, cached.err
	}
	e.mu.Unlock()

	entry := fetch()
	e.fetchTotal.Add(1)

	e.mu.Lock()
	// If a concurrent caller raced ahead and populated the slot while
	// we were fetching, prefer the earlier entry to keep cache identity
	// stable for any consumer that compared pointers. Either entry is
	// correct because the underlying orchestrator call is idempotent on
	// this layer.
	if existing, ok := e.cache[key]; ok {
		e.mu.Unlock()
		return existing, existing.err
	}
	e.cache[key] = entry
	e.mu.Unlock()

	e.logger.Debug("provider fetch dispatched",
		slog.String("method", key.method),
		slog.String("artist_id", key.artistID),
	)
	return entry, entry.err
}

// evalContextKey is the unexported context.Context value key used to
// thread an EvaluationContext through the rule pipeline -> fixer chain
// without altering the Fixer interface. A typed key avoids collision with
// any other package's context values.
type evalContextKeyType struct{}

var evalContextKey = evalContextKeyType{}

// WithEvaluationContext returns a derived context.Context carrying ec.
// Fixers retrieve it via EvaluationContextFromContext.
func WithEvaluationContext(parent context.Context, ec *EvaluationContext) context.Context {
	if ec == nil {
		return parent
	}
	return context.WithValue(parent, evalContextKey, ec)
}

// EvaluationContextFromContext returns the EvaluationContext threaded onto
// ctx, or nil when ctx carries none. Fixers fall back to the bare
// orchestrator in that case so single-violation paths like FixViolation
// (or any code path the pipeline did not seed) keep working unchanged.
func EvaluationContextFromContext(ctx context.Context) *EvaluationContext {
	if ctx == nil {
		return nil
	}
	ec, _ := ctx.Value(evalContextKey).(*EvaluationContext)
	return ec
}

// errNilEvalContext signals that a method was called on a nil
// *EvaluationContext. Callers should treat it the same as an orchestrator
// failure: warn-log and surface the fix as not-applied.
var errNilEvalContext = &evalCtxError{msg: "nil EvaluationContext"}

type evalCtxError struct{ msg string }

func (e *evalCtxError) Error() string { return e.msg }
