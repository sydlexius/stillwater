package maintenance

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
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

// setupTestDBWithImages opens a fresh SQLite DB and applies the project's
// production migrations so the test exercises the same schema the running
// service would see. Hand-rolling the subset of tables we touch here would
// silently drift when 001_initial_schema.sql gains a new NOT NULL column on
// artist_images or artists, so we run the real migration end-to-end.
func setupTestDBWithImages(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ExecContext(context.Background(), "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("enabling WAL: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db, dbPath
}

func TestStatus(t *testing.T) {
	db, dbPath := setupTestDB(t)
	svc := NewService(db, dbPath, "", slog.Default())

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
	svc := NewService(db, dbPath, "", slog.Default())

	// Insert some data to make optimize meaningful
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			"test."+string(rune('A'+i%26)), "value"); err != nil {
			t.Fatalf("seeding optimize row %d: %v", i, err)
		}
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
	svc := NewService(db, dbPath, "", slog.Default())

	// Insert and delete data to create freeable space
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES (?, ?)",
			"vacuum_test_"+string(rune('A'+i%26))+string(rune('0'+i/26)), "x"); err != nil {
			t.Fatalf("seeding vacuum row %d: %v", i, err)
		}
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM settings WHERE key LIKE 'vacuum_test_%'"); err != nil {
		t.Fatalf("cleaning vacuum rows: %v", err)
	}

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
	svc := NewService(db, dbPath, "", slog.Default())

	// Default when not set
	if !svc.getBoolSetting(context.Background(), "nonexistent", true) {
		t.Error("expected true fallback")
	}

	// Set to true
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES ('test.bool', 'true')"); err != nil {
		t.Fatalf("seeding bool=true: %v", err)
	}
	if !svc.getBoolSetting(ctx, "test.bool", false) {
		t.Error("expected true")
	}

	// Set to false
	if _, err := db.ExecContext(ctx, "UPDATE settings SET value = 'false' WHERE key = 'test.bool'"); err != nil {
		t.Fatalf("seeding bool=false: %v", err)
	}
	if svc.getBoolSetting(context.Background(), "test.bool", true) {
		t.Error("expected false")
	}
}

func TestGetIntSetting(t *testing.T) {
	db, dbPath := setupTestDB(t)
	svc := NewService(db, dbPath, "", slog.Default())

	// Default when not set
	if v := svc.getIntSetting(context.Background(), "nonexistent", 42); v != 42 {
		t.Errorf("expected 42, got %d", v)
	}

	// Set to 12
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES ('test.int', '12')"); err != nil {
		t.Fatalf("seeding int=12: %v", err)
	}
	if v := svc.getIntSetting(context.Background(), "test.int", 0); v != 12 {
		t.Errorf("expected 12, got %d", v)
	}
}

// TestScanExistsFlags verifies that ScanExistsFlags clears exists_flag for
// rows whose image files are missing and leaves rows with real files untouched.
func TestScanExistsFlags(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	// Inject an explicit cache dir so the "no path" scenario exercises the
	// cache-dir fallback branch in artistImageDir rather than the degenerate
	// "unconfigured cache dir" branch.
	cacheDir := t.TempDir()
	svc := NewService(db, dbPath, cacheDir, slog.Default())
	ctx := context.Background()

	// Create a real image directory with a valid file so we can verify the
	// scan does NOT clear a flag when the file actually exists.
	realDir := t.TempDir()
	realFile := filepath.Join(realDir, "folder.jpg")
	if err := os.WriteFile(realFile, []byte("img"), 0o644); err != nil {
		t.Fatalf("creating real image file: %v", err)
	}

	// Build a deterministic missing-path under t.TempDir() rather than a
	// hardcoded absolute path: the latter would flake on any host that happens
	// to have "/tmp/does-not-exist-in-tests" present.
	missingPath := filepath.Join(t.TempDir(), "missing")

	// Seed artists: real path, missing path, and no path (cache-dir fallback).
	for _, a := range []struct {
		id   string
		path string
	}{
		{"artist-real", realDir},
		{"artist-missing", missingPath},
		{"artist-nocache", ""},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`, a.id, a.id, a.path,
		); err != nil {
			t.Fatalf("seeding artist %s: %v", a.id, err)
		}
	}

	// Seed artist_images rows.
	// Row 1: real file exists      -- flag must NOT be cleared.
	// Row 2: missing path (ENOENT) -- flag MUST be cleared.
	// Row 3: cache dir miss        -- flag MUST be cleared (cache dir
	//                                 exists but artist subdir doesn't,
	//                                 so FindExistingImage sees ENOENT).
	// Row 4: exists_flag=0         -- must remain untouched (not even checked).
	for _, row := range []struct {
		id        string
		artistID  string
		imageType string
		exists    int
	}{
		{"img-real", "artist-real", "thumb", 1},
		{"img-missing", "artist-missing", "thumb", 1},
		{"img-nocache", "artist-nocache", "fanart", 1},
		{"img-zero", "artist-real", "fanart", 0},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
			 VALUES (?, ?, ?, 0, ?)`,
			row.id, row.artistID, row.imageType, row.exists,
		); err != nil {
			t.Fatalf("seeding artist_image %s: %v", row.id, err)
		}
	}

	if err := svc.ScanExistsFlags(ctx); err != nil {
		t.Fatalf("ScanExistsFlags: %v", err)
	}

	// Helper: read exists_flag for a given image ID.
	flagFor := func(id string) int {
		var f int
		if err := db.QueryRowContext(ctx,
			`SELECT exists_flag FROM artist_images WHERE id = ?`, id).Scan(&f); err != nil {
			t.Fatalf("reading flag for %s: %v", id, err)
		}
		return f
	}

	// Real file: flag must remain 1.
	if got := flagFor("img-real"); got != 1 {
		t.Errorf("img-real: expected exists_flag=1 (file present), got %d", got)
	}
	// Missing path: flag must be cleared to 0.
	if got := flagFor("img-missing"); got != 0 {
		t.Errorf("img-missing: expected exists_flag=0 (file absent), got %d", got)
	}
	// Cache dir miss: flag must be cleared to 0.
	if got := flagFor("img-nocache"); got != 0 {
		t.Errorf("img-nocache: expected exists_flag=0 (no cache dir), got %d", got)
	}
	// Already-zero row: must remain 0 and not have been re-touched.
	if got := flagFor("img-zero"); got != 0 {
		t.Errorf("img-zero: expected exists_flag=0 (was already 0), got %d", got)
	}
}

