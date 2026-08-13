package rule

// event_driven_destruction_test.go pins the two guards from #2967: the paths
// that can DESTROY an event-driven rule's violation must refuse to touch it.
//
// WHY THIS SURFACE IS DIFFERENT FROM EVERY OTHER VIOLATION
//
// An ordinary rule's violation is cheap to lose: the next evaluation pass runs
// the checker again and re-raises it. An event-driven rule has no checker that
// runs -- its findings are raised at the write/push chokepoints, and one of
// them (mbid_resolves) took two rate-limited MusicBrainz requests to reach. So
// for these rules "deleted" and "resolved by something that should not have"
// are both permanent, and no amount of re-running rules brings them back.
//
// HOW THESE TESTS AVOID PASSING VACUOUSLY
//
// A guard test is easy to write in a form that would stay green with the guard
// deleted, so each test here states the trap it is avoiding:
//
//   - The ordinary row was never eligible for deletion anyway (wrong status,
//     inside the cutoff, no row at all). Then "the event-driven row survived"
//     says nothing, because NOTHING was deleted. Every clear test below asserts
//     the ordinary row was eligible AND that it actually went.
//   - The retraction call did nothing for an unrelated reason (no rows to
//     retract). A positive control drives the same call on an ordinary rule in
//     the same database and requires it to retract, proving the machinery is
//     live and would have reached the event-driven row.
//   - Asserting on a map's iteration order. eventDrivenRules is a map, so the
//     allow-list test asserts MEMBERSHIP only, never position.

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

