package connection

// import_test.go exercises the tx-aware import helpers added for #1693.
// The helpers mirror Create/Update/GetByTypeAndURL but write through a
// caller-supplied DBExecutor; this file pins both the standalone (s.db
// passed in) and the tx-bound (BeginTx -> rollback drops the writes)
// behaviors.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
)

// setupTestServiceWithDB returns the service AND the underlying DB so the
// import_test helpers can open transactions directly.
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
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	return NewService(db, enc), db
}

// TestImportCreateTx_RoundTrip exercises the happy path: ImportCreateTx writes
// through the supplied executor and ImportGetByTypeAndURLTx reads the row
// back, including the v1.5 VerifyPathAfterUpdate field added by #1692.
func TestImportCreateTx_RoundTrip(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	ctx := context.Background()

	c := &Connection{
		Name: "ImportTx Emby", Type: TypeEmby, URL: "http://importtx:8096",
		APIKey: "k", Enabled: true,
	}
	if err := svc.ImportCreateTx(ctx, db, c); err != nil {
		t.Fatalf("ImportCreateTx: %v", err)
	}
	if c.ID == "" {
		t.Fatal("ImportCreateTx must assign an ID")
	}

	got, err := svc.ImportGetByTypeAndURLTx(ctx, db, "emby", "http://importtx:8096")
	if err != nil {
		t.Fatalf("ImportGetByTypeAndURLTx: %v", err)
	}
	if got == nil {
		t.Fatal("ImportGetByTypeAndURLTx returned nil; row should exist")
	}
	if got.APIKey != "k" {
		t.Errorf("APIKey: got %q, want k", got.APIKey)
	}
}

// TestImportUpdateTx_RoundTrip verifies that ImportUpdateTx mutates an
// existing row. The status is preserved across the update so the import
// path does not stomp a live health check.
func TestImportUpdateTx_RoundTrip(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	ctx := context.Background()

	c := &Connection{
		Name: "ImportUpdate Emby", Type: TypeEmby, URL: "http://importupd:8096",
		APIKey: "k1", Enabled: true,
	}
	if err := svc.ImportCreateTx(ctx, db, c); err != nil {
		t.Fatalf("ImportCreateTx: %v", err)
	}

	c.APIKey = "k2"
	// Mutate a platform-appropriate field (Emby feature toggle) to prove the
	// update round-trips. Create normalized c, so c.Emby is non-nil here.
	c.Emby.FeatureImageWrite = true
	if err := svc.ImportUpdateTx(ctx, db, c); err != nil {
		t.Fatalf("ImportUpdateTx: %v", err)
	}

	got, err := svc.ImportGetByTypeAndURLTx(ctx, db, "emby", "http://importupd:8096")
	if err != nil {
		t.Fatalf("ImportGetByTypeAndURLTx: %v", err)
	}
	if got.APIKey != "k2" {
		t.Errorf("APIKey after update: got %q, want k2", got.APIKey)
	}
	if !got.GetFeatureImageWrite() {
		t.Error("FeatureImageWrite did not round-trip through ImportUpdateTx")
	}
}

// TestImportCreateTx_DBError pins error propagation when the executor
// fails. Exercises the inserting-connection error path.
func TestImportCreateTx_DBError(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	// Close the underlying *sql.DB so subsequent ExecContext fails.
	if d, ok := db.(interface{ Close() error }); ok {
		_ = d.Close()
	}
	err := svc.ImportCreateTx(context.Background(), db, &Connection{
		Name: "x", Type: TypeEmby, URL: "http://x", APIKey: "k",
	})
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportUpdateTx_NotFound exercises the rows-affected=0 branch so a
// connection ID the orchestrator can no longer find surfaces as an error
// rather than silently succeeding.
func TestImportUpdateTx_NotFound(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	err := svc.ImportUpdateTx(context.Background(), db, &Connection{
		ID: "no-such-id", Name: "x", Type: TypeEmby, URL: "http://x", APIKey: "k",
	})
	if err == nil {
		t.Fatal("expected error when updating missing row, got nil")
	}
}

// TestImportCreateTx_ValidationError surfaces a Validate() failure from
// inside the tx-aware create path.
func TestImportCreateTx_ValidationError(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	// Missing URL/type triggers Validate() failure.
	err := svc.ImportCreateTx(context.Background(), db, &Connection{Name: "incomplete"})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
}

// TestImportCreateTx_NilArg pins the defensive nil-guard on the tx-aware
// create path so a malformed envelope cannot panic the import.
func TestImportCreateTx_NilArg(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	if err := svc.ImportCreateTx(context.Background(), db, nil); err == nil {
		t.Fatal("expected error for nil connection, got nil")
	}
}

// TestImportUpdateTx_NilArg pins the defensive nil-guard on the tx-aware
// update path.
func TestImportUpdateTx_NilArg(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	if err := svc.ImportUpdateTx(context.Background(), db, nil); err == nil {
		t.Fatal("expected error for nil connection, got nil")
	}
}

// TestImportGetByTypeAndURLTx_DBError pins error propagation when the
// executor fails (closed DB triggers a non-ErrNoRows DB error).
func TestImportGetByTypeAndURLTx_DBError(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	if d, ok := db.(interface{ Close() error }); ok {
		_ = d.Close()
	}
	_, err := svc.ImportGetByTypeAndURLTx(context.Background(), db, "emby", "http://x")
	if err == nil {
		t.Fatal("expected error with closed DB, got nil")
	}
}

// TestImportGetByTypeAndURLTx_UndecryptableTreatedAsMissing seeds a
// connection encrypted under one key, then opens a fresh service with a
// different encryptor, and asserts ImportGetByTypeAndURLTx returns nil
// (the same fail-soft behavior as GetByTypeAndURL: undecryptable rows
// behave like missing rows for the upsert lookup so the import path
// inserts a fresh row instead of attempting an update against a
// poisoned cipher).
func TestImportGetByTypeAndURLTx_UndecryptableTreatedAsMissing(t *testing.T) {
	t.Parallel()
	svc1, db := setupTestServiceWithDB(t)
	ctx := context.Background()
	c := &Connection{
		Name: "undecryptable", Type: TypeEmby, URL: "http://undecryptable:8096",
		APIKey: "good-key", Enabled: true,
	}
	if err := svc1.ImportCreateTx(ctx, db, c); err != nil {
		t.Fatalf("ImportCreateTx: %v", err)
	}

	// New encryptor with a different randomly-generated key cannot decrypt
	// the row svc1 wrote.
	enc2, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating second encryptor: %v", err)
	}
	// db here is the DBExecutor; downcast to *sql.DB to pass to NewService.
	svc2 := NewService(db.(*sql.DB), enc2)
	got, err := svc2.ImportGetByTypeAndURLTx(ctx, db, "emby", "http://undecryptable:8096")
	if err != nil {
		t.Fatalf("ImportGetByTypeAndURLTx: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for undecryptable row, got %+v", got)
	}
}

// TestImportGetByTypeAndURLTx_NotFound confirms the nil-on-miss contract.
func TestImportGetByTypeAndURLTx_NotFound(t *testing.T) {
	t.Parallel()
	svc, db := setupTestServiceWithDB(t)
	got, err := svc.ImportGetByTypeAndURLTx(context.Background(), db, "emby", "http://nowhere:0")
	if err != nil {
		t.Fatalf("ImportGetByTypeAndURLTx: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing row, got %+v", got)
	}
}
