package webhook

// import_test.go exercises the tx-aware import helpers added for #1693.

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
)

// setupTestServiceWithDB returns the service AND the underlying DB so the
// import_test helpers can drive the DBExecutor-shaped methods directly.
func setupTestServiceWithDB(t *testing.T) (*Service, DBExecutor) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewService(db), db
}

// TestImportCreateAndGetTx pins the happy path for the tx-aware helpers.
func TestImportCreateAndGetTx(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	ctx := context.Background()

	w := &Webhook{
		Name: "ImportTx Hook", URL: "https://imp.example/hook", Type: TypeGeneric,
		Events: []string{"artist.new"}, Enabled: true,
	}
	if err := svc.ImportCreateTx(ctx, db, w); err != nil {
		t.Fatalf("ImportCreateTx: %v", err)
	}
	if w.ID == "" {
		t.Fatal("ImportCreateTx must assign an ID")
	}

	got, err := svc.ImportGetByNameAndURLTx(ctx, db, "ImportTx Hook", "https://imp.example/hook")
	if err != nil {
		t.Fatalf("ImportGetByNameAndURLTx: %v", err)
	}
	if got == nil {
		t.Fatal("ImportGetByNameAndURLTx returned nil; row should exist")
	}

	got.Enabled = false
	if err := svc.ImportUpdateTx(ctx, db, got); err != nil {
		t.Fatalf("ImportUpdateTx: %v", err)
	}
	reloaded, err := svc.ImportGetByNameAndURLTx(ctx, db, "ImportTx Hook", "https://imp.example/hook")
	if err != nil {
		t.Fatalf("reload after ImportUpdateTx: %v", err)
	}
	if reloaded.Enabled {
		t.Error("Enabled should be false after ImportUpdateTx")
	}
}

// TestImportCreateTx_ValidationErrors covers the empty-name and empty-URL
// guard clauses so both required-field branches are credited.
func TestImportCreateTx_ValidationErrors(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	ctx := context.Background()

	if err := svc.ImportCreateTx(ctx, db, &Webhook{URL: "https://x"}); err == nil {
		t.Error("expected error on empty name")
	}
	if err := svc.ImportCreateTx(ctx, db, &Webhook{Name: "x"}); err == nil {
		t.Error("expected error on empty URL")
	}
}

// TestImportCreateTx_DefaultsTypeToGeneric verifies the type-defaulting
// branch fires when no Type is supplied.
func TestImportCreateTx_DefaultsTypeToGeneric(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	ctx := context.Background()

	w := &Webhook{Name: "default-type", URL: "https://x", Events: []string{"e"}}
	if err := svc.ImportCreateTx(ctx, db, w); err != nil {
		t.Fatalf("ImportCreateTx: %v", err)
	}
	if w.Type != TypeGeneric {
		t.Errorf("Type: got %q, want %q (default)", w.Type, TypeGeneric)
	}
}

// TestImportUpdateTx_NotFound exercises the rows-affected=0 branch.
func TestImportUpdateTx_NotFound(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	err := svc.ImportUpdateTx(context.Background(), db, &Webhook{
		ID: "no-such-id", Name: "x", URL: "https://x",
	})
	if err == nil {
		t.Fatal("expected error when updating missing row, got nil")
	}
}

// TestImportCreateTx_DBError covers the inserting-webhook error branch.
func TestImportCreateTx_DBError(t *testing.T) {
	t.Parallel()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	svc := NewService(db)
	_ = db.Close()
	werr := svc.ImportCreateTx(context.Background(), db, &Webhook{
		Name: "x", URL: "https://x", Type: TypeGeneric,
	})
	if werr == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportGetByNameAndURLTx_NotFound confirms the nil-on-miss contract.
func TestImportGetByNameAndURLTx_NotFound(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	got, err := svc.ImportGetByNameAndURLTx(context.Background(), db, "nope", "https://nowhere")
	if err != nil {
		t.Fatalf("ImportGetByNameAndURLTx: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing row, got %+v", got)
	}
}
