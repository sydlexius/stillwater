package rule

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// Issue #2972: evaluation writes used to be unordered. Two evaluations of the
// same artist can be in flight at once (a pipeline run and a health-subscriber
// evaluation fired by an ArtistUpdated event), and nothing makes them commit in
// the order they started. Whichever wrote LAST decided the stored verdict, so a
// delayed OLDER pass erased a NEWER failure, and an older FAIL outlived the pass
// that fixed it.
//
// The fix orders both write paths on rule_results.evaluated_at. These tests pin
// that contract by stamping the timestamps explicitly -- never by sleeping, and
// never by letting wall-clock order decide -- and by reading the surviving
// verdict straight out of both tables.

// orderingRuleID is a rule that exists in the seeded default set and is NOT
// event-driven, so RecordRulePass and UpsertViolation both act on it rather
// than short-circuiting on the IsEventDriven guard.
const orderingRuleID = "nfo_exists"

// Two fixed evaluation times, one second apart. One second is the granularity
// evaluated_at is stored at, so this is the smallest genuinely orderable gap
// and keeps the fixtures honest about what the column can actually express.
var (
	olderEval = time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	newerEval = time.Date(2026, 8, 12, 10, 0, 1, 0, time.UTC)
)

// orderingFixture is the seeded (db, artist) pair every case below starts from.
type orderingFixture struct {
	db *sql.DB
	a  *artist.Artist
}

// newOrderingFixture builds a real SQLite database with the default rules
// seeded and one artist, and asserts the rule under test is present and not
// event-driven. Without that second check every case here could pass against a
// rule nothing ever writes.
func newOrderingFixture(t *testing.T) (*orderingFixture, *Service) {
	t.Helper()
	db := setupSubscriberTestDB(t)
	svc := NewService(db)
	if err := svc.SeedDefaults(t.Context()); err != nil {
		t.Fatalf("seeding rule defaults: %v", err)
	}
	if _, err := svc.GetByID(t.Context(), orderingRuleID); err != nil {
		t.Fatalf("precondition: rule %q must exist in the seeded set: %v", orderingRuleID, err)
	}
	if IsEventDriven(orderingRuleID) {
		t.Fatalf("precondition: rule %q is event-driven, so the write paths under test short-circuit on it", orderingRuleID)
	}

	a := &artist.Artist{Name: "Ordering Fixture", SortName: "Ordering Fixture", Path: "/music/ordering-fixture"}
	if err := artist.NewService(db).Create(t.Context(), a); err != nil {
		t.Fatalf("creating fixture artist: %v", err)
	}
	return &orderingFixture{db: db, a: a}, svc
}

// writeFail records a FAIL verdict for the fixture pair, stamped with the given
// evaluation time. This is the real production fail path (UpsertViolation
// writes the violation row and the sibling rule_results fail row in one
// transaction), not a hand-rolled INSERT.
func (f *orderingFixture) writeFail(t *testing.T, svc *Service, evaluatedAt time.Time) {
	t.Helper()
	if err := svc.UpsertViolation(t.Context(), &RuleViolation{
		RuleID:      orderingRuleID,
		ArtistID:    f.a.ID,
		ArtistName:  f.a.Name,
		Severity:    "error",
		Message:     "ordering fixture failure",
		Fixable:     true,
		Status:      ViolationStatusOpen,
		EvaluatedAt: evaluatedAt,
	}); err != nil {
		t.Fatalf("writing fail verdict at %s: %v", evaluatedAt.Format(time.RFC3339), err)
	}
}

// writePass records a PASS verdict for the fixture pair through RecordRulePass,
// the single production entry point for a pass (it writes the pass row and
// resolves the active violation atomically, #2519). Returns whether it reported
// clearing a violation.
func (f *orderingFixture) writePass(t *testing.T, svc *Service, evaluatedAt time.Time) bool {
	t.Helper()
	resolved, err := svc.RecordRulePass(t.Context(), f.a.ID, orderingRuleID, evaluatedAt)
	if err != nil {
		t.Fatalf("writing pass verdict at %s: %v", evaluatedAt.Format(time.RFC3339), err)
	}
	return resolved
}

