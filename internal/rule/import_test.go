package rule

// import_test.go exercises the tx-aware import helpers added for #1693.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestImportGetByIDTx_RoundTrip verifies the tx-aware reader returns a
// seeded default rule.
func TestImportGetByIDTx_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	got, err := svc.ImportGetByIDTx(ctx, db, RuleThumbExists)
	if err != nil {
		t.Fatalf("ImportGetByIDTx: %v", err)
	}
	if got.ID != RuleThumbExists {
		t.Errorf("rule id: got %q, want %q", got.ID, RuleThumbExists)
	}
}

// TestImportGetByIDTx_NotFound pins the ErrNotFound contract so the
// orchestrator's skip-unknown-id branch keeps working.
func TestImportGetByIDTx_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	_, err := svc.ImportGetByIDTx(ctx, db, "future_unknown_rule")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestImportGetByIDTx_DBError verifies that a closed-DB lookup propagates
// as an error (not a silent ErrNotFound that would mask data loss).
func TestImportGetByIDTx_DBError(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	_ = db.Close()
	_, err := svc.ImportGetByIDTx(context.Background(), db, RuleThumbExists)
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("closed-DB error must not masquerade as ErrNotFound: %v", err)
	}
}

// TestImportUpdateTx_DisabledRuleCleansUp exercises the cleanupDisabledRuleStateTx
// branch by disabling a rule and asserting (a) the Enabled column actually
// flipped on the row, and (b) cleanupDisabledRuleStateTx deleted the seeded
// rule_results row tied to the rule.
func TestImportUpdateTx_DisabledRuleCleansUp(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	r, err := svc.GetByID(ctx, RuleThumbExists)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !r.Enabled {
		t.Fatalf("baseline: seeded rule should be enabled, got Enabled=false")
	}

	// Seed a synthetic artist + rule_results row so cleanupDisabledRuleStateTx
	// has an observable side effect (it removes the row from rule_results
	// for the disabled rule). FK on rule_results.artist_id forces the
	// artist insert.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('cleanup-test-artist', 'Cleanup Test', 'cleanup test', '/tmp/cleanup-test-artist', ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seeding artist row: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO rule_results (artist_id, rule_id, passed, evaluated_at)
		 VALUES ('cleanup-test-artist', ?, 0, ?)`,
		RuleThumbExists, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seeding rule_results row: %v", err)
	}

	r.Enabled = false
	if err := svc.ImportUpdateTx(ctx, db, r); err != nil {
		t.Fatalf("ImportUpdateTx disable: %v", err)
	}
	disabled, err := svc.GetByID(ctx, RuleThumbExists)
	if err != nil {
		t.Fatalf("GetByID after disable: %v", err)
	}
	if disabled.Enabled {
		t.Errorf("Enabled should be false after disable; got true")
	}
	// Cleanup post-condition: the seeded rule_results row must be gone.
	var cnt int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rule_results WHERE rule_id = ?`, RuleThumbExists,
	).Scan(&cnt); err != nil {
		t.Fatalf("counting rule_results after disable: %v", err)
	}
	if cnt != 0 {
		t.Errorf("rule_results rows for disabled rule: got %d, want 0", cnt)
	}
}

// TestImportUpdateTx_EnabledRoundTrip writes a rule via the tx-aware update
// path and reads it back through the standard List/Get path to confirm the
// row was modified.
func TestImportUpdateTx_EnabledRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	r, err := svc.GetByID(ctx, RuleThumbExists)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	// Capture the seeded baseline. The test asserts state transitions
	// relative to the captured values rather than hardcoded constants so
	// future changes to the rule seeds do not produce false-positive
	// failures here.
	baselineEnabled := r.Enabled
	baselineMode := r.AutomationMode
	if !baselineEnabled {
		t.Fatalf("baseline: seeded RuleThumbExists should be enabled, got Enabled=false")
	}

	// Pick a target AutomationMode that differs from the baseline so the
	// transition is observable. AutomationModeManual is the broadest non-
	// auto value; flip to auto if baseline is already manual, otherwise
	// flip to manual.
	targetMode := AutomationModeAuto
	if baselineMode == AutomationModeAuto {
		targetMode = AutomationModeManual
	}

	r.Enabled = false
	r.AutomationMode = targetMode
	if err := svc.ImportUpdateTx(ctx, db, r); err != nil {
		t.Fatalf("ImportUpdateTx: %v", err)
	}

	reloaded, err := svc.GetByID(ctx, RuleThumbExists)
	if err != nil {
		t.Fatalf("reload after ImportUpdateTx: %v", err)
	}
	if reloaded.Enabled {
		t.Errorf("Enabled should have flipped to false; got true (baseline was %v)", baselineEnabled)
	}
	if reloaded.AutomationMode != targetMode {
		t.Errorf("AutomationMode should have flipped to %q; got %q (baseline was %q)", targetMode, reloaded.AutomationMode, baselineMode)
	}
}

// TestImportUpdateTx_NilArg pins the defensive nil-guard on the tx-aware
// import path.
func TestImportUpdateTx_NilArg(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	if err := svc.ImportUpdateTx(context.Background(), db, nil); err == nil {
		t.Fatal("expected error for nil rule, got nil")
	}
}
