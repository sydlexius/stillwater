package publish

import (
	"context"
	"testing"
)

// --- Dry run (#3139) ------------------------------------------------------

// TestPruneDryRun_DeletesNothingButReportsThePlan. The rehearsal is the only
// safety net before an irreversible sweep, so its no-delete guarantee is
// asserted by ATTEMPTING the run against a client that records every delete --
// and against a fixture the live run below is shown to actually reduce.
func TestPruneDryRun_DeletesNothingButReportsThePlan(t *testing.T) {
	dup, distinct := []byte("AAA"), []byte("BBB")
	fake := &fakeBackdropClient{backdrops: [][]byte{dup, dup, dup, distinct}, failAt: -1, failDeleteAt: -1}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(),
		PlatformBackdropPruneScope{AllArtists: true, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("a DRY RUN deleted %v on the platform", fake.deleted)
	}
	if len(fake.backdrops) != 4 {
		t.Fatalf("a DRY RUN changed the platform: %d backdrops remain, want 4", len(fake.backdrops))
	}
	if res.BackdropsRemoved != 0 || res.ArtistsProcessed != 0 {
		t.Errorf("dry run reported removed=%d processed=%d, want 0/0", res.BackdropsRemoved, res.ArtistsProcessed)
	}
	if !res.DryRun {
		t.Error("DryRun not echoed on the result; a rehearsal must be distinguishable from a run that deleted")
	}
	// The plan is the whole deliverable of a dry run. An empty plan would mean
	// the operator rehearsed and learned nothing -- and would pass the "deleted
	// nothing" assertion above just as happily.
	if len(res.Plan) != 2 {
		t.Fatalf("dry run produced %d plan entries, want 2", len(res.Plan))
	}
	for _, e := range res.Plan {
		if e.Outcome != PrunePlanPlanned {
			t.Errorf("dry-run entry %+v: outcome %q, want %q", e, e.Outcome, PrunePlanPlanned)
		}
		if e.Survivor != 0 {
			t.Errorf("entry %+v: survivor %d, want the lowest index of the byte-identical group", e, e.Survivor)
		}
	}
	if res.Plan[0].Index != 2 || res.Plan[1].Index != 1 {
		t.Errorf("plan indices = %d,%d; want 2,1 (descending, the delete order)", res.Plan[0].Index, res.Plan[1].Index)
	}
}