// storedResult reads the surviving rule_results verdict for the fixture pair
// directly from the table, so no service-layer default can satisfy an
// assertion. Fails the test when the row is missing: every case here writes at
// least one verdict first, so an absent row means a write silently vanished.
func (f *orderingFixture) storedResult(t *testing.T) (passed bool, evaluatedAt string) {
	t.Helper()
	var passedInt int
	err := f.db.QueryRowContext(t.Context(),
		`SELECT passed, evaluated_at FROM rule_results WHERE artist_id = ? AND rule_id = ?`,
		f.a.ID, orderingRuleID).Scan(&passedInt, &evaluatedAt)
	if err != nil {
		t.Fatalf("reading rule_results row for the fixture pair: %v", err)
	}
	return passedInt != 0, evaluatedAt
}

// assertStoredEvaluatedAt pins the row's ordering stamp. Asserting the verdict
// alone would let a guard that rejected the whole write pass a staleness test
// while also breaking the fresh-write case, so the timestamp is checked
// alongside it.
func (f *orderingFixture) assertStoredEvaluatedAt(t *testing.T, want time.Time) {
	t.Helper()
	_, got := f.storedResult(t)
	if wantTS := want.UTC().Format(time.RFC3339); got != wantTS {
		t.Errorf("rule_results.evaluated_at = %q, want %q", got, wantTS)
	}
}

// assertPrecondition asserts the state the case is about to write ONTO. Without
// it a case whose seed silently failed would assert against an empty table and
// pass having proven nothing.
func (f *orderingFixture) assertPrecondition(t *testing.T, wantPassed bool, wantEvaluatedAt time.Time) {
	t.Helper()
	passed, gotTS := f.storedResult(t)
	if passed != wantPassed {
		t.Fatalf("precondition: stored verdict passed = %v, want %v", passed, wantPassed)
	}
	if wantTS := wantEvaluatedAt.UTC().Format(time.RFC3339); gotTS != wantTS {
		t.Fatalf("precondition: stored evaluated_at = %q, want %q -- the two writes must genuinely carry different timestamps in the intended order", gotTS, wantTS)
	}
}

// TestWriteOrdering_StalePassCannotEraseNewerFailure is the defect's headline
// direction: the newer FAIL commits first, then the older PASS arrives late.
// Before the guard the pass won on write order, flipping passed to 1 and
// resolving the violation, so the real failure vanished from the compliance
// report until the next full run.
//
// Mutants this kills: dropping the WHERE from upsertRuleResultPassExec (the
// stale pass overwrites the row), and dropping the applied check in
// RecordRulePass (the row survives but the violation is resolved anyway,
// leaving the two tables contradicting each other).
func TestWriteOrdering_StalePassCannotEraseNewerFailure(t *testing.T) {
	f, svc := newOrderingFixture(t)

	f.writeFail(t, svc, newerEval)
	f.assertPrecondition(t, false, newerEval)
	if got := violationStatus(t, f.db, f.a.ID, orderingRuleID); got != ViolationStatusOpen {
		t.Fatalf("precondition: violation status = %q, want %q", got, ViolationStatusOpen)
	}

	resolved := f.writePass(t, svc, olderEval)

	if passed, _ := f.storedResult(t); passed {
		t.Error("stale pass overwrote the newer failure: rule_results.passed = 1, want 0")
	}
	f.assertStoredEvaluatedAt(t, newerEval)
	if resolved {
		t.Error("stale pass reported resolving a violation, want false -- it cleared nothing")
	}
	if got := violationStatus(t, f.db, f.a.ID, orderingRuleID); got != ViolationStatusOpen {
		t.Errorf("stale pass resolved the newer failure's violation: status = %q, want %q", got, ViolationStatusOpen)
	}
}

