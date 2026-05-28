package rule

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// countingEvalProvider records every call so a test can assert that N
// rule invocations coalesce into a single upstream call when they share
// an EvaluationContext. Each method increments its own counter.
type countingEvalProvider struct {
	fetchImagesCalls atomic.Int64
	fetchMetaCalls   atomic.Int64
	fetchFieldCalls  atomic.Int64
	searchCalls      atomic.Int64

	// imagesResult is what FetchImages returns when set; zero values
	// otherwise are fine because the eval-ctx test never inspects the
	// payload, only the call count.
	imagesResult *provider.FetchResult
	imagesErr    error

	metaResult *provider.FetchResult
	metaErr    error

	fieldResult []provider.FieldProviderResult
	fieldErr    error

	searchResult []provider.ArtistSearchResult
	searchErr    error
}

func (c *countingEvalProvider) FetchImages(_ context.Context, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	c.fetchImagesCalls.Add(1)
	return c.imagesResult, c.imagesErr
}

func (c *countingEvalProvider) FetchMetadata(_ context.Context, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	c.fetchMetaCalls.Add(1)
	return c.metaResult, c.metaErr
}

func (c *countingEvalProvider) FetchFieldFromProviders(_ context.Context, _, _, _ string, _ map[provider.ProviderName]string) ([]provider.FieldProviderResult, error) {
	c.fetchFieldCalls.Add(1)
	return c.fieldResult, c.fieldErr
}

func (c *countingEvalProvider) Search(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	c.searchCalls.Add(1)
	return c.searchResult, c.searchErr
}

// TestEvaluationContext_CoalescesImageFetches is the spec's first
// acceptance criterion: N rules needing the same provider produce
// exactly one provider call per artist per pass.
func TestEvaluationContext_CoalescesImageFetches(t *testing.T) {
	count := &countingEvalProvider{
		imagesResult: &provider.FetchResult{Images: nil},
	}
	a := &artist.Artist{ID: "artist-1", Name: "Test", MusicBrainzID: "mbid-1"}
	ec := NewEvaluationContext(a, count, testLogger())

	// Simulate 5 rules calling FetchImages on the same (artist, mbid,
	// providerIDs) tuple within one evaluation pass.
	for i := 0; i < 5; i++ {
		_, err := ec.FetchImages(context.Background(), "mbid-1", map[provider.ProviderName]string{
			provider.NameAudioDB: "audio-1",
			provider.NameDiscogs: "dis-1",
		})
		if err != nil {
			t.Fatalf("FetchImages: %v", err)
		}
	}

	if got := count.fetchImagesCalls.Load(); got != 1 {
		t.Errorf("FetchImages dispatched %d times; want 1 (coalesced)", got)
	}
	fetches, dedups := ec.Counters()
	if fetches != 1 {
		t.Errorf("provider_fetch_total = %d; want 1", fetches)
	}
	if dedups != 4 {
		t.Errorf("provider_fetch_dedup_total = %d; want 4", dedups)
	}
}

// TestEvaluationContext_CoalescesMetadataAndField verifies the metadata
// and field-fetch surfaces coalesce on their own keys. A bio rule and a
// junk-bio rule both call FetchMetadata; an origin rule calls
// FetchFieldFromProviders. Each fetch surface dedups independently.
func TestEvaluationContext_CoalescesMetadataAndField(t *testing.T) {
	count := &countingEvalProvider{
		metaResult: &provider.FetchResult{},
	}
	a := &artist.Artist{ID: "artist-2", Name: "Test"}
	ec := NewEvaluationContext(a, count, testLogger())

	ids := map[provider.ProviderName]string{provider.NameMusicBrainz: "mb-1"}

	// Three FetchMetadata calls -- one for bio, one for junk-bio
	// re-fetch, one a hypothetical third metadata rule.
	for i := 0; i < 3; i++ {
		if _, err := ec.FetchMetadata(context.Background(), "mbid-2", "Test", ids); err != nil {
			t.Fatalf("FetchMetadata: %v", err)
		}
	}
	// Two FetchFieldFromProviders calls for the same field.
	for i := 0; i < 2; i++ {
		if _, err := ec.FetchFieldFromProviders(context.Background(), "mbid-2", "Test", "origin", ids); err != nil {
			t.Fatalf("FetchFieldFromProviders: %v", err)
		}
	}

	if got := count.fetchMetaCalls.Load(); got != 1 {
		t.Errorf("FetchMetadata dispatched %d times; want 1 (coalesced)", got)
	}
	if got := count.fetchFieldCalls.Load(); got != 1 {
		t.Errorf("FetchFieldFromProviders dispatched %d times; want 1 (coalesced)", got)
	}
	fetches, dedups := ec.Counters()
	if fetches != 2 {
		t.Errorf("provider_fetch_total = %d; want 2 (one metadata + one field)", fetches)
	}
	if dedups != 3 {
		t.Errorf("provider_fetch_dedup_total = %d; want 3 (2 meta hits + 1 field hit)", dedups)
	}
}

