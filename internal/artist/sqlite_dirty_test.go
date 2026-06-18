package artist

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestListDirtyIDs_NeverEvaluated verifies that artists with NULL
// rules_evaluated_at are always considered dirty -- this is the
// initial-bootstrap path for fresh installs and newly imported artists.
func TestListDirtyIDs_NeverEvaluated(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Never Evaluated", "/music/never")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ids, err := svc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != a.ID {
		t.Fatalf("ListDirtyIDs = %v, want [%s]", ids, a.ID)
	}
}

// TestListDirtyIDs_CleanArtistOmitted verifies that an artist whose
// rules_evaluated_at is set and is not flagged dirty drops out of the
// dirty set -- the property that makes incremental Run Rules cheap.
func TestListDirtyIDs_CleanArtistOmitted(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Clean", "/music/clean")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := svc.MarkRulesEvaluated(ctx, a.ID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkRulesEvaluated: %v", err)
	}

	ids, err := svc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ListDirtyIDs = %v, want []", ids)
	}
}

// TestListDirtyIDs_DirtyAfterEvaluated verifies the round trip: evaluate
// (clean), mark dirty (mutated again), expect the artist back in the set.
func TestListDirtyIDs_DirtyAfterEvaluated(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Mutated", "/music/mutated")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	evalAt := time.Now().UTC()
	if err := svc.MarkRulesEvaluated(ctx, a.ID, evalAt); err != nil {
		t.Fatalf("MarkRulesEvaluated: %v", err)
	}

	// Use a strictly later timestamp so the comparison is unambiguous;
	// SQLite's TEXT timestamp comparison is lexicographic on RFC 3339
	// values, so a 1-second gap is sufficient and avoids any same-second
	// flakiness on very fast machines.
	if err := svc.MarkDirty(ctx, a.ID, evalAt.Add(time.Second)); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}

	ids, err := svc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != a.ID {
		t.Fatalf("ListDirtyIDs = %v, want [%s] after MarkDirty", ids, a.ID)
	}
}

// TestListDirtyIDs_ExcludesLockedAndExcluded verifies that artists with
// is_excluded=1 or locked=1 never appear in the dirty list, matching the
// pipeline's own skip semantics so progress counters are not inflated.
func TestListDirtyIDs_ExcludesLockedAndExcluded(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	excluded := testArtist("Excluded", "/music/excluded")
	excluded.IsExcluded = true
	if err := svc.Create(ctx, excluded); err != nil {
		t.Fatalf("creating excluded: %v", err)
	}

	locked := testArtist("Locked", "/music/locked")
	locked.Locked = true
	now := time.Now().UTC()
	locked.LockedAt = &now
	if err := svc.Create(ctx, locked); err != nil {
		t.Fatalf("creating locked: %v", err)
	}

	clean := testArtist("Eligible", "/music/eligible")
	if err := svc.Create(ctx, clean); err != nil {
		t.Fatalf("creating eligible: %v", err)
	}

	ids, err := svc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != clean.ID {
		t.Fatalf("ListDirtyIDs = %v, want [%s]", ids, clean.ID)
	}
}

// TestMarkAllDirty_StampsEligibleArtists verifies that MarkAllDirty
// touches every non-excluded, non-locked artist in a single statement.
// This is the path triggered when a brand-new rule is added.
func TestMarkAllDirty_StampsEligibleArtists(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	for i, name := range []string{"A", "B", "C"} {
		a := testArtist(name, "/music/"+name)
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist %d: %v", i, err)
		}
		// Mark all three as evaluated up front so they would otherwise
		// fall out of the dirty set; MarkAllDirty must put them back in.
		if err := svc.MarkRulesEvaluated(ctx, a.ID, time.Now().UTC()); err != nil {
			t.Fatalf("MarkRulesEvaluated: %v", err)
		}
	}

	excluded := testArtist("Excluded", "/music/excluded")
	excluded.IsExcluded = true
	if err := svc.Create(ctx, excluded); err != nil {
		t.Fatalf("creating excluded: %v", err)
	}

	// Also seed a locked artist so the rows-affected check guards the full
	// eligibility predicate (is_excluded = 0 AND locked = 0), not just the
	// excluded half. Without this, a regression that stamps locked rows
	// would still pass the downstream ListDirtyIDs assertion because
	// ListDirtyIDs itself filters locked artists out.
	locked := testArtist("Locked", "/music/locked")
	locked.Locked = true
	lockedNow := time.Now().UTC()
	locked.LockedAt = &lockedNow
	if err := svc.Create(ctx, locked); err != nil {
		t.Fatalf("creating locked: %v", err)
	}

	// Use a clearly later timestamp to ensure dirty_since > rules_evaluated_at.
	n, err := svc.MarkAllDirty(ctx, time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatalf("MarkAllDirty: %v", err)
	}
	if n != 3 {
		t.Fatalf("MarkAllDirty rows affected = %d, want 3 (A, B, C only; excluded + locked skipped)", n)
	}

	ids, err := svc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("ListDirtyIDs = %v (len %d), want 3 dirty artists", ids, len(ids))
	}
}

