package artist

import (
	"context"
	"testing"
	"time"
)

// TestListDirtyIDs_NeverEvaluated verifies that artists with NULL
// rules_evaluated_at are always considered dirty -- this is the
// initial-bootstrap path for fresh installs and newly imported artists.
func TestListDirtyIDs_NeverEvaluated(t *testing.T) {
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
