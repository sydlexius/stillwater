package database

import (
	"context"
	"database/sql"
	"testing"
)

// openMigratedDB opens an in-memory SQLite database, runs the 001 migration
// + runtime helpers, and turns on PRAGMA foreign_keys so cascade behavior is
// actually exercised. Production main calls EnableForeignKeys for the same
// reason; tests in this file replicate that path because their assertions
// depend on FK CASCADE / FK rejection actually firing.
func openMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	return db
}

func seedConnection(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		VALUES (?, 'Conn', 'emby', 'http://t', 'k', 1, 'ok', datetime('now'), datetime('now'))
	`, id)
	if err != nil {
		t.Fatalf("seeding connection: %v", err)
	}
}

func seedArtist(t *testing.T, db *sql.DB, id, name string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		VALUES (?, ?, ?, ?, datetime('now'), datetime('now'))
	`, id, name, name, "/music/"+name)
	if err != nil {
		t.Fatalf("seeding artist %s: %v", id, err)
	}
}

// TestArtistPlatformIDsCascadeOnArtistDelete verifies that deleting an artist
// row removes its artist_platform_ids row via ON DELETE CASCADE. This is the
// regression test called for in issue #1078.
func TestArtistPlatformIDsCascadeOnArtistDelete(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	seedConnection(t, db, "conn-cascade")
	seedArtist(t, db, "a-1", "ArtistOne")

	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		VALUES ('a-1', 'conn-cascade', 'platform-1')
	`); err != nil {
		t.Fatalf("inserting platform id: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM artists WHERE id = 'a-1'`); err != nil {
		t.Fatalf("deleting artist: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'a-1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("artist_platform_ids count = %d, want 0 (CASCADE should have removed it)", n)
	}
}

// TestArtistPlatformIDsUniqueConstraint covers issue #1076: inserting two
// rows with the same (connection_id, platform_artist_id) must be rejected
// by the UNIQUE index, regardless of artist_id.
func TestArtistPlatformIDsUniqueConstraint(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	seedConnection(t, db, "c-1")
	seedArtist(t, db, "a-1", "First")
	seedArtist(t, db, "a-2", "Second")

	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		VALUES ('a-1', 'c-1', 'shared')
	`); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		VALUES ('a-2', 'c-1', 'shared')
	`)
	if err == nil {
		t.Fatal("expected unique constraint violation on duplicate (connection_id, platform_artist_id)")
	}
}

// TestEnsureArtistPlatformIDsUnique_DedupesExisting covers the runtime
// dedupe helper. It inserts a duplicate state on a DB without the index,
// then asserts the helper collapses the duplicates and creates the index.
//
// The setup must bypass the index that Migrate already created in the
// openMigratedDB helper; we drop it, insert the duplicates, and re-run the
// helper to simulate "legacy database with pre-existing duplicates".
func TestEnsureArtistPlatformIDsUnique_DedupesExisting(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_artist_platform_ids_unique`); err != nil {
		t.Fatalf("dropping unique index: %v", err)
	}

	seedConnection(t, db, "c-1")
	seedArtist(t, db, "a-old", "Old")
	seedArtist(t, db, "a-new", "New")
	// Newer updated_at on a-new makes it the keeper; tie-break is artist id.
	if _, err := db.ExecContext(ctx,
		`UPDATE artists SET updated_at = '2025-01-01T00:00:00Z' WHERE id = 'a-old'`); err != nil {
		t.Fatalf("updating a-old: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE artists SET updated_at = '2026-01-01T00:00:00Z' WHERE id = 'a-new'`); err != nil {
		t.Fatalf("updating a-new: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		VALUES ('a-old', 'c-1', 'shared'), ('a-new', 'c-1', 'shared')
	`); err != nil {
		t.Fatalf("inserting duplicates: %v", err)
	}

	if err := ensureArtistPlatformIDsUnique(db); err != nil {
		t.Fatalf("ensureArtistPlatformIDsUnique: %v", err)
	}

	// Both artist rows remain. The dedup helper resolves the unique-key
	// conflict by deleting only the losing platform mapping; legacy
	// duplicates are not always true duplicate artists, so we never erase
	// an artist row out from under its images, rule state, or library
	// association during a startup repair.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id IN ('a-old', 'a-new')`).Scan(&n); err != nil {
		t.Fatalf("count artists: %v", err)
	}
	if n != 2 {
		t.Errorf("artist count = %d, want 2 (both artists must remain after dedup)", n)
	}

	// Exactly one mapping row survives, and it points to the keeper.
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE connection_id = 'c-1' AND platform_artist_id = 'shared'`).Scan(&n); err != nil {
		t.Fatalf("count mappings: %v", err)
	}
	if n != 1 {
		t.Errorf("mapping count = %d, want 1 (loser mapping should be deleted)", n)
	}

	var keeper string
	if err := db.QueryRowContext(ctx,
		`SELECT artist_id FROM artist_platform_ids WHERE connection_id = 'c-1' AND platform_artist_id = 'shared'`).Scan(&keeper); err != nil {
		t.Fatalf("querying keeper: %v", err)
	}
	if keeper != "a-new" {
		t.Errorf("keeper = %q, want a-new", keeper)
	}

	// The losing artist's other associations are untouched. The keeper now
	// owns the mapping; the loser stays in artists with whatever other
	// state it had.
	var loserName string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM artists WHERE id = 'a-old'`).Scan(&loserName); err != nil {
		t.Errorf("losing artist row was unexpectedly removed: %v", err)
	}
	if loserName != "Old" {
		t.Errorf("losing artist name = %q, want Old", loserName)
	}

	// Index is back; a duplicate insert is now rejected.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		VALUES ('a-third', 'Third', 'Third', '/m/Third', datetime('now'), datetime('now'))
	`); err != nil {
		t.Fatalf("inserting a-third: %v", err)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		VALUES ('a-third', 'c-1', 'shared')
	`)
	if err == nil {
		t.Error("expected unique constraint violation after dedup helper ran")
	}
}

// TestCleanupOrphanArtistPlatformIDs covers the safety net for issue #1078.
// Insert a row with foreign keys disabled (mimicking the suspected legacy
// path), then run the cleanup and assert the orphan is gone.
func TestCleanupOrphanArtistPlatformIDs(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	seedConnection(t, db, "c-1")
	seedArtist(t, db, "a-1", "Real")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		VALUES ('a-1', 'c-1', 'p-1')
	`); err != nil {
		t.Fatalf("inserting good row: %v", err)
	}

	// Insert an orphan by temporarily disabling foreign key enforcement.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disabling fks: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		VALUES ('does-not-exist', 'c-1', 'p-orphan')
	`); err != nil {
		t.Fatalf("inserting orphan: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("re-enabling fks: %v", err)
	}

	if err := cleanupOrphanArtistPlatformIDs(db); err != nil {
		t.Fatalf("cleanupOrphanArtistPlatformIDs: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE platform_artist_id = 'p-orphan'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("orphan count = %d, want 0", n)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE platform_artist_id = 'p-1'`).Scan(&n); err != nil {
		t.Fatalf("count good row: %v", err)
	}
	if n != 1 {
		t.Errorf("good row count = %d, want 1", n)
	}
}
