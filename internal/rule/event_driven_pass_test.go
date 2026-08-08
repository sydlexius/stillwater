package rule

// event_driven_pass_test.go pins the #2519 invariant: when a re-evaluation finds
// a rule now PASSES, any active violation for that (artist, rule) is resolved --
// on the EVENT-DRIVEN path, not only the pipeline path.
//
// THE DEFECT THIS GUARDS
//
// "Persist an evaluation result" was implemented twice and the two drifted:
//
//   pipeline (fixer.go persistPassResults)  -- writes the pass row AND calls
//                                              ResolveViolationIfActive
//   subscriber (health_subscriber.go)       -- wrote the pass row and STOPPED
//
// So an ArtistUpdated event -- which an image write fires -- recorded the pass
// and orphaned the violation. The UI reads the violation, so the operator saw a
// failure quoting the OLD image's dimensions against an image that no longer
// existed, and the only escape was dismissing a finding that was not real.
//
// WHY THESE TESTS LOOK PARANOID
//
// This surface is unusually good at passing vacuously, so each test asserts its
// preconditions rather than assuming them. The traps, all of which produce a
// GREEN test over a BROKEN implementation:
//
//   - No violation seeded. violationStatus returns "" for ErrNoRows, and "" is
//     not "open", so a naive assertion passes against an EMPTY TABLE.
//   - The rule was never evaluated (disabled / no checker / capability-gated /
//     event-driven), so it is not in RulesConsidered and the pass branch never
//     runs. The violation stays put and that reads as "correct".
//   - The rule never actually failed, so the test proves a hand-written row got
//     cleaned up rather than that a fail -> pass transition resolved it.
//   - Something ELSE resolved it. Never drive a pipeline run here.
//   - Asserting "resolved" without checking HOW: RetractRuleVerdict also
//     resolves, but DELETES the rule_results row. Only a genuine pass leaves
//     passed=1, so both facts are asserted together.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
)

// bioOnlyEngine seeds the default rules, disables everything except bio_exists,
// and returns a real DB-backed engine plus service.
//
// bio_exists is the right fixture for a fail -> pass transition: checkBioExists
// is a pure function of a.Biography (checkers.go) with no filesystem access and
// no capability gate, so the transition is driven by one field and cannot be
// diverted into the SKIPPED path -- which resolves violations by a DIFFERENT
// mechanism (RetractRuleVerdict) and would make the assertion ambiguous about
// which mechanism fired.
//
// Everything else is disabled so a stray rule cannot resolve the violation under
// test or add noise to RulesConsidered.
func bioOnlyEngine(t *testing.T, db *sql.DB) (*Engine, *Service) {
	t.Helper()
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	// Raw UPDATE rather than svc.Update: Update runs cleanupDisabledRuleState,
	// which would soft-resolve the very violations these tests seed.
	if _, err := db.ExecContext(ctx,
		`UPDATE rules SET enabled = 0 WHERE id != ?`, RuleBioExists); err != nil {
		t.Fatalf("disabling other rules: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE rules SET enabled = 1 WHERE id = ?`, RuleBioExists); err != nil {
		t.Fatalf("enabling %s: %v", RuleBioExists, err)
	}
	return NewEngine(svc, db, nil, nil, testLogger()), svc
}

// assertBioConsidered fails unless bio_exists was actually EVALUATED for this
// artist. Without it every assertion here is vacuous: a rule that never ran
// leaves the violation untouched, which is indistinguishable from "the fix did
// not resolve it".
//
// Hardcodes the rule rather than taking it as a parameter: every test in this
// file drives the same fixture, and a parameter that only ever receives one
// value invites a caller to pass a rule the fixture does not enable -- which
// would fail here for a confusing reason.
func assertBioConsidered(t *testing.T, res *EvaluationResult) {
	t.Helper()
	const ruleID = RuleBioExists
	for _, id := range res.RulesConsidered {
		if id == ruleID {
			return
		}
	}
	for _, s := range res.RulesSkipped {
		if s.RuleID == ruleID {
			t.Fatalf("precondition: rule %s was SKIPPED (%s), not considered. A skipped rule is "+
				"cleared by RetractRuleVerdict, so this test would be measuring the wrong mechanism",
				ruleID, s.Reason)
		}
	}
	t.Fatalf("precondition: rule %s is not in RulesConsidered (%v), so it was never evaluated and "+
		"the pass branch never ran", ruleID, res.RulesConsidered)
}

// seedBioViolation inserts an ACTIVE violation for bio_exists plus the matching
// failing rule_results row, i.e. the state a real failing evaluation leaves.
func seedBioViolation(t *testing.T, svc *Service, a *artist.Artist, status string) {
	t.Helper()
	rv := &RuleViolation{
		RuleID:     RuleBioExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "warning",
		Message:    "artist has no biography",
		Fixable:    true,
		Status:     status,
	}
	if err := svc.UpsertViolation(t.Context(), rv); err != nil {
		t.Fatalf("seeding %s violation: %v", status, err)
	}
}

// armRuleResultsUpdateTrap makes any UPDATE of the given rule's rule_results
// rows fail, so a test can prove what happens when the pass write aborts.
// Mirrors armRuleResultsDeleteTrap / armViolationUpdateTrap in
// retract_skipped_results_test.go.
//
// Each test gets its own database (setupTestDB copies a template), so there is
// no cleanup and no cross-test leakage. quoteSQLLiteral stays because a trigger
// body cannot take bound parameters.
func armRuleResultsUpdateTrap(t *testing.T, db *sql.DB, ruleID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf(`
		CREATE TRIGGER trap_update_rule_results
		BEFORE UPDATE ON rule_results
		WHEN OLD.rule_id = %s
		BEGIN
			SELECT RAISE(ABORT, 'injected: rule_results UPDATE failed');
		END;`, quoteSQLLiteral(ruleID))); err != nil {
		t.Fatalf("arming the rule_results UPDATE trap: %v", err)
	}
}

