package api

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
)

// testSessionSecret is a fixed 64-byte hex string used as the CSRF SessionSecret
// in Router-constructing tests. NewCSRF panics on an empty or short secret
// (fail-fast design), so every test that calls Router.Handler() must supply one.
// Using a shared constant here keeps test setup DRY and prevents future helpers
// from silently introducing the same panic.
const testSessionSecret = "61b7a3d1c8e2f9054a36b7f2d1e4c8a9b0e3f6d2c1a4b7e0f3d9c6a2b5e8f1d4"

// templateDBPath holds the path to the pre-migrated SQLite file that TestMain
// creates once. Each test copies it via newTestDB instead of re-running all
// migrations from scratch.
var templateDBPath string

// TestMain creates a single pre-migrated template database for the package,
// then runs all tests. Each test copies the template via newTestDB so the
// migration cost is paid once per `go test` invocation.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "api-test-template-*")
	if err != nil {
		panic("creating temp dir: " + err.Error())
	}

	templateDBPath = filepath.Join(dir, "template.db")
	db, err := database.Open(templateDBPath)
	if err != nil {
		panic("opening template db: " + err.Error())
	}
	if err := database.Migrate(db); err != nil {
		panic("migrating template db: " + err.Error())
	}
	// Checkpoint WAL so the template file is fully self-contained before copy.
	if _, err := db.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		panic("checkpointing template db: " + err.Error())
	}
	_ = db.Close()

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// newTestDB copies the pre-migrated template database into a fresh temp
// directory and opens it. The returned *sql.DB is registered for cleanup
// when the test ends. Accepts testing.TB so it can be called from both
// *testing.T and *testing.F (fuzz test) contexts.
func newTestDB(t testing.TB) *sql.DB {
	t.Helper()
	src, err := os.ReadFile(templateDBPath)
	if err != nil {
		t.Fatalf("reading template db: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "test.db")
	if err := os.WriteFile(dst, src, 0o600); err != nil {
		t.Fatalf("writing test db: %v", err)
	}
	db, err := database.Open(dst)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
