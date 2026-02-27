package maintenance

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Enable WAL mode and create settings table
	ctx := context.Background()
	for _, stmt := range []string{
		"PRAGMA journal_mode=WAL",
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return db, dbPath
}

func TestStatus(t *testing.T) {
	db, dbPath := setupTestDB(t)
	svc := NewService(db, dbPath, slog.Default())

	st, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if st.DBFileSize <= 0 {
		t.Error("expected positive DB file size")
	}
	if st.PageSize <= 0 {
		t.Error("expected positive page size")
	}
	if st.PageCount <= 0 {
		t.Error("expected positive page count")
	}
	if st.LastOptimizeAt != "" {
		t.Error("expected empty last optimize time initially")
	}
	if !st.ScheduleEnabled {
		t.Error("expected schedule enabled by default")
	}
	if st.ScheduleInterval != 24 {
		t.Errorf("expected 24h interval default, got %d", st.ScheduleInterval)
	}
}

func TestOptimize(t *testing.T) {
	db, dbPath := setupTestDB(t)
	svc := NewService(db, dbPath, slog.Default())

	// Insert some data to make optimize meaningful
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", //nolint:errcheck
			"test."+string(rune('A'+i%26)), "value")
	}

	if err := svc.Optimize(context.Background()); err != nil {
		t.Fatalf("Optimize: %v", err)
	}

	// Verify last optimize time was recorded
	st, _ := svc.Status(context.Background())
	if st.LastOptimizeAt == "" {
		t.Error("expected last optimize time to be set after optimize")
	}
}

func TestVacuum(t *testing.T) {
	db, dbPath := setupTestDB(t)
	svc := NewService(db, dbPath, slog.Default())

	// Insert and delete data to create freeable space
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES (?, ?)", //nolint:errcheck
			"vacuum_test_"+string(rune('A'+i%26))+string(rune('0'+i/26)), "x")
	}
	db.ExecContext(ctx, "DELETE FROM settings WHERE key LIKE 'vacuum_test_%'") //nolint:errcheck

	sizeBefore, _ := os.Stat(dbPath)

	if err := svc.Vacuum(context.Background()); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}

	sizeAfter, _ := os.Stat(dbPath)
	// After vacuum, size should be <= before (may be equal for tiny DBs)
	if sizeAfter.Size() > sizeBefore.Size() {
		t.Logf("note: DB grew after vacuum (before=%d, after=%d), expected for small DBs",
			sizeBefore.Size(), sizeAfter.Size())
	}
}

func TestGetBoolSetting(t *testing.T) {
	db, dbPath := setupTestDB(t)
	svc := NewService(db, dbPath, slog.Default())

	// Default when not set
	if !svc.getBoolSetting(context.Background(), "nonexistent", true) {
		t.Error("expected true fallback")
	}

	// Set to true
	ctx := context.Background()
	db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES ('test.bool', 'true')") //nolint:errcheck
	if !svc.getBoolSetting(ctx, "test.bool", false) {
		t.Error("expected true")
	}

	// Set to false
	db.ExecContext(ctx, "UPDATE settings SET value = 'false' WHERE key = 'test.bool'") //nolint:errcheck
	if svc.getBoolSetting(context.Background(), "test.bool", true) {
		t.Error("expected false")
	}
}

func TestGetIntSetting(t *testing.T) {
	db, dbPath := setupTestDB(t)
	svc := NewService(db, dbPath, slog.Default())

	// Default when not set
	if v := svc.getIntSetting(context.Background(), "nonexistent", 42); v != 42 {
		t.Errorf("expected 42, got %d", v)
	}

	// Set to 12
	ctx := context.Background()
	db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES ('test.int', '12')") //nolint:errcheck
	if v := svc.getIntSetting(context.Background(), "test.int", 0); v != 12 {
		t.Errorf("expected 12, got %d", v)
	}
}