// TestScanExistsFlagsEmpty verifies ScanExistsFlags succeeds on an empty table.
func TestScanExistsFlagsEmpty(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	if err := svc.ScanExistsFlags(context.Background()); err != nil {
		t.Fatalf("ScanExistsFlags on empty table: %v", err)
	}
}

// TestScanExistsFlagsUnresolvableDirSkips verifies that a row with no artist
// path and no cache-dir fallback is SKIPPED (flag preserved) rather than
// cleared. This matters in prod because a misconfigured imageCacheDir would
// otherwise silently corrupt flags for every cache-only artist.
func TestScanExistsFlagsUnresolvableDirSkips(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	// imageCacheDir="" makes the fallback unresolvable.
	svc := NewService(db, dbPath, "", slog.Default())
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES ('a-unresolvable', 'a-unresolvable', '')`); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
		 VALUES ('i-unresolvable', 'a-unresolvable', 'thumb', 0, 1)`); err != nil {
		t.Fatalf("seed image: %v", err)
	}

	if err := svc.ScanExistsFlags(ctx); err != nil {
		t.Fatalf("ScanExistsFlags: %v", err)
	}

	var flag int
	if err := db.QueryRowContext(ctx,
		`SELECT exists_flag FROM artist_images WHERE id = 'i-unresolvable'`).Scan(&flag); err != nil {
		t.Fatalf("reading flag: %v", err)
	}
	if flag != 1 {
		t.Errorf("unresolvable-dir row: expected exists_flag=1 (skipped), got %d", flag)
	}
}