// TestCountEligibleArtists verifies the denominator count used by progress
// reporting matches what ListDirtyIDs/RunAll would walk in scope=all.
func TestCountEligibleArtists(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	for _, name := range []string{"A", "B", "C"} {
		a := testArtist(name, "/music/"+name)
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}
	excluded := testArtist("Excluded", "/music/excluded")
	excluded.IsExcluded = true
	if err := svc.Create(ctx, excluded); err != nil {
		t.Fatalf("creating excluded: %v", err)
	}
	locked := testArtist("Locked", "/music/locked")
	locked.Locked = true
	now := time.Now().UTC()
	locked.LockedAt = &now
	if err := svc.Create(ctx, locked); err != nil {
		t.Fatalf("creating locked: %v", err)
	}

	got, err := svc.CountEligibleArtists(ctx)
	if err != nil {
		t.Fatalf("CountEligibleArtists: %v", err)
	}
	if got != 3 {
		t.Fatalf("CountEligibleArtists = %d, want 3 (excluded and locked are skipped)", got)
	}
}

// TestUpdate_DoesNotClobberDirtyTracking verifies the ownership boundary
// between dirty tracking and the catch-all Update statement: a regular
// Update must not overwrite dirty_since/rules_evaluated_at, otherwise
// a write-then-event race would silently drop the dirty mark.
func TestUpdate_DoesNotClobberDirtyTracking(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Dirty Tracker", "/music/dirty")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	dirtyAt := time.Now().UTC()
	if err := svc.MarkDirty(ctx, a.ID, dirtyAt); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	evalAt := dirtyAt.Add(-time.Second) // intentionally older
	if err := svc.MarkRulesEvaluated(ctx, a.ID, evalAt); err != nil {
		t.Fatalf("MarkRulesEvaluated: %v", err)
	}

	// Mutate something irrelevant via the catch-all Update path. If
	// Update touched dirty_since/rules_evaluated_at, the artist would
	// either disappear from the dirty list or appear with a clobbered
	// timestamp.
	loaded, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	loaded.Biography = "irrelevant change"
	loaded.DirtySince = nil       // pretend the caller never read these
	loaded.RulesEvaluatedAt = nil // and would otherwise zero them
	if err := svc.Update(ctx, loaded); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reread, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID after Update: %v", err)
	}
	if reread.DirtySince == nil {
		t.Fatalf("dirty_since was cleared by Update; want preserved")
	}
	if reread.RulesEvaluatedAt == nil {
		t.Fatalf("rules_evaluated_at was cleared by Update; want preserved")
	}

	ids, err := svc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != a.ID {
		t.Fatalf("ListDirtyIDs after Update = %v, want [%s]", ids, a.ID)
	}
}

// TestUpdateAfterRuleEvaluation_DoesNotStampDirty pins the invariant that
// the rule pipeline's self-writeback path must not re-mark the artist dirty.
// If this regressed to calling markDirtyBestEffort, dirty_since would land
// one wall-clock second after the walker's startedAt on RFC3339 boundaries
// and the artist would re-appear in ListDirtyIDs immediately, flaking the
// scheduled sweep.
func TestUpdateAfterRuleEvaluation_DoesNotStampDirty(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Post-Evaluation", "/music/post-eval")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate the walker having just stamped rules_evaluated_at; the
	// artist is clean and must stay clean across the pipeline's own
	// health-score writeback.
	evalAt := time.Now().UTC()
	if err := svc.MarkRulesEvaluated(ctx, a.ID, evalAt); err != nil {
		t.Fatalf("MarkRulesEvaluated: %v", err)
	}

	loaded, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	loaded.Biography = "health-score writeback"
	if err := svc.UpdateAfterRuleEvaluation(ctx, loaded); err != nil {
		t.Fatalf("UpdateAfterRuleEvaluation: %v", err)
	}

	reread, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID after UpdateAfterRuleEvaluation: %v", err)
	}
	if reread.DirtySince != nil {
		t.Fatalf("dirty_since = %v, want nil (UpdateAfterRuleEvaluation must not stamp)", reread.DirtySince)
	}
	if reread.Biography != "health-score writeback" {
		t.Fatalf("Biography = %q, want the written value (Update body must still run)", reread.Biography)
	}

	ids, err := svc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ListDirtyIDs = %v, want []; UpdateAfterRuleEvaluation must not re-dirty the artist", ids)
	}
}