// TestEvaluationContext_DistinctKeysDoNotCoalesce verifies that the key
// includes mbid/name/providerIDs so semantically distinct calls do NOT
// reuse each other's payload. A second rule with a different MBID, name,
// or provider-ID fingerprint must trigger a fresh fetch.
func TestEvaluationContext_DistinctKeysDoNotCoalesce(t *testing.T) {
	count := &countingEvalProvider{
		imagesResult: &provider.FetchResult{},
	}
	a := &artist.Artist{ID: "artist-3", Name: "Test"}
	ec := NewEvaluationContext(a, count, testLogger())

	// Same MBID, different provider-ID hints.
	if _, err := ec.FetchImages(context.Background(), "mbid-3", map[provider.ProviderName]string{provider.NameDiscogs: "A"}); err != nil {
		t.Fatalf("Fetch1: %v", err)
	}
	if _, err := ec.FetchImages(context.Background(), "mbid-3", map[provider.ProviderName]string{provider.NameDiscogs: "B"}); err != nil {
		t.Fatalf("Fetch2: %v", err)
	}
	// Different MBID.
	if _, err := ec.FetchImages(context.Background(), "mbid-4", map[provider.ProviderName]string{provider.NameDiscogs: "A"}); err != nil {
		t.Fatalf("Fetch3: %v", err)
	}

	if got := count.fetchImagesCalls.Load(); got != 3 {
		t.Errorf("FetchImages dispatched %d times; want 3 (each key distinct)", got)
	}
}

// TestEvaluationContext_ErrorsAreCached verifies the spec's error
// semantics: "if a fetch fails, the failure is cached ... so subsequent
// rules in the same pass do not retry and compound the failure".
func TestEvaluationContext_ErrorsAreCached(t *testing.T) {
	sentinel := errors.New("provider down")
	count := &countingEvalProvider{
		imagesResult: nil,
		imagesErr:    sentinel,
	}
	a := &artist.Artist{ID: "artist-4", Name: "Test"}
	ec := NewEvaluationContext(a, count, testLogger())

	for i := 0; i < 4; i++ {
		_, err := ec.FetchImages(context.Background(), "mbid-5", nil)
		if !errors.Is(err, sentinel) {
			t.Fatalf("call %d returned %v; want sentinel", i, err)
		}
	}
	if got := count.fetchImagesCalls.Load(); got != 1 {
		t.Errorf("FetchImages dispatched %d times; want 1 (error cached)", got)
	}
	fetches, dedups := ec.Counters()
	if fetches != 1 || dedups != 3 {
		t.Errorf("counters = (%d, %d); want (1, 3)", fetches, dedups)
	}
}

