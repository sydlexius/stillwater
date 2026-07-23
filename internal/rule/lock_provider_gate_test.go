package rule

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Issue #2754: the artist-level lock must suppress OUTBOUND PROVIDER TRAFFIC
// made on a locked artist's behalf.
//
// The reachability that makes this a real defect rather than a theoretical one:
// the lock explicitly still permits MANUAL edits; a manual edit publishes
// event.ArtistUpdated; main.go subscribes HealthSubscriber.HandleEvent to that
// event; evaluateArtist calls engine.Evaluate. So an operator who locks an
// artist and then makes a permitted edit causes Stillwater to issue a
// MusicBrainz request for that artist automatically, in the background, with no
// operator action.
//
// READ THIS BEFORE EDITING: every "zero provider calls" assertion below is
// paired with a POSITIVE CONTROL on an otherwise IDENTICAL unlocked artist. The
// stub fetcher is trivially never called when the harness is wrong (no MBID, no
// NFO, an empty discography, a disabled rule), so an unpaired zero-calls
// assertion proves nothing. If a control fails, fix the harness, not the
// assertion.

// lockedDiscographyArtist seeds an artist that genuinely REACHES the discography
// checker's provider call, with the lock set as requested. Both NFO
// preconditions are load-bearing: an MBID, or the checker returns before
// fetching; at least one album, or Signal 1 (empty discography) short-circuits
// and flags a violation WITHOUT a fetch.
func lockedDiscographyArtist(t *testing.T, name string, locked bool) *artist.Artist {
	t.Helper()
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name: name,
		Albums: []nfo.DiscographyAlbum{
			{Title: "Debut", MusicBrainzReleaseGroupID: "rg-1"},
		},
	})
	return &artist.Artist{
		ID:            "artist-" + name,
		Name:          name,
		Path:          dir,
		MusicBrainzID: "11111111-2222-3333-4444-555555555555",
		Locked:        locked,
	}
}

// underCoveringFetcher reports four Album release groups. Paired with the
// one-album NFO above that is 25% coverage, below the 50% default threshold, so
// an UNLOCKED artist in this scenario both calls the fetcher AND is flagged.
// That makes the locked case's difference unambiguous: it is the fetch that
// disappeared, not the scenario.
func underCoveringFetcher() *stubReleaseGroupFetcher {
	return &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			{ID: "rg-1", Title: "Debut", PrimaryType: "Album"},
			{ID: "rg-2", Title: "Second", PrimaryType: "Album"},
			{ID: "rg-3", Title: "Third", PrimaryType: "Album"},
			{ID: "rg-4", Title: "Fourth", PrimaryType: "Album"},
		},
	}
}

// TestDiscographyChecker_LockedArtistMakesNoReleaseGroupCall is the NON-
// INVOCATION proof: it asserts the fetcher's CALL COUNT, not a status field. A
// status assertion would still pass if the request went out and the result was
// merely discarded, which is precisely the behavior the maintainer objected to.
//
// The positive control is the same scenario with Locked=false and is mandatory:
// without it the whole test passes vacuously the moment the fetcher stops being
// reachable for anyone.
//
// REVERT-AND-RERUN: drop the lock check from releaseGroupFetcherFor and the
// locked case's call count becomes 1, taking this RED.
func TestDiscographyChecker_LockedArtistMakesNoReleaseGroupCall(t *testing.T) {
	ctx := context.Background()

	// POSITIVE CONTROL first: an UNLOCKED artist in the identical scenario must
	// make exactly one call and must be flagged.
	unlockedFetcher := underCoveringFetcher()
	unlockedEngine := newDiscographyTestEngine(unlockedFetcher)
	unlocked := lockedDiscographyArtist(t, "Unlocked", false)

	v := unlockedEngine.makeDiscographyChecker()(ctx, unlocked, RuleConfig{CoverageThreshold: 50})
	if unlockedFetcher.calls != 1 {
		t.Fatalf("positive control FAILED: an UNLOCKED artist made %d release-group call(s), want 1. "+
			"The fetcher is not reachable in this scenario, so the zero-calls assertion below "+
			"would pass vacuously. Fix the harness, not the assertion.", unlockedFetcher.calls)
	}
	if v == nil {
		t.Fatal("positive control FAILED: an UNLOCKED artist at 25% coverage was not flagged, " +
			"so the coverage branch did not actually run")
	}

	// The regression: a LOCKED artist, same scenario, must make no call at all.
	lockedFetcher := underCoveringFetcher()
	lockedEngine := newDiscographyTestEngine(lockedFetcher)
	locked := lockedDiscographyArtist(t, "Locked", true)

	lockedViolation := lockedEngine.makeDiscographyChecker()(ctx, locked, RuleConfig{CoverageThreshold: 50})
	if lockedFetcher.calls != 0 {
		t.Errorf("a LOCKED artist made %d release-group call(s), want 0. Stillwater issued an "+
			"outbound MusicBrainz request for an artist the operator declared finished (#2754).",
			lockedFetcher.calls)
	}

	// A skipped coverage check must not invent a violation. Nothing is wrong with
	// the artist; the check simply has no reliable upstream count to compare
	// against, which is byte-for-byte the situation the existing mbCount <= 0
	// branch already handles by accepting the NFO. Reporting a violation here
	// would tell the operator their locked artist is broken, and would offer a
	// Fixable action the lock forbids taking.
	if lockedViolation != nil {
		t.Errorf("a LOCKED artist was flagged with a coverage violation the checker could not "+
			"substantiate (no upstream count was fetched): %+v", lockedViolation)
	}
}

