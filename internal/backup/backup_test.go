package backup

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.ExecContext(context.Background(), "CREATE TABLE test (id INTEGER PRIMARY KEY, value TEXT)")
	if err != nil {
		t.Fatalf("creating table: %v", err)
	}
	_, err = db.ExecContext(context.Background(), "INSERT INTO test (value) VALUES ('hello')")
	if err != nil {
		t.Fatalf("inserting row: %v", err)
	}
	return db
}

func TestBackup(t *testing.T) {
	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 7, logger)

	info, err := svc.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if info.Filename == "" {
		t.Error("expected non-empty filename")
	}
	if info.Size == 0 {
		t.Error("expected non-zero file size")
	}

	// Verify the backup is a valid SQLite database
	backupPath := filepath.Join(backupDir, info.Filename)
	backupDB, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatalf("opening backup: %v", err)
	}
	defer backupDB.Close()

	var value string
	err = backupDB.QueryRowContext(context.Background(), "SELECT value FROM test WHERE id = 1").Scan(&value)
	if err != nil {
		t.Fatalf("querying backup: %v", err)
	}
	if value != "hello" {
		t.Errorf("expected 'hello', got %q", value)
	}
}

func TestListBackups(t *testing.T) {
	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 7, logger)

	// Create 3 backups with small delays
	for i := 0; i < 3; i++ {
		_, err := svc.Backup(context.Background())
		if err != nil {
			t.Fatalf("Backup %d: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond) // Ensure different timestamps
	}

	backups, err := svc.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 3 {
		t.Fatalf("expected 3 backups, got %d", len(backups))
	}

	// Should be sorted newest first
	if !backups[0].CreatedAt.After(backups[1].CreatedAt) {
		t.Error("expected backups sorted by date descending")
	}
}

func TestPrune(t *testing.T) {
	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 2, logger) // Keep only 2

	// Create 4 backups
	for i := 0; i < 4; i++ {
		_, err := svc.Backup(context.Background())
		if err != nil {
			t.Fatalf("Backup %d: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	if err := svc.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	backups, err := svc.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups after prune: %v", err)
	}
	if len(backups) != 2 {
		t.Fatalf("expected 2 backups after prune, got %d", len(backups))
	}
}

func TestListBackupsEmptyDir(t *testing.T) {
	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "nonexistent")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 7, logger)

	backups, err := svc.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("expected 0 backups, got %d", len(backups))
	}
}

func TestDelete(t *testing.T) {
	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 7, logger)

	info, err := svc.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Delete should succeed
	if err := svc.Delete(info.Filename); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify file is gone
	backups, err := svc.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("expected 0 backups after delete, got %d", len(backups))
	}

	// Delete with invalid filename should fail
	if err := svc.Delete("../evil.db"); err == nil {
		t.Error("expected error for invalid filename")
	}

	// Delete nonexistent file should fail
	if err := svc.Delete("stillwater-20260101-000000.db"); err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestPruneWithMaxAge(t *testing.T) {
	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 100, logger) // High retention to not trigger count-based pruning

	// Create backup files with old timestamps
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatal(err)
	}

	// Create a "recent" backup (today)
	recentName := "stillwater-" + time.Now().UTC().Format("20060102-150405") + ".db"
	if err := os.WriteFile(filepath.Join(backupDir, recentName), []byte("recent"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create an "old" backup (60 days ago)
	oldTime := time.Now().UTC().AddDate(0, 0, -60)
	oldName := "stillwater-" + oldTime.Format("20060102-150405") + ".db"
	if err := os.WriteFile(filepath.Join(backupDir, oldName), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Set max age to 30 days and prune
	svc.SetMaxAgeDays(30)
	if err := svc.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	backups, err := svc.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected 1 backup after age-based prune, got %d", len(backups))
	}
	if backups[0].Filename != recentName {
		t.Errorf("expected recent backup to survive, got %s", backups[0].Filename)
	}
}

func TestIsValidBackupFilename(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid", "stillwater-20260220-143022.db", true},
		{"path traversal", "../stillwater-20260220-143022.db", false},
		{"backslash", "..\\stillwater-20260220-143022.db", false},
		{"wrong prefix", "backup-20260220-143022.db", false},
		{"wrong extension", "stillwater-20260220-143022.sql", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidBackupFilename(tt.input); got != tt.want {
				t.Errorf("IsValidBackupFilename(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