// TestEvaluationContext_SearchCoalesces verifies the Search surface
// dedups on artist name. fixMBID may run for multiple rules in the same
// pass (defensive call sites); the duplicate Search would burn a slot of
// the MB rate limit budget for no extra information.
func TestEvaluationContext_SearchCoalesces(t *testing.T) {
	count := &countingEvalProvider{
		searchResult: []provider.ArtistSearchResult{{MusicBrainzID: "mb-result"}},
	}
	a := &artist.Artist{ID: "artist-5", Name: "Searchee"}
	ec := NewEvaluationContext(a, count, testLogger())

	for i := 0; i < 3; i++ {
		if _, err := ec.Search(context.Background(), "Searchee"); err != nil {
			t.Fatalf("Search: %v", err)
		}
	}
	if got := count.searchCalls.Load(); got != 1 {
		t.Errorf("Search dispatched %d times; want 1", got)
	}
}

// TestEvaluationContext_ContextPropagation verifies that
// WithEvaluationContext / EvaluationContextFromContext round-trips. The
// fixer-side coalescer helpers depend on this round-trip to find the
// active eval context.
func TestEvaluationContext_ContextPropagation(t *testing.T) {
	a := &artist.Artist{ID: "artist-6", Name: "Test"}
	ec := NewEvaluationContext(a, &countingEvalProvider{}, testLogger())

	if got := EvaluationContextFromContext(context.Background()); got != nil {
		t.Errorf("bare context returned %v; want nil", got)
	}
	ctx := WithEvaluationContext(context.Background(), ec)
	got := EvaluationContextFromContext(ctx)
	if got != ec {
		t.Errorf("EvaluationContextFromContext returned %v; want %v", got, ec)
	}

	// Nil pass-through: WithEvaluationContext(ctx, nil) returns ctx
	// unchanged so callers can use it unconditionally.
	if got := WithEvaluationContext(ctx, nil); got != ctx {
		t.Error("WithEvaluationContext(ctx, nil) modified ctx")
	}
}

// TestEvaluationContext_ConcurrentAccess exercises the cache mutex under
// parallel access. The current rule loop is sequential per artist but
// the lock is mandatory for Phase 2 (#1134) and #1135 forward-compat, so
// the test pins the race detector against parallel callers for the same
// key and for distinct keys.
func TestEvaluationContext_ConcurrentAccess(t *testing.T) {
	count := &countingEvalProvider{
		imagesResult: &provider.FetchResult{},
	}
	a := &artist.Artist{ID: "artist-7", Name: "Test"}
	ec := NewEvaluationContext(a, count, testLogger())

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Half the goroutines hit the same key (coalesce);
			// the other half use distinct MBIDs so the cache
			// inserts under contention.
			mbid := "shared-mbid"
			if i%2 == 1 {
				mbid = "mbid-" + string(rune('A'+i))
			}
			_, _ = ec.FetchImages(context.Background(), mbid, nil)
		}(i)
	}
	wg.Wait()

	// Shared-mbid: must collapse to 1 call. Distinct-mbid: 8 unique
	// keys -> 8 calls. Total upper bound is 9.
	got := count.fetchImagesCalls.Load()
	if got < 1 || got > 9 {
		t.Errorf("FetchImages dispatched %d times; want in [1, 9]", got)
	}
}

// TestEvaluationContext_NilSafe pins the nil-receiver safety: methods on
// a nil *EvaluationContext return errNilEvalContext rather than panicking.
// Production code should never construct a nil context, but the
// defensive guard simplifies future call sites that may forget to seed
// one.
func TestEvaluationContext_NilSafe(t *testing.T) {
	var ec *EvaluationContext
	if _, err := ec.FetchImages(context.Background(), "x", nil); err == nil {
		t.Error("nil receiver FetchImages returned nil error")
	}
	if _, err := ec.FetchMetadata(context.Background(), "x", "y", nil); err == nil {
		t.Error("nil receiver FetchMetadata returned nil error")
	}
	if _, err := ec.FetchFieldFromProviders(context.Background(), "x", "y", "f", nil); err == nil {
		t.Error("nil receiver FetchFieldFromProviders returned nil error")
	}
	if _, err := ec.Search(context.Background(), "x"); err == nil {
		t.Error("nil receiver Search returned nil error")
	}
	fetches, dedups := ec.Counters()
	if fetches != 0 || dedups != 0 {
		t.Errorf("nil counters = (%d, %d); want (0, 0)", fetches, dedups)
	}
}
