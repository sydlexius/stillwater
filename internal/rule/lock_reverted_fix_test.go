package rule

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// #3037: a fixer must not claim credit for a write the lock guard reverted.
//
// READ THIS BEFORE EDITING. Every "the violation was dismissed" assertion is
// PAIRED with a positive control on an otherwise identical UNLOCKED artist that
// must be RESOLVED. The dismiss branch is reachable only when a fixer genuinely
// ran and genuinely wrote a guarded field, so an unpaired assertion would pass
// the moment the harness stopped reaching the fixer at all.

// lockRevertFixture seeds one artist with `bio` and an OPEN, fixable
// bio_exists violation, pinning `lockFields` through the dedicated lock mutator
// -- not by setting LockedFields on the struct, which the chokepoint pins away.
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
	// PRECONDITIONS. Without both of these the test is about an artist whose
	// biography was never pinned, and the guard would never fire.
	if stored.Biography != bio {
		t.Fatalf("precondition: biography = %q, want %q", stored.Biography, bio)
	}
	if len(stored.LockedFields) != len(lockFields) {
		t.Fatalf("precondition: locked_fields = %v, want %v; the lock did not persist so the "+
			"guard will never fire and this test would pass vacuously", stored.LockedFields, lockFields)
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

// TestFixViolation_LockRevertedFixIsDismissedNotResolved is the headline
// regression. A fixer whose ONLY change was reverted must not report Fixed and
// must not resolve the row.
func TestFixViolation_LockRevertedFixIsDismissedNotResolved(t *testing.T) {
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
			"under test is not reachable in this harness", fr.Message)
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("control reload: %v", err)
	}
	if stored.Biography != "a rule wrote this" {
		t.Fatalf("positive control FAILED: the UNLOCKED biography is %q, so the fixer's write "+
			"never landed and the locked case proves nothing", stored.Biography)
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
	// PRECONDITION: the fixer must have RUN. A dismiss reached because the
	// fixer never fired would be the same verdict for an unrelated reason.
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
			"revert the write, so there is no reverted fix to report on", stored.Biography, pinned)
	}

	if fr.Fixed {
		t.Errorf("FixResult.Fixed = true for a change the lock guard reverted; the operator is "+
			"shown a repair that did not happen (#3037). Message: %s", fr.Message)
	}
	if !fr.Dismissed {
		t.Errorf("FixResult.Dismissed = false; re-running this fixer can only produce the same " +
			"refusal, so an open row hands the operator a Fix button that does nothing")
	}
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusDismissed {
		t.Errorf("violation status = %q, want dismissed; a resolved row closes a finding that "+
			"was never repaired", got)
	}
}

// TestFixViolation_PartiallyRevertedFixStillResolves is the other side of the
// gate, and it errs the RECOVERABLE way. One surviving change is a real repair,
// so the row is resolved -- a wrongly-resolved row re-raises on the next
// evaluation, whereas a wrongly-dismissed one has no un-dismiss route
// (UpsertViolation pins 'dismissed', ReopenViolation only accepts 'resolved').
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
	// PRECONDITIONS: exactly one of the two writes was reverted. Both surviving
	// or both reverted would make this a different test.
	if stored.Biography != "the operator wrote this" {
		t.Fatalf("precondition: the LOCKED biography is %q; it should have been reverted", stored.Biography)
	}
	if stored.Origin != "Somewhere, XX" {
		t.Fatalf("precondition: the UNLOCKED origin is %q; it should have landed, so this is not "+
			"a partial revert at all", stored.Origin)
	}

	if !fr.Fixed {
		t.Errorf("Fixed = false for a fix that really did change origin; a partial repair is a "+
			"repair and denying it costs the operator a Recent Activity entry. Message: %s", fr.Message)
	}
	if fr.Dismissed {
		t.Error("Dismissed = true for a PARTIALLY reverted fix; a dismiss is unrecoverable and " +
			"this row still describes a real change that landed")
	}
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusResolved {
		t.Errorf("violation status = %q, want resolved", got)
	}
}

// TestRunForArtist_LockRevertedAutoFixIsDismissedWithABaselineRow covers the
// UNATTENDED path, which is the one that repeats forever, and pins the
// two-upsert trap: recording 'dismissed' directly writes NO rule_results row
// (UpsertViolation emits the paired FAIL row only for open/pending) and every
// later pass preserves 'dismissed', so the row would never appear and
// offlineHealthScore would refuse to score the artist -- freezing its health
// permanently. The baseline open upsert is what prevents that.
func TestRunForArtist_LockRevertedAutoFixIsDismissedWithABaselineRow(t *testing.T) {
	// A SHORT pinned biography, so the bio_exists CHECKER genuinely flags this
	// artist and the auto path actually dispatches a fix. A long one passes the
	// rule, no violation is raised, and the code under test is never reached.
	const pinned = "short"
	db, artistSvc, a, ruleSvc, _, ctx := lockRevertFixture(t, pinned, "biography")
	enableRuleAuto(t, ctx, ruleSvc, RuleBioExists)

	// FIRST-PASS CONDITIONS. The fixture pre-seeds an open violation for the
	// FixViolation tests, and that seed also writes the paired rule_results FAIL
	// row -- which would MASK the trap this test exists to catch. Clearing both
	// makes this pass the artist's first, so the only writer of either row is
	// the code under test.
	if _, err := db.ExecContext(ctx, `DELETE FROM rule_violations WHERE artist_id = ?`, a.ID); err != nil {
		t.Fatalf("clearing seeded violations: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM rule_results WHERE artist_id = ?`, a.ID); err != nil {
		t.Fatalf("clearing seeded rule results: %v", err)
	}
	if _, exists := ruleResultRow(t, db, a.ID, RuleBioExists); exists {
		t.Fatal("precondition: a rule_results row survived the clear, so the trap below is masked")
	}
	// The replacement is ALSO too short for bio_exists (MinLength 10). That is
	// the faithful shape: the guard reverts the write, so the rule genuinely
	// still fails after the pass, and no pass row is written. A replacement long
	// enough to pass would make the post-fix evaluation -- which runs against
	// the in-memory struct, BEFORE the persist reverts it -- write a passed=1
	// row that masks the trap this test exists to catch.
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

	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusDismissed {
		t.Fatalf("the auto path left the violation %q for a fix the lock guard fully reverted, "+
			"want dismissed; an unattended pass will re-attempt and re-open it forever", got)
	}

	// THE TRAP: the paired rule_results row must exist. Without the baseline
	// open upsert it never would, and offlineHealthScore refuses to score an
	// artist with no complete evaluation -- its health freezes permanently.
	if _, exists := ruleResultRow(t, db, a.ID, RuleBioExists); !exists {
		t.Error("no rule_results row exists for the dismissed rule. The dismiss went straight to " +
			"'dismissed' without the open baseline upsert, so UpsertViolation never wrote the " +
			"paired FAIL row and this artist's health score is frozen forever (#3060's trap).")
	}
}

// bioAndOriginFixer changes TWO guarded fields, so a test can pin one and leave
// the other free. That is what makes a PARTIAL revert reachable at all: with a
// single-field fixer every revert is total and the partial branch is dead code.
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
