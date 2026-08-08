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
	"database/sql"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
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

	NewHealthSubscriber(engine, artistSvc, testLogger()).evaluateArtist(ctx, a.ID)

	if got := violationStatus(t, db, a.ID, RuleBioExists); got != ViolationStatusDismissed {
		t.Errorf("dismissed is terminal (#1107) and must survive a pass: got %q, want %q",
			got, ViolationStatusDismissed)
	}
}
