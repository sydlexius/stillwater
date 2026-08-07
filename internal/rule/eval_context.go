package rule

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// EvalProvider is the union of provider-call surfaces the
// EvaluationContext coalesces. Defining it as an interface (rather than
// holding a *provider.Orchestrator) lets tests drive the production
// SetOrchestrator wiring with a stub instead of spinning up the full
// orchestrator dependency chain; the real *provider.Orchestrator
// satisfies it for production wiring.
type EvalProvider interface {
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
// Phase 2 lifetime (issue #1134): if the calling context carries a
// PassContext (see pass_context.go), dispatch first checks the pass-scoped
// LRU cache before its own per-artist cache. On a pass-cache hit the entry
// is promoted into the local cache so subsequent same-pass rules on the
// same artist also hit the fast path without re-consulting the PassContext.
// On a pass-cache miss, the completed entry is stored into the PassContext
// after the upstream fetch so that a future re-entry of the same artist
// (within the same RunAllScoped invocation) finds it there. The key already
// includes artist_id, so no struct changes are needed.
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
	orch     EvalProvider
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
//
// done is closed once the entry's payload fields are populated. The
// publishing goroutine inserts the placeholder under the cache lock,
// releases the lock, runs the actual fetch, populates the fields, and
// closes done. Late callers that find the placeholder in the cache wait
// on done before reading the payload -- the singleflight pattern that
// guarantees one upstream fetch per cache key even under parallel
// callers (the W4 telemetry decision depends on the counter staying
// honest under Phase 2 parallel evaluation).
type evalCacheEntry struct {
	fetch         *provider.FetchResult
	search        []provider.ArtistSearchResult
	field         []provider.FieldProviderResult
	releaseGroups []provider.ReleaseGroupInfo
	err           error
	done          chan struct{}
}

// alreadyDoneCh is a closed channel reused as the done signal for cache
// entries that are populated synchronously (notably the nil-orchestrator
// sentinel in dispatch). Sharing one closed channel keeps the hot path
// from allocating per call when we know the entry is already terminal.
var alreadyDoneCh = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// NewEvaluationContext constructs a fresh per-artist context. orch is the
// real provider orchestrator; logger is used to emit cache-hit / fetch
// debug records that future telemetry scrapes can pick up via the
// "evalctx" component tag.
//
// Passing a nil orchestrator is supported for tests that exercise paths
// not reaching a provider call -- methods will return a sentinel error
// instead of panicking.
func NewEvaluationContext(a *artist.Artist, orch EvalProvider, logger *slog.Logger) *EvaluationContext {
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
//
// Output layout: the canonical priority-order subset first (the common
// case from the existing ImageFixer cache key), then every remaining map
// key sorted alphabetically. The sort over extras keeps the fingerprint
// stable across Go's randomized map iteration so two equivalent maps
// produce the same string. Skipping the extras path -- as the first
// version of this function did -- silently collides any two calls that
// differ only in an unlisted provider name.
func providerIDFingerprint(ids map[provider.ProviderName]string) string {
	if len(ids) == 0 {
		return ""
	}
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
	seen := make(map[provider.ProviderName]struct{}, len(canonical))
	var buf []byte
	for _, n := range canonical {
		seen[n] = struct{}{}
		buf = append(buf, byte('|'))
		buf = append(buf, string(n)...)
		buf = append(buf, '=')
		buf = append(buf, ids[n]...)
	}
	extras := make([]string, 0, len(ids))
	for n := range ids {
		if _, isCanonical := seen[n]; isCanonical {
			continue
		}
		extras = append(extras, string(n))
	}
	sort.Strings(extras)
	for _, raw := range extras {
		buf = append(buf, byte('|'))
		buf = append(buf, raw...)
		buf = append(buf, '=')
		buf = append(buf, ids[provider.ProviderName(raw)]...)
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
		<-cached.done
		e.dedupTotal.Add(1)
		e.logger.Debug("provider fetch dedup",
			slog.String("method", "images"),
			slog.String("artist_id", e.artistID),
			slog.String("mbid", mbid),
		)
		return cached.fetch, cached.err
	}
	// Unlock before dispatch so unrelated keys can fetch concurrently.
	// dispatch publishes a singleflight placeholder under the lock and
	// closes its done channel after the upstream fetch completes; any
	// parallel caller that races past this fast-path miss will see the
	// placeholder under dispatch's own lock check, wait on done, and
	// dedup-count without re-issuing the upstream call.
	e.mu.Unlock()
	result, err := e.dispatch(ctx, key, func(fetchCtx context.Context) *evalCacheEntry {
		// fetchCtx, not ctx: the coalescer detached it, and re-capturing the
		// caller's ctx here would reintroduce #2880 verbatim. The timeout is
		// re-applied because WithoutCancel strips the caller's deadline.
		sharedCtx, cancel := context.WithTimeout(fetchCtx, fetchTimeout)
		defer cancel()
		fr, ferr := e.orch.FetchImages(sharedCtx, mbid, providerIDs)
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
		<-cached.done
		e.dedupTotal.Add(1)
		e.logger.Debug("provider fetch dedup",
			slog.String("method", "metadata"),
			slog.String("artist_id", e.artistID),
			slog.String("name", name),
		)
		return cached.fetch, cached.err
	}
	e.mu.Unlock()
	result, err := e.dispatch(ctx, key, func(fetchCtx context.Context) *evalCacheEntry {
		sharedCtx, cancel := context.WithTimeout(fetchCtx, fetchTimeout)
		defer cancel()
		fr, ferr := e.orch.FetchMetadata(sharedCtx, mbid, name, providerIDs)
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
		<-cached.done
		e.dedupTotal.Add(1)
		e.logger.Debug("provider fetch dedup",
			slog.String("method", "field"),
			slog.String("artist_id", e.artistID),
			slog.String("field", field),
		)
		return cached.field, cached.err
	}
	e.mu.Unlock()
	result, err := e.dispatch(ctx, key, func(fetchCtx context.Context) *evalCacheEntry {
		sharedCtx, cancel := context.WithTimeout(fetchCtx, fetchTimeout)
		defer cancel()
		results, ferr := e.orch.FetchFieldFromProviders(sharedCtx, mbid, name, field, providerIDs)
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
		<-cached.done
		e.dedupTotal.Add(1)
		e.logger.Debug("provider fetch dedup",
			slog.String("method", "search"),
			slog.String("artist_id", e.artistID),
			slog.String("name", name),
		)
		return cached.search, cached.err
	}
	e.mu.Unlock()
	result, err := e.dispatch(ctx, key, func(fetchCtx context.Context) *evalCacheEntry {
		sharedCtx, cancel := context.WithTimeout(fetchCtx, fetchTimeout)
		defer cancel()
		results, ferr := e.orch.Search(sharedCtx, name)
		return &evalCacheEntry{search: results, err: ferr}
	})
	return result.search, err
}

// GetReleaseGroups coalesces MusicBrainz release-group fetches keyed by
// (artist_id, mbid). Unlike FetchImages/FetchMetadata/FetchFieldFromProviders/
// Search, the release-group fetcher is NOT part of the EvalProvider surface:
// it is a separate registry-resolved MusicBrainz adapter held by the Engine
// (Engine.releaseGroupFetcher). The caller therefore supplies the fetch
// closure and the EvaluationContext contributes only the singleflight cache.
//
// This exists for issue #2476: a single scoped run of one artist invokes the
// discography_populated checker twice under the same per-artist context -- once
// in the pre-fix evaluation and once in the post-fix re-evaluation (see
// runForArtist in fixer.go). With the checker fetching directly on the Engine
// those two passes each queried MusicBrainz; routing through here collapses
// them to a single upstream call.
func (e *EvaluationContext) GetReleaseGroups(ctx context.Context, mbid string, fetch func(context.Context) ([]provider.ReleaseGroupInfo, error)) ([]provider.ReleaseGroupInfo, error) {
	if e == nil {
		return nil, errNilEvalContext
	}
	key := evalCacheKey{
		artistID: e.artistID,
		method:   "releasegroups",
		detail:   mbid,
	}
	e.mu.Lock()
	if cached, ok := e.cache[key]; ok {
		e.mu.Unlock()
		<-cached.done
		e.dedupTotal.Add(1)
		e.logger.Debug("provider fetch dedup",
			slog.String("method", "releasegroups"),
			slog.String("artist_id", e.artistID),
			slog.String("mbid", mbid),
		)
		return cached.releaseGroups, cached.err
	}
	e.mu.Unlock()
	// The caller's closure receives the coalescer-DETACHED context, not the
	// caller's own (#2880). Detaching strips the caller's DEADLINE along with
	// its cancellation, so this method applies fetchTimeout as a BACKSTOP --
	// not as the intended bound.
	//
	// The intended bound belongs to the closure, which knows the right value
	// (discography and the album gate both cap at discographyFetchTimeout,
	// which is shorter). Nested WithTimeout takes the MINIMUM, so a closure
	// that applies its own tighter cap is unaffected by this one: 30s backstop
	// + 15s call-site cap still expires at 15s. The backstop exists solely so
	// that a closure which forgets to cap itself runs BOUNDED rather than
	// forever -- before #2880 such a closure inherited the caller's deadline,
	// and detachment must not silently turn that into an unbounded fetch.
	result, err := e.dispatch(ctx, key, func(fetchCtx context.Context) *evalCacheEntry {
		boundedCtx, cancel := context.WithTimeout(fetchCtx, fetchTimeout)
		defer cancel()
		rgs, ferr := fetch(boundedCtx)
		return &evalCacheEntry{releaseGroups: rgs, err: ferr}
	})
	return result.releaseGroups, err
}

// detachedFetchContext returns the context a COALESCED upstream fetch must run
// under: the caller's VALUES, none of the caller's CANCELLATION (#2880).
//
// The failure this prevents: two rules evaluate the same artist concurrently
// and coalesce onto one upstream call. Rule A's context is canceled -- its
// artist finished, its deadline expired, its pass was aborted -- and, if the
// shared fetch ran under A's context, that fetch dies. Rule B never agreed to
// A's deadline, is still running, and gets a canceled error. Worse, dispatch
// CACHES that error under the cache key, so every later caller in the pass is
// served A's cancellation as though its own fetch had failed.
//
// context.WithoutCancel severs cancellation and deadline while KEEPING the
// values, which is load-bearing here: the EvaluationContext, any PassContext,
// and the metadata language preferences all ride the context downstream, and a
// context.Background() would silently drop them.
//
// NO TIMEOUT IS APPLIED HERE, deliberately. WithoutCancel strips the caller's
// deadline, so the fetch must be re-bounded -- but the right VALUE is
// per-call-site: the discography path caps at discographyFetchTimeout (15s)
// while the provider paths use fetchTimeout (30s). Each call site therefore
// applies its own, exactly as ruleAlbumGate.fetchGroups does (#2878).
//
// To be precise about why the cap is not simply set here instead: nested
// WithTimeout takes the MINIMUM, so a central 30s would NOT lengthen a call
// site that also caps at 15s -- it would just be redundant there. The failure
// case is a call site that applies NO cap of its own and would then silently
// inherit 30s where the caller had intended 15s. Keeping the value at the call
// site keeps that intent explicit and local. Each dispatch entry point still
// applies fetchTimeout as a BACKSTOP so a closure that forgets to cap itself
// is bounded rather than unbounded.
//
// The coalescer owns DETACHMENT; the call site owns DURATION.
func detachedFetchContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// dispatch is the singleflight publisher for a coalesced provider call.
// It guarantees exactly one fetch() per cache key by inserting an
// in-flight placeholder entry under the cache lock BEFORE running fetch;
// any parallel goroutine that arrives between the per-method fast-path
// miss and this point finds the placeholder under dispatch's own lock
// check, waits on placeholder.done, and dedup-counts without re-issuing
// the upstream call. This is the contract change that makes
// provider_fetch_total stay honest under Phase 2 (#1134) parallel
// evaluation -- without it, two racers both miss the initial check,
// both call fetch(), and the second one's increment inflates the
// telemetry by one with no compensating dedup hit (the Greptile P2
// finding).
//
// Phase 2 pass-cache path: before inserting a placeholder, dispatch
// checks whether the calling context carries a PassContext. If the
// PassContext has a completed entry for this key, the entry is promoted
// into the local per-artist cache and returned immediately (the Phase 2
// provider_cache_hit_total counter is incremented on the PassContext). On
// a successful upstream fetch the completed entry is stored into the
// PassContext so that future re-entries of the same artist in the same
// RunAllScoped pass find it there.
//
// nil-orchestrator path: surfaces the typed sentinel instead of a
// nil-deref panic inside fetch(). The sentinel entry is cached the same
// way any other failure is, so subsequent rules in the same pass do not
// retry; it uses the shared alreadyDoneCh so callers waiting on done
// observe the populated entry immediately.
//
// CONTEXT OWNERSHIP (#2880). fetch receives a context this function OWNS,
// never the caller's. dispatch is a singleflight: whoever loses the race
// waits on the winner's result, so the winner's fetch is SHARED work. A
// context belonging to one caller must therefore never reach it -- see
// detachedFetchContext for the failure mode. Callers that want their own
// cancellation keep it on their DIRECT (non-coalesced) branch, which never
// enters dispatch.
func (e *EvaluationContext) dispatch(ctx context.Context, key evalCacheKey, fetch func(context.Context) *evalCacheEntry) (*evalCacheEntry, error) {
	if e.orch == nil {
		entry := &evalCacheEntry{err: errNilEvalContext, done: alreadyDoneCh}
		e.mu.Lock()
		if existing, ok := e.cache[key]; ok {
			e.mu.Unlock()
			<-existing.done
			e.dedupTotal.Add(1)
			return existing, existing.err
		}
		e.cache[key] = entry
		e.mu.Unlock()
		return entry, entry.err
	}

	// Phase 2: consult the pass-scoped LRU cache before acquiring the
	// per-artist lock. getOrReserve returns one of three outcomes:
	//
	//   reserved=false, entry=nil: key not in pass cache (impossible;
	//     getOrReserve inserts a placeholder in this case).
	//
	//   reserved=false, entry non-nil: key already in pass cache
	//     (completed or in-flight). Wait on entry.done, then promote
	//     into the local cache and return. This is the cross-artist
	//     cache-hit path.
	//
	//   reserved=true: we own the placeholder; fall through to the
	//     upstream fetch and call finalize when done.
	passCtx := passContextFromContext(ctx)
	if passCtx != nil {
		passEntry, reserved := passCtx.getOrReserve(key)
		if !reserved {
			// Another EC already has (or is fetching) this key.
			// Wait for completion, promote into local cache, return.
			<-passEntry.done
			// dispatch is the sole incrementer of cacheHitTotal: this single
			// Add(1) counts both the already-completed case (entry.done was
			// already closed when getOrReserve returned) and the in-flight-
			// then-completed case (we just waited on entry.done above). The
			// getOrReserve path is deliberately silent on this counter; see
			// pass_context.go for the matching invariant.
			passCtx.cacheHitTotal.Add(1)
			e.mu.Lock()
			if _, alreadyLocal := e.cache[key]; !alreadyLocal {
				e.cache[key] = passEntry
			}
			e.mu.Unlock()
			e.dedupTotal.Add(1)
			e.logger.Debug("provider fetch pass-cache hit",
				slog.String("method", key.method),
				slog.String("artist_id", key.artistID),
			)
			return passEntry, passEntry.err
		}
		// reserved=true: we own the pass-cache placeholder (passEntry) for
		// this key. Re-use it directly as the per-artist placeholder below
		// so the pass-level and EC-level singleflight share one done
		// channel; after the upstream fetch we copy the result into
		// passEntry and call finalize.
		// Re-use the pass-cache placeholder as the per-artist placeholder
		// by inserting it directly into the local cache under the lock.
		// This way any goroutine using THIS EvaluationContext that races
		// on the same key also waits on the one shared done channel.
		e.mu.Lock()
		if cached, ok := e.cache[key]; ok {
			// Another goroutine using THIS EC snuck in; release the
			// pass-cache reservation by populating its placeholder from
			// the existing local entry (which is either completing or
			// complete).
			e.mu.Unlock()
			<-cached.done
			// Populate the pass-cache placeholder with the same result
			// so other ECs waiting on it get the right payload.
			passEntry.fetch = cached.fetch
			passEntry.search = cached.search
			passEntry.field = cached.field
			passEntry.releaseGroups = cached.releaseGroups
			passEntry.err = cached.err
			close(passEntry.done)
			passCtx.finalize(key)
			e.dedupTotal.Add(1)
			return cached, cached.err
		}
		// Insert the pass-cache placeholder into the per-artist local
		// cache so both the pass-level and the EC-level singleflight
		// point at the same done channel.
		e.cache[key] = passEntry
		e.mu.Unlock()

		// Fetch outside the lock, under a context detached from THIS caller:
		// the result is written into passEntry, which every other
		// EvaluationContext in the pass may be waiting on (#2880).
		filled := fetch(detachedFetchContext(ctx))
		passEntry.fetch = filled.fetch
		passEntry.search = filled.search
		passEntry.field = filled.field
		passEntry.releaseGroups = filled.releaseGroups
		passEntry.err = filled.err
		e.fetchTotal.Add(1)
		close(passEntry.done)
		passCtx.finalize(key)

		e.logger.Debug("provider fetch dispatched (pass-cache reserved)",
			slog.String("method", key.method),
			slog.String("artist_id", key.artistID),
		)
		return passEntry, passEntry.err
	}

	// Singleflight publish: re-check the cache under the lock to absorb
	// any racer that arrived between the per-method fast-path miss and
	// this point; if absent, insert a placeholder with an unclosed done
	// channel so late callers will wait rather than redispatch.
	e.mu.Lock()
	if cached, ok := e.cache[key]; ok {
		e.mu.Unlock()
		<-cached.done
		e.dedupTotal.Add(1)
		return cached, cached.err
	}
	placeholder := &evalCacheEntry{done: make(chan struct{})}
	e.cache[key] = placeholder
	e.mu.Unlock()

	// Fetch outside the lock so unrelated keys can fetch concurrently.
	// The placeholder is already published, so its identity is what
	// every caller will observe -- copying the populated fields into it
	// after the fetch is the standard singleflight finish.
	// This path is only reached when no PassContext is on ctx (single-artist
	// endpoints like RunRule, FixViolation). When a PassContext is present
	// the entire dispatch is handled in the pass-ctx block above, which
	// returns early in all code paths.
	//
	// Detached from THIS caller for the #2880 reason: the placeholder is
	// already published, so a racer on this same EvaluationContext may
	// already be waiting on it, and this caller's cancellation must not kill
	// the work that racer is awaiting.
	filled := fetch(detachedFetchContext(ctx))
	placeholder.fetch = filled.fetch
	placeholder.search = filled.search
	placeholder.field = filled.field
	placeholder.releaseGroups = filled.releaseGroups
	placeholder.err = filled.err
	e.fetchTotal.Add(1)
	close(placeholder.done)

	e.logger.Debug("provider fetch dispatched",
		slog.String("method", key.method),
		slog.String("artist_id", key.artistID),
	)
	return placeholder, placeholder.err
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
