package rule

// event_driven_disable_test.go pins the #2614 invariant: disabling an
// EVENT-DRIVEN rule must never destroy its violations.
//
// WHY THIS IS DATA LOSS RATHER THAN CLEANUP
//
// cleanupDisabledRuleState rests on an unstated premise: a disabled rule never
// runs again, so nothing will ever mark its violations resolved, so
// soft-resolving them now is safe housekeeping (#1143). That premise holds for
// EVALUATED rules, whose findings a later Run Rules re-derives from artist
// state.
//
// It is false for EVENT-DRIVEN rules. Their violations are raised at write/push
// chokepoints, and their checkers cannot re-derive them -- the registered
// checker for the collision rule returns nil for every artist unconditionally
// (engine.go). So a disable does not park those findings, it destroys them, and
// re-enabling recovers nothing.
//
// The engine already enforces this exact invariant on the evaluation path:
// eligibleRules skips event-driven rules BEFORE consulting Enabled, with the
// comment "keying that safety off the Enabled toggle would put silent data loss
// one UI click away". The disable path is the other route to that same click.
// These tests are the second enforcement point.
//
// TWO SITES, DELIBERATELY TESTED SEPARATELY. cleanupDisabledRuleState
// (service.go) and cleanupDisabledRuleStateTx (import.go) are mirrored logic
// reached by different callers -- manual Update / DisableFilesystemRules, and
// the settings-import transaction. Guarding one leaves the other open, so a
// test that exercised only one path would pass over a live hole.

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// seedCollisionViolation inserts an artist plus an active violation and a
// rule_results row for an event-driven rule, mimicking what the #2613 notifier
// leaves behind at an image-write chokepoint.
//
// status is a parameter because BOTH active states must survive: cleanup
// soft-resolves `open` AND `pending_choice`, so a test that only seeded `open`
// would miss half of what the guard has to protect.
func seedCollisionViolation(t *testing.T, db DBExecutor, artistID, status string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		artistID, "Collision "+artistID, "collision "+artistID, "/tmp/"+artistID, now, now,
	); err != nil {
		t.Fatalf("seeding artist %s: %v", artistID, err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO rule_violations
		   (id, rule_id, artist_id, artist_name, severity, message, fixable, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'warning', 'shared backdrop with another artist', 1, ?, ?, ?)`,
		"viol-"+artistID, RuleCrossArtistBackdropCollision, artistID,
		"Collision "+artistID, status, now, now,
	); err != nil {
		t.Fatalf("seeding %s violation for %s: %v", status, artistID, err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO rule_results (artist_id, rule_id, passed, evaluated_at)
		 VALUES (?, ?, 0, ?)`,
		artistID, RuleCrossArtistBackdropCollision, now,
	); err != nil {
		t.Fatalf("seeding rule_results for %s: %v", artistID, err)
	}
}

// collisionStatus reads the stored status of the collision violation for an
// artist. Thin wrapper over the package's existing violationStatus helper
// (retract_skipped_results_test.go) rather than a second implementation: that
// helper returns "" for a row that is GONE, which matters here, since a hard
// delete and a soft-resolve are different failures and both must be visible.
func collisionStatus(t *testing.T, db *sql.DB, artistID string) string {
	t.Helper()
	return violationStatus(t, db, artistID, RuleCrossArtistBackdropCollision)
}