// TestWriteOrdering_StaleFailCannotOutliveNewerPass is the other direction: the
// newer PASS commits first, then the older FAIL arrives late. Before the guard
// the stale failure won on write order and kept counting against compliance
// with no evaluation left that agreed with it.
//
// Mutant this kills: dropping the WHERE from upsertRuleResultFailExec, or
// dropping the staleness skip at the top of UpsertViolation.
func TestWriteOrdering_StaleFailCannotOutliveNewerPass(t *testing.T) {
	f, svc := newOrderingFixture(t)

	f.writePass(t, svc, newerEval)
	f.assertPrecondition(t, true, newerEval)

	f.writeFail(t, svc, olderEval)

	if passed, _ := f.storedResult(t); !passed {
		t.Error("stale fail overwrote the newer pass: rule_results.passed = 0, want 1")
	}
	f.assertStoredEvaluatedAt(t, newerEval)
	if got := violationStatus(t, f.db, f.a.ID, orderingRuleID); got == ViolationStatusOpen {
		t.Error("stale fail raised an open violation over a newer pass, want no open violation")
	}
}

// TestWriteOrdering_StaleWriteDoesNotBumpUpdatedAt covers the trap that makes a
// partial guard self-defeating. A guard that protects the verdict columns but
// still lets a stale write bump rule_violations.updated_at leaves the row
// CLAIMING a freshness it does not have, and any later decision that reads it
// inherits the lie. The whole stale write must be a no-op, every column of it.
//
// Mutant this kills: turning the staleness skip in UpsertViolation into a
// per-column CASE that covers status and candidates but not updated_at.
func TestWriteOrdering_StaleWriteDoesNotBumpUpdatedAt(t *testing.T) {
	f, svc := newOrderingFixture(t)

	f.writeFail(t, svc, newerEval)
	f.assertPrecondition(t, false, newerEval)

	readViolationRow := func() (updatedAt, message string) {
		t.Helper()
		if err := f.db.QueryRowContext(t.Context(),
			`SELECT updated_at, message FROM rule_violations WHERE artist_id = ? AND rule_id = ?`,
			f.a.ID, orderingRuleID).Scan(&updatedAt, &message); err != nil {
			t.Fatalf("reading rule_violations row: %v", err)
		}
		return updatedAt, message
	}
	beforeUpdatedAt, beforeMessage := readViolationRow()

	// A stale re-raise carrying a DIFFERENT message, so a write that lands is
	// visible in the data rather than being indistinguishable from a no-op.
	if err := svc.UpsertViolation(t.Context(), &RuleViolation{
		RuleID:      orderingRuleID,
		ArtistID:    f.a.ID,
		ArtistName:  f.a.Name,
		Severity:    "error",
		Message:     "stale re-raise that must not land",
		Fixable:     true,
		Status:      ViolationStatusOpen,
		EvaluatedAt: olderEval,
	}); err != nil {
		t.Fatalf("stale re-raise returned an error, want a silent no-op: %v", err)
	}

	afterUpdatedAt, afterMessage := readViolationRow()
	if afterUpdatedAt != beforeUpdatedAt {
		t.Errorf("stale write bumped rule_violations.updated_at: %q -> %q, want unchanged", beforeUpdatedAt, afterUpdatedAt)
	}
	if afterMessage != beforeMessage {
		t.Errorf("stale write replaced rule_violations.message: %q -> %q, want unchanged", beforeMessage, afterMessage)
	}
	f.assertStoredEvaluatedAt(t, newerEval)
}

