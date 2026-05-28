package rule

import (
	"context"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// pcPut is a test helper that inserts a completed entry into a PassContext
// using the two-phase getOrReserve / finalize API. It mirrors what
// EvaluationContext.dispatch does for the pass-cache reserved path.
// Using the internal API surface keeps tests honest about the contract.
func pcPut(pc *PassContext, key evalCacheKey, entry *evalCacheEntry) {
	placeholder, reserved := pc.getOrReserve(key)
	if !reserved {
		// Key already present; nothing to do.
		return
	}
	placeholder.fetch = entry.fetch
	placeholder.search = entry.search
	placeholder.field = entry.field
	placeholder.err = entry.err
	close(placeholder.done)
	pc.finalize(key)
}

// pcExists is a test helper that returns true if key is present in the
// PassContext (as a completed entry). Uses the read-only peek method so
// the check does not alter the LRU or insert placeholder entries.
func pcExists(pc *PassContext, key evalCacheKey) bool {
	entry := pc.peek(key)
	if entry == nil {
		return false
	}
	// Verify it is a completed entry (not an in-flight placeholder).
	select {
	case <-entry.done:
		return true
	default:
		return false
	}
}

// TestPassContext_LRUEviction verifies that the oldest-accessed entry is
// evicted when the LRU reaches capacity, and that newer entries survive.
func TestPassContext_LRUEviction(t *testing.T) {
	pc := NewPassContext(3, testLogger())

	completed := &evalCacheEntry{done: alreadyDoneCh}

	k1 := evalCacheKey{artistID: "a1", method: "images", detail: "d1"}
	k2 := evalCacheKey{artistID: "a2", method: "images", detail: "d2"}
	k3 := evalCacheKey{artistID: "a3", method: "images", detail: "d3"}
	k4 := evalCacheKey{artistID: "a4", method: "images", detail: "d4"}

	pcPut(pc, k1, completed)
	pcPut(pc, k2, completed)
	pcPut(pc, k3, completed)

	// Cache is now at capacity (3). Inserting k4 must evict k1 (the
	// oldest / least-recently-used since we inserted in FIFO order and
	// have not accessed k1 since).
	pcPut(pc, k4, completed)

	if pcExists(pc, k1) {
		t.Error("k1 should have been evicted as LRU but is still present")
	}
	if !pcExists(pc, k2) {
		t.Error("k2 should still be present after k1 eviction")
	}
	if !pcExists(pc, k3) {
		t.Error("k3 should still be present after k1 eviction")
	}
	if !pcExists(pc, k4) {
		t.Error("k4 should be present (just inserted)")
	}

	_, evictions, _ := pc.Counters()
	if evictions != 1 {
		t.Errorf("evictions = %d; want 1", evictions)
	}
}

// TestPassContext_LRUAccessOrderEviction verifies that accessing an entry
// promotes it in the LRU so a more-recently-used entry is not the one
// evicted when a new entry arrives.
func TestPassContext_LRUAccessOrderEviction(t *testing.T) {
	pc := NewPassContext(2, testLogger())

	completed := &evalCacheEntry{done: alreadyDoneCh}
	k1 := evalCacheKey{artistID: "a1", method: "images", detail: "d1"}
	k2 := evalCacheKey{artistID: "a2", method: "images", detail: "d2"}
	k3 := evalCacheKey{artistID: "a3", method: "images", detail: "d3"}

	pcPut(pc, k1, completed)
	pcPut(pc, k2, completed)

	// Access k1 so it becomes the most-recently-used.
	if !pcExists(pc, k1) {
		t.Fatal("k1 should be present before eviction test")
	}

	// Insert k3 -- now at capacity=2, so the LRU (k2) must be evicted,
	// not k1 which was just accessed.
	pcPut(pc, k3, completed)

	if pcExists(pc, k2) {
		t.Error("k2 should have been evicted (LRU after k1 was accessed)")
	}
	if !pcExists(pc, k1) {
		t.Error("k1 should survive (recently accessed)")
	}
	if !pcExists(pc, k3) {
		t.Error("k3 should be present (just inserted)")
	}
}

// TestPassContext_LRUSkipsInFlightEviction verifies that the LRU eviction
// path refuses to evict an in-flight placeholder (done channel still open),
// even when the cache is at capacity. Evicting an in-flight placeholder
// would orphan goroutines waiting on its done channel and let a later
// getOrReserve for the same key dispatch a second upstream fetch, breaking
// the exactly-one-upstream-fetch-per-key invariant the test suite asserts.
func TestPassContext_LRUSkipsInFlightEviction(t *testing.T) {
	pc := NewPassContext(2, testLogger())

	k1 := evalCacheKey{artistID: "a1", method: "images", detail: "d1"}
	k2 := evalCacheKey{artistID: "a2", method: "images", detail: "d2"}
	k3 := evalCacheKey{artistID: "a3", method: "images", detail: "d3"}

	// Reserve k1 and k2 without finalizing -- both placeholders remain
	// in-flight (their done channels stay open).
	p1, r1 := pc.getOrReserve(k1)
	_, r2 := pc.getOrReserve(k2)
	if !r1 || !r2 {
		t.Fatal("expected both initial reservations to return reserved=true")
	}

	// Reserve k3 -- at capacity, but every entry in the LRU is in-flight,
	// so eviction must be skipped. The cache temporarily exceeds size; no
	// in-flight placeholder is removed.
	_, r3 := pc.getOrReserve(k3)
	if !r3 {
		t.Fatal("expected k3 reservation to return reserved=true")
	}

	if pc.peek(k1) == nil {
		t.Error("k1 should still be present (in-flight, must not be evicted)")
	}
	if pc.peek(k2) == nil {
		t.Error("k2 should still be present (in-flight, must not be evicted)")
	}
	if pc.peek(k3) == nil {
		t.Error("k3 should be present (just reserved)")
	}

	_, evictions, _ := pc.Counters()
	if evictions != 0 {
		t.Errorf("evictions = %d; want 0 (every entry was in-flight)", evictions)
	}

	// Now finalize k1. k1 becomes evictable; k2 is still in-flight; k3 is
	// in-flight. Reserve a fourth key -- eviction must pick k1 (the only
	// completed entry), NOT k2 or k3.
	close(p1.done)
	pc.finalize(k1)

	k4 := evalCacheKey{artistID: "a4", method: "images", detail: "d4"}
	_, r4 := pc.getOrReserve(k4)
	if !r4 {
		t.Fatal("expected k4 reservation to return reserved=true")
	}

	if pc.peek(k1) != nil {
		t.Error("k1 should be evicted (it was the only completed entry)")
	}
	if pc.peek(k2) == nil {
		t.Error("k2 should survive (still in-flight)")
	}
	if pc.peek(k3) == nil {
		t.Error("k3 should survive (still in-flight)")
	}
	if pc.peek(k4) == nil {
		t.Error("k4 should be present (just reserved)")
	}

	_, evictions, _ = pc.Counters()
	if evictions != 1 {
		t.Errorf("evictions = %d; want 1 (k1 finalized, then evicted)", evictions)
	}
}

// TestPassContext_Invalidate_RemovesMatchingEntries verifies that
// Invalidate(artistID, providerName) removes only entries whose key
// matches that artistID AND whose detail contains the providerName string,
// and leaves unrelated entries untouched.
func TestPassContext_Invalidate_RemovesMatchingEntries(t *testing.T) {
	pc := NewPassContext(10, testLogger())
	completed := &evalCacheEntry{done: alreadyDoneCh}

	// FetchImages key for artist-1 that includes "audiodb" in detail.
	kImages := evalCacheKey{
		artistID: "artist-1",
		method:   "images",
		detail:   "mbid1|audiodb=audio-1|discogs=disc-1",
	}
	// FetchMetadata key for artist-1 that also includes "audiodb".
	kMeta := evalCacheKey{
		artistID: "artist-1",
		method:   "metadata",
		detail:   "mbid1|name1|audiodb=audio-1",
	}
	// Search key for artist-1 whose detail is the name (no provider tag).
	kSearch := evalCacheKey{
		artistID: "artist-1",
		method:   "search",
		detail:   "Artist One",
	}
	// Key for a different artist (must not be removed).
	kOther := evalCacheKey{
		artistID: "artist-2",
		method:   "images",
		detail:   "mbid2|audiodb=audio-2",
	}

	pcPut(pc, kImages, completed)
	pcPut(pc, kMeta, completed)
	pcPut(pc, kSearch, completed)
	pcPut(pc, kOther, completed)

	pc.Invalidate("artist-1", provider.NameAudioDB)

	if pcExists(pc, kImages) {
		t.Error("kImages should have been invalidated (audiodb in detail)")
	}
	if pcExists(pc, kMeta) {
		t.Error("kMeta should have been invalidated (audiodb in detail)")
	}
	// Search key's detail is "Artist One" which does not contain "audiodb".
	if !pcExists(pc, kSearch) {
		t.Error("kSearch should NOT be invalidated (no audiodb in detail)")
	}
	// Different artist must not be touched.
	if !pcExists(pc, kOther) {
		t.Error("kOther (different artist) must NOT be invalidated")
	}

	_, _, invalidations := pc.Counters()
	if invalidations != 2 {
		t.Errorf("invalidation count = %d; want 2 (kImages + kMeta)", invalidations)
	}
}

// TestPassContext_InvalidateArtist_RemovesAll verifies that
// InvalidateArtist removes every entry for the given artistID regardless
// of method or detail, and leaves other artists untouched.
func TestPassContext_InvalidateArtist_RemovesAll(t *testing.T) {
	pc := NewPassContext(10, testLogger())
	completed := &evalCacheEntry{done: alreadyDoneCh}

	k1 := evalCacheKey{artistID: "artist-1", method: "images", detail: "d1"}
	k2 := evalCacheKey{artistID: "artist-1", method: "search", detail: "name1"}
	k3 := evalCacheKey{artistID: "artist-2", method: "images", detail: "d3"}

	pcPut(pc, k1, completed)
	pcPut(pc, k2, completed)
	pcPut(pc, k3, completed)

	pc.InvalidateArtist("artist-1")

	if pcExists(pc, k1) {
		t.Error("k1 (artist-1) should be gone after InvalidateArtist")
	}
	if pcExists(pc, k2) {
		t.Error("k2 (artist-1) should be gone after InvalidateArtist")
	}
	if !pcExists(pc, k3) {
		t.Error("k3 (artist-2) must NOT be removed by InvalidateArtist(artist-1)")
	}
}

// TestPassContext_CrossArtistReuse is the headline acceptance test for
// Phase 2: a second artist that needs the same (method, detail) payload
// as a prior artist within one pass should produce zero additional
// provider calls because the PassContext already holds the result.
//
// The test also verifies the correctness invariant from the issue spec:
// cross-artist coalescing NEVER happens (different artistIDs produce
// different cache keys), so each artist still makes its own upstream
// call.
func TestPassContext_CrossArtistReuse(t *testing.T) {
	count := &countingEvalProvider{
		imagesResult: &provider.FetchResult{Images: nil},
	}

	pc := NewPassContext(DefaultPassCacheSize, testLogger())
	ctx := WithPassContext(context.Background(), pc)

	// Artist 1 evaluation.
	a1 := &artist.Artist{ID: "artist-1", Name: "Band One", MusicBrainzID: "mbid-shared"}
	ec1 := NewEvaluationContext(a1, count, testLogger())
	ctx1 := WithEvaluationContext(ctx, ec1)

	ids := map[provider.ProviderName]string{
		provider.NameAudioDB: "audio-1",
	}

	_, err := ec1.FetchImages(ctx1, "mbid-shared", ids)
	if err != nil {
		t.Fatalf("ec1 FetchImages: %v", err)
	}

	// One real fetch should have happened.
	if got := count.fetchImagesCalls.Load(); got != 1 {
		t.Fatalf("after ec1: FetchImages calls = %d; want 1", got)
	}

	// Artist 2 evaluation -- SAME mbid + providerIDs but different artistID.
	// The PassContext should NOT serve this: different artistID means a
	// different cache key. We expect a second upstream call.
	a2 := &artist.Artist{ID: "artist-2", Name: "Band Two", MusicBrainzID: "mbid-shared"}
	ec2 := NewEvaluationContext(a2, count, testLogger())
	ctx2 := WithEvaluationContext(ctx, ec2)

	_, err = ec2.FetchImages(ctx2, "mbid-shared", ids)
	if err != nil {
		t.Fatalf("ec2 FetchImages: %v", err)
	}

	// Two provider calls: one per artist (different artistID in the key).
	// This verifies the correctness check: cross-artist coalescing NEVER
	// happens even when mbid + providerIDs are identical.
	if got := count.fetchImagesCalls.Load(); got != 2 {
		t.Errorf("after ec2 (different artist): FetchImages calls = %d; want 2 (separate cache keys)", got)
	}

	// Pass-cache hit counter should still be 0: same key cannot exist
	// because the artistIDs differ.
	hits, _, _ := pc.Counters()
	if hits != 0 {
		t.Errorf("pass-cache hits = %d; want 0 (different artists have different keys)", hits)
	}
}

// TestPassContext_ReentryCoalescing verifies that a re-entry of the SAME
// artist within one pass (e.g. dirtied by a fix and re-evaluated) produces
// zero additional provider calls because the PassContext holds the prior
// result. This is the primary Phase 2 win scenario.
func TestPassContext_ReentryCoalescing(t *testing.T) {
	count := &countingEvalProvider{
		imagesResult: &provider.FetchResult{Images: nil},
	}

	pc := NewPassContext(DefaultPassCacheSize, testLogger())
	ctx := WithPassContext(context.Background(), pc)

	a := &artist.Artist{ID: "artist-reentry", Name: "Re-Entry Band", MusicBrainzID: "mbid-re"}
	ids := map[provider.ProviderName]string{provider.NameAudioDB: "audio-re"}

	// First pass: artist evaluated, provider call made, result stored in PassContext.
	ec1 := NewEvaluationContext(a, count, testLogger())
	ctx1 := WithEvaluationContext(ctx, ec1)
	_, err := ec1.FetchImages(ctx1, "mbid-re", ids)
	if err != nil {
		t.Fatalf("ec1 FetchImages: %v", err)
	}

	if got := count.fetchImagesCalls.Load(); got != 1 {
		t.Fatalf("after first pass: FetchImages calls = %d; want 1", got)
	}

	// Second pass (re-entry): a new EvaluationContext for the SAME artist.
	// It should find the result in the PassContext and not call the provider.
	ec2 := NewEvaluationContext(a, count, testLogger())
	ctx2 := WithEvaluationContext(ctx, ec2)
	_, err = ec2.FetchImages(ctx2, "mbid-re", ids)
	if err != nil {
		t.Fatalf("ec2 FetchImages: %v", err)
	}

	// Provider must still have been called exactly once.
	if got := count.fetchImagesCalls.Load(); got != 1 {
		t.Errorf("after re-entry: FetchImages calls = %d; want 1 (pass-cache hit)", got)
	}

	hits, _, _ := pc.Counters()
	if hits != 1 {
		t.Errorf("pass-cache hits = %d; want 1 (re-entry found cached result)", hits)
	}
}

// TestPassContext_InvalidationCausesRefetch verifies that after calling
// Invalidate, a re-entry of the same artist triggers a fresh provider
// call because the cached entry was removed.
func TestPassContext_InvalidationCausesRefetch(t *testing.T) {
	count := &countingEvalProvider{
		imagesResult: &provider.FetchResult{Images: nil},
	}

	pc := NewPassContext(DefaultPassCacheSize, testLogger())
	ctx := WithPassContext(context.Background(), pc)

	a := &artist.Artist{ID: "artist-inv", Name: "Invalidation Test"}
	ids := map[provider.ProviderName]string{provider.NameAudioDB: "audio-inv"}

	// First pass.
	ec1 := NewEvaluationContext(a, count, testLogger())
	ctx1 := WithEvaluationContext(ctx, ec1)
	_, err := ec1.FetchImages(ctx1, "mbid-inv", ids)
	if err != nil {
		t.Fatalf("ec1: %v", err)
	}
	if got := count.fetchImagesCalls.Load(); got != 1 {
		t.Fatalf("after first pass: calls = %d; want 1", got)
	}

	// Simulate a fix that changes audiodb-sourced state for this artist.
	pc.Invalidate("artist-inv", provider.NameAudioDB)

	// Re-entry after invalidation: must produce a fresh provider call.
	ec2 := NewEvaluationContext(a, count, testLogger())
	ctx2 := WithEvaluationContext(ctx, ec2)
	_, err = ec2.FetchImages(ctx2, "mbid-inv", ids)
	if err != nil {
		t.Fatalf("ec2: %v", err)
	}
	if got := count.fetchImagesCalls.Load(); got != 2 {
		t.Errorf("after invalidation + re-entry: calls = %d; want 2 (fresh fetch required)", got)
	}

	// Pass-cache hits counter should be 0: the only time we checked
	// the pass cache after invalidation it was empty, resulting in a
	// new reservation.
	hits, _, _ := pc.Counters()
	if hits != 0 {
		t.Errorf("pass-cache hits = %d; want 0 (invalidated before re-entry)", hits)
	}
}

// TestPassContext_ConcurrentAccess exercises the PassContext singleflight
// under parallel goroutines hitting overlapping keys. Each goroutine creates
// its own EvaluationContext (as RunAllScoped does per artist), but the
// PassContext's getOrReserve ensures that only ONE of the concurrent
// goroutines that share a key issues the actual upstream provider call;
// all others wait on the placeholder's done channel.
//
// This mirrors the shape of TestEvaluationContext_ConcurrentAccess:
// half the goroutines hit the same key, the other half use distinct keys.
// Expected total provider calls: 9 (8 unique + 1 shared).
func TestPassContext_ConcurrentAccess(t *testing.T) {
	count := &countingEvalProvider{
		imagesResult: &provider.FetchResult{},
	}

	pc := NewPassContext(DefaultPassCacheSize, testLogger())
	baseCtx := WithPassContext(context.Background(), pc)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// All even-index goroutines use the SAME artistID + mbid.
			// Odd-index goroutines use distinct IDs.
			artistID := "concurrent-artist"
			mbid := "shared-mbid"
			if i%2 == 1 {
				artistID = "concurrent-artist-" + string(rune('A'+i))
				mbid = "mbid-" + string(rune('A'+i))
			}
			a := &artist.Artist{ID: artistID, Name: artistID}
			ec := NewEvaluationContext(a, count, testLogger())
			ctx := WithEvaluationContext(baseCtx, ec)
			_, _ = ec.FetchImages(ctx, mbid, nil)
		}(i)
	}
	wg.Wait()

	// 8 goroutines share (concurrent-artist, shared-mbid) -> 1 call.
	// 8 goroutines have distinct artistIDs + mbids -> 8 calls.
	// Total: 9 provider calls, regardless of scheduling order, because
	// the PassContext's getOrReserve singleflight collapses the 8
	// concurrent reservations into one fetch.
	if got := count.fetchImagesCalls.Load(); got != 9 {
		t.Errorf("FetchImages dispatched %d times; want 9 (8 unique + 1 shared)", got)
	}
}

