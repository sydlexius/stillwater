package rule

import (
	"container/list"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sydlexius/stillwater/internal/provider"
)

// DefaultPassCacheSize is the maximum number of cache entries held by a
// PassContext. When the LRU reaches this limit, the oldest-accessed entry is
// evicted before a new one is inserted.
//
// The value 500 is drawn from the issue spec and covers a typical Run-All
// pass over a multi-hundred-artist library where the same artist may be
// re-evaluated (e.g. dirty-after-fix) before the pass finishes. The ceiling
// is a memory guard: each entry holds a *provider.FetchResult which is a
// few hundred bytes for the metadata case and up to a few KB for the images
// case.
const DefaultPassCacheSize = 500

// PassContext holds the shared provider-fetch cache for a single
// RunAllScoped invocation. Its lifetime matches exactly one call to
// RunAllScoped: constructed at the top of that function, plumbed onto the
// context.Context, and falls out of scope when the function returns.
//
// EvaluationContext.dispatch looks for a PassContext on its context before
// checking its own per-artist local cache. On a hit the PassContext
// increments provider_cache_hit_total (the Phase 2 counter); on a miss
// EvaluationContext proceeds with its own singleflight publish, then
// stores the completed entry into the PassContext for the next artist that
// needs the same payload.
//
// IMPORTANT -- explicit invalidation only. A rule fix that writes changes
// for an artist (new image saved, NFO field updated) MUST call
// Invalidate(artistID, providerName) after the write so that any subsequent
// re-evaluation of that artist does not reuse the pre-fix provider payload.
// The compiler cannot enforce this convention; it is documented here so
// future rule authors are aware of the contract. There is NO automatic
// change-detection invalidation (out of scope for Phase 2).
//
// Thread safety: all methods are safe for concurrent use. The internal
// mutex guards both the LRU list and the map; it is held only for the
// minimum span needed to read or mutate those structures. Callers MUST NOT
// hold the PassContext mutex while calling back into EvaluationContext
// methods (deadlock risk: EvaluationContext.dispatch holds its own mutex
// while consulting the PassContext).
type PassContext struct {
	mu   sync.Mutex
	size int
	// lru is the eviction-order list; elements hold *passEntry.
	lru     *list.List
	entries map[evalCacheKey]*list.Element

	// Atomic counters. cacheHitTotal counts successful cross-artist
	// cache hits (distinct from provider_fetch_dedup_total which
	// counts within-artist dedup via EvaluationContext's own cache).
	// evictionTotal and invalidationTotal are informational for
	// end-of-pass logging.
	cacheHitTotal     atomic.Uint64
	evictionTotal     atomic.Uint64
	invalidationTotal atomic.Uint64

	logger *slog.Logger
}

// passEntry is the value stored in the LRU list. It carries the cache key
// (so Invalidate can locate entries by prefix scan) and the full cache
// entry pointer from EvaluationContext.
type passEntry struct {
	key   evalCacheKey
	entry *evalCacheEntry
}