// TestHealthSubscriber_ResolvesViolationOnFailToPass is the #2519 regression
// test. It drives a GENUINE fail -> pass transition (the artist starts with no
// biography and gains one) and asserts the event-driven path resolves the
// violation it left behind.
//
// Mutant this kills: reverting evaluateArtist's pass branch to a bare
// UpsertRuleResultPass that does not resolve.
func TestHealthSubscriber_ResolvesViolationOnFailToPass(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, svc := bioOnlyEngine(t, db)
	artistSvc := artist.NewService(db)

	// FAILING state first: no biography.
	a := apiOnlyArtist(t, db, "Fail To Pass")
	failing, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("evaluating failing state: %v", err)
	}
	assertBioConsidered(t, failing)
	if len(failing.Violations) == 0 {
		t.Fatalf("precondition: the artist must actually FAIL bio_exists before the transition, "+
			"got 0 violations (considered=%v)", failing.RulesConsidered)
	}
	seedBioViolation(t, svc, a, ViolationStatusOpen)
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Fatalf("precondition: seeded violation should be %q, got %q (a silently-failed seed "+
			"makes every assertion below vacuous)", ViolationStatusOpen, got)
	}

	// THE TRANSITION: give the artist a biography so the rule now passes.
	a.Biography = "A biography comfortably longer than the ten character minimum."
	if err := artistSvc.Update(ctx, a); err != nil {
		t.Fatalf("updating artist biography: %v", err)
	}
	passing, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("evaluating passing state: %v", err)
	}
	assertBioConsidered(t, passing)
	if len(passing.Violations) != 0 {
		t.Fatalf("precondition: the artist must PASS after gaining a biography, got %d violations",
			len(passing.Violations))
	}

	NewHealthSubscriber(engine, artistSvc, testLogger()).evaluateArtist(ctx, a.ID)

	// THE ASSERTION. Both halves matter: RetractRuleVerdict also produces
	// "resolved", but deletes the rule_results row. Only a genuine pass leaves
	// passed=1, so checking the pair proves WHICH mechanism ran.
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusResolved {
		t.Errorf("a rule that now PASSES must resolve its stale violation on the event-driven "+
			"path: got status %q, want %q", got, ViolationStatusResolved)
	}
	passed, exists := ruleResultRow(t, db, a.ID, RuleBioExists)
	if !exists {
		t.Errorf("the pass row must exist (a missing row means retraction ran, not a pass)")
	} else if !passed {
		t.Errorf("the rule_results row must record passed=1, got passed=0")
	}
}