// TestPassContext_FallbackWithoutPassContext verifies that an
// EvaluationContext with no PassContext on its context behaves exactly as
// Phase 1: the per-artist local cache is the only cache, and the same
// provider call within one artist's evaluation is coalesced but a second
// artist does NOT benefit from the first's fetch.
func TestPassContext_FallbackWithoutPassContext(t *testing.T) {
	count := &countingEvalProvider{
		imagesResult: &provider.FetchResult{},
	}

	// No PassContext on the base context.
	ctx := context.Background()

	ids := map[provider.ProviderName]string{provider.NameAudioDB: "audio-a"}

	// Artist 1: 3 rules call FetchImages -> 1 provider call (per-artist dedup).
	a1 := &artist.Artist{ID: "fallback-a1", Name: "Artist 1"}
	ec1 := NewEvaluationContext(a1, count, testLogger())
	ctx1 := WithEvaluationContext(ctx, ec1)
	for i := 0; i < 3; i++ {
		if _, err := ec1.FetchImages(ctx1, "mbid-fb", ids); err != nil {
			t.Fatalf("ec1 call %d: %v", i, err)
		}
	}
	if got := count.fetchImagesCalls.Load(); got != 1 {
		t.Errorf("after a1: calls = %d; want 1 (per-artist coalesce)", got)
	}

	// Artist 2: same mbid + ids, but different artistID -> different key ->
	// fresh provider call (no PassContext to bridge across artists).
	a2 := &artist.Artist{ID: "fallback-a2", Name: "Artist 2"}
	ec2 := NewEvaluationContext(a2, count, testLogger())
	ctx2 := WithEvaluationContext(ctx, ec2)
	if _, err := ec2.FetchImages(ctx2, "mbid-fb", ids); err != nil {
		t.Fatalf("ec2: %v", err)
	}
	if got := count.fetchImagesCalls.Load(); got != 2 {
		t.Errorf("after a2 (no PassContext): calls = %d; want 2 (no cross-artist reuse)", got)
	}
}

// TestPassContext_NilSafe verifies that Counters() on a nil *PassContext
// returns zeros without panicking, matching the nil-safe contract of
// EvaluationContext.
func TestPassContext_NilSafe(t *testing.T) {
	var pc *PassContext
	hits, evictions, invalidations := pc.Counters()
	if hits != 0 || evictions != 0 || invalidations != 0 {
		t.Errorf("nil PassContext counters = (%d, %d, %d); want (0, 0, 0)",
			hits, evictions, invalidations)
	}
}

// TestPassContext_ContextRoundTrip verifies WithPassContext /
// passContextFromContext plumbing.
func TestPassContext_ContextRoundTrip(t *testing.T) {
	pc := NewPassContext(10, testLogger())

	if got := passContextFromContext(context.Background()); got != nil {
		t.Errorf("bare context returned %v; want nil", got)
	}

	ctx := WithPassContext(context.Background(), pc)
	if got := passContextFromContext(ctx); got != pc {
		t.Errorf("passContextFromContext returned %v; want %v", got, pc)
	}

	// Nil pass-through.
	if got := WithPassContext(ctx, nil); got != ctx {
		t.Error("WithPassContext(ctx, nil) should return ctx unchanged")
	}
}