// NewPassContext constructs a fresh PassContext with the given capacity.
// Passing size <= 0 uses DefaultPassCacheSize. logger may be nil (falls
// back to slog.Default). The context.Context returned by
// WithPassContext(parent, pc) is what gets passed into RunAllScoped's
// processArtist closure.
func NewPassContext(size int, logger *slog.Logger) *PassContext {
	if size <= 0 {
		size = DefaultPassCacheSize
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PassContext{
		size:    size,
		lru:     list.New(),
		entries: make(map[evalCacheKey]*list.Element, size),
		logger:  logger.With(slog.String("component", "rule.passctx")),
	}
}

// getOrReserve looks up key in the LRU. There are three possible outcomes:
//
//  1. The key is absent: a new in-flight placeholder is inserted under the
//     lock and returned together with reserved=true. The caller MUST
//     complete the placeholder (populate its fields and close its done
//     channel) and then call finalize to update the LRU accounting.
//
//  2. The key exists with an in-flight placeholder (done not yet closed):
//     the entry is returned with reserved=false. The caller waits on
//     entry.done and then reads the payload -- the singleflight contract
//     guarantees exactly one upstream fetch per key across all concurrent
//     EvaluationContext instances.
//
//  3. The key exists with a completed entry (done already closed):
//     the entry is moved to the front (LRU touch), cacheHitTotal is
//     incremented, and the entry is returned with reserved=false.
//
// This three-outcome design extends the EvaluationContext singleflight
// pattern to the pass-scoped layer so concurrent goroutines (each with
// their own EvaluationContext) that race on the same key see exactly one
// upstream provider call.
func (p *PassContext) getOrReserve(key evalCacheKey) (entry *evalCacheEntry, reserved bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if elem, ok := p.entries[key]; ok {
		p.lru.MoveToFront(elem)
		existing := elem.Value.(*passEntry).entry
		// cacheHitTotal is NOT incremented here. The dispatch caller in
		// eval_context.go owns the counter unconditionally: a single
		// Add(1) after <-entry.done returns covers both the already-
		// completed case (done was closed before this return) and the
		// in-flight-then-completed case (done closes after the upstream
		// fetch this caller will now wait on). Counting here would
		// double-count for the already-completed case.
		return existing, false
	}

	// Reserve a slot with an in-flight placeholder. This must happen
	// under the lock so concurrent callers that arrive before the
	// upstream fetch completes find the placeholder and wait rather
	// than independently dispatching their own fetch.
	placeholder := &evalCacheEntry{done: make(chan struct{})}

	// Evict the LRU entry when at capacity, but never evict an in-flight
	// placeholder. An entry whose done channel is still open has goroutines
	// waiting on it; evicting it would let a later getOrReserve for the
	// same key insert a fresh placeholder and dispatch a second upstream
	// fetch, breaking the exactly-one-upstream-fetch-per-key invariant.
	// Walk from the LRU tail toward the front and pick the oldest entry
	// whose fetch has already completed. If every entry is in-flight
	// (unlikely in practice; fetches complete in milliseconds), skip
	// eviction this round and let the cache temporarily exceed p.size --
	// the size bound is a hint, not a hard invariant.
	if p.lru.Len() >= p.size {
		for elem := p.lru.Back(); elem != nil; elem = elem.Prev() {
			pe := elem.Value.(*passEntry)
			if isClosedChan(pe.entry.done) {
				p.lru.Remove(elem)
				delete(p.entries, pe.key)
				p.evictionTotal.Add(1)
				break
			}
		}
	}

	elem := p.lru.PushFront(&passEntry{key: key, entry: placeholder})
	p.entries[key] = elem
	return placeholder, true
}

// finalize is called by the reserver (the caller that received reserved=true
// from getOrReserve) after the upstream fetch has been completed and
// close(placeholder.done) has been called. It updates the pass-cache hit
// counter for any goroutines that found the placeholder in-flight and are
// now returning via the wait path -- those goroutines call
// cacheHitTotal.Add(1) themselves after the wait, so finalize does NOT
// increment the counter here.
//
// Currently finalize has no extra work to do (the placeholder is already
// in the LRU from getOrReserve), but it exists as an extension point and
// to make the two-phase contract explicit at the call site.
func (p *PassContext) finalize(_ evalCacheKey) {
	// No-op for now; reserved for future use (e.g. size recalculation
	// after a large FetchResult payload is populated into the placeholder).
}

// peek returns the cache entry for key if present, or nil if absent. Unlike
// getOrReserve, peek does NOT insert a placeholder when the key is absent;
// it is purely read-only (but does move a found entry to the front of the
// LRU to record recency). Used by tests to assert cache state without
// altering it.
func (p *PassContext) peek(key evalCacheKey) *evalCacheEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	elem, ok := p.entries[key]
	if !ok {
		return nil
	}
	p.lru.MoveToFront(elem)
	return elem.Value.(*passEntry).entry
}

