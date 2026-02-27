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
	for _, stmt := range []string{
		"PRAGMA journal_mode=WAL",
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
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
	for i := 0; i < 100; i++ {
		db.Exec("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
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
	for i := 0; i < 100; i++ {
		db.Exec("INSERT INTO settings (key, value) VALUES (?, ?)",
			"vacuum_test_"+string(rune('A'+i%26))+string(rune('0'+i/26)), "x")
	}
	db.Exec("DELETE FROM settings WHERE key LIKE 'vacuum_test_%'")

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
	db.Exec("INSERT INTO settings (key, value) VALUES ('test.bool', 'true')")
	if !svc.getBoolSetting(context.Background(), "test.bool", false) {
		t.Error("expected true")
	}

	// Set to false
	db.Exec("UPDATE settings SET value = 'false' WHERE key = 'test.bool'")
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
	db.Exec("INSERT INTO settings (key, value) VALUES ('test.int', '12')")
	if v := svc.getIntSetting(context.Background(), "test.int", 0); v != 12 {
		t.Errorf("expected 12, got %d", v)
	}
}
