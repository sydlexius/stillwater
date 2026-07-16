package rule

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Issue #2476 (Item 4): a single scoped run of one artist invokes each
// provider-backed checker TWICE under one per-artist EvaluationContext -- once
// in the pre-fix evaluation and once in the post-fix re-evaluation (see
// runForArtist in fixer.go). Before this fix the checkers called the provider
// directly on the Engine, so those two passes each hit MusicBrainz and a
// library sweep issued ~one query per checker per artist where one would do.
// These tests pin that the checkers now route through the coalescer so the two
// passes collapse to a single upstream call.
//
// The tests invoke the checker closure twice under the SAME context, which is
// exactly what the two EvaluateScoped calls in runForArtist do (they share the
// ctx returned by withEvalContext). A non-nil EvalProvider is required only
// because the pipeline attaches an EvaluationContext at all only when an
// orchestrator is wired; the release-group fetch does not go through that
// provider.

// TestDiscographyChecker_CoalescesReleaseGroupFetchAcrossPasses is the Item-4
// acceptance test for the discography_populated checker. Two checker passes over
// one artist sharing one EvaluationContext must issue exactly one release-group
// fetch, not one per pass.
//
// Revert check: change fetchReleaseGroupsCoalesced back to the bare
// e.releaseGroupFetcher.GetReleaseGroups call and the fetcher is hit twice --
// this test then fails on `calls = 2`.
func TestDiscographyChecker_CoalescesReleaseGroupFetchAcrossPasses(t *testing.T) {
	dir := t.TempDir()
	// One album on disk against four MusicBrainz release groups -> 25% coverage,
	// below the 50% threshold, so the coverage branch (the ONLY branch that
	// fetches) runs and raises a violation on every pass.
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name:   "Test Artist",
		Albums: []nfo.DiscographyAlbum{{Title: "Debut", MusicBrainzReleaseGroupID: "rg-1"}},
	})

	fetcher := &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			{ID: "rg-1", Title: "Debut", PrimaryType: "Album"},
			{ID: "rg-2", Title: "Second", PrimaryType: "Album"},
			{ID: "rg-3", Title: "Third", PrimaryType: "Album"},
			{ID: "rg-4", Title: "Fourth", PrimaryType: "Album"},
		},
	}
	e := newDiscographyTestEngine(fetcher)
	checker := e.makeDiscographyChecker()

	a := &artist.Artist{ID: "art-1", Name: "Test Artist", Path: dir, MusicBrainzID: "mbid-abc"}
	// A single EvaluationContext for the whole pass, exactly as the pipeline
	// builds once per artist and shares between the pre-fix and post-fix
	// EvaluateScoped calls. Its orchestrator is unused by the release-group path.
	ec := NewEvaluationContext(a, &countingEvalProvider{}, testLogger())
	ctx := WithEvaluationContext(context.Background(), ec)

	cfg := RuleConfig{CoverageThreshold: 50}

	// PRECONDITION: both passes must raise the coverage violation, or the fetch
	// branch never runs and "calls == 1" would pass vacuously.
	v1 := checker(ctx, a, cfg)
	v2 := checker(ctx, a, cfg)
	if v1 == nil || v2 == nil {
		t.Fatalf("precondition: the coverage branch (the only one that fetches) must fire on both "+
			"passes; got v1=%v v2=%v", v1, v2)
	}

	if fetcher.calls != 1 {
		t.Errorf("release-group fetcher called %d times across two checker passes sharing one "+
			"EvaluationContext; want 1. The pre-fix and post-fix passes of a scoped run must "+
			"coalesce to a single MusicBrainz query (#2476).", fetcher.calls)
	}

	// The dedup counter is the coalescer's own witness that the second pass was
	// served from cache rather than re-fetched.
	if _, dedups := ec.Counters(); dedups != 1 {
		t.Errorf("EvaluationContext recorded %d dedups; want 1 (the second pass must hit the cache)", dedups)
	}
}

