package publish

import (
	"context"
	"testing"
)

// --- Plan outcomes (#3139) ------------------------------------------------

// TestPrunePlan_EveryEntryCarriesItsRealOutcome. Reporting a plan of two
// alongside backdrops_removed=1 gives an operator two numbers and no way to
// know which describes their library. Each entry carries what ACTUALLY
// happened to it, written by the same loop that performs the work, so the plan
// cannot drift into being a prediction filed next to a count.
func TestPrunePlan_EveryEntryCarriesItsRealOutcome(t *testing.T) {
	dup, distinct := []byte("AAA"), []byte("BBB")
	fake := &fakeBackdropClient{backdrops: [][]byte{dup, dup, dup, distinct}, failAt: -1, failDeleteAt: -1}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(),
		PlatformBackdropPruneScope{AllArtists: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(res.Plan) != 2 {
		t.Fatalf("plan has %d entries, want 2", len(res.Plan))
	}
	deleted := 0
	for _, e := range res.Plan {
		switch e.Outcome {
		case PrunePlanDeleted:
			deleted++
		case "":
			t.Errorf("plan entry %+v carries no outcome; an entry with no outcome is a claim with no evidence", e)
		default:
			t.Errorf("plan entry %+v: outcome %q, want %q", e, e.Outcome, PrunePlanDeleted)
		}
	}
	// The cross-check that makes the outcomes load-bearing rather than
	// decorative: they must agree with the counter the API also reports.
	if deleted != res.BackdropsRemoved {
		t.Errorf("%d entries marked deleted but BackdropsRemoved = %d; the plan and the count disagree, which is exactly the defect", deleted, res.BackdropsRemoved)
	}
}

// TestPrunePlan_ASkippedEntryIsRecordedAsSkippedNotDeleted. A pre-delete
// re-verify that refuses an entry must SAY so.
//
// Asserted alongside a sibling entry that really is deleted, in the same run.
// A fixture where everything is skipped would pass just as well against code
// that labeled every entry "skipped" unconditionally.
func TestPrunePlan_ASkippedEntryIsRecordedAsSkippedNotDeleted(t *testing.T) {
	dup, intruder := []byte("AAA"), []byte("ZZZ")
	// A concurrent platform write replaces index 1's content after detection.
	// Index 1 is the LAST candidate deleted (descending order) and no delete
	// below it has occurred, so the verify knob addresses the slot it means to.
	fake := &fakeBackdropClient{
		backdrops:      [][]byte{dup, dup, dup},
		failAt:         -1,
		failDeleteAt:   -1,
		mutateAtVerify: map[int][]byte{1: intruder},
	}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(),
		PlatformBackdropPruneScope{AllArtists: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(res.Plan) != 2 {
		t.Fatalf("plan has %d entries, want 2", len(res.Plan))
	}
	var deleted, skipped int
	for _, e := range res.Plan {
		switch e.Outcome {
		case PrunePlanDeleted:
			deleted++
			if e.Index == 1 {
				t.Errorf("index 1's content changed since detection but its entry claims %q; a refused delete reported as a delete is worse than no report", PrunePlanDeleted)
			}
		case PrunePlanSkipped:
			skipped++
			if e.Index != 1 {
				t.Errorf("entry for index %d marked skipped, but only index 1 was written to concurrently", e.Index)
			}
		default:
			t.Errorf("entry %+v: unexpected outcome %q", e, e.Outcome)
		}
	}
	// BOTH halves, from one run: the guard fired exactly once, and the
	// untouched sibling still went through.
	if skipped != 1 {
		t.Errorf("%d entries skipped, want exactly 1", skipped)
	}
	if deleted != 1 {
		t.Errorf("%d entries deleted, want exactly 1; a run where nothing succeeds cannot distinguish a working guard from a broken tier", deleted)
	}
	if res.SkippedChanged != 1 {
		t.Errorf("SkippedChanged = %d, want 1", res.SkippedChanged)
	}
	if deleted != res.BackdropsRemoved {
		t.Errorf("%d entries marked deleted but BackdropsRemoved = %d", deleted, res.BackdropsRemoved)
	}
}

// TestPrunePlan_AFailedDeleteIsRecordedAsFailedAndStopsTheConnection. The
// remaining entries keep the "planned" outcome they were created with, which
// is the honest record: they were never attempted.
func TestPrunePlan_AFailedDeleteIsRecordedAsFailedAndStopsTheConnection(t *testing.T) {
	dup := []byte("AAA")
	// Deletes run 3, 2, 1. The delete of index 2 fails, so index 1 is never
	// attempted.
	fake := &fakeBackdropClient{backdrops: [][]byte{dup, dup, dup, dup}, failAt: -1, failDeleteAt: 2}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(),
		PlatformBackdropPruneScope{AllArtists: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	got := map[int]string{}
	for _, e := range res.Plan {
		got[e.Index] = e.Outcome
	}
	want := map[int]string{3: PrunePlanDeleted, 2: PrunePlanFailed, 1: PrunePlanPlanned}
	for idx, w := range want {
		if got[idx] != w {
			t.Errorf("index %d: outcome %q, want %q (full plan: %+v)", idx, got[idx], w, res.Plan)
		}
	}
	if res.BackdropsRemoved != 1 {
		t.Errorf("BackdropsRemoved = %d, want 1", res.BackdropsRemoved)
	}
}

// TestPrunePlan_MultiArtistOutcomesStayWithTheirOwnArtist is the hostile-
// review Finding 2 reproduction (#3157, ported into this split): a mutation
// replacing pruneOneArtist's `planBase := len(result.Plan)` with a constant 0
// leaves the ENTIRE ./internal/publish/ suite green, because every other
// plan-outcome test in this file is built on
// newTestPublisherWithOneArtistOnePlatform -- a single artist on a single
// connection, where planBase is 0 on every real run too, so the mutant and
// the correct code are indistinguishable to them.
//
// planBase is what lets pruneOneArtist's delete loop address ITS OWN entries
// in result.Plan via `entry := &result.Plan[planBase+i]` after a PRIOR
// artist/connection has already appended entries ahead of it. Forcing it to 0
// makes that indexing alias back into whichever entries happen to occupy the
// front of the slice -- for a second artist processed after a first with the
// same redundant-entry count, that is the FIRST artist's own entry. So under
// the mutant, artist two's delete loop writes its Outcome onto artist one's
// Plan entry (a write with no visible effect if artist one's own outcome
// happens to end up the same value) while artist two's REAL entry -- appended
// later in the slice, never touched by the aliased pointer -- keeps
// whatever Outcome it was created with ("planned") and never learns that its
// backdrop actually was deleted. That is exactly the plan/count disagreement
// the `outcome` field exists to prevent: a caller trusting artist two's plan
// entry over BackdropsRemoved would wrongly believe nothing was deleted for
// that artist.
//
// newScopedPruneTestPublisher (backdrop_prune_scope_test.go) is reused rather
// than inventing a second multi-artist fixture: it already wires two
// independent artist/connection/client triples ("a1"/p1 and "a2"/p2) exactly
// as this needs, keyed by platform artist ID so each artist's fake genuinely
// only sees its own backdrops.
func TestPrunePlan_MultiArtistOutcomesStayWithTheirOwnArtist(t *testing.T) {
	one := &fakeBackdropClient{backdrops: dupPair(), failAt: -1, failDeleteAt: -1}
	two := &fakeBackdropClient{backdrops: dupPair(), failAt: -1, failDeleteAt: -1}
	p := newScopedPruneTestPublisher(t, map[string]*fakeBackdropClient{"p1": one, "p2": two})

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(),
		PlatformBackdropPruneScope{AllArtists: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	// PRECONDITIONS: both connections must actually have deleted their one
	// redundant backdrop, or the outcome assertions below would be checking
	// entries against a run that never really happened.
	if len(one.deleted) != 1 {
		t.Fatalf("precondition: artist one's connection deleted %v, want exactly one delete", one.deleted)
	}
	if len(two.deleted) != 1 {
		t.Fatalf("precondition: artist two's connection deleted %v, want exactly one delete", two.deleted)
	}
	if res.BackdropsRemoved != 2 {
		t.Fatalf("precondition: BackdropsRemoved = %d, want 2 (one per artist)", res.BackdropsRemoved)
	}
	if len(res.Plan) != 2 {
		t.Fatalf("precondition: plan has %d entries, want 2 (one per artist)", len(res.Plan))
	}

	// THE ASSERTION: each artist's OWN plan entry -- looked up by ArtistID,
	// never by raw slice position, since the whole point is that position
	// alone is not trustworthy under the mutant -- must report "deleted".
	// Under the planBase-forced-to-0 mutant, artist two's real entry is never
	// touched by its own delete loop and is left at "planned".
	byArtist := map[string]PlatformBackdropPrunePlanEntry{}
	for _, e := range res.Plan {
		byArtist[e.ArtistID] = e
	}
	for _, artistID := range []string{"a1", "a2"} {
		e, ok := byArtist[artistID]
		if !ok {
			t.Fatalf("no plan entry found for artist %q; full plan: %+v", artistID, res.Plan)
		}
		if e.Outcome != PrunePlanDeleted {
			t.Errorf("artist %q's plan entry %+v: outcome %q, want %q -- its own delete succeeded but its plan entry does not say so", artistID, e, e.Outcome, PrunePlanDeleted)
		}
	}
}
