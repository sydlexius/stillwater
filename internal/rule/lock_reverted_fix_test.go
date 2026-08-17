package rule

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// #3037: a fixer must not claim credit for a write the lock guard reverted.
//
// READ THIS BEFORE EDITING. Every "the row stayed open" assertion is PAIRED
// with a positive control on an otherwise identical UNLOCKED artist that must
// be RESOLVED. That branch is reachable only when a fixer genuinely ran and
// genuinely wrote a guarded field, so an unpaired assertion would pass the
// moment the harness stopped reaching the fixer at all.

// lockRevertFixture seeds one artist with `bio` and an OPEN, fixable
// bio_exists violation, pinning `lockFields` through the lock mutator -- not by
// setting LockedFields on the struct, which the chokepoint pins away.
func lockRevertFixture(t *testing.T, bio string, lockFields ...string) (*sql.DB, *artist.Service, *artist.Artist, *Service, *RuleViolation, context.Context) {
	t.Helper()
	db := setupTestDB(t)
	ctx := context.Background()

	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{Name: "Lock Revert Subject", SortName: "Lock Revert Subject", Path: t.TempDir(), Biography: bio}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if len(lockFields) > 0 {
		if err := artistSvc.SetLockedFields(ctx, a.ID, lockFields); err != nil {
			t.Fatalf("locking %v: %v", lockFields, err)
		}
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	// PRECONDITIONS: without both, the guard never fires and every later
	// assertion holds vacuously.
	if stored.Biography != bio {
		t.Fatalf("precondition: biography = %q, want %q", stored.Biography, bio)
	}
	if len(stored.LockedFields) != len(lockFields) {
		t.Fatalf("precondition: locked_fields = %v, want %v; the lock did not persist", stored.LockedFields, lockFields)
	}

	rv := &RuleViolation{
		RuleID:     RuleBioExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "error",
		Message:    "biography needs work",
		Fixable:    true,
		Status:     ViolationStatusOpen,
	}
	if err := ruleSvc.UpsertViolation(ctx, rv); err != nil {
		t.Fatalf("upserting violation: %v", err)
	}
	return db, artistSvc, stored, ruleSvc, rv, ctx
}

// TestFixViolation_LockRevertedFixLeavesTheRowOpen is the headline regression.
// A fixer whose ONLY change was reverted must not report Fixed and must not
// resolve the row.
//
// OPEN, not dismissed, and that is argued: a lock is operator-REVOCABLE, so the
// outcome is not terminal. A dismissed row has no un-dismiss route
// (UpsertViolation pins 'dismissed' per #1107; ReopenViolation only accepts
// 'resolved'), which would mean that after the operator unlocks, the finding
// never returns and the field is never repaired.
func TestFixViolation_LockRevertedFixLeavesTheRowOpen(t *testing.T) {
	const pinned = "the operator wrote this by hand"

	// POSITIVE CONTROL: the identical fixer on an UNLOCKED artist must land,
	// report Fixed, and RESOLVE. If this fails, the harness never reaches the
	// write and the locked case below would be dismissed for the wrong reason.
	db, artistSvc, a, ruleSvc, rv, ctx := lockRevertFixture(t, pinned)
	fixer := &bioOverwritingFixer{ruleID: RuleBioExists, newBio: "a rule wrote this"}
	pipeline := NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	fr, err := pipeline.FixViolation(ctx, rv.ID)
	if err != nil {
		t.Fatalf("control FixViolation: %v", err)
	}
	if !fr.Fixed {
		t.Fatalf("positive control FAILED: an UNLOCKED fix reported Fixed=false (%s); the write "+
			"under test is unreachable here", fr.Message)
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("control reload: %v", err)
	}
	if stored.Biography != "a rule wrote this" {
		t.Fatalf("positive control FAILED: the UNLOCKED biography is %q, so the write never "+
			"landed and the locked case proves nothing", stored.Biography)
	}
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusResolved {
		t.Fatalf("positive control FAILED: an UNLOCKED fix left the violation %q, want resolved", got)
	}

	// THE REGRESSION: the same fixer, same rule, on an artist with biography
	// PINNED. The chokepoint restores the value, so the fix did not happen.
	db, artistSvc, a, ruleSvc, rv, ctx = lockRevertFixture(t, pinned, "biography")
	fixer = &bioOverwritingFixer{ruleID: RuleBioExists, newBio: "a rule wrote this"}
	pipeline = NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	fr, err = pipeline.FixViolation(ctx, rv.ID)
	if err != nil {
		t.Fatalf("FixViolation: %v", err)
	}
	// PRECONDITION: the fixer must have RUN, or the open row means nothing.
	if fixer.fixCalls == 0 {
		t.Fatal("precondition: the fixer was never invoked, so nothing exercised the reverted write")
	}
	stored, err = artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	// PRECONDITION: the guard must actually have reverted it.
	if stored.Biography != pinned {
		t.Fatalf("precondition: the pinned biography is %q, want %q -- the chokepoint did not "+
			"revert the write", stored.Biography, pinned)
	}

	if fr.Fixed {
		t.Errorf("FixResult.Fixed = true for a change the lock guard reverted; the operator is "+
			"shown a repair that did not happen (#3037). Message: %s", fr.Message)
	}
	if fr.Dismissed {
		t.Errorf("FixResult.Dismissed = true; a lock is operator-REVOCABLE, so this is not a " +
			"terminal outcome, and a dismissed row can never be un-dismissed")
	}
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Errorf("violation status = %q, want open; a resolved row closes a finding that "+
			"was never repaired, and a dismissed one closes it permanently", got)
	}
}