// TestDiscographyChecker_WithoutEvalContextStillFetches is the negative control
// for the test above: with no EvaluationContext on ctx (the single-violation
// path), each pass legitimately fetches. Without this, the coalescing test could
// pass simply because the fetcher was never reached.
func TestDiscographyChecker_WithoutEvalContextStillFetches(t *testing.T) {
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name:   "Test Artist",
		Albums: []nfo.DiscographyAlbum{{Title: "Debut", MusicBrainzReleaseGroupID: "rg-1"}},
	})
	fetcher := &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			{ID: "rg-1", PrimaryType: "Album"},
			{ID: "rg-2", PrimaryType: "Album"},
			{ID: "rg-3", PrimaryType: "Album"},
			{ID: "rg-4", PrimaryType: "Album"},
		},
	}
	e := newDiscographyTestEngine(fetcher)
	checker := e.makeDiscographyChecker()

	a := &artist.Artist{ID: "art-1", Name: "Test Artist", Path: dir, MusicBrainzID: "mbid-abc"}
	cfg := RuleConfig{CoverageThreshold: 50}

	// Bare context: no EvaluationContext attached.
	if v := checker(context.Background(), a, cfg); v == nil {
		t.Fatal("precondition: coverage violation must fire so the fetch branch runs")
	}
	if v := checker(context.Background(), a, cfg); v == nil {
		t.Fatal("precondition: coverage violation must fire so the fetch branch runs")
	}
	if fetcher.calls != 2 {
		t.Errorf("fetcher called %d times across two checker passes with NO EvaluationContext; "+
			"want 2 (nothing to coalesce against). If this is 1 the checker is caching outside the "+
			"per-pass context, which would leak stale release groups across artists.", fetcher.calls)
	}
}

// TestNameLanguageChecker_CoalescesMetadataFetchAcrossPasses is the Item-4
// acceptance test for the name_language_pref checker. Two checker passes over
// one artist sharing one EvaluationContext must issue exactly one metadata
// fetch.
//
// Revert check: change fetchMetadataCoalesced back to the bare
// e.metadataProvider.FetchMetadata call and the provider is hit twice -- this
// test then fails on `fetchMetaCalls = 2`.
func TestNameLanguageChecker_CoalescesMetadataFetchAcrossPasses(t *testing.T) {
	// The alias lookup returns a localized name so the checker builds a fixable
	// violation; the count, not the payload, is what this test asserts.
	prov := &countingEvalProvider{
		metaResult: &provider.FetchResult{
			Metadata: &provider.ArtistMetadata{Name: "Localized", SortName: "Localized"},
		},
	}
	e := &Engine{logger: testLogger(), metadataProvider: prov}
	checker := e.makeNameLanguagePrefChecker()

	// A Cyrillic name against an English preference is a script mismatch, so the
	// checker proceeds past the early-out and into lookupPreferredAlias (the
	// fetch). SortName is empty (ScriptUnknown, which always matches), leaving
	// Name as the sole mismatch.
	a := &artist.Artist{ID: "art-1", Name: "Пример", MusicBrainzID: "mbid-abc"}

	ec := NewEvaluationContext(a, prov, testLogger())
	ctx := WithEvaluationContext(
		provider.WithMetadataLanguages(context.Background(), []string{"en"}),
		ec,
	)

	// PRECONDITION: both passes must raise the violation, or the fetch never runs
	// and the count assertion is vacuous.
	v1 := checker(ctx, a, RuleConfig{})
	v2 := checker(ctx, a, RuleConfig{})
	if v1 == nil || v2 == nil {
		t.Fatalf("precondition: the script-mismatch violation must fire on both passes so the "+
			"alias fetch runs; got v1=%v v2=%v", v1, v2)
	}

	if got := prov.fetchMetaCalls.Load(); got != 1 {
		t.Errorf("metadata provider called %d times across two checker passes sharing one "+
			"EvaluationContext; want 1. The pre-fix and post-fix passes of a scoped run must "+
			"coalesce to a single provider call (#2476).", got)
	}
	if _, dedups := ec.Counters(); dedups != 1 {
		t.Errorf("EvaluationContext recorded %d dedups; want 1 (the second pass must hit the cache)", dedups)
	}
}
