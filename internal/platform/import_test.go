package platform

// import_test.go exercises the tx-aware import helpers added for #1693.

import (
	"context"
	"testing"
)

// TestImportCreateAndGetTx verifies that a profile written via ImportCreateTx
// is visible to ImportGetByNameTx and ImportUpdateTx mutates the row in place.
func TestImportCreateAndGetTx(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	p := &Profile{
		Name: "ImportTx Profile", NFOEnabled: true, NFOFormat: "kodi",
	}
	if err := svc.ImportCreateTx(ctx, db, p); err != nil {
		t.Fatalf("ImportCreateTx: %v", err)
	}
	if p.ID == "" {
		t.Fatal("ImportCreateTx must assign an ID")
	}

	got, err := svc.ImportGetByNameTx(ctx, db, "ImportTx Profile")
	if err != nil {
		t.Fatalf("ImportGetByNameTx: %v", err)
	}
	if got == nil {
		t.Fatal("ImportGetByNameTx returned nil; profile should exist")
	}
	if got.NFOFormat != "kodi" {
		t.Errorf("NFOFormat: got %q, want kodi", got.NFOFormat)
	}

	got.NFOFormat = "emby"
	if err := svc.ImportUpdateTx(ctx, db, got); err != nil {
		t.Fatalf("ImportUpdateTx: %v", err)
	}
	reloaded, err := svc.ImportGetByNameTx(ctx, db, "ImportTx Profile")
	if err != nil {
		t.Fatalf("reload after ImportUpdateTx: %v", err)
	}
	if reloaded.NFOFormat != "emby" {
		t.Errorf("NFOFormat after update: got %q, want emby", reloaded.NFOFormat)
	}
}

// TestImportCreateTx_DBError pins error propagation.
func TestImportCreateTx_DBError(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	_ = db.Close()
	err := svc.ImportCreateTx(context.Background(), db, &Profile{Name: "x"})
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportGetByNameTx_NotFound confirms the nil-on-miss contract.
func TestImportGetByNameTx_NotFound(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	got, err := svc.ImportGetByNameTx(context.Background(), db, "does-not-exist")
	if err != nil {
		t.Fatalf("ImportGetByNameTx: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing row, got %+v", got)
	}
}
