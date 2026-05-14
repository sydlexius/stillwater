package artist

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
)

// templateDBPath holds the path to the pre-migrated SQLite file that TestMain
// creates once. Each test copies it via newTestDB instead of re-running all
// migrations from scratch.
var templateDBPath string

// TestMain creates a single pre-migrated template database for the package,
// then runs all tests. Each test copies the template via newTestDB so the
// migration cost is paid once per `go test` invocation.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "artist-test-template-*")
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
	if err := database.EnableForeignKeys(db); err != nil {
		panic("enabling foreign keys on template db: " + err.Error())
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
// when the test ends.
func newTestDB(t *testing.T) *sql.DB {
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
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys on test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