// TestScanExistsFlagsStatErrorSkips verifies that when os.Stat on the image
// directory fails with an error other than fs.ErrNotExist (e.g. permission
// denied from an unreadable parent), the scanner SKIPS the row and preserves
// the flag rather than treating the stat failure as "file absent". This is
// the critical safety guard identified during pre-push review -- without it,
// a single permission-denied directory would wipe flags for every artist
// under it on the next scheduled scan.
func TestScanExistsFlagsStatErrorSkips(t *testing.T) {
	// The chmod 0o000 trick is Unix-only. Windows uses ACLs and Go's os.Chmod
	// maps differently; skip rather than fake it.
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o000 semantics are Unix-specific")
	}
	// Running as root bypasses permission bits entirely, so the stat would
	// succeed and the branch we want to exercise never fires.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}

	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	ctx := context.Background()

	// Create parent dir with a real child dir inside, then drop the parent's
	// execute bit so traversal (needed to stat the child) fails with EACCES.
	parent := t.TempDir()
	child := filepath.Join(parent, "unreadable-artist")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	// Restore permissions on cleanup so t.TempDir can remove the tree.
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	// Seed the artist pointing at the unreadable child dir with a stale flag.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES ('a-eacces', 'a-eacces', ?)`, child); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
		 VALUES ('i-eacces', 'a-eacces', 'thumb', 0, 1)`); err != nil {
		t.Fatalf("seed image: %v", err)
	}

	if err := svc.ScanExistsFlags(ctx); err != nil {
		t.Fatalf("ScanExistsFlags: %v", err)
	}

	var flag int
	if err := db.QueryRowContext(ctx,
		`SELECT exists_flag FROM artist_images WHERE id = 'i-eacces'`).Scan(&flag); err != nil {
		t.Fatalf("reading flag: %v", err)
	}
	if flag != 1 {
		t.Errorf("stat-error row: expected exists_flag=1 (skipped), got %d; non-ENOENT stat errors must not clear flags", flag)
	}
}

// TestScanExistsFlagsCanceledContext verifies that a canceled context causes
// the top-level SELECT to fail with an error, which ScanExistsFlags returns
// to its caller rather than proceeding with an incomplete scan.
func TestScanExistsFlagsCanceledContext(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := svc.ScanExistsFlags(ctx)
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
	// Assert the cancellation cause specifically so a future regression that
	// fails ScanExistsFlags for an unrelated reason (e.g. a closed DB) cannot
	// masquerade as a passing "respects cancel" test.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestArtistImageDir exercises all three branches of the directory-resolution
// helper, including the defensive "return empty" fallback that the scanner
// uses as the signal to skip a row. Covering all branches here lets the
// integration tests above focus on scan behavior rather than permutations of
// this helper.
func TestArtistImageDir(t *testing.T) {
	db, dbPath := setupTestDB(t)
	cacheDir := "/var/lib/stillwater/cache/images"
	svc := NewService(db, dbPath, cacheDir, slog.Default())

	tests := []struct {
		name       string
		artistPath string
		artistID   string
		want       string
	}{
		{"artist path takes precedence", "/music/library/Tycho", "abc123", "/music/library/Tycho"},
		{"cache-dir fallback joins artistID", "", "abc123", filepath.Join(cacheDir, "abc123")},
		{"unresolvable: empty path + empty cache dir", "", "abc123", ""},
		{"unresolvable: empty path + empty artistID", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "unresolvable: empty path + empty cache dir" {
				got := NewService(db, dbPath, "", slog.Default()).artistImageDir(tc.artistPath, tc.artistID)
				if got != tc.want {
					t.Errorf("artistImageDir(%q, %q) = %q, want %q", tc.artistPath, tc.artistID, got, tc.want)
				}
				return
			}
			got := svc.artistImageDir(tc.artistPath, tc.artistID)
			if got != tc.want {
				t.Errorf("artistImageDir(%q, %q) = %q, want %q", tc.artistPath, tc.artistID, got, tc.want)
			}
		})
	}
}

// TestStartExistsFlagScanner_RunsStartupAndTick verifies the scanner performs
// its initial scan after startupDelay and then continues on the ticker. The
// test seeds a stale exists_flag=1 row, runs the scanner with millisecond
// timings, and confirms the flag is cleared before the scanner is canceled.
func TestStartExistsFlagScanner_RunsStartupAndTick(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	ctx := context.Background()

	// Seed one artist with a path under t.TempDir() that we never create;
	// exists_flag=1 should be cleared by the first scan. Using t.TempDir()
	// instead of a hardcoded absolute path keeps the test deterministic
	// regardless of host filesystem state.
	missingPath := filepath.Join(t.TempDir(), "missing")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES ('a1', 'a1', ?)`, missingPath); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
		 VALUES ('i1', 'a1', 'thumb', 0, 1)`); err != nil {
		t.Fatalf("seed image: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		svc.StartExistsFlagScanner(runCtx, 10*time.Millisecond, 10*time.Millisecond)
		close(done)
	}()

	// Poll for the flag to go to 0 (proves the startup scan ran).
	deadline := time.Now().Add(3 * time.Second)
	cleared := false
	for time.Now().Before(deadline) {
		var flag int
		if err := db.QueryRowContext(ctx,
			`SELECT exists_flag FROM artist_images WHERE id = 'i1'`).Scan(&flag); err == nil && flag == 0 {
			cleared = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cleared {
		cancel()
		<-done
		t.Fatal("exists_flag was not cleared within 3s")
	}

	// Re-stamp exists_flag=1 and wait for the ticker to clear it again, to
	// prove the loop (not just the startup scan) is running.
	if _, err := db.ExecContext(ctx,
		`UPDATE artist_images SET exists_flag = 1 WHERE id = 'i1'`); err != nil {
		t.Fatalf("restamp: %v", err)
	}
	tickDeadline := time.Now().Add(3 * time.Second)
	tickCleared := false
	for time.Now().Before(tickDeadline) {
		var flag int
		if err := db.QueryRowContext(ctx,
			`SELECT exists_flag FROM artist_images WHERE id = 'i1'`).Scan(&flag); err == nil && flag == 0 {
			tickCleared = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scanner did not stop within 2s of cancel")
	}
	if !tickCleared {
		t.Fatal("ticker did not re-run scan within 3s of re-stamping flag")
	}
}

// TestStartExistsFlagScanner_CanceledDuringStartupDelay verifies the scanner
// exits cleanly when the context is canceled before startupDelay elapses,
// without attempting any scan or starting the ticker loop.
//
// Proving prompt exit is not sufficient: a regression that runs an immediate
// scan and then exited on cancel would also satisfy that. To guard the
// "startup delay must block all scanning" contract, seed a stale exists_flag=1
// row pointing at a missing path and assert it is still 1 after the scanner
// exits. If any scan had run, the row would have been cleared.
func TestStartExistsFlagScanner_CanceledDuringStartupDelay(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	ctx := context.Background()

	// Seed a stale row whose image directory does not exist, so any scan that
	// did run would clear the flag.
	missingPath := filepath.Join(t.TempDir(), "missing")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES ('a-startup', 'a-startup', ?)`, missingPath); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
		 VALUES ('i-startup', 'a-startup', 'thumb', 0, 1)`); err != nil {
		t.Fatalf("seed image: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		// Long startupDelay; we'll cancel before it elapses.
		svc.StartExistsFlagScanner(runCtx, time.Hour, 30*time.Second)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scanner did not stop within 2s of cancel during startup delay")
	}

	// The scanner must not have run any scan during the startup delay.
	var flag int
	if err := db.QueryRowContext(ctx,
		`SELECT exists_flag FROM artist_images WHERE id = 'i-startup'`).Scan(&flag); err != nil {
		t.Fatalf("reading flag: %v", err)
	}
	if flag != 1 {
		t.Errorf("startup-delay contract: expected exists_flag=1 (no scan ran), got %d", flag)
	}
}

// stubForeignArtistLister implements ForeignArtistLister for the
// foreign-file scanner tests. Returns no artists so the scanner exercises
// only its lifecycle code (start, run-once, stop on cancel).
type stubForeignArtistLister struct{}

func (stubForeignArtistLister) List(_ context.Context, _ artist.ListParams) ([]artist.Artist, int, error) {
	return nil, 0, nil
}

func TestStartForeignFileScanner_StopsOnCancel(t *testing.T) {
	db, dbPath := setupTestDB(t)
	// Foreign-file scanner needs the foreign tables; create a minimal subset
	// here rather than running the full migration so the test is fast.
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS foreign_files (
			id TEXT PRIMARY KEY,
			artist_id TEXT NOT NULL,
			file_path TEXT NOT NULL,
			file_name TEXT NOT NULL,
			size_bytes INTEGER NOT NULL DEFAULT 0,
			detected_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(artist_id, file_path))`,
		`CREATE TABLE IF NOT EXISTS foreign_file_allowlist (
			id TEXT PRIMARY KEY,
			scope TEXT NOT NULL,
			artist_id TEXT,
			file_name TEXT NOT NULL,
			note TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	svc := NewService(db, dbPath, "", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// Use millisecond cadence so the goroutine completes its first
		// scan immediately, then exits on cancel.
		svc.StartForeignFileScanner(ctx, stubForeignArtistLister{}, time.Millisecond, time.Millisecond)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("foreign-file scanner did not stop within 2s of cancel")
	}
}

func TestStartForeignFileScanner_NilListerNoOp(t *testing.T) {
	db, dbPath := setupTestDB(t)
	svc := NewService(db, dbPath, "", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	// Passing nil should return immediately without panicking.
	svc.StartForeignFileScanner(context.Background(), nil, 0, 0)
}

// -- get*Setting error-discrimination tests -----------------------------------

// warnLogger returns a bytes.Buffer and a *slog.Logger that writes WARN+ to it.
// This lets tests assert that the right Warn log line was (or was not) emitted.
func warnLogger() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return &buf, lg
}

// TestGetBoolSetting_DBError verifies that a genuine DB error (closed connection)
// returns the fallback AND emits a Warn log line.
func TestGetBoolSetting_DBError(t *testing.T) {
	t.Parallel()
	db, dbPath := setupTestDB(t)
	buf, lg := warnLogger()
	svc := NewService(db, dbPath, "", lg)

	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	got := svc.getBoolSetting(context.Background(), "any.key", false)
	if got {
		t.Errorf("getBoolSetting DB error: got true, want false (fallback)")
	}
	logged := buf.String()
	if !strings.Contains(logged, "reading bool setting") {
		t.Errorf("getBoolSetting DB error: expected warn log, got %q", logged)
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("getBoolSetting DB error: expected WARN level, got %q", logged)
	}
}

// TestGetBoolSetting_AbsentKey verifies that a missing row returns the
// fallback without emitting any log line.
func TestGetBoolSetting_AbsentKey(t *testing.T) {
	t.Parallel()
	db, dbPath := setupTestDB(t)
	buf, lg := warnLogger()
	svc := NewService(db, dbPath, "", lg)

	got := svc.getBoolSetting(context.Background(), "no.such.key", true)
	if !got {
		t.Errorf("getBoolSetting absent key: got false, want true (fallback)")
	}
	if buf.Len() != 0 {
		t.Errorf("getBoolSetting absent key: expected no log, got %q", buf.String())
	}
}

// TestGetIntSetting_DBError verifies that a genuine DB error returns the
// fallback AND emits a Warn log line.
func TestGetIntSetting_DBError(t *testing.T) {
	t.Parallel()
	db, dbPath := setupTestDB(t)
	buf, lg := warnLogger()
	svc := NewService(db, dbPath, "", lg)

	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	got := svc.getIntSetting(context.Background(), "any.key", 77)
	if got != 77 {
		t.Errorf("getIntSetting DB error: got %d, want 77 (fallback)", got)
	}
	logged := buf.String()
	if !strings.Contains(logged, "reading int setting") {
		t.Errorf("getIntSetting DB error: expected warn log, got %q", logged)
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("getIntSetting DB error: expected WARN level, got %q", logged)
	}
}

// TestGetIntSetting_AbsentKey verifies that a missing row returns the
// fallback without emitting any log line.
func TestGetIntSetting_AbsentKey(t *testing.T) {
	t.Parallel()
	db, dbPath := setupTestDB(t)
	buf, lg := warnLogger()
	svc := NewService(db, dbPath, "", lg)

	got := svc.getIntSetting(context.Background(), "no.such.key", 55)
	if got != 55 {
		t.Errorf("getIntSetting absent key: got %d, want 55 (fallback)", got)
	}
	if buf.Len() != 0 {
		t.Errorf("getIntSetting absent key: expected no log, got %q", buf.String())
	}
}

// TestGetIntSetting_InvalidValue verifies that a stored non-integer value
// returns the fallback AND emits a Warn log line (Item 2 observability).
// TestGetIntSetting_InvalidValue verifies that a stored value that is not a
// valid integer returns the fallback and logs a Warn. The cases cover both a
// fully non-numeric value and a partial-numeric value (e.g. "12abc"): the
// latter is the case strconv.Atoi rejects but the old fmt.Sscanf(v, "%d", &n)
// would silently accept (parsing the leading "12" and ignoring the rest).
func TestGetIntSetting_InvalidValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
	}{
		{"non_numeric", "notanint"},
		{"leading_numeric_trailing_garbage", "12abc"},
		{"float_like", "3.14"},
		{"whitespace_padded", " 12 "},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, dbPath := setupTestDB(t)
			ctx := context.Background()
			if _, err := db.ExecContext(ctx,
				`INSERT INTO settings (key, value) VALUES ('test.bad', ?)`, tc.value); err != nil {
				t.Fatalf("seeding bad int: %v", err)
			}

			buf, lg := warnLogger()
			svc := NewService(db, dbPath, "", lg)

			got := svc.getIntSetting(ctx, "test.bad", 33)
			if got != 33 {
				t.Errorf("getIntSetting(%q): got %d, want 33 (fallback)", tc.value, got)
			}
			logged := buf.String()
			if !strings.Contains(logged, "int setting value is not a valid integer") {
				t.Errorf("getIntSetting(%q): expected warn log, got %q", tc.value, logged)
			}
			if !strings.Contains(logged, "level=WARN") {
				t.Errorf("getIntSetting(%q): expected WARN level, got %q", tc.value, logged)
			}
		})
	}
}

// TestStatus_LastOptimizeAt_DBError verifies that when the last_optimize_at
// query hits a non-ErrNoRows DB error, Status() logs a Warn and returns
// an empty LastOptimizeAt rather than silently swallowing the error.
func TestStatus_LastOptimizeAt_DBError(t *testing.T) {
	t.Parallel()
	db, dbPath := setupTestDB(t)
	buf, lg := warnLogger()
	svc := NewService(db, dbPath, "", lg)

	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	st, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status returned error (should tolerate DB failures): %v", err)
	}
	if st.LastOptimizeAt != "" {
		t.Errorf("Status DB error: LastOptimizeAt = %q, want empty", st.LastOptimizeAt)
	}
	logged := buf.String()
	if !strings.Contains(logged, "reading last_optimize_at") {
		t.Errorf("Status DB error: expected 'reading last_optimize_at' warn, got %q", logged)
	}
}

// TestRestoreExistsFlags is the batch-remediation test for issue #2668. It
// asserts the traversal restores exists_flag=1 ONLY for slots whose file is
// positively confirmed on disk, and leaves every other row cleared: a slot with
// no file, a slot whose directory is absent or unreadable, and a pathless row
// with no cache-dir fallback. It also pins design-lock #3 -- a LOCKED row whose
// file is present IS restored, with its lock untouched -- and the slot-aware
// guard, that a present fanart slot 0 does not resurrect an empty slot 1 --
// paired with its positive counterpart, that a fanart slot 1 whose ordinal file
// IS on disk does get restored.
func TestRestoreExistsFlags(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the unreadable-dir case cannot fire")
	}
	db, dbPath := setupTestDBWithImages(t)
	// cacheDir="" so a pathless artist is genuinely unresolvable.
	svc := NewService(db, dbPath, "", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ctx := context.Background()
	libRoot := t.TempDir()

	// present: thumb/0 and fanart/0 files on disk; fanart/1 has NO file.
	presentDir := filepath.Join(libRoot, "present")
	writeImage(t, filepath.Join(presentDir, "folder.jpg"), 100, 100)
	writeImage(t, filepath.Join(presentDir, "backdrop.jpg"), 100, 100)
	const presentID = "11111111-0000-0000-0000-000000000001"
	seedArtist(t, db, presentID, presentDir)
	seedImageRow(t, db, presentID, "thumb", 0, 0, 0)  // confirmed -> restore
	seedImageRow(t, db, presentID, "fanart", 0, 0, 0) // confirmed -> restore
	seedImageRow(t, db, presentID, "fanart", 1, 0, 0) // no backdrop2.jpg -> stays 0

	// multi-fanart: TWO fanart files on disk, so the UPPER ordinal is genuinely
	// backed. This is the positive counterpart to presentDir's slot-1 case and
	// the core claim for multi-fanart libraries: slot_index is a DiscoverFanart
	// ordinal, so slot 1 resolves to backdrop2.jpg and must be restored.
	multiDir := filepath.Join(libRoot, "multi")
	writeImage(t, filepath.Join(multiDir, "backdrop.jpg"), 100, 100)
	writeImage(t, filepath.Join(multiDir, "backdrop2.jpg"), 100, 100)
	const multiID = "77777777-0000-0000-0000-000000000001"
	seedArtist(t, db, multiID, multiDir)
	seedImageRow(t, db, multiID, "fanart", 1, 0, 0) // ordinal 1 on disk -> restore

	// locked: file present, row locked and stale-missing -> restored, lock kept.
	lockedDir := filepath.Join(libRoot, "locked")
	writeImage(t, filepath.Join(lockedDir, "folder.jpg"), 100, 100)
	const lockedID = "22222222-0000-0000-0000-000000000001"
	seedArtist(t, db, lockedID, lockedDir)
	seedImageRow(t, db, lockedID, "thumb", 0, 0, 1) // restore -> flag 1, locked stays 1

	// missing dir: single-slot reads a clean ENOENT miss, fanart reads a dir
	// read error; both must leave the flag cleared.
	const missingID = "33333333-0000-0000-0000-000000000001"
	seedArtist(t, db, missingID, filepath.Join(libRoot, "does-not-exist"))
	seedImageRow(t, db, missingID, "thumb", 0, 0, 0)  // stays 0
	seedImageRow(t, db, missingID, "fanart", 0, 0, 0) // stays 0 (unverifiable)

	// unreadable dir (EACCES): the definitive "cannot tell" case -- must skip,
	// never restore on an unverifiable read.
	unreadableDir := filepath.Join(libRoot, "unreadable")
	if err := os.MkdirAll(unreadableDir, 0o755); err != nil {
		t.Fatalf("mkdir unreadable: %v", err)
	}
	writeImage(t, filepath.Join(unreadableDir, "folder.jpg"), 100, 100)
	if err := os.Chmod(unreadableDir, 0o000); err != nil {
		t.Fatalf("chmod unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadableDir, 0o755) })
	const unreadableID = "44444444-0000-0000-0000-000000000001"
	seedArtist(t, db, unreadableID, unreadableDir)
	seedImageRow(t, db, unreadableID, "thumb", 0, 0, 0) // unverifiable -> stays 0

	// pathless: no path, no cache dir -> unresolvable -> left cleared.
	const pathlessID = "55555555-0000-0000-0000-000000000001"
	seedArtist(t, db, pathlessID, "")
	seedImageRow(t, db, pathlessID, "thumb", 0, 0, 0) // stays 0

	// already-set: exists_flag=1 is not even selected and must remain 1.
	alreadyDir := filepath.Join(libRoot, "already")
	writeImage(t, filepath.Join(alreadyDir, "folder.jpg"), 100, 100)
	const alreadyID = "66666666-0000-0000-0000-000000000001"
	seedArtist(t, db, alreadyID, alreadyDir)
	seedImageRow(t, db, alreadyID, "thumb", 0, 1, 0) // stays 1

	res, err := svc.RestoreExistsFlags(ctx, ExistsFlagRestoreOpts{Commit: true})
	if err != nil {
		t.Fatalf("RestoreExistsFlags: %v", err)
	}
	// The structured result (#2669) must describe the SAME pass the row
	// assertions below verify, so assert its counters against the fixture:
	// 9 cleared rows are examined, 4 are confirmed present and restored, 2 are
	// probed and unanswerable (the EACCES directory, and the fanart slot under
	// the absent directory, whose fanart resolution errors rather than
	// reporting a clean miss), and 1 is unresolvable (the pathless artist, for
	// which no directory could be derived so no probe was ever attempted).
	// Skipped and Unresolvable are both held apart from the confirmed-absent
	// single-slot row, which is counted only in Checked -- "definitively
	// absent" is not "cannot tell", and "could not answer" is not "never
	// asked".
	if res.DryRun {
		t.Error("Commit:true must not report a dry run")
	}
	if res.Checked != 9 || res.Restored != 4 || res.Skipped != 2 || res.Unresolvable != 1 || res.Failed != 0 {
		t.Errorf("result = %+v; want checked 9 restored 4 skipped 2 unresolvable 1 failed 0", *res)
	}

	assertFlag := func(id, typ string, slot, wantFlag, wantLocked int) {
		t.Helper()
		flag, locked := slotFlags(t, db, id, typ, slot)
		if flag != wantFlag || locked != wantLocked {
			t.Errorf("%s %s/%d = flag %d locked %d; want flag %d locked %d",
				id[:8], typ, slot, flag, locked, wantFlag, wantLocked)
		}
	}

	assertFlag(presentID, "thumb", 0, 1, 0)    // confirmed on disk -> restored
	assertFlag(presentID, "fanart", 0, 1, 0)   // confirmed on disk -> restored
	assertFlag(presentID, "fanart", 1, 0, 0)   // slot-aware: no file -> left cleared
	assertFlag(multiID, "fanart", 1, 1, 0)     // ordinal 1 backed by backdrop2.jpg -> restored
	assertFlag(lockedID, "thumb", 0, 1, 1)     // design-lock #3: restored, lock kept
	assertFlag(missingID, "thumb", 0, 0, 0)    // absent single-slot -> left cleared
	assertFlag(missingID, "fanart", 0, 0, 0)   // unverifiable fanart -> left cleared
	assertFlag(unreadableID, "thumb", 0, 0, 0) // EACCES -> never restored
	assertFlag(pathlessID, "thumb", 0, 0, 0)   // unresolvable -> left cleared
	assertFlag(alreadyID, "thumb", 0, 1, 0)    // already set -> untouched
}

// TestRestoreExistsFlagsPreview is the pass-level half of #2669's byte-identical
// guarantee: Commit:false must run the identical scan and on-disk confirmation
// and report what WOULD be restored, without flipping a single flag.
//
// It asserts BOTH halves, because either one alone is satisfiable by a bug: a
// pass that returned early and did nothing would leave the flags untouched
// (passing a flags-only assertion) while reporting a useless zero, and a pass
// that computed the right number while still writing would report correctly
// (passing a counts-only assertion) while destroying the guarantee.
//
// The ArtistID scoping is asserted here too, on the same fixture: the second
// artist's confirmable row must NOT appear in a scoped pass's Checked count,
// or a scoped request would silently do library-wide work.
func TestRestoreExistsFlagsPreview(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ctx := context.Background()
	libRoot := t.TempDir()

	// Two artists, each with one cleared row whose file IS on disk, so a commit
	// pass would restore both.
	const aID = "aaaaaaaa-0000-0000-0000-000000000001"
	const bID = "bbbbbbbb-0000-0000-0000-000000000001"
	for id, name := range map[string]string{aID: "a", bID: "b"} {
		dir := filepath.Join(libRoot, name)
		writeImage(t, filepath.Join(dir, "folder.jpg"), 64, 64)
		seedArtist(t, db, id, dir)
		seedImageRow(t, db, id, "thumb", 0, 0, 0)
	}

	res, err := svc.RestoreExistsFlags(ctx, ExistsFlagRestoreOpts{Commit: false})
	if err != nil {
		t.Fatalf("RestoreExistsFlags preview: %v", err)
	}
	if !res.DryRun {
		t.Error("Commit:false must report a dry run")
	}
	// The preview computed the real plan...
	if res.Checked != 2 || res.Restored != 2 || res.Failed != 0 {
		t.Errorf("preview result = %+v; want checked 2 restored 2 failed 0", *res)
	}
	// ...and wrote nothing.
	for _, id := range []string{aID, bID} {
		if flag, _ := slotFlags(t, db, id, "thumb", 0); flag != 0 {
			t.Errorf("%s thumb/0 = %d after a preview; a preview must not write", id[:8], flag)
		}
	}

	// Scoped preview: only artist a is in scope.
	scoped, err := svc.RestoreExistsFlags(ctx, ExistsFlagRestoreOpts{ArtistID: aID})
	if err != nil {
		t.Fatalf("scoped preview: %v", err)
	}
	if scoped.Checked != 1 || scoped.Restored != 1 {
		t.Errorf("scoped preview = %+v; want checked 1 restored 1", *scoped)
	}

	// Scoped COMMIT writes exactly the scoped artist and leaves the other alone.
	if _, err := svc.RestoreExistsFlags(ctx, ExistsFlagRestoreOpts{Commit: true, ArtistID: aID}); err != nil {
		t.Fatalf("scoped commit: %v", err)
	}
	if flag, _ := slotFlags(t, db, aID, "thumb", 0); flag != 1 {
		t.Errorf("scoped artist thumb/0 = %d after commit; want 1", flag)
	}
	if flag, _ := slotFlags(t, db, bID, "thumb", 0); flag != 0 {
		t.Errorf("out-of-scope artist thumb/0 = %d; a scoped commit must not touch it", flag)
	}
}

// TestRestoreExistsFlagsEmpty verifies RestoreExistsFlags succeeds on an empty
// table (nothing to restore).
func TestRestoreExistsFlagsEmpty(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	res, err := svc.RestoreExistsFlags(context.Background(), ExistsFlagRestoreOpts{Commit: true})
	if err != nil {
		t.Fatalf("RestoreExistsFlags on empty table: %v", err)
	}
	if res.Checked != 0 || res.Restored != 0 || res.Skipped != 0 || res.Failed != 0 {
		t.Errorf("empty table result = %+v; want all zero", *res)
	}
}

// TestRestoreExistsFlagsCanceledContext verifies a canceled context fails the
// top-level SELECT and RestoreExistsFlags returns that error rather than
// proceeding with an incomplete scan.
func TestRestoreExistsFlagsCanceledContext(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := svc.RestoreExistsFlags(ctx, ExistsFlagRestoreOpts{Commit: true})
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestRestoreExistsFlagsCanceledMidLoopAborts pins that a context canceled
// once the pass has reached its UPDATE loop ABORTS the pass, rather than
// letting every remaining ExecContext fail on the dead context and reporting
// the result as N write failures.
//
// The scenario is an operator issuing commit:true against a large library whose
// client then disconnects. Without the loop's ctx.Err() check the function
// returns (res, nil) -- a NIL error -- with Failed equal to the number of
// remaining rows, so the endpoint answers HTTP 200 with a report that is silent
// about hundreds of writes that never landed. A canceled context is one aborted
// pass, not N data-write failures, and the caller must be told which.
//
// Distinct from TestRestoreExistsFlagsCanceledContext, which cancels before the
// call and is satisfied by the initial QueryContext failing; that test never
// reaches the UPDATE loop and so does not guard this.
//
// CONSTRUCTION. A plain canceled context cannot exercise this: database/sql
// would fail the opening QueryContext and the pass would abort long before the
// loop, so the test would pass with the loop check removed and guard nothing.
// errNoDoneCtx instead reports Err() == context.Canceled while leaving Done()
// nil, which is what context.Background() already returns. database/sql only
// consults Done() on its fast paths, so every statement in the pass still
// executes normally and the pass's OWN explicit ctx.Err() check is the single
// thing that can observe the cancellation. That is exactly the code under test:
// with the check removed, all three UPDATE statements succeed and the function returns
// (res, nil).
type errNoDoneCtx struct{ context.Context }

func (errNoDoneCtx) Err() error { return context.Canceled }

func TestRestoreExistsFlagsCanceledMidLoopAborts(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	libRoot := t.TempDir()

	// Several artists, each with a cleared row whose file IS on disk, so the
	// confirmation phase yields a multi-entry present set and the UPDATE loop
	// would iterate more than once.
	ids := []string{
		"c1111111-0000-0000-0000-000000000001",
		"c2222222-0000-0000-0000-000000000001",
		"c3333333-0000-0000-0000-000000000001",
	}
	for i, id := range ids {
		dir := filepath.Join(libRoot, fmt.Sprintf("artist%d", i))
		writeImage(t, filepath.Join(dir, "folder.jpg"), 64, 64)
		seedArtist(t, db, id, dir)
		seedImageRow(t, db, id, "thumb", 0, 0, 0)
	}

	// Sanity-check the fixture BEFORE asserting the abort: a preview over the
	// same rows must report a non-empty present set, or the UPDATE loop would
	// never have been entered and the assertions below would pass vacuously.
	plan, err := svc.RestoreExistsFlags(context.Background(), ExistsFlagRestoreOpts{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if plan.Restored != len(ids) {
		t.Fatalf("preview would restore %d of %d rows; the abort assertions need a "+
			"multi-row UPDATE loop to abort", plan.Restored, len(ids))
	}

	res, err := svc.RestoreExistsFlags(errNoDoneCtx{context.Background()}, ExistsFlagRestoreOpts{Commit: true})
	if err == nil {
		t.Fatalf("canceled commit returned nil error; result = %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want it to wrap context.Canceled", err)
	}
	if res != nil {
		t.Errorf("canceled commit returned a result %+v; an aborted pass must not hand back "+
			"a report that reads like a completed one", *res)
	}
	// The artifact, not the counters: nothing was written.
	for _, id := range ids {
		if flag, _ := slotFlags(t, db, id, "thumb", 0); flag != 0 {
			t.Errorf("%s thumb/0 = %d after an aborted pass; want 0", id[:8], flag)
		}
	}
}

// TestConfirmSlotOnDisk exercises the slot-aware disk-confirmation helper's
// three return shapes directly: confirmed present, definitively absent, and
// unverifiable. It covers single-slot and fanart types plus the "slot > 0 for a
// single-slot type has no naming" and "unknown type" edge cases, and both
// directions of the fanart ORDINAL mapping: an upper slot with no backing file
// stays unconfirmed, while an upper slot whose ordinal file IS on disk confirms.
func TestConfirmSlotOnDisk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "folder.jpg"), 100, 100)   // thumb slot 0
	writeImage(t, filepath.Join(dir, "backdrop.jpg"), 100, 100) // fanart slot 0

	// Separate directory holding TWO fanart files under the same convention, so
	// the positive upper-ordinal case has somewhere to live without disturbing
	// the single-file "fanart slot 1 absent" case above. backdrop2.jpg is the
	// name Stillwater writes for fanart index 1 under the backdrop convention
	// (FanartFilename, non-Kodi numbering), so ResolveFanart returns it as the
	// SECOND path -- DiscoverFanart ordinal 1.
	multiDir := t.TempDir()
	writeImage(t, filepath.Join(multiDir, "backdrop.jpg"), 100, 100)  // fanart slot 0
	writeImage(t, filepath.Join(multiDir, "backdrop2.jpg"), 100, 100) // fanart slot 1

	unreadable := filepath.Join(t.TempDir(), "blind")
	if err := os.MkdirAll(unreadable, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	cases := []struct {
		name      string
		dir       string
		imageType string
		slot      int
		wantFound bool
		wantErr   bool
	}{
		{"thumb present", dir, "thumb", 0, true, false},
		{"fanart slot 0 present", dir, "fanart", 0, true, false},
		{"fanart slot 1 absent", dir, "fanart", 1, false, false},
		{"fanart slot 1 present", multiDir, "fanart", 1, true, false},
		{"thumb absent dir (clean miss)", filepath.Join(t.TempDir(), "nope"), "thumb", 0, false, false},
		{"fanart absent dir (read error)", filepath.Join(t.TempDir(), "nope"), "fanart", 0, false, true},
		{"single-slot type at slot > 0 has no naming", dir, "thumb", 1, false, false},
		{"unknown image type", dir, "poster", 0, false, false},
		{"thumb unverifiable dir (EACCES)", unreadable, "thumb", 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, err := confirmSlotOnDisk(tc.dir, tc.imageType, tc.slot)
			if found != tc.wantFound {
				t.Errorf("found = %v, want %v", found, tc.wantFound)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