// TestHealthSubscriber_ResolvesPendingChoiceOnPass covers AC3's second half.
// ResolveViolationIfActive clears BOTH open and pending_choice; RetractRuleVerdict
// clears only open. A test seeding just `open` cannot tell the two apart, so
// this pins the pending_choice edge specifically.
//
// The contrast with #2509 is deliberate and load-bearing: a SKIPPED rule
// preserves pending_choice ("automation must never overrule a human decision",
// #1107) because the rule did not run. A rule that RAN and PASSED is a different
// case -- the finding is genuinely gone, so the parked choice is moot.
func TestHealthSubscriber_ResolvesPendingChoiceOnPass(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, svc := bioOnlyEngine(t, db)
	artistSvc := artist.NewService(db)

	a := apiOnlyArtist(t, db, "Pending Choice Pass")
	failing, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("evaluating failing state: %v", err)
	}
	assertBioConsidered(t, failing)
	if len(failing.Violations) == 0 {
		t.Fatalf("precondition: the artist must actually FAIL bio_exists first")
	}
	seedBioViolation(t, svc, a, ViolationStatusPendingChoice)
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusPendingChoice {
		t.Fatalf("precondition: seeded violation should be %q, got %q",
			ViolationStatusPendingChoice, got)
	}

	a.Biography = "A biography comfortably longer than the ten character minimum."
	if err := artistSvc.Update(ctx, a); err != nil {
		t.Fatalf("updating artist biography: %v", err)
	}

	NewHealthSubscriber(engine, artistSvc, testLogger()).evaluateArtist(ctx, a.ID)

	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusResolved {
		t.Errorf("a PASSING rule must resolve a pending_choice violation too (the finding is "+
			"genuinely gone, so the parked choice is moot): got %q, want %q",
			got, ViolationStatusResolved)
	}
}

// TestRecordRulePass_IsAtomic proves the pass row and the resolve land in ONE
// transaction.
//
// This is the only test here that can tell one transaction from two sequential
// statements: every other assertion checks the END STATE, which is identical
// either way on the happy path. Without it, RecordRulePass could be two bare
// ExecContext calls and the whole suite would still be green -- while a crash
// between them left passed=1 beside an active violation, which IS the #2519 bug
// arriving through the code that was supposed to fix it.
//
// The trap is a real SQLite BEFORE UPDATE trigger that ABORTs the violation
// UPDATE. If the writes share a transaction, the pass row must roll back with
// it; if they do not, the pass row survives and the tables disagree.
func TestRecordRulePass_IsAtomic(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	_, svc := bioOnlyEngine(t, db)

	a := apiOnlyArtist(t, db, "Atomic Pass")
	seedBioViolation(t, svc, a, ViolationStatusOpen)
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Fatalf("precondition: seeded violation should be %q, got %q", ViolationStatusOpen, got)
	}
	// PRECONDITION: seeding the violation ALSO wrote a FAILING rule_results row
	// -- UpsertViolation writes both in one transaction. So the pre-state is
	// passed=0, and the question below is whether the aborted call manages to
	// flip it to passed=1.
	passed, exists := ruleResultRow(t, db, a.ID, RuleBioExists)
	if !exists {
		t.Fatalf("precondition: expected the seeded violation to leave a rule_results row")
	}
	if passed {
		t.Fatalf("precondition: the seeded row should record a FAILURE (passed=0), got passed=1")
	}

	armViolationUpdateTrap(t, db, RuleBioExists)

	resolved, err := svc.RecordRulePass(ctx, a.ID, RuleBioExists, time.Now().UTC())
	if err == nil {
		t.Fatalf("precondition: the trap must make this call fail, got nil error "+
			"(resolved=%v). Without a real failure this test proves nothing", resolved)
	}
	if resolved {
		t.Errorf("a failed call must not report a resolved violation, got resolved=true")
	}

	// THE ASSERTION: the pass write must have rolled back with the failed UPDATE,
	// leaving the row still recording a FAILURE. If it reads passed=1, the two
	// writes were independent -- rule_results would say the rule passes while the
	// violation is still active, which is exactly the split state #2519 is about,
	// reproduced by the code meant to fix it.
	if nowPassed, stillExists := ruleResultRow(t, db, a.ID, RuleBioExists); !stillExists {
		t.Errorf("the pre-existing rule_results row vanished; the rollback should have left it intact")
	} else if nowPassed {
		t.Errorf("rule_results flipped to passed=1 even though the violation UPDATE aborted, so the "+
			"two writes are NOT in one transaction: the row claims a pass while the violation is "+
			"still %q", violationStatus(t, db, a.ID, RuleBioExists))
	}
	// And the violation must be untouched.
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Errorf("the violation should be unchanged after the aborted update: got %q, want %q",
			got, ViolationStatusOpen)
	}
}