// TestFixViolation_PartiallyRevertedFixStillResolves is the OVER-gating guard:
// ALL, not ANY. A rule whose fixer writes two fields with one locked still did
// useful work on the other, and treating that as a failed fix would turn a
// per-FIELD lock into a per-RULE one.
func TestFixViolation_PartiallyRevertedFixStillResolves(t *testing.T) {
	db, artistSvc, a, ruleSvc, rv, ctx := lockRevertFixture(t, "the operator wrote this", "biography")
	fixer := &bioAndOriginFixer{ruleID: RuleBioExists, newBio: "a rule wrote this", newOrigin: "Somewhere, XX"}
	pipeline := NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	fr, err := pipeline.FixViolation(ctx, rv.ID)
	if err != nil {
		t.Fatalf("FixViolation: %v", err)
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	// PRECONDITIONS: exactly one of the two writes was reverted.
	if stored.Biography != "the operator wrote this" {
		t.Fatalf("precondition: the LOCKED biography is %q; it should have been reverted", stored.Biography)
	}
	if stored.Origin != "Somewhere, XX" {
		t.Fatalf("precondition: the UNLOCKED origin is %q; it should have landed", stored.Origin)
	}

	if !fr.Fixed {
		t.Errorf("Fixed = false although origin really changed; a lock on ONE of a rule's "+
			"fields must not disable the rule. Message: %s", fr.Message)
	}
	if fr.Dismissed {
		t.Error("Dismissed = true for a PARTIALLY reverted fix that really did change a field")
	}
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusResolved {
		t.Errorf("violation status = %q, want resolved", got)
	}
}

// TestRunForArtist_LockRevertedAutoFixStaysOpenWithItsResultRow covers the
// UNATTENDED path. That is the one that matters at scale: FixViolation is one
// artist at a time, while the scheduled pass sweeps the whole library, so a
// correction that holds only on the click path protects nothing overnight.
//
// It also pins the health trap. Had this path written 'dismissed',
// UpsertViolation would emit NO paired rule_results FAIL row (#1107 writes it
// only for an open/pending row) and every later pass would preserve the
// dismiss, so the row would never appear and offlineHealthScore would refuse to
// score the artist -- freezing its health permanently. Writing the row OPEN
// sidesteps the trap rather than managing it, and this test asserts the
// rule_results row exists to prove that.
func TestRunForArtist_LockRevertedAutoFixStaysOpenWithItsResultRow(t *testing.T) {
	// SHORT, so the bio_exists CHECKER genuinely flags this artist and the auto
	// path dispatches a fix. A long one passes the rule and reaches nothing.
	const pinned = "short"
	db, artistSvc, a, ruleSvc, _, ctx := lockRevertFixture(t, pinned, "biography")
	enableRuleAuto(t, ctx, ruleSvc, RuleBioExists)

	// FIRST-PASS CONDITIONS. The fixture's seeded violation also writes the
	// paired rule_results FAIL row, which would MASK the trap below. Clearing
	// both makes the code under test the only writer of either row.
	if _, err := db.ExecContext(ctx, `DELETE FROM rule_violations WHERE artist_id = ?`, a.ID); err != nil {
		t.Fatalf("clearing seeded violations: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM rule_results WHERE artist_id = ?`, a.ID); err != nil {
		t.Fatalf("clearing seeded rule results: %v", err)
	}
	if _, exists := ruleResultRow(t, db, a.ID, RuleBioExists); exists {
		t.Fatal("precondition: a rule_results row survived the clear, so the trap below is masked")
	}
	// ALSO too short, which is the faithful shape: the guard reverts the write,
	// so the rule still fails after the pass. A passing replacement would make
	// the post-fix evaluation (run against the in-memory struct, BEFORE the
	// persist reverts it) write a passed=1 row that masks the trap below.
	fixer := &bioOverwritingFixer{ruleID: RuleBioExists, newBio: "brief bio"}
	pipeline := NewPipeline(NewEngine(ruleSvc, nil, nil, nil, testLogger()), artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	if _, err := pipeline.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}
	// PRECONDITION: the auto path must have dispatched a fix. A pass where the
	// checker never flagged the artist reaches none of the code under test.
	if fixer.fixCalls == 0 {
		t.Fatal("precondition: no fix was attempted, so the auto-fix dismiss path was never reached")
	}

	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Fatalf("the auto path left the violation %q for a fix the lock guard fully reverted, "+
			"want open; a resolved row hides a finding that was never repaired, and a "+
			"dismissed one hides it permanently even after the operator unlocks", got)
	}

	// THE TRAP: the paired rule_results row must exist. Without the baseline
	// open upsert it never would, and offlineHealthScore refuses to score an
	// artist with no complete evaluation -- its health freezes permanently.
	if _, exists := ruleResultRow(t, db, a.ID, RuleBioExists); !exists {
		t.Error("no rule_results row exists for this rule. UpsertViolation writes the paired FAIL " +
			"row only for an open/pending row (#1107), so a status that is not open leaves the " +
			"artist with no evaluation baseline and offlineHealthScore freezes its health " +
			"permanently -- #3060's trap.")
	}
}

// bioAndOriginFixer changes TWO guarded fields, so a test can pin one and leave
// the other free -- with a single-field fixer every revert is total and the
// partial branch is unreachable.
type bioAndOriginFixer struct {
	ruleID    string
	newBio    string
	newOrigin string
	fixCalls  int
}

func (f *bioAndOriginFixer) CanFix(v *Violation) bool { return v.RuleID == f.ruleID }

func (f *bioAndOriginFixer) Fix(_ context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	f.fixCalls++
	a.Biography = f.newBio
	a.Origin = f.newOrigin
	return &FixResult{RuleID: v.RuleID, Fixed: true, Message: "overwrote biography and origin"}, nil
}
