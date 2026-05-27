package rule

// import_test.go exercises the tx-aware import helpers added for #1693.

import (
	"context"
	"errors"
	"testing"
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
// branch by disabling a rule and asserting the helper ran without error.
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
	// Already-disabled by default; flip enabled true then back to false
	// to force the cleanup path.
	r.Enabled = true
	if err := svc.ImportUpdateTx(ctx, db, r); err != nil {
		t.Fatalf("ImportUpdateTx enable: %v", err)
	}
	r.Enabled = false
	if err := svc.ImportUpdateTx(ctx, db, r); err != nil {
		t.Fatalf("ImportUpdateTx disable: %v", err)
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
	r.AutomationMode = AutomationModeAuto
	r.Enabled = false
	if err := svc.ImportUpdateTx(ctx, db, r); err != nil {
		t.Fatalf("ImportUpdateTx: %v", err)
	}

	reloaded, err := svc.GetByID(ctx, RuleThumbExists)
	if err != nil {
		t.Fatalf("reload after ImportUpdateTx: %v", err)
	}
	if reloaded.Enabled {
		t.Error("Enabled should be false after ImportUpdateTx")
	}
	if reloaded.AutomationMode != AutomationModeAuto {
		t.Errorf("AutomationMode: got %q, want %q", reloaded.AutomationMode, AutomationModeAuto)
	}
}
