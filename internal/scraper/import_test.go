package scraper

// import_test.go exercises the tx-aware import helpers added for #1693.

import (
	"context"
	"log/slog"
	"testing"
)

// TestImportSaveConfigTx_NewScopeAssignsID verifies that a fresh scope gets
// a UUID assigned and the row is written through the supplied executor.
func TestImportSaveConfigTx_NewScopeAssignsID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, slog.Default())
	ctx := context.Background()

	cfg := &ScraperConfig{}
	if err := svc.ImportSaveConfigTx(ctx, db, "import-tx-scope", cfg, nil); err != nil {
		t.Fatalf("ImportSaveConfigTx: %v", err)
	}
	if cfg.ID == "" {
		t.Fatal("ImportSaveConfigTx must assign an ID for a new scope")
	}

	loaded, _, err := svc.GetRawConfig(ctx, "import-tx-scope")
	if err != nil {
		t.Fatalf("GetRawConfig: %v", err)
	}
	if loaded == nil {
		t.Fatal("GetRawConfig returned nil; scope should exist")
	}
}

// TestImportSaveConfigTx_DBError verifies that executor failures propagate
// as errors. Covers both the lookup-existing-id error path and the eventual
// INSERT error path.
func TestImportSaveConfigTx_DBError(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, slog.Default())
	_ = db.Close()
	err := svc.ImportSaveConfigTx(context.Background(), db, "closed-scope",
		&ScraperConfig{}, nil)
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportSaveConfigTx_WithOverrides exercises the overrides-not-nil
// branch in saveConfigRowTx.
func TestImportSaveConfigTx_WithOverrides(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, slog.Default())
	ctx := context.Background()

	cfg := &ScraperConfig{}
	overrides := &Overrides{Fields: map[FieldName]bool{FieldBiography: true}}
	if err := svc.ImportSaveConfigTx(ctx, db, "with-overrides", cfg, overrides); err != nil {
		t.Fatalf("ImportSaveConfigTx: %v", err)
	}
	_, gotOverrides, err := svc.GetRawConfig(ctx, "with-overrides")
	if err != nil {
		t.Fatalf("GetRawConfig: %v", err)
	}
	if gotOverrides == nil || !gotOverrides.Fields[FieldBiography] {
		t.Errorf("overrides not persisted: %+v", gotOverrides)
	}
}

// TestImportSaveConfigTx_ExistingScopePreservesID verifies the upsert path:
// passing a cfg without an ID for an existing scope must reuse the existing
// row instead of inserting a duplicate (the original SaveConfig contract).
func TestImportSaveConfigTx_ExistingScopePreservesID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, slog.Default())
	ctx := context.Background()

	first := &ScraperConfig{}
	if err := svc.ImportSaveConfigTx(ctx, db, "tx-upsert-scope", first, nil); err != nil {
		t.Fatalf("first ImportSaveConfigTx: %v", err)
	}
	originalID := first.ID

	// Second call with an empty cfg.ID must reuse the existing row's ID.
	second := &ScraperConfig{}
	if err := svc.ImportSaveConfigTx(ctx, db, "tx-upsert-scope", second, nil); err != nil {
		t.Fatalf("second ImportSaveConfigTx: %v", err)
	}
	if second.ID != originalID {
		t.Errorf("second call ID: got %q, want %q (upsert must preserve existing id)", second.ID, originalID)
	}
}

// TestImportSaveConfigTx_NilArg pins the defensive nil-guard on the
// tx-aware import path.
func TestImportSaveConfigTx_NilArg(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db, slog.Default())
	if err := svc.ImportSaveConfigTx(context.Background(), db, "scope", nil, nil); err == nil {
		t.Fatal("expected error for nil cfg, got nil")
	}
}