// Invalidate removes all cache entries for the given artistID that are
// associated with the given providerName. Because providerName is embedded
// in the evalCacheKey.detail string (via providerIDFingerprint) rather than
// stored as a separate field, invalidation scans all entries whose
// artistID matches and whose detail contains the providerName string.
//
// For example, if providerName is "audiodb" and a FetchImages key was built
// with detail = "mbid|audiodb=audio-1|...", that entry matches and is
// removed. Entries for the same artistID but a different provider (e.g.
// a Search key whose detail is the artist name) are NOT removed unless the
// artist name happens to contain the providerName string -- callers that
// need finer-grained invalidation should call InvalidateArtist instead.
//
// This is a linear scan (O(n) over matching artistID entries). At the
// expected cache sizes (<= 500 entries) and invalidation frequencies (rare
// -- only when a fix actually mutates provider-sourced state), the cost is
// negligible compared to a provider network call.
func (p *PassContext) Invalidate(artistID string, providerName provider.ProviderName) {
	needle := string(providerName)
	p.mu.Lock()
	for key, elem := range p.entries {
		if key.artistID != artistID {
			continue
		}
		// The providerName appears in the detail string for every
		// method that builds its key via providerIDFingerprint (images,
		// metadata, field). Search uses the artist name as detail and
		// is not provider-specific, so it is intentionally NOT removed
		// here -- a re-search for the same artist name is still valid.
		if strings.Contains(key.detail, needle) {
			p.lru.Remove(elem)
			delete(p.entries, key)
			p.invalidationTotal.Add(1)
		}
	}
	p.mu.Unlock()
}

// InvalidateArtist removes ALL cache entries for the given artistID
// regardless of method or provider. Use this when a fix has mutated
// the artist in a way that invalidates all cached provider data (e.g. a
// new MBID was assigned).
func (p *PassContext) InvalidateArtist(artistID string) {
	p.mu.Lock()
	for key, elem := range p.entries {
		if key.artistID == artistID {
			p.lru.Remove(elem)
			delete(p.entries, key)
			p.invalidationTotal.Add(1)
		}
	}
	p.mu.Unlock()
}

// Counters returns a snapshot of the pass-level cache counters.
// cacheHits: entries served from the shared pass cache (cross-artist re-use).
// evictions: LRU evictions due to capacity pressure.
// invalidations: individual entry removals due to explicit Invalidate calls.
func (p *PassContext) Counters() (cacheHits, evictions, invalidations uint64) {
	if p == nil {
		return 0, 0, 0
	}
	return p.cacheHitTotal.Load(), p.evictionTotal.Load(), p.invalidationTotal.Load()
}

// passContextKeyType is the unexported key for PassContext in context.Context.
type passContextKeyType struct{}

var passContextKey = passContextKeyType{}

// WithPassContext returns a child context carrying pc. The PassContext is
// retrieved by EvaluationContext.dispatch via passContextFromContext.
func WithPassContext(parent context.Context, pc *PassContext) context.Context {
	if pc == nil {
		return parent
	}
	return context.WithValue(parent, passContextKey, pc)
}

// passContextFromContext retrieves the PassContext stored on ctx, or nil
// if none is present. Used by EvaluationContext.dispatch to decide whether
// to consult the shared pass cache before its own per-artist cache.
func passContextFromContext(ctx context.Context) *PassContext {
	if ctx == nil {
		return nil
	}
	pc, _ := ctx.Value(passContextKey).(*PassContext)
	return pc
}

// isClosedChan reports whether ch has been closed, without consuming a
// value from it. Used by the LRU eviction path to skip in-flight cache
// placeholders (whose done channel is still open) so concurrent waiters
// are never stranded by a capacity-driven eviction.
func isClosedChan(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