// TestWriteOrdering_FreshWritesStillWin is the POSITIVE CONTROL, and it is the
// reason it exists: a guard that rejected EVERYTHING would pass both staleness
// tests above while breaking evaluation entirely, because nothing would ever
// record a new verdict again. Each case here drives a genuinely NEWER or
// equal-time write that MUST apply.
//
// The equal-timestamp cases are not an edge case dressed up as one. A single
// pipeline pass stamps every row it writes with one startedAt, and for a rule it
// repairs it writes the fail row and then the pass row at that same timestamp.
// If an equal timestamp were rejected, the ordinary fix flow would leave
// passed=0 next to a resolved violation on every repair.
func TestWriteOrdering_FreshWritesStillWin(t *testing.T) {
	tests := []struct {
		name string
		// seed and then follow are applied in order; want* describe the state
		// the follow-up write must leave behind.
		seedPass    bool
		seedAt      time.Time
		followPass  bool
		followAt    time.Time
		wantPassed  bool
		wantOpenVio bool
	}{
		{
			name:     "newer pass clears an older failure",
			seedPass: false, seedAt: olderEval,
			followPass: true, followAt: newerEval,
			wantPassed: true, wantOpenVio: false,
		},
		{
			name:     "newer fail overrides an older pass",
			seedPass: true, seedAt: olderEval,
			followPass: false, followAt: newerEval,
			wantPassed: false, wantOpenVio: true,
		},
		{
			name:     "equal-time pass clears a failure from the same pass",
			seedPass: false, seedAt: newerEval,
			followPass: true, followAt: newerEval,
			wantPassed: true, wantOpenVio: false,
		},
		{
			name:     "equal-time fail applies over a pass from the same pass",
			seedPass: true, seedAt: newerEval,
			followPass: false, followAt: newerEval,
			wantPassed: false, wantOpenVio: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, svc := newOrderingFixture(t)

			if tc.seedPass {
				f.writePass(t, svc, tc.seedAt)
			} else {
				f.writeFail(t, svc, tc.seedAt)
			}
			f.assertPrecondition(t, tc.seedPass, tc.seedAt)

			if tc.followPass {
				f.writePass(t, svc, tc.followAt)
			} else {
				f.writeFail(t, svc, tc.followAt)
			}

			if passed, _ := f.storedResult(t); passed != tc.wantPassed {
				t.Errorf("rule_results.passed = %v, want %v -- the fresh write did not apply", passed, tc.wantPassed)
			}
			f.assertStoredEvaluatedAt(t, tc.followAt)

			gotOpen := violationStatus(t, f.db, f.a.ID, orderingRuleID) == ViolationStatusOpen
			if gotOpen != tc.wantOpenVio {
				t.Errorf("open violation present = %v, want %v -- rule_violations disagrees with rule_results", gotOpen, tc.wantOpenVio)
			}
		})
	}
}