// TestRecordRulePass_SkipsEventDrivenRules pins the defensive guard.
//
// Event-driven violations are raised at the write/push chokepoints and their
// checkers cannot re-derive them, so auto-resolving one destroys a finding
// nothing can rebuild -- the same data loss #2614 had to add a second
// enforcement point to prevent. The engine already excludes these rules from
// evaluation structurally, so this is defense in depth and not a live path;
// an UNTESTED guard is exactly the kind that quietly stops working, which is
// why it is pinned rather than trusted.
//
// Mutant this kills: deleting the IsEventDriven early return.
func TestRecordRulePass_SkipsEventDrivenRules(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	a := apiOnlyArtist(t, db, "Event Driven Untouched")

	// An active collision violation, exactly as the notifier would leave it.
	if err := svc.UpsertViolation(ctx, &RuleViolation{
		RuleID:     RuleCrossArtistBackdropCollision,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "warning",
		Message:    "shared backdrop with another artist",
		Fixable:    true,
		Status:     ViolationStatusOpen,
	}); err != nil {
		t.Fatalf("seeding collision violation: %v", err)
	}
	if got := violationStatus(t, db, a.ID, RuleCrossArtistBackdropCollision); got != ViolationStatusOpen {
		t.Fatalf("precondition: seeded violation should be %q, got %q", ViolationStatusOpen, got)
	}
	// PRECONDITION: the rule must actually BE event-driven, or this test asserts
	// nothing about the guard.
	if !IsEventDriven(RuleCrossArtistBackdropCollision) {
		t.Fatalf("precondition: %s must be event-driven for this test to mean anything",
			RuleCrossArtistBackdropCollision)
	}

	resolved, err := svc.RecordRulePass(ctx, a.ID, RuleCrossArtistBackdropCollision, time.Now().UTC())
	if err != nil {
		t.Fatalf("RecordRulePass on an event-driven rule should be a silent no-op, got error: %v", err)
	}
	if resolved {
		t.Errorf("RecordRulePass must not report resolving an event-driven violation, got resolved=true")
	}

	if got := violationStatus(t, db, a.ID, RuleCrossArtistBackdropCollision); got != ViolationStatusOpen {
		t.Errorf("an event-driven violation must survive RecordRulePass -- nothing can re-raise it "+
			"(#2614): got %q, want %q", got, ViolationStatusOpen)
	}
}

// TestRecordRulePass_RollsBackWhenThePassWriteFails is the mirror of the
// atomicity test above: there the violation UPDATE fails, here the pass-row
// write does. Both directions must leave the pair consistent, and only testing
// one direction leaves half the transaction unproven.
func TestRecordRulePass_RollsBackWhenThePassWriteFails(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	_, svc := bioOnlyEngine(t, db)

	a := apiOnlyArtist(t, db, "Pass Write Fails")
	seedBioViolation(t, svc, a, ViolationStatusOpen)
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Fatalf("precondition: seeded violation should be %q, got %q", ViolationStatusOpen, got)
	}

	// Abort the rule_results UPDATE. The seeded violation already left a failing
	// row, so the pass write takes the ON CONFLICT UPDATE branch.
	armRuleResultsUpdateTrap(t, db, RuleBioExists)

	resolved, err := svc.RecordRulePass(ctx, a.ID, RuleBioExists, time.Now().UTC())
	if err == nil {
		t.Fatalf("precondition: the trap must make this call fail, got nil error (resolved=%v)", resolved)
	}
	if resolved {
		t.Errorf("a failed call must not report a resolved violation, got resolved=true")
	}

	// The violation must NOT have been resolved by a write that partially applied.
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Errorf("the violation was resolved even though the pass write aborted, so the two writes "+
			"are not in one transaction: got %q, want %q", got, ViolationStatusOpen)
	}
}

