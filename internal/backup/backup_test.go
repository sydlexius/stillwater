package backup

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// testClock is a Clock for tests. Each call to Now advances the internal
// counter by one second so sequential Backup calls produce distinct filenames
// without requiring time.Sleep.
type testClock struct {
	mu  sync.Mutex
	cur time.Time
}

func newTestClock() *testClock {
	return &testClock{cur: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.cur
	c.cur = c.cur.Add(time.Second)
	return t
}

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

// TestBackup_SnapshotIsOwnerOnly verifies that a backup snapshot is written
// with 0600 permissions (owner read/write only), since it contains a full
// copy of the application database including encrypted secrets.
func TestBackup_SnapshotIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}

	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 7, logger)

	info, err := svc.Backup(context.Background())
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	fi, err := os.Stat(filepath.Join(backupDir, info.Filename))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup snapshot mode = %o, want 0600", perm)
	}
}

// TestBackup_ChmodFailurePropagates covers the error branch where restricting
// the snapshot's permissions fails, injected via the osChmod hook (same
// pattern as osRename in internal/filesystem). This hits the first (staging)
// chmod call.
func TestBackup_ChmodFailurePropagates(t *testing.T) {
	orig := osChmod
	t.Cleanup(func() { osChmod = orig })
	osChmod = func(name string, mode os.FileMode) error {
		return errors.New("simulated chmod failure")
	}

	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 7, logger)

	_, err := svc.Backup(context.Background())
	if err == nil {
		t.Fatal("expected error from simulated chmod failure")
	}
	if !strings.Contains(err.Error(), "restricting backup permissions") {
		t.Errorf("error = %q, want it to contain 'restricting backup permissions'", err.Error())
	}
}

// TestBackup_FinalChmodFailurePropagates covers the second (belt-and-
// suspenders) chmod call, made on dest after the rename, by letting the
// first (staging) call through and only failing from the second call on.
func TestBackup_FinalChmodFailurePropagates(t *testing.T) {
	orig := osChmod
	t.Cleanup(func() { osChmod = orig })
	calls := 0
	osChmod = func(name string, mode os.FileMode) error {
		calls++
		if calls == 1 {
			return orig(name, mode)
		}
		return errors.New("simulated final chmod failure")
	}

	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 7, logger)

	_, err := svc.Backup(context.Background())
	if err == nil {
		t.Fatal("expected error from simulated final chmod failure")
	}
	if !strings.Contains(err.Error(), "restricting backup permissions") {
		t.Errorf("error = %q, want it to contain 'restricting backup permissions'", err.Error())
	}
}

// TestBackup_MkdirTempFailurePropagates covers the error branch where the
// staging directory cannot be created, injected via the osMkdirTemp hook.
func TestBackup_MkdirTempFailurePropagates(t *testing.T) {
	orig := osMkdirTemp
	t.Cleanup(func() { osMkdirTemp = orig })
	osMkdirTemp = func(dir, pattern string) (string, error) {
		return "", errors.New("simulated mkdirtemp failure")
	}

	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 7, logger)

	_, err := svc.Backup(context.Background())
	if err == nil {
		t.Fatal("expected error from simulated mkdirtemp failure")
	}
	if !strings.Contains(err.Error(), "creating staging directory") {
		t.Errorf("error = %q, want it to contain 'creating staging directory'", err.Error())
	}
}

// TestBackup_LinkFailurePropagates covers the error branch where moving the
// finished snapshot into backupDir fails, injected via the osLink hook. A
// non-ErrExist failure (unlike a collision, which is retried with a suffix)
// must propagate.
func TestBackup_LinkFailurePropagates(t *testing.T) {
	orig := osLink
	t.Cleanup(func() { osLink = orig })
	osLink = func(oldpath, newpath string) error {
		return errors.New("simulated link failure")
	}

	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 7, logger)

	_, err := svc.Backup(context.Background())
	if err == nil {
		t.Fatal("expected error from simulated link failure")
	}
	if !strings.Contains(err.Error(), "moving backup into place") {
		t.Errorf("error = %q, want it to contain 'moving backup into place'", err.Error())
	}
}

// fixedClock always returns the same instant, forcing two Backup calls to
// compute the identical base filename so they collide on the destination path.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// TestBackup_SameSecondNoClobber is the regression test for the silent
// backup-clobber data-loss bug (#2181 review). Two backups in the same
// wall-clock second must both survive: the second must NOT overwrite the
// first. Against the old plain-os.Rename code both calls computed the same
// filename and the second silently clobbered the first (this test then sees
// identical filenames / a single file and fails), so this is a red/green
// regression guard.
func TestBackup_SameSecondNoClobber(t *testing.T) {
	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc := NewService(db, backupDir, 7, logger).WithClock(fixedClock{t: fixed})

	first, err := svc.Backup(context.Background())
	if err != nil {
		t.Fatalf("first Backup: %v", err)
	}
	second, err := svc.Backup(context.Background())
	if err != nil {
		t.Fatalf("second Backup: %v", err)
	}

	if first.Filename == second.Filename {
		t.Fatalf("both backups share filename %q; the second clobbered the first", first.Filename)
	}

	// Both snapshots must exist on disk, be non-empty, and be valid SQLite.
	for _, info := range []*BackupInfo{first, second} {
		path := filepath.Join(backupDir, info.Filename)
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", info.Filename, err)
		}
		if fi.Size() == 0 {
			t.Errorf("backup %s is empty", info.Filename)
		}
		bdb, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open backup %s: %v", info.Filename, err)
		}
		var value string
		err = bdb.QueryRowContext(context.Background(), "SELECT value FROM test WHERE id = 1").Scan(&value)
		bdb.Close()
		if err != nil {
			t.Fatalf("query backup %s: %v", info.Filename, err)
		}
		if value != "hello" {
			t.Errorf("backup %s content = %q, want 'hello'", info.Filename, value)
		}
	}

	// ListBackups must report exactly two distinct backups.
	backups, err := svc.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 2 {
		t.Fatalf("expected 2 backups after two same-second Backup calls, got %d", len(backups))
	}
}