// TestLatestRulesEvaluatedAt_EmptyTable verifies that nil is returned when the
// artists table is empty -- the no-data bootstrap path.
func TestLatestRulesEvaluatedAt_EmptyTable(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	got, err := svc.LatestRulesEvaluatedAt(ctx)
	if err != nil {
		t.Fatalf("LatestRulesEvaluatedAt on empty table: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

// TestLatestRulesEvaluatedAt_AllNullEvals verifies that nil is returned when
// artists exist but none has been evaluated yet (rules_evaluated_at IS NULL).
func TestLatestRulesEvaluatedAt_AllNullEvals(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Unevaluated", "/music/unevaluated")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.LatestRulesEvaluatedAt(ctx)
	if err != nil {
		t.Fatalf("LatestRulesEvaluatedAt: %v", err)
	}
	if got != nil {
		t.Fatalf("all rules_evaluated_at NULL: want nil, got %v", got)
	}
}

// TestLatestRulesEvaluatedAt_ExcludedIgnored verifies that an excluded artist
// (is_excluded=1) with a newer stamp does not influence the result. Only
// non-excluded artists contribute to the MAX.
func TestLatestRulesEvaluatedAt_ExcludedIgnored(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	earlier := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)

	normal := testArtist("Normal", "/music/normal")
	if err := svc.Create(ctx, normal); err != nil {
		t.Fatalf("Create normal: %v", err)
	}
	if err := svc.MarkRulesEvaluated(ctx, normal.ID, earlier); err != nil {
		t.Fatalf("MarkRulesEvaluated normal: %v", err)
	}

	excluded := testArtist("Excluded", "/music/excluded")
	excluded.IsExcluded = true
	if err := svc.Create(ctx, excluded); err != nil {
		t.Fatalf("Create excluded: %v", err)
	}
	if err := svc.MarkRulesEvaluated(ctx, excluded.ID, later); err != nil {
		t.Fatalf("MarkRulesEvaluated excluded: %v", err)
	}

	got, err := svc.LatestRulesEvaluatedAt(ctx)
	if err != nil {
		t.Fatalf("LatestRulesEvaluatedAt: %v", err)
	}
	if got == nil {
		t.Fatal("want non-nil, got nil")
	}
	if !got.Equal(earlier) {
		t.Fatalf("excluded artist stamp leaked: got %v, want %v", got, earlier)
	}
}

// TestLatestRulesEvaluatedAt_ReturnsMax verifies that the chronological
// maximum is returned when multiple non-excluded artists have been evaluated.
func TestLatestRulesEvaluatedAt_ReturnsMax(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	stamps := []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour)}

	for i, ts := range stamps {
		a := testArtist(fmt.Sprintf("Artist%d", i), fmt.Sprintf("/music/%d", i))
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create artist %d: %v", i, err)
		}
		if err := svc.MarkRulesEvaluated(ctx, a.ID, ts); err != nil {
			t.Fatalf("MarkRulesEvaluated artist %d: %v", i, err)
		}
	}

	got, err := svc.LatestRulesEvaluatedAt(ctx)
	if err != nil {
		t.Fatalf("LatestRulesEvaluatedAt: %v", err)
	}
	if got == nil {
		t.Fatal("want non-nil, got nil")
	}
	want := stamps[len(stamps)-1]
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v (chronological MAX)", got, want)
	}
}

// TestLatestRulesEvaluatedAt_RoundTrip verifies that a value written by
// MarkRulesEvaluated survives storage as RFC3339 UTC and is returned with
// second-level precision intact by LatestRulesEvaluatedAt.
func TestLatestRulesEvaluatedAt_RoundTrip(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Truncate to seconds: SQLite stores RFC3339 which has 1-second resolution.
	stamp := time.Date(2025, 3, 15, 14, 30, 45, 0, time.UTC)

	a := testArtist("RoundTrip", "/music/roundtrip")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.MarkRulesEvaluated(ctx, a.ID, stamp); err != nil {
		t.Fatalf("MarkRulesEvaluated: %v", err)
	}

	got, err := svc.LatestRulesEvaluatedAt(ctx)
	if err != nil {
		t.Fatalf("LatestRulesEvaluatedAt: %v", err)
	}
	if got == nil {
		t.Fatal("want non-nil, got nil")
	}
	if !got.Equal(stamp) {
		t.Fatalf("round-trip mismatch: got %v, want %v", got, stamp)
	}
	if got.Location() != time.UTC {
		t.Fatalf("expected UTC location, got %v", got.Location())
	}
}

// TestUpdate_DoesStampDirty is the companion to the test above: a regular
// Update must stamp dirty_since so external mutations (API handlers,
// scanners, bulk executor) still schedule a re-evaluation. Together these
// two tests pin the boundary between external and self-writeback paths.
func TestUpdate_DoesStampDirty(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("External Mutation", "/music/external")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.MarkRulesEvaluated(ctx, a.ID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkRulesEvaluated: %v", err)
	}

	loaded, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	loaded.Biography = "external write"
	if err := svc.Update(ctx, loaded); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reread, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID after Update: %v", err)
	}
	if reread.DirtySince == nil {
		t.Fatalf("dirty_since was not stamped by Update; external mutations must re-schedule evaluation")
	}
}