// TestHealthSubscriber_SurvivesRecordRulePassFailure covers the subscriber's
// error branch: a failed persist must be logged and must NOT abort the rest of
// the evaluation.
//
// The subscriber runs off an event with nobody to return an error to, so its
// only options are "log and continue" or "die silently mid-loop". Continuing is
// right -- one rule failing to persist should not stop the others -- but that
// choice is only correct if it is actually exercised, and nothing did.
//
// This also closes the one uncovered line the patch-coverage gate flagged on
// this file. Worth being precise about why it was uncovered: the diff adds a
// single executable line here (the `if`), and Go splits it into two coverage
// blocks -- the condition, which the other tests execute, and the error BODY,
// which nothing reached. The gate attributes the added line to the body.
func TestHealthSubscriber_SurvivesRecordRulePassFailure(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, svc := bioOnlyEngine(t, db)
	artistSvc := artist.NewService(db)

	a := apiOnlyArtist(t, db, "Persist Fails")
	seedBioViolation(t, svc, a, ViolationStatusOpen)
	a.Biography = "A biography comfortably longer than the ten character minimum."
	if err := artistSvc.Update(ctx, a); err != nil {
		t.Fatalf("updating artist biography: %v", err)
	}
	passing, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("evaluating passing state: %v", err)
	}
	assertBioConsidered(t, passing)
	if len(passing.Violations) != 0 {
		t.Fatalf("precondition: the artist must PASS so the subscriber takes the pass branch, "+
			"got %d violations", len(passing.Violations))
	}

	// Make the persist fail for real, by aborting the rule_results write.
	armRuleResultsUpdateTrap(t, db, RuleBioExists)

	// PRECONDITION: the trap must actually make RecordRulePass fail, or the
	// error branch is never entered and this test proves nothing.
	if _, err := svc.RecordRulePass(ctx, a.ID, RuleBioExists, time.Now().UTC()); err == nil {
		t.Fatalf("precondition: the trap should make RecordRulePass fail, got nil error")
	}

	// The subscriber must not panic or abort; it logs and moves on.
	NewHealthSubscriber(engine, artistSvc, testLogger()).evaluateArtist(ctx, a.ID)

	// And the failure must be honest: nothing resolved, because nothing was
	// written. A swallowed error that reported success would be worse than the
	// bug this PR fixes.
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Errorf("a FAILED persist must leave the violation untouched: got %q, want %q",
			got, ViolationStatusOpen)
	}
	if passed, exists := ruleResultRow(t, db, a.ID, RuleBioExists); !exists {
		t.Errorf("the pre-existing rule_results row should still be there")
	} else if passed {
		t.Errorf("rule_results must not record a pass when the write was aborted")
	}
}

