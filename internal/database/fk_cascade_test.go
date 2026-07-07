package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// TestRuntimeFKCascadeSurvivesConnectionChurn is the direct regression test for
// issue #2272: the ON DELETE CASCADE from artists to its child tables must fire
// even on a database/sql connection that was recycled (discarded and reopened)
// after the pool was first created.
//
// The pre-fix design enabled foreign keys with a one-shot PRAGMA on the pool
// (EnableForeignKeys) after Open. PRAGMA foreign_keys is PER-CONNECTION and
// defaults OFF, so a connection the pool later opened came up FK-off and the
// cascade silently no-opped, orphaning child rows (confirmed on live prod). The
// fix is OpenRuntime, whose DSN carries foreign_keys(1) so every connection --
// including recycled ones -- enforces FKs.
//
// To exercise the exact failure this test forces connection churn: it raises
// SetMaxOpenConns above 1 and sets SetMaxIdleConns(0) so database/sql closes
// each connection on release, guaranteeing the next acquire opens a fresh
// underlying SQLite connection. It then deletes the parent artist through a
// deliberately-fresh connection and asserts the child rows are gone.
func TestRuntimeFKCascadeSurvivesConnectionChurn(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/stillwater.db"

	// Bootstrap exactly like production: migrate on a short-lived FK-off handle,
	// close it, then serve from the long-lived FK-on runtime pool.
	migDB, err := Open(dbPath)
	if err != nil {
		t.Fatalf("opening migration handle: %v", err)
	}
	if err := Migrate(migDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := migDB.Close(); err != nil {
		t.Fatalf("closing migration handle: %v", err)
	}

	db, err := OpenRuntime(dbPath)
	if err != nil {
		t.Fatalf("opening runtime pool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Verification self-check, mirroring production startup.
	if err := VerifyForeignKeys(db); err != nil {
		t.Fatalf("verifying foreign keys on runtime pool: %v", err)
	}

	// Force connection churn: more than one connection allowed, and no idle
	// connections retained, so every acquire gets a freshly opened connection.
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(0)
	db.SetConnMaxLifetime(time.Millisecond)

	seedConnection(t, db, "conn-churn")
	seedArtist(t, db, "a-churn", "ChurnArtist")

	// Seed child rows in two tables that cascade from artists.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		VALUES ('a-churn', 'conn-churn', 'pid-churn')
	`); err != nil {
		t.Fatalf("seeding artist_platform_ids: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_images (id, artist_id, image_type, slot_index)
		VALUES ('img-churn', 'a-churn', 'thumb', 0)
	`); err != nil {
		t.Fatalf("seeding artist_images: %v", err)
	}

	// Churn the pool: run several throwaway queries so the connections that
	// serviced the seeds are discarded and reopened. With MaxIdleConns(0) each
	// of these opens and closes a fresh connection.
	for i := 0; i < 5; i++ {
		var one int
		if err := db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("churn query %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Grab an explicit, guaranteed-fresh connection and confirm FK enforcement
	// is active on it (this is the connection the pre-fix build would have left
	// FK-off). Then delete the parent through that same fresh connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquiring fresh connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var fkOn int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fkOn); err != nil {
		t.Fatalf("reading foreign_keys pragma on fresh connection: %v", err)
	}
	if fkOn != 1 {
		t.Fatalf("foreign_keys = %d on a recycled connection, want 1 (this is the #2272 bug: cascade would no-op)", fkOn)
	}

	if _, err := conn.ExecContext(ctx, `DELETE FROM artists WHERE id = 'a-churn'`); err != nil {
		t.Fatalf("deleting artist: %v", err)
	}

	for _, c := range []struct {
		table string
		query string
	}{
		{"artist_platform_ids", `SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'a-churn'`},
		{"artist_images", `SELECT COUNT(*) FROM artist_images WHERE artist_id = 'a-churn'`},
	} {
		var n int
		if err := db.QueryRowContext(ctx, c.query).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", c.table, err)
		}
		if n != 0 {
			t.Errorf("%s rows for a-churn = %d, want 0 (ON DELETE CASCADE must fire on the recycled connection)", c.table, n)
		}
	}
}

// TestVerifyForeignKeys proves the runtime self-check genuinely detects FK
// enforcement state rather than masking it: it must pass on an OpenRuntime
// handle (FK enforced via the DSN pragma on every fresh connection) and FAIL on
// a plain Open handle (FK-off, because VerifyForeignKeys does NOT mutate FK on
// before reading). If VerifyForeignKeys mutated (like EnableForeignKeys), the
// Open case below would wrongly pass -- that is exactly the useless-self-check
// regression this test guards against (Codoki finding, #2272).
func TestVerifyForeignKeys(t *testing.T) {
	dbPath := t.TempDir() + "/stillwater.db"

	runtimeDB, err := OpenRuntime(dbPath)
	if err != nil {
		t.Fatalf("opening runtime pool: %v", err)
	}
	t.Cleanup(func() { _ = runtimeDB.Close() })

	if err := VerifyForeignKeys(runtimeDB); err != nil {
		t.Errorf("VerifyForeignKeys on OpenRuntime handle: got error %v, want nil (DSN pragma enforces FK)", err)
	}

	// A plain Open handle never enforces FK (no PRAGMA = ON, no DSN pragma), so
	// a NON-mutating check must report the failure.
	offPath := t.TempDir() + "/stillwater-off.db"
	offDB, err := Open(offPath)
	if err != nil {
		t.Fatalf("opening FK-off handle: %v", err)
	}
	t.Cleanup(func() { _ = offDB.Close() })

	if err := VerifyForeignKeys(offDB); err == nil {
		t.Error("VerifyForeignKeys on FK-off Open handle: got nil, want a non-nil error (a mutating check would wrongly pass here)")
	}
}

// TestMigration019_GeneralOrphanCleanup verifies that migration 019 sweeps
// orphaned CHILD rows (a child row whose artist_id points at a non-existent
// artist) across every child table, while preserving rows keyed to a valid
// parent and rows with a NULL artist_id (foreign_file_allowlist global scope).
// It also asserts idempotency: re-running the migration path is a no-op.
func TestMigration019_GeneralOrphanCleanup(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Stage the schema at version 018, just before 019.
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose SetDialect: %v", err)
	}
	if err := goose.UpTo(db, "migrations", 18); err != nil {
		t.Fatalf("goose UpTo 18: %v", err)
	}

	ctx := context.Background()

	// Valid parent whose children must survive, plus a library and connection
	// so its artist_libraries / artist_platform_ids rows are seedable.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		VALUES ('keep', 'Keep', 'Keep', '/music/keep', datetime('now'), datetime('now'))
	`); err != nil {
		t.Fatalf("seeding keep artist: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		VALUES ('lib-019', 'Lib', '/music', 'regular', 'manual', datetime('now'), datetime('now'))
	`); err != nil {
		t.Fatalf("seeding library: %v", err)
	}
	seedConnection(t, db, "conn-019")

	// FK enforcement is OFF on an Open handle, so orphan child rows referencing
	// the non-existent 'ghost' artist can be inserted -- exactly the corrupt
	// state migration 019 exists to clean up. Migration 019 also runs FK-off
	// (goose runs before FK enforcement), so this matches production.
	//
	// Each table gets: one ORPHAN row (artist_id='ghost', no parent) that must
	// be deleted, and one KEEP row (artist_id='keep') that must survive.
	seeds := []struct {
		table  string
		orphan string
		keep   string
	}{
		{
			"artist_libraries",
			`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
			 VALUES ('ghost', 'lib-019', 'filesystem', datetime('now'))`,
			`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
			 VALUES ('keep', 'lib-019', 'filesystem', datetime('now'))`,
		},
		{
			"artist_platform_ids",
			`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
			 VALUES ('ghost', 'conn-019', 'pid-ghost')`,
			`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
			 VALUES ('keep', 'conn-019', 'pid-keep')`,
		},
		{
			"artist_provider_ids",
			`INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
			 VALUES ('ghost', 'musicbrainz', 'mbid-ghost', datetime('now'))`,
			`INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
			 VALUES ('keep', 'musicbrainz', 'mbid-keep', datetime('now'))`,
		},
		{
			"artist_images",
			`INSERT INTO artist_images (id, artist_id, image_type, slot_index)
			 VALUES ('img-ghost', 'ghost', 'thumb', 0)`,
			`INSERT INTO artist_images (id, artist_id, image_type, slot_index)
			 VALUES ('img-keep', 'keep', 'thumb', 0)`,
		},
		{
			"artist_aliases",
			`INSERT INTO artist_aliases (id, artist_id, alias, source, created_at)
			 VALUES ('alias-ghost', 'ghost', 'Ghost Alias', 'manual', datetime('now'))`,
			`INSERT INTO artist_aliases (id, artist_id, alias, source, created_at)
			 VALUES ('alias-keep', 'keep', 'Keep Alias', 'manual', datetime('now'))`,
		},
		{
			"band_members",
			`INSERT INTO band_members (id, artist_id, member_name, sort_order, created_at, updated_at)
			 VALUES ('bm-ghost', 'ghost', 'Ghost Member', 0, datetime('now'), datetime('now'))`,
			`INSERT INTO band_members (id, artist_id, member_name, sort_order, created_at, updated_at)
			 VALUES ('bm-keep', 'keep', 'Keep Member', 0, datetime('now'), datetime('now'))`,
		},
		{
			"nfo_snapshots",
			`INSERT INTO nfo_snapshots (id, artist_id, content, created_at)
			 VALUES ('nfo-ghost', 'ghost', '<artist/>', datetime('now'))`,
			`INSERT INTO nfo_snapshots (id, artist_id, content, created_at)
			 VALUES ('nfo-keep', 'keep', '<artist/>', datetime('now'))`,
		},
		{
			"metadata_changes",
			`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
			 VALUES ('mc-ghost', 'ghost', 'biography', '', 'x', 'manual', strftime('%Y-%m-%dT%H:%M:%SZ','now'))`,
			`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
			 VALUES ('mc-keep', 'keep', 'biography', '', 'x', 'manual', strftime('%Y-%m-%dT%H:%M:%SZ','now'))`,
		},
		{
			"mb_snapshots",
			`INSERT INTO mb_snapshots (id, artist_id, field, mb_value, fetched_at)
			 VALUES ('mb-ghost', 'ghost', 'biography', 'v', strftime('%Y-%m-%dT%H:%M:%SZ','now'))`,
			`INSERT INTO mb_snapshots (id, artist_id, field, mb_value, fetched_at)
			 VALUES ('mb-keep', 'keep', 'biography', 'v', strftime('%Y-%m-%dT%H:%M:%SZ','now'))`,
		},
		{
			"rule_violations",
			`INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, message, created_at)
			 VALUES ('rv-ghost', 'rule-019', 'ghost', 'ghost', 'test', datetime('now'))`,
			`INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, message, created_at)
			 VALUES ('rv-keep', 'rule-019', 'keep', 'keep', 'test', datetime('now'))`,
		},
		{
			"rule_results",
			`INSERT INTO rule_results (artist_id, rule_id, passed, evaluated_at)
			 VALUES ('ghost', 'rule-019', 0, datetime('now'))`,
			`INSERT INTO rule_results (artist_id, rule_id, passed, evaluated_at)
			 VALUES ('keep', 'rule-019', 0, datetime('now'))`,
		},
		{
			"foreign_files",
			`INSERT INTO foreign_files (id, artist_id, file_path, file_name, size_bytes, detected_at)
			 VALUES ('ff-ghost', 'ghost', '/music/ghost/x.jpg', 'x.jpg', 100, datetime('now'))`,
			`INSERT INTO foreign_files (id, artist_id, file_path, file_name, size_bytes, detected_at)
			 VALUES ('ff-keep', 'keep', '/music/keep/x.jpg', 'x.jpg', 100, datetime('now'))`,
		},
		{
			"foreign_file_allowlist",
			`INSERT INTO foreign_file_allowlist (id, scope, artist_id, file_name, created_at)
			 VALUES ('ffa-ghost', 'artist', 'ghost', 'x.jpg', datetime('now'))`,
			`INSERT INTO foreign_file_allowlist (id, scope, artist_id, file_name, created_at)
			 VALUES ('ffa-keep', 'artist', 'keep', 'x.jpg', datetime('now'))`,
		},
	}

	// Migration 015 ends with PRAGMA foreign_keys = ON, which persists on this
	// single-connection :memory: handle. Turn enforcement OFF so the orphan
	// rows (artist_id='ghost', no parent) can be inserted -- the exact corrupt
	// state 019 cleans up. Goose runs 019 FK-off in production too.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disabling FKs for orphan seed: %v", err)
	}

	for _, s := range seeds {
		if _, err := db.ExecContext(ctx, s.orphan); err != nil {
			t.Fatalf("seeding orphan row in %s: %v", s.table, err)
		}
		if _, err := db.ExecContext(ctx, s.keep); err != nil {
			t.Fatalf("seeding keep row in %s: %v", s.table, err)
		}
	}

	// A global-scope allowlist row: artist_id IS NULL. Migration 019 must never
	// touch it (the IS NOT NULL guard preserves it).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO foreign_file_allowlist (id, scope, artist_id, file_name, created_at)
		VALUES ('ffa-global', 'global', NULL, 'global.jpg', datetime('now'))
	`); err != nil {
		t.Fatalf("seeding global allowlist row: %v", err)
	}

	// Apply migration 019 (and any reconciliation) via the real Migrate path.
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate (applies 019): %v", err)
	}

	assertState := func(phase string) {
		t.Helper()
		for _, s := range seeds {
			var orphanN int
			if err := db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM `+s.table+` WHERE artist_id = 'ghost'`).Scan(&orphanN); err != nil {
				t.Fatalf("[%s] counting orphans in %s: %v", phase, s.table, err)
			}
			if orphanN != 0 {
				t.Errorf("[%s] %s orphan rows for 'ghost' = %d, want 0 (migration 019 must sweep them)", phase, s.table, orphanN)
			}
			var keepN int
			if err := db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM `+s.table+` WHERE artist_id = 'keep'`).Scan(&keepN); err != nil {
				t.Fatalf("[%s] counting keep rows in %s: %v", phase, s.table, err)
			}
			if keepN != 1 {
				t.Errorf("[%s] %s keep rows for 'keep' = %d, want 1 (valid-parent rows must be preserved)", phase, s.table, keepN)
			}
		}
		var globalN int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM foreign_file_allowlist WHERE id = 'ffa-global' AND artist_id IS NULL`).Scan(&globalN); err != nil {
			t.Fatalf("[%s] counting global allowlist row: %v", phase, err)
		}
		if globalN != 1 {
			t.Errorf("[%s] global allowlist row count = %d, want 1 (NULL artist_id rows must be preserved)", phase, globalN)
		}
	}

	assertState("after 019")

	// Idempotency: re-running the migration path on a DB already at 019 must be
	// a no-op -- no error, no further deletions, survivors untouched.
	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate after 019: %v", err)
	}
	assertState("second Migrate")
}