func ruleResultCount(t *testing.T, db DBExecutor, ruleID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM rule_results WHERE rule_id = ?`, ruleID,
	).Scan(&n); err != nil {
		t.Fatalf("counting rule_results for %s: %v", ruleID, err)
	}
	return n
}

// TestUpdate_DisablingEventDrivenRulePreservesViolations covers the manual
// Update path (service.go), the route an operator takes by toggling the rule
// off in the UI.
//
// PRECONDITIONS ARE ASSERTED, not assumed. Without the "seeded as active"
// check, a seed that silently failed would leave nothing to destroy and the
// test would pass having proven nothing.
func TestUpdate_DisablingEventDrivenRulePreservesViolations(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Both active states, because cleanup soft-resolves both.
	seedCollisionViolation(t, db, "collide-open", ViolationStatusOpen)
	seedCollisionViolation(t, db, "collide-pending", ViolationStatusPendingChoice)

	if got := collisionStatus(t, db, "collide-open"); got != ViolationStatusOpen {
		t.Fatalf("precondition: seeded violation should be %q, got %q", ViolationStatusOpen, got)
	}
	if got := collisionStatus(t, db, "collide-pending"); got != ViolationStatusPendingChoice {
		t.Fatalf("precondition: seeded violation should be %q, got %q", ViolationStatusPendingChoice, got)
	}
	if n := ruleResultCount(t, db, RuleCrossArtistBackdropCollision); n != 2 {
		t.Fatalf("precondition: expected 2 seeded rule_results rows, got %d", n)
	}

	// MODEL THE REAL OPERATOR JOURNEY: enable, then disable.
	//
	// The collision rule seeds DISABLED on purpose (service.go), so an operator
	// who wants collision findings must first turn it on -- which is exactly why
	// the later toggle-off is dangerous: by then a backlog has accumulated.
	//
	// This ordering is load-bearing, not ceremony. Update only runs the cleanup
	// on the enabled -> disabled TRANSITION, so setting Enabled=false on a rule
	// that is ALREADY disabled skips cleanup entirely: the violations would
	// survive, and the test would PASS against the unfixed code while proving
	// nothing. The enable step is what makes the disable destructive.
	r, err := svc.GetByID(ctx, RuleCrossArtistBackdropCollision)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !r.Enabled {
		r.Enabled = true
		if err := svc.Update(ctx, r); err != nil {
			t.Fatalf("Update enable: %v", err)
		}
		if r, err = svc.GetByID(ctx, RuleCrossArtistBackdropCollision); err != nil {
			t.Fatalf("GetByID after enable: %v", err)
		}
	}
	if !r.Enabled {
		t.Fatalf("precondition: rule must be ENABLED before the disable, or the "+
			"cleanup transition never fires and this test is vacuous (Enabled=%v)", r.Enabled)
	}
	// The enable must not have disturbed the seeded rows.
	if n := ruleResultCount(t, db, RuleCrossArtistBackdropCollision); n != 2 {
		t.Fatalf("precondition: enable should not touch rule_results, got %d want 2", n)
	}

	r.Enabled = false
	if err := svc.Update(ctx, r); err != nil {
		t.Fatalf("Update disable: %v", err)
	}

	// The disable itself must still take effect -- the guard skips the
	// CLEANUP, not the toggle. Without this, a guard that accidentally
	// short-circuited the whole Update would pass the survival checks below
	// while breaking the feature.
	disabled, err := svc.GetByID(ctx, RuleCrossArtistBackdropCollision)
	if err != nil {
		t.Fatalf("GetByID after disable: %v", err)
	}
	if disabled.Enabled {
		t.Errorf("rule should be disabled after Update; got Enabled=true")
	}

	if got := collisionStatus(t, db, "collide-open"); got != ViolationStatusOpen {
		t.Errorf("open collision violation must survive disable: got status %q, want %q",
			got, ViolationStatusOpen)
	}
	if got := collisionStatus(t, db, "collide-pending"); got != ViolationStatusPendingChoice {
		t.Errorf("pending_choice collision violation must survive disable: got status %q, want %q",
			got, ViolationStatusPendingChoice)
	}
	if n := ruleResultCount(t, db, RuleCrossArtistBackdropCollision); n != 2 {
		t.Errorf("rule_results rows must survive disable for an event-driven rule: got %d, want 2", n)
	}
}

// TestImportUpdateTx_DisablingEventDrivenRulePreservesViolations covers the
// settings-import path (import.go). Same invariant, different caller: the
// issue notes fixing only service.go leaves this hole open.
func TestImportUpdateTx_DisablingEventDrivenRulePreservesViolations(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	seedCollisionViolation(t, db, "import-open", ViolationStatusOpen)
	seedCollisionViolation(t, db, "import-pending", ViolationStatusPendingChoice)

	if got := collisionStatus(t, db, "import-open"); got != ViolationStatusOpen {
		t.Fatalf("precondition: seeded violation should be %q, got %q", ViolationStatusOpen, got)
	}
	if n := ruleResultCount(t, db, RuleCrossArtistBackdropCollision); n != 2 {
		t.Fatalf("precondition: expected 2 seeded rule_results rows, got %d", n)
	}

	r, err := svc.GetByID(ctx, RuleCrossArtistBackdropCollision)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	r.Enabled = false
	if err := svc.ImportUpdateTx(ctx, db, r); err != nil {
		t.Fatalf("ImportUpdateTx disable: %v", err)
	}

	disabled, err := svc.GetByID(ctx, RuleCrossArtistBackdropCollision)
	if err != nil {
		t.Fatalf("GetByID after import disable: %v", err)
	}
	if disabled.Enabled {
		t.Errorf("rule should be disabled after ImportUpdateTx; got Enabled=true")
	}

	if got := collisionStatus(t, db, "import-open"); got != ViolationStatusOpen {
		t.Errorf("open collision violation must survive an import disable: got status %q, want %q",
			got, ViolationStatusOpen)
	}
	if got := collisionStatus(t, db, "import-pending"); got != ViolationStatusPendingChoice {
		t.Errorf("pending_choice collision violation must survive an import disable: got status %q, want %q",
			got, ViolationStatusPendingChoice)
	}
	if n := ruleResultCount(t, db, RuleCrossArtistBackdropCollision); n != 2 {
		t.Errorf("rule_results rows must survive an import disable: got %d, want 2", n)
	}
}

// TestUpdate_DisablingEvaluatedRuleStillCleansUp is the OTHER HALF of the
// guard, and the one that stops it from being a blanket "never clean up".
//
// A guard keyed on the wrong predicate -- or one that simply returned early for
// every rule -- would pass both tests above while silently disabling #1143's
// cleanup for the entire evaluated catalogue. That regression is invisible to a
// survival-only test suite, so this asserts the ORIGINAL behavior is intact
// for an ordinary evaluated rule: its violations still soft-resolve and its
// rule_results rows are still deleted.
func TestUpdate_DisablingEvaluatedRuleStillCleansUp(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('evaluated-artist', 'Evaluated', 'evaluated', '/tmp/evaluated', ?, ?)`,
		now, now,
	); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO rule_violations
		   (id, rule_id, artist_id, artist_name, severity, message, fixable, status, created_at, updated_at)
		 VALUES ('viol-evaluated', ?, 'evaluated-artist', 'Evaluated', 'warning', 'missing thumb', 1, ?, ?, ?)`,
		RuleThumbExists, ViolationStatusOpen, now, now,
	); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO rule_results (artist_id, rule_id, passed, evaluated_at)
		 VALUES ('evaluated-artist', ?, 0, ?)`,
		RuleThumbExists, now,
	); err != nil {
		t.Fatalf("seeding rule_results: %v", err)
	}

	// Sanity: the rule under test must NOT be event-driven, or this test
	// would assert the guard's behavior rather than the cleanup's.
	if IsEventDriven(RuleThumbExists) {
		t.Fatalf("precondition: %s must not be event-driven for this test to mean anything", RuleThumbExists)
	}

	r, err := svc.GetByID(ctx, RuleThumbExists)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	r.Enabled = false
	if err := svc.Update(ctx, r); err != nil {
		t.Fatalf("Update disable: %v", err)
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM rule_violations WHERE id = 'viol-evaluated'`,
	).Scan(&status); err != nil {
		t.Fatalf("reading evaluated-rule violation: %v", err)
	}
	if status != ViolationStatusResolved {
		t.Errorf("an EVALUATED rule's violation must still soft-resolve on disable (#1143): got %q, want %q",
			status, ViolationStatusResolved)
	}
	if n := ruleResultCount(t, db, RuleThumbExists); n != 0 {
		t.Errorf("an EVALUATED rule's rule_results must still be deleted on disable (#1143): got %d, want 0", n)
	}
}