// TestPipeline_PassPersistenceIsAtomic pins that the PIPELINE goes through
// RecordRulePass, not just that it happens to resolve.
//
// Why a separate test when pipeline resolve behavior is already covered:
// existing tests (TestPipeline_ReEvalPassResolvesStaleViolation and friends)
// assert the END STATE, which the pre-consolidation two-call form produces just
// as well. Measured: reverting fixer.go to that two-call form passes the ENTIRE
// suite -- so the consolidation's central claim, that no future path can
// implement half of it, was unpinned on the pipeline half. Someone could revert
// that routing tomorrow and nothing would go red.
//
// Atomicity is the property that distinguishes them, because only the shared
// routine has a transaction. Trap the violation UPDATE and drive the real
// pipeline: if it routes through RecordRulePass the pass row rolls back with the
// failed resolve; if it issues two bare statements the pass row survives and the
// tables disagree -- the #2519 split state, on the path that was supposed to be
// correct all along.
func TestPipeline_PassPersistenceIsAtomic(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, svc := bioOnlyEngine(t, db)
	artistSvc := artist.NewService(db)

	a := apiOnlyArtist(t, db, "Pipeline Atomic")
	seedBioViolation(t, svc, a, ViolationStatusOpen)
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Fatalf("precondition: seeded violation should be %q, got %q", ViolationStatusOpen, got)
	}
	passed, exists := ruleResultRow(t, db, a.ID, RuleBioExists)
	if !exists || passed {
		t.Fatalf("precondition: expected a seeded FAILING rule_results row (exists=%v passed=%v)",
			exists, passed)
	}

	// Make the rule pass, so the pipeline takes the pass-persistence branch.
	a.Biography = "A biography comfortably longer than the ten character minimum."
	if err := artistSvc.Update(ctx, a); err != nil {
		t.Fatalf("updating artist biography: %v", err)
	}

	armViolationUpdateTrap(t, db, RuleBioExists)

	// PRECONDITION: the rule must actually PASS now, or the pipeline never takes
	// the pass-persistence branch and everything below holds for the wrong reason.
	passing, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("evaluating passing state: %v", err)
	}
	assertBioConsidered(t, passing)
	if len(passing.Violations) != 0 {
		t.Fatalf("precondition: the artist must PASS so the pipeline reaches pass persistence, "+
			"got %d violations", len(passing.Violations))
	}

	// Drive the REAL pipeline, not RecordRulePass directly -- the routing is
	// exactly what is under test.
	p := NewPipeline(engine, artistSvc, svc, nil, nil, testLogger())
	res, runErr := p.RunForArtist(ctx, a)

	// PRECONDITION: the run must have REACHED the pass write and been stopped by
	// the trap. Without this the assertion below passes for the wrong reason
	// whenever the run aborts earlier -- during evaluation, or in a fixer step --
	// because the row simply stays at its seeded passed=0. That is the same
	// vacuity trap this file's header warns about, and every other test here
	// guards against it.
	// PersistFailures counts artists whose writes did not stick (#2724). The trap
	// aborts one, so it must be non-zero; a clean run means the pass write was
	// never attempted.
	if runErr == nil && (res == nil || res.PersistFailures == 0) {
		t.Fatalf("precondition: the trap should have made pass persistence fail, but the run "+
			"reported no failures (err=%v res=%+v) -- the pass branch was probably never reached",
			runErr, res)
	}

	// THE ASSERTION: the pass row must NOT have flipped. If it reads passed=1
	// while the violation is still open, the pipeline wrote the two independently.
	if nowPassed, stillExists := ruleResultRow(t, db, a.ID, RuleBioExists); !stillExists {
		t.Errorf("the rule_results row vanished; the rollback should have left it intact")
	} else if nowPassed {
		t.Errorf("rule_results flipped to passed=1 while the violation UPDATE aborted, so the "+
			"pipeline is NOT routing through RecordRulePass: the row claims a pass but the "+
			"violation is still %q", violationStatus(t, db, a.ID, RuleBioExists))
	}
}