// TestVerifyForeignKeys_ClosedPoolErrors covers the connection-acquisition
// error branch: a self-check against an already-closed pool must fail loudly
// rather than reporting FK enforcement as active.
func TestVerifyForeignKeys_ClosedPoolErrors(t *testing.T) {
	t.Parallel()
	db, err := OpenRuntime(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := VerifyForeignKeys(db); err == nil {
		t.Error("VerifyForeignKeys on a closed pool = nil, want error")
	}
}

// TestOpen_UncreatableParentErrors covers open()'s MkdirAll failure branch: a
// path whose parent component is a regular file (not a directory) cannot have
// its directory created, so Open must return a wrapped error.
func TestOpen_UncreatableParentErrors(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "iam-a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// filepath.Dir("<file>/child.db") == "<file>", which MkdirAll cannot create
	// because a non-directory already exists at that path.
	if _, err := Open(filepath.Join(file, "child.db")); err == nil {
		t.Error("Open under a regular-file parent = nil, want error")
	}
}

// TestEnableForeignKeys_ClosedPoolErrors covers the connection-acquisition
// error branch: enabling FK on an already-closed pool must fail loudly.
func TestEnableForeignKeys_ClosedPoolErrors(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "closed-enable.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := EnableForeignKeys(db); err == nil {
		t.Error("EnableForeignKeys on a closed pool = nil, want error")
	}
}

// TestOpen_PingFailureErrors covers open()'s ping-failure branch: a path that is
// an existing directory opens lazily but cannot be pinged as a SQLite database,
// so open() must close the handle and return a wrapped error.
func TestOpen_PingFailureErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // an existing directory is not a valid SQLite file
	if _, err := Open(dir); err == nil {
		t.Error("Open on a directory path = nil, want a ping error")
	}
}