// TestLinkIntoPlace_ConcurrentSameNameNoClobber exercises the collision
// primitive directly under -race: many goroutines all try to move their own
// staging file into the SAME base destination name at once. os.Link's atomic
// ErrExist-on-collision guarantees every caller gets a distinct final name and
// none clobbers another -- the property a stat-then-rename could not provide.
// Testing linkIntoPlace directly (rather than full Backup) keeps this free of
// SQLite VACUUM locking flakiness while still covering the concurrent path.
func TestLinkIntoPlace_ConcurrentSameNameNoClobber(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	const base = "stillwater-20250101-120000.db"

	var wg sync.WaitGroup
	names := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			staging := filepath.Join(dir, ".staging-"+string(rune('a'+i)))
			if err := os.WriteFile(staging, []byte("snapshot"), 0o600); err != nil {
				errs[i] = err
				return
			}
			names[i], errs[i] = linkIntoPlace(staging, dir, base)
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: linkIntoPlace failed: %v", i, errs[i])
			continue
		}
		if seen[names[i]] {
			t.Errorf("duplicate final name %q -- a snapshot was clobbered", names[i])
		}
		seen[names[i]] = true
		if _, err := os.Stat(filepath.Join(dir, names[i])); err != nil {
			t.Errorf("final file %q missing: %v", names[i], err)
		}
	}
	if len(seen) != n {
		t.Errorf("expected %d distinct snapshots, got %d", n, len(seen))
	}

	// No staging files should remain; every link is followed by a remove.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") {
			t.Errorf("leftover staging file: %s", e.Name())
		}
	}
}

// TestBackup_StagingDirIsOwnerOnly closes the TOCTOU window identified in
// review: VACUUM INTO creates its output file at the process umask
// (typically 0644), so writing directly into backupDir (created 0750, i.e.
// group-traversable) and chmod-ing afterward would leave the full database
// -- including encrypted secrets -- group/other-readable for the entire
// VACUUM duration. This test captures the directory containing the snapshot
// at the moment of the first chmod call (i.e. immediately after VACUUM INTO
// completes, before anything is visible in backupDir) and asserts it is
// 0700 (owner-only), and that nothing has appeared at the public dest path
// yet.
func TestBackup_StagingDirIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}

	orig := osChmod
	t.Cleanup(func() { osChmod = orig })
	var stagingDirPerm os.FileMode
	var destExistedDuringVacuum bool
	captured := false
	osChmod = func(name string, mode os.FileMode) error {
		if !captured {
			captured = true
			dir := filepath.Dir(name)
			fi, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			stagingDirPerm = fi.Mode().Perm()
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "stillwater-20250101-120000.db")); err == nil {
				destExistedDuringVacuum = true
			}
		}
		return orig(name, mode)
	}

	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(db, backupDir, 7, logger).WithClock(newTestClock())

	if _, err := svc.Backup(context.Background()); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if !captured {
		t.Fatal("osChmod was never called; test did not observe the staging window")
	}
	if stagingDirPerm != 0o700 {
		t.Errorf("staging directory mode = %o, want 0700 (owner-only) so the snapshot is never group/other-readable during VACUUM", stagingDirPerm)
	}
	if destExistedDuringVacuum {
		t.Error("dest path was visible in backupDir before the snapshot was permission-restricted and renamed into place")
	}

	// No staging directories should survive a successful backup.
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("leftover staging directory in backupDir: %s", e.Name())
		}
	}
}

func TestListBackups(t *testing.T) {
	db := setupTestDB(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// The fake clock advances by 1s per call so each Backup gets a unique
	// filename without requiring time.Sleep.
	svc := NewService(db, backupDir, 7, logger).WithClock(newTestClock())

	// Create 3 backups -- no sleep needed with the injected clock.
	for i := 0; i < 3; i++ {
		_, err := svc.Backup(context.Background())
		if err != nil {
			t.Fatalf("Backup %d: %v", i, err)
		}
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
	svc := NewService(db, backupDir, 2, logger).WithClock(newTestClock()) // Keep only 2

	// Create 4 backups -- no sleep needed with the injected clock.
	for i := 0; i < 4; i++ {
		_, err := svc.Backup(context.Background())
		if err != nil {
			t.Fatalf("Backup %d: %v", i, err)
		}
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