// TestDiscographyChecker_LockedArtistStillGetsEmptyDiscographySignal is the
// UNDER-gating counterpart: the lock suppresses the provider FETCH only, never
// local-state evaluation. Signal 1 reads the NFO off disk and needs no network,
// so a locked artist with a genuinely empty discography must STILL be flagged.
//
// Without this, a fix that skipped the checker wholesale for locked artists
// would look correct and silently stop reporting a real, locally-detectable
// defect.
func TestDiscographyChecker_LockedArtistStillGetsEmptyDiscographySignal(t *testing.T) {
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{Name: "Locked Empty"}) // zero albums

	fetcher := &stubReleaseGroupFetcher{}
	e := newDiscographyTestEngine(fetcher)

	a := &artist.Artist{
		ID: "artist-locked-empty", Name: "Locked Empty", Path: dir,
		MusicBrainzID: "mbid-abc", Locked: true,
	}
	v := e.makeDiscographyChecker()(context.Background(), a, RuleConfig{})
	if v == nil {
		t.Fatal("a LOCKED artist with an empty discography was not flagged; the lock must " +
			"suppress the provider FETCH, not local-state evaluation")
	}
	if v.RuleID != RuleDiscographyPopulated {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleDiscographyPopulated)
	}
	if fetcher.calls != 0 {
		t.Errorf("the empty-discography signal made %d call(s), want 0", fetcher.calls)
	}
}

// TestEvaluate_LockedArtistStillScoredWithoutProviderCalls is the over-gating
// guard at the ENGINE level, on a real DB-backed engine.
//
// Two things must hold simultaneously and they pull in opposite directions:
//
//   - zero outbound release-group calls for the locked artist, and
//   - a real health score, computed over a real (non-zero) rule denominator.
//
// A fix that gated at eligibility -- dropping provider-backed rules from the
// evaluated set, or skipping locked artists entirely -- would satisfy the first
// and quietly blank the health score of every locked artist in the library. That
// is why the score is asserted here rather than assumed.
func TestEvaluate_LockedArtistStillScoredWithoutProviderCalls(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	enableDiscographyRule(t, ruleSvc)
	engine, rg, _ := engineWithProviderStubs(t, ruleSvc, db)

	// POSITIVE CONTROL: an UNLOCKED artist reaches the stub through the full
	// engine path. This proves the wiring, the MBID and the rule enablement.
	unlocked := providerBackedArtist(t, "Engine Unlocked")
	unlockedResult, err := engine.Evaluate(ctx, unlocked)
	if err != nil {
		t.Fatalf("positive control: evaluating unlocked artist: %v", err)
	}
	if rg.calls == 0 {
		t.Fatal("positive control FAILED: an UNLOCKED artist made no release-group call through " +
			"engine.Evaluate. The stub is unreachable, so the zero-calls assertion below would " +
			"pass vacuously. Fix the harness, not the assertion.")
	}
	if unlockedResult.RulesTotal == 0 {
		t.Fatal("positive control FAILED: the unlocked evaluation considered zero rules")
	}

	// The locked artist: same shape, lock set.
	locked := providerBackedArtist(t, "Engine Locked")
	locked.Locked = true

	before := rg.calls
	lockedResult, err := engine.Evaluate(ctx, locked)
	if err != nil {
		t.Fatalf("evaluating locked artist: %v", err)
	}

	if got := rg.calls - before; got != 0 {
		t.Errorf("evaluating a LOCKED artist made %d release-group call(s); want 0 (#2754)", got)
	}

	// ...and it was still genuinely EVALUATED. Comparing against the unlocked
	// run's denominator is what makes this assertion sharp: an equal rule count
	// proves the lock removed a FETCH, not a RULE.
	if lockedResult.RulesTotal != unlockedResult.RulesTotal {
		t.Errorf("locked artist considered %d rules, unlocked considered %d; the lock must "+
			"suppress provider traffic, not rule eligibility",
			lockedResult.RulesTotal, unlockedResult.RulesTotal)
	}
	if lockedResult.HealthScore <= 0 {
		t.Errorf("locked artist got HealthScore %v; a locked artist must still receive a real "+
			"health score from the local-state rules", lockedResult.HealthScore)
	}
}