// TestWriteOrdering_DirectResultWritesAreOrdered covers the two exported
// result-row entry points, UpsertRuleResultFail and UpsertRuleResultPass,
// rather than the UpsertViolation / RecordRulePass routines that wrap them.
//
// This is not duplicate coverage. On the wrapped fail path, UpsertViolation's
// Go-side staleness skip returns before the SQL ever runs, so the WHERE
// predicate in upsertRuleResultFailExec is SHADOWED there and a test driving
// only that path cannot tell whether the predicate exists. UpsertRuleResultFail
// has no Go-side guard in front of it, so the predicate is the entire
// enforcement -- which makes this the test that holds it up.
//
// Mutants this kills: dropping either WHERE clause in sqlite_rule_results.go.
func TestWriteOrdering_DirectResultWritesAreOrdered(t *testing.T) {
	t.Run("stale direct fail does not overwrite a newer pass", func(t *testing.T) {
		f, svc := newOrderingFixture(t)
		if err := svc.UpsertRuleResultPass(t.Context(), f.a.ID, orderingRuleID, newerEval); err != nil {
			t.Fatalf("seeding pass row: %v", err)
		}
		f.assertPrecondition(t, true, newerEval)

		if err := svc.UpsertRuleResultFail(t.Context(), f.a.ID, orderingRuleID, "", "stale direct fail", olderEval); err != nil {
			t.Fatalf("stale direct fail: %v", err)
		}

		if passed, _ := f.storedResult(t); !passed {
			t.Error("stale direct fail overwrote the newer pass: rule_results.passed = 0, want 1")
		}
		f.assertStoredEvaluatedAt(t, newerEval)
	})

	t.Run("stale direct pass does not overwrite a newer fail", func(t *testing.T) {
		f, svc := newOrderingFixture(t)
		if err := svc.UpsertRuleResultFail(t.Context(), f.a.ID, orderingRuleID, "", "seeded fail", newerEval); err != nil {
			t.Fatalf("seeding fail row: %v", err)
		}
		f.assertPrecondition(t, false, newerEval)

		if err := svc.UpsertRuleResultPass(t.Context(), f.a.ID, orderingRuleID, olderEval); err != nil {
			t.Fatalf("stale direct pass: %v", err)
		}

		if passed, _ := f.storedResult(t); passed {
			t.Error("stale direct pass overwrote the newer fail: rule_results.passed = 1, want 0")
		}
		f.assertStoredEvaluatedAt(t, newerEval)
	})

	// Positive control for this pair of entry points specifically: a guard
	// that rejected every direct write would pass both cases above.
	t.Run("fresh direct fail applies over an older pass", func(t *testing.T) {
		f, svc := newOrderingFixture(t)
		if err := svc.UpsertRuleResultPass(t.Context(), f.a.ID, orderingRuleID, olderEval); err != nil {
			t.Fatalf("seeding pass row: %v", err)
		}
		f.assertPrecondition(t, true, olderEval)

		if err := svc.UpsertRuleResultFail(t.Context(), f.a.ID, orderingRuleID, "", "fresh direct fail", newerEval); err != nil {
			t.Fatalf("fresh direct fail: %v", err)
		}

		if passed, _ := f.storedResult(t); passed {
			t.Error("fresh direct fail did not apply: rule_results.passed = 1, want 0")
		}
		f.assertStoredEvaluatedAt(t, newerEval)
	})
}

// TestWriteOrdering_UnstampedRaiseStillApplies covers the callers that ran no
// evaluation at all. RaiseBackdropCollision and RaiseMBIDValidationFailure
// build a RuleViolation with no EvaluatedAt, because a collision or a failed
// MBID re-validation happens AT the moment of the event rather than as the
// outcome of an evaluation pass. Those raises must still land: a raise is
// additive, and refusing one loses a finding nothing can re-derive.
//
// Mutant this kills: treating a zero EvaluatedAt as "older than everything" and
// skipping the write, rather than falling back to the service clock.
func TestWriteOrdering_UnstampedRaiseStillApplies(t *testing.T) {
	f, svc := newOrderingFixture(t)

	// Seed a pass stamped in the FUTURE relative to the raise's wall-clock
	// fallback would be the hostile case, but the honest one is a stamp in the
	// past: an unstamped raise means "now", and now is newer than any stored
	// evaluation, so it applies.
	f.writePass(t, svc, olderEval)
	f.assertPrecondition(t, true, olderEval)

	if err := svc.UpsertViolation(t.Context(), &RuleViolation{
		RuleID:     orderingRuleID,
		ArtistID:   f.a.ID,
		ArtistName: f.a.Name,
		Severity:   "warning",
		Message:    "raised by a non-evaluation caller",
		Fixable:    true,
		Status:     ViolationStatusOpen,
		// EvaluatedAt deliberately left zero.
	}); err != nil {
		t.Fatalf("unstamped raise: %v", err)
	}

	if passed, _ := f.storedResult(t); passed {
		t.Error("unstamped raise was skipped as stale: rule_results.passed = 1, want 0")
	}
	if got := violationStatus(t, f.db, f.a.ID, orderingRuleID); got != ViolationStatusOpen {
		t.Errorf("unstamped raise did not open a violation: status = %q, want %q", got, ViolationStatusOpen)
	}
}
