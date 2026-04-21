package maintenance

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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

// setupTestDBWithImages creates a DB with the artists and artist_images tables
// in addition to the standard settings table.
func setupTestDBWithImages(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db, dbPath := setupTestDB(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS artists (
			id   TEXT NOT NULL PRIMARY KEY,
			path TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS artist_images (
			id           TEXT NOT NULL PRIMARY KEY,
			artist_id    TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
			image_type   TEXT NOT NULL,
			slot_index   INTEGER NOT NULL DEFAULT 0,
			exists_flag  INTEGER NOT NULL DEFAULT 0,
			low_res      INTEGER NOT NULL DEFAULT 0,
			placeholder  TEXT NOT NULL DEFAULT '',
			width        INTEGER NOT NULL DEFAULT 0,
			height       INTEGER NOT NULL DEFAULT 0,
			phash        TEXT NOT NULL DEFAULT '',
			file_format  TEXT NOT NULL DEFAULT '',
			source       TEXT NOT NULL DEFAULT '',
			last_written_at TEXT NOT NULL DEFAULT '',
			locked       INTEGER NOT NULL DEFAULT 0,
			UNIQUE(artist_id, image_type, slot_index)
		)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup images tables: %v", err)
		}
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
	svc := NewService(db, dbPath, "", slog.Default())

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
	svc := NewService(db, dbPath, "", slog.Default())

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
	svc := NewService(db, dbPath, "", slog.Default())

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

	// Seed artists: real path, missing path, and no path (cache-dir fallback).
	for _, a := range []struct {
		id   string
		path string
	}{
		{"artist-real", realDir},
		{"artist-missing", "/tmp/does-not-exist-in-tests"},
		{"artist-nocache", ""},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, path) VALUES (?, ?)`, a.id, a.path,
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
		`INSERT INTO artists (id, path) VALUES ('a-unresolvable', '')`); err != nil {
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
		`INSERT INTO artists (id, path) VALUES ('a-eacces', ?)`, child); err != nil {
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

	if err := svc.ScanExistsFlags(ctx); err == nil {
		t.Fatal("expected error from canceled context, got nil")
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

	// Seed one artist with a path that doesn't exist; exists_flag=1 should
	// be cleared by the first scan.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, path) VALUES ('a1', '/nope-does-not-exist')`); err != nil {
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
func TestStartExistsFlagScanner_CanceledDuringStartupDelay(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// Long startupDelay; we'll cancel before it elapses.
		svc.StartExistsFlagScanner(ctx, time.Hour, 30*time.Second)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scanner did not stop within 2s of cancel during startup delay")
	}
}