// TestHealthSubscriber_ResolvesViolationViaTheRealEventBus proves the WIRING,
// not the transition logic.
//
// Every other test in this file calls evaluateArtist directly, which is the
// right way to test the transition but leaves the joins BEFORE it unproven:
//
//	image write -> Publish(ArtistUpdated) -> bus -> HandleEvent -> debounce
//	   -> evaluateArtist -> RecordRulePass
//	                        ^^^^^^^^^^^^^^^^^^^^ everything else covers only this
//
// If any earlier join were broken, all six of those tests stay GREEN and the
// bug stays live in production. That is a false green with full coverage --
// the same shape as the defect this PR fixes, one level up.
//
// This closes the bus -> HandleEvent -> debounce -> evaluateArtist joins by
// driving a REAL event.Bus with a REAL subscription, exactly as
// cmd/stillwater/main.go wires it. The remaining join, that the image handler
// publishes at all, lives in internal/api and is covered there by the handlers'
// own tests.
//
// It costs ~3s of wall clock (a 2s debounce plus a 500ms tick), which is why it
// is ONE test rather than the pattern for the whole file.
func TestHealthSubscriber_ResolvesViolationViaTheRealEventBus(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, svc := bioOnlyEngine(t, db)
	artistSvc := artist.NewService(db)

	a := apiOnlyArtist(t, db, "Real Bus")
	failing, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("evaluating failing state: %v", err)
	}
	assertBioConsidered(t, failing)
	if len(failing.Violations) == 0 {
		t.Fatalf("precondition: the artist must actually FAIL bio_exists first")
	}
	seedBioViolation(t, svc, a, ViolationStatusOpen)
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusOpen {
		t.Fatalf("precondition: seeded violation should be %q, got %q", ViolationStatusOpen, got)
	}

	a.Biography = "A biography comfortably longer than the ten character minimum."
	if err := artistSvc.Update(ctx, a); err != nil {
		t.Fatalf("updating artist biography: %v", err)
	}

	// Wire it the way main.go does: a real bus, a real subscription, the
	// subscriber's own goroutine draining the debounce queue.
	sub := NewHealthSubscriber(engine, artistSvc, testLogger())
	bus := event.NewBus(testLogger(), 16)
	bus.Subscribe(event.ArtistUpdated, sub.HandleEvent)

	// BOTH goroutines are required, and forgetting either is silent. Publish only
	// enqueues onto a channel; without bus.Start draining it the event sits in the
	// buffer forever and nothing is dispatched. Without sub.Start the debounce
	// queue is never drained. main.go starts both, so the test must too.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go bus.Start()
	defer bus.Stop()
	go sub.Start(runCtx)

	// The event an image write publishes (handlers_image.go).
	bus.Publish(event.Event{
		Type: event.ArtistUpdated,
		Data: map[string]any{"artist_id": a.ID},
	})

	// Poll rather than sleep a fixed span: the debounce is 2s and the tick 500ms,
	// so the earliest possible completion is ~2s and a fixed sleep would either
	// flake or waste time. Deadline is generous because a loaded runner can lag.
	deadline := time.Now().Add(20 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if got = violationStatus(t, db, a.ID, RuleBioExists); got == ViolationStatusResolved {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if got != ViolationStatusResolved {
		t.Errorf("publishing ArtistUpdated on a real bus must reach RecordRulePass and resolve "+
			"the violation: got status %q, want %q. A broken join anywhere in "+
			"Publish -> Subscribe -> HandleEvent -> debounce -> evaluateArtist would land here "+
			"while every direct-call test stays green", got, ViolationStatusResolved)
	}
	// Same discriminator as the direct tests: retraction also resolves, but
	// deletes the row. Only a genuine pass leaves passed=1.
	if passed, exists := ruleResultRow(t, db, a.ID, RuleBioExists); !exists {
		t.Errorf("the pass row must exist (a missing row means retraction ran, not a pass)")
	} else if !passed {
		t.Errorf("the rule_results row must record passed=1, got passed=0")
	}
}

// TestHealthSubscriber_LeavesDismissedViolationDismissed pins AC4. Dismissed is
// terminal (#1107) and must survive a pass.
//
// The dismissed row is on a rule that IS considered and IS passing -- a
// dismissed row on an unevaluated rule would be untouched trivially and would
// prove nothing.
func TestHealthSubscriber_LeavesDismissedViolationDismissed(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, svc := bioOnlyEngine(t, db)
	artistSvc := artist.NewService(db)

	a := apiOnlyArtist(t, db, "Dismissed Stays")
	failing, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("evaluating failing state: %v", err)
	}
	assertBioConsidered(t, failing)
	seedBioViolation(t, svc, a, ViolationStatusDismissed)
	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusDismissed {
		t.Fatalf("precondition: seeded violation should be %q, got %q",
			ViolationStatusDismissed, got)
	}

	a.Biography = "A biography comfortably longer than the ten character minimum."
	if err := artistSvc.Update(ctx, a); err != nil {
		t.Fatalf("updating artist biography: %v", err)
	}
	passing, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("evaluating passing state: %v", err)
	}
	assertBioConsidered(t, passing)
	// assertBioConsidered proves the rule was EVALUATED, not that it now PASSES.
	// Without this the rule could still be failing, the subscriber would never
	// take the pass branch, and the dismissed row would survive for a reason
	// unrelated to the invariant under test.
	if len(passing.Violations) != 0 {
		t.Fatalf("precondition: the artist must PASS so the subscriber takes the pass branch, "+
			"got %d violations", len(passing.Violations))
	}

	NewHealthSubscriber(engine, artistSvc, testLogger()).evaluateArtist(ctx, a.ID)

	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusDismissed {
		t.Errorf("dismissed is terminal (#1107) and must survive a pass: got %q, want %q",
			got, ViolationStatusDismissed)
	}
}
