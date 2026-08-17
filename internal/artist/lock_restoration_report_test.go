package artist

import (
	"context"
	"slices"
	"testing"
)

// #3037: Service.update REPORTS the fields the guard restored, so a caller can
// tell "my change landed" from "the guard reverted it". Update returns nil in
// both cases -- restore-and-continue is deliberate -- which is exactly why the
// report has to exist.
//
// Every assertion below is paired with a POSITIVE CONTROL that establishes the
// write genuinely reached the guard. Without it, an empty report proves nothing:
// a harness that never wrote anything produces the same empty slice.

// TestUpdateReportingLocks_NamesTheRestoredField is the headline property.
func TestUpdateReportingLocks_NamesTheRestoredField(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newTestDB(t))

	// POSITIVE CONTROL: the same write on an UNLOCKED artist changes the value
	// and reports nothing restored. If this fails the harness never reaches the
	// biography write, and the locked case's report would be empty for the wrong
	// reason.
	unlocked := seedLockGuardArtist(t, svc, "Unlocked", "original")
	unlocked.Biography = "an automated writer's replacement"
	restored, err := svc.UpdateReportingLocks(ctx, unlocked)
	if err != nil {
		t.Fatalf("control UpdateReportingLocks: %v", err)
	}
	if len(restored) != 0 {
		t.Fatalf("positive control FAILED: an UNLOCKED write reported %v restored, want none", restored)
	}
	got, err := svc.GetByID(ctx, unlocked.ID)
	if err != nil {
		t.Fatalf("control reload: %v", err)
	}
	if got.Biography != "an automated writer's replacement" {
		t.Fatalf("positive control FAILED: the UNLOCKED biography is %q, so this harness never reaches the write "+
			"and the locked assertion below would pass vacuously", got.Biography)
	}

	// The regression: a locked field's restoration is NAMED, and an unlocked
	// field changed by the same write is NOT (the report is per-field, not
	// "something was locked").
	locked := seedLockGuardArtist(t, svc, "Locked", "the operator wrote this", "biography")
	locked.Biography = "an automated writer's replacement"
	locked.Origin = "Somewhere, XX"
	restored, err = svc.UpdateReportingLocks(ctx, locked)
	if err != nil {
		t.Fatalf("UpdateReportingLocks: %v", err)
	}
	if !slices.Equal(restored, []string{"biography"}) {
		t.Fatalf("restored = %v, want [biography]. A caller reading a nil report treats a reverted "+
			"write as a successful one, which is the defect (#3037).", restored)
	}
	// The run paths use the sibling verb; both funnel through Service.update,
	// and a report on one and not the other would be an honesty hole in the
	// UNATTENDED path -- the one that repeats nightly.
	a2 := seedLockGuardArtist(t, svc, "Run Path", "pinned", "biography")
	a2.Biography = "replacement"
	runRestored, err := svc.UpdateAfterRuleEvaluationReportingLocks(ctx, a2)
	if err != nil {
		t.Fatalf("UpdateAfterRuleEvaluationReportingLocks: %v", err)
	}
	if !slices.Equal(runRestored, []string{"biography"}) {
		t.Fatalf("the run-path verb reported %v restored, want [biography]", runRestored)
	}
	stored, err := svc.GetByID(ctx, locked.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if stored.Biography != "the operator wrote this" {
		t.Errorf("biography = %q, want the pinned value", stored.Biography)
	}
	if stored.Origin != "Somewhere, XX" {
		t.Errorf("origin = %q, want the unlocked change to have landed", stored.Origin)
	}
}

// TestGuardedFieldSnapshot_DiffNamesOnlyChangedFields pins the intent-recovery
// half. The pipeline has no other way to learn which guarded fields a fixer
// touched -- a fixer declares nothing, it assigns onto the struct.
func TestGuardedFieldSnapshot_DiffNamesOnlyChangedFields(t *testing.T) {
	a := &Artist{Name: "Before", Biography: "before", Origin: "before"}
	before := GuardedFieldSnapshot(a)
	if len(before) == 0 {
		t.Fatal("precondition: the snapshot is empty, so the diff below cannot detect anything")
	}

	// Nothing changed yet: the diff must be empty, or every fix would look like
	// it touched every field and the reverted-fix test would be trivially true.
	if changed := ChangedGuardedFields(before, a); len(changed) != 0 {
		t.Fatalf("an unmutated artist reported %v changed, want none", changed)
	}

	a.Biography = "after"
	a.Origin = "after"
	a.Name = "Before" // unchanged on purpose: it must NOT appear
	if changed := ChangedGuardedFields(before, a); !slices.Equal(changed, []string{"biography", "origin"}) {
		t.Fatalf("changed = %v, want [biography origin] in sorted order", changed)
	}

	// A nil snapshot must report nothing rather than every field: a caller with
	// no baseline cannot substantiate an intent, and claiming one would let the
	// dismiss path fire on a fix it never measured.
	if changed := ChangedGuardedFields(nil, a); changed != nil {
		t.Errorf("a nil snapshot reported %v changed, want nil", changed)
	}
}