// resolveViolationInThePast forces a violation to look exactly like the rows the
// old cleanupDisabledRuleState left behind: status=resolved with a resolved_at
// comfortably older than any plausible clear cutoff.
//
// A raw UPDATE, not a service call: no service method sets a backdated
// resolved_at, and the fixture needs the row to be eligible for deletion, which
// is the precondition the whole test rests on.
func resolveViolationInThePast(t *testing.T, svc *Service, artistID, ruleID string, daysAgo int) {
	t.Helper()
	ts := time.Now().UTC().AddDate(0, 0, -daysAgo).Format(time.RFC3339)
	res, err := svc.db.ExecContext(t.Context(), `
		UPDATE rule_violations
		   SET status = ?, resolved_at = ?, updated_at = ?
		 WHERE artist_id = ? AND rule_id = ?
	`, ViolationStatusResolved, ts, ts, artistID, ruleID)
	if err != nil {
		t.Fatalf("backdating the resolved violation for %s: %v", ruleID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("reading rows affected while backdating %s: %v", ruleID, err)
	}
	if n != 1 {
		t.Fatalf("backdating %s updated %d rows, want 1 (the fixture did not land, so every "+
			"assertion below would be vacuous)", ruleID, n)
	}
}

// TestClearResolvedViolations_SparesEventDrivenRules is the #2967 regression
// test for the delete path.
//
// DELETE /api/v1/notifications/resolved calls this with a hard-coded 7-day
// cutoff, is not admin-gated and is not scheduled, so one operator click runs
// it. Every violation the old disable-time cleanup soft-resolved is already far
// past that cutoff, which means the unguarded query destroys the entire
// remaining record of those findings in a single statement.
//
// Mutant this kills: dropping the `AND rule_id IN (...)` clause.
func TestClearResolvedViolations_SparesEventDrivenRules(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	a := apiOnlyArtist(t, db, "Clear Resolved")

	const protectedRule = RuleCrossArtistBackdropCollision
	const ordinaryRule = RuleBioExists

	// PRECONDITION: the two rules must actually differ along the axis under
	// test. If both were event-driven (or neither were) the test would prove
	// nothing about the allow-list.
	if !IsEventDriven(protectedRule) {
		t.Fatalf("precondition: %s must be event-driven for this test to mean anything", protectedRule)
	}
	if IsEventDriven(ordinaryRule) {
		t.Fatalf("precondition: %s must NOT be event-driven -- it is the control that proves the "+
			"delete actually ran", ordinaryRule)
	}

	seedViolation(t, svc, a, protectedRule, ViolationStatusOpen)
	seedViolation(t, svc, a, ordinaryRule, ViolationStatusOpen)
	// Both rows resolved, both 30 days old, i.e. both squarely inside the
	// endpoint's 7-day cutoff and both eligible for the unguarded DELETE.
	resolveViolationInThePast(t, svc, a.ID, protectedRule, 30)
	resolveViolationInThePast(t, svc, a.ID, ordinaryRule, 30)

	// PRECONDITIONS on the fixture itself, asserted against the table rather
	// than assumed from the seeding calls above.
	for _, id := range []string{protectedRule, ordinaryRule} {
		if got := violationStatus(t, db, a.ID, id); got != ViolationStatusResolved {
			t.Fatalf("precondition: violation for %s should be %q before clearing, got %q",
				id, ViolationStatusResolved, got)
		}
		if !violationResolvedBefore(t, db, a.ID, id, time.Now().UTC().AddDate(0, 0, -7)) {
			t.Fatalf("precondition: violation for %s must be older than the 7-day cutoff, or the "+
				"clear would skip it for a reason unrelated to the guard", id)
		}
	}

	if err := svc.ClearResolvedViolations(ctx, 7); err != nil {
		t.Fatalf("ClearResolvedViolations: %v", err)
	}

	// THE CONTROL: the ordinary row must be GONE. Without this the assertion
	// below would also pass on a routine that deletes nothing at all, which is
	// a guard that works by breaking the feature.
	if got := violationStatus(t, db, a.ID, ordinaryRule); got != "" {
		t.Errorf("an ordinary resolved violation past the cutoff must still be cleared: got status "+
			"%q, want the row deleted", got)
	}

	// THE ASSERTION: the event-driven row must survive. Nothing can re-raise
	// it, so this resolved row is the operator's only remaining record of the
	// finding (#2967).
	if got := violationStatus(t, db, a.ID, protectedRule); got != ViolationStatusResolved {
		t.Errorf("an event-driven violation must survive ClearResolvedViolations -- nothing can "+
			"re-derive it, so deleting it is permanent data loss: got %q, want %q",
			got, ViolationStatusResolved)
	}
}

// violationResolvedBefore reports whether the stored resolved_at is earlier than
// the given instant, reading the column directly rather than trusting the
// fixture that wrote it.
func violationResolvedBefore(t *testing.T, db *sql.DB, artistID, ruleID string, before time.Time) bool {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(t.Context(),
		`SELECT COALESCE(resolved_at, '') FROM rule_violations WHERE artist_id = ? AND rule_id = ?`,
		artistID, ruleID).Scan(&raw); err != nil {
		t.Fatalf("reading resolved_at for (%s, %s): %v", artistID, ruleID, err)
	}
	if raw == "" {
		t.Fatalf("resolved_at for (%s, %s) is empty, so the row is not a resolved row at all",
			artistID, ruleID)
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parsing resolved_at %q for (%s, %s): %v", raw, artistID, ruleID, err)
	}
	return ts.Before(before)
}

// TestClearableRuleIDs_ExcludesEveryEventDrivenRule pins the allow-list itself,
// which is the part that must keep working for rules nobody has written yet.
//
// The clear test above names two specific rules; this one walks the whole
// eventDrivenRules set, so a rule ADDED to that set later is protected without
// anyone remembering to add a test for it. That automatic-protection property
// is the reason the allow-list is derived rather than hand-written.
//
// Membership only, never position: eventDrivenRules is a map and its iteration
// order is unspecified.
func TestClearableRuleIDs_ExcludesEveryEventDrivenRule(t *testing.T) {
	ids := clearableRuleIDs()

	// PRECONDITIONS. An empty allow-list would satisfy the exclusion assertion
	// trivially while disabling the feature, and an empty eventDrivenRules set
	// would make the loop below run zero times.
	if len(ids) == 0 {
		t.Fatalf("precondition: the clearable allow-list must not be empty, or the exclusion check " +
			"below is vacuous and the clear endpoint deletes nothing")
	}
	if len(eventDrivenRules) == 0 {
		t.Fatalf("precondition: eventDrivenRules must not be empty, or this test checks nothing")
	}

	for id := range eventDrivenRules {
		if slices.Contains(ids, id) {
			t.Errorf("event-driven rule %s appears in the clearable allow-list, so its resolved "+
				"violations can be permanently deleted", id)
		}
	}

	// And the list must still contain ordinary rules, or the guard has been
	// "fixed" by turning the feature off.
	if !slices.Contains(ids, RuleBioExists) {
		t.Errorf("ordinary rule %s is missing from the clearable allow-list; the clear endpoint "+
			"would silently stop clearing it", RuleBioExists)
	}
}

// TestRetractRuleVerdict_SkipsEventDrivenRules pins the second guard.
//
// Retraction resolves an OPEN violation for a rule reported as SKIPPED. Today
// no event-driven rule can be reported skipped, because Engine.eligibleRules
// excludes them before the skip is recorded -- so this path is unreachable and
// the guard is defense in depth, not a live bug fix. It is pinned because the
// invariant that makes it unreachable lives in another file: a future change to
// eligibleRules would arm this path silently and destroy findings nothing can
// rebuild.
//
// Mutant this kills: deleting the IsEventDriven early return in
// RetractRuleVerdict.
func TestRetractRuleVerdict_SkipsEventDrivenRules(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	a := apiOnlyArtist(t, db, "Retract Event Driven")

	const protectedRule = RuleMBIDResolves
	const ordinaryRule = RuleBioExists

	if !IsEventDriven(protectedRule) {
		t.Fatalf("precondition: %s must be event-driven for this test to mean anything", protectedRule)
	}
	if IsEventDriven(ordinaryRule) {
		t.Fatalf("precondition: %s must NOT be event-driven -- it is the positive control", ordinaryRule)
	}

	// WIRE THE MACHINERY THAT WOULD MISBEHAVE: an OPEN violation plus its paired
	// rule_results row, which is exactly the state retraction acts on. Without
	// these rows the call would short-circuit on its own existence probe and
	// return false whether or not the guard is present.
	seedViolation(t, svc, a, protectedRule, ViolationStatusOpen)
	seedViolation(t, svc, a, ordinaryRule, ViolationStatusOpen)
	for _, id := range []string{protectedRule, ordinaryRule} {
		if got := violationStatus(t, db, a.ID, id); got != ViolationStatusOpen {
			t.Fatalf("precondition: violation for %s should be %q, got %q", id, ViolationStatusOpen, got)
		}
		if _, exists := ruleResultRow(t, db, a.ID, id); !exists {
			t.Fatalf("precondition: seeding the violation for %s should have left a rule_results "+
				"row for retraction to delete", id)
		}
	}

	// POSITIVE CONTROL FIRST: the same call on an ordinary rule must genuinely
	// retract. This proves the fixture reaches the writes, so a "nothing
	// happened" result for the event-driven rule is the GUARD and not an inert
	// call.
	retracted, err := svc.RetractRuleVerdict(ctx, a.ID, ordinaryRule)
	if err != nil {
		t.Fatalf("positive control: RetractRuleVerdict on %s: %v", ordinaryRule, err)
	}
	if !retracted {
		t.Fatalf("positive control FAILED: retraction of ordinary rule %s reported nothing "+
			"withdrawn, so this fixture cannot show the guard doing anything", ordinaryRule)
	}
	if got := violationStatus(t, db, a.ID, ordinaryRule); got != ViolationStatusResolved {
		t.Fatalf("positive control FAILED: ordinary rule %s should have been resolved by "+
			"retraction, got %q", ordinaryRule, got)
	}

	// THE ASSERTION.
	retracted, err = svc.RetractRuleVerdict(ctx, a.ID, protectedRule)
	if err != nil {
		t.Fatalf("RetractRuleVerdict on an event-driven rule should be a silent no-op, got error: %v", err)
	}
	if retracted {
		t.Errorf("RetractRuleVerdict must not report withdrawing an event-driven verdict, got true")
	}
	if got := violationStatus(t, db, a.ID, protectedRule); got != ViolationStatusOpen {
		t.Errorf("an event-driven violation must survive retraction -- nothing can re-raise it: "+
			"got %q, want %q", got, ViolationStatusOpen)
	}
	if _, exists := ruleResultRow(t, db, a.ID, protectedRule); !exists {
		t.Errorf("the event-driven rule_results row must survive retraction; retraction deletes it " +
			"outright and no evaluation would ever write it back")
	}
}
