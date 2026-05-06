package database

import (
	"context"
	"database/sql"
	"strings"
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
// regression test called for in
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

// TestArtistPlatformIDsUniqueConstraint covers inserting two
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

// TestCleanupOrphanArtistPlatformIDs covers the safety net for
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

// seedLibrary inserts a libraries row with the given id, source, and optional
// connection_id. source is one of 'filesystem' | 'manual' | a platform name.
func seedLibrary(t *testing.T, db *sql.DB, id, source, connID string) {
	t.Helper()
	var connArg interface{}
	if connID != "" {
		connArg = connID
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO libraries (id, name, path, type, source, connection_id, created_at, updated_at)
		VALUES (?, ?, '/music', 'regular', ?, ?, datetime('now'), datetime('now'))
	`, id, "lib-"+id, source, connArg)
	if err != nil {
		t.Fatalf("seeding library %s: %v", id, err)
	}
}

// ensureLegacyLibraryIDColumn re-adds the (post-004 dropped)
// artists.library_id column on a freshly migrated DB so the legacy backfill
// and duplicate-collapse helpers can be exercised against a "pre-1004"
// data shape. Idempotent: a no-op when the column is already present.
// Lazy-called from seedArtistWithLibrary so individual tests do not need
// to remember the setup step.
func ensureLegacyLibraryIDColumn(t *testing.T, db *sql.DB) {
	t.Helper()
	has, err := columnExists(db, "artists", "library_id")
	if err != nil {
		t.Fatalf("checking for legacy library_id column: %v", err)
	}
	if has {
		return
	}
	if _, err := db.ExecContext(context.Background(),
		`ALTER TABLE artists ADD COLUMN library_id TEXT REFERENCES libraries(id) DEFAULT NULL`); err != nil {
		t.Fatalf("re-adding legacy library_id column: %v", err)
	}
}

// seedArtistWithLibrary inserts an artist tied to a specific library_id
// (legacy path that the M:N migration backfills from). created_at lets the
// test control the canonical-pick tie-breaker. Lazy-installs the legacy
// library_id column on first use.
func seedArtistWithLibrary(t *testing.T, db *sql.DB, id, name, libraryID, createdAt string) {
	t.Helper()
	ensureLegacyLibraryIDColumn(t, db)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO artists (id, name, sort_name, path, library_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
	`, id, name, name, "/music/"+name, libraryID, createdAt)
	if err != nil {
		t.Fatalf("seeding artist %s: %v", id, err)
	}
}

// seedConnectionWithType inserts a connection row with an explicit type so
// the migration can derive the membership source ('emby' | 'jellyfin' | ...).
func seedConnectionWithType(t *testing.T, db *sql.DB, id, connType string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		VALUES (?, ?, ?, 'http://t', 'k', 1, 'ok', datetime('now'), datetime('now'))
	`, id, "Conn-"+id, connType)
	if err != nil {
		t.Fatalf("seeding connection %s: %v", id, err)
	}
}

// TestArtistLibrariesBackfillFromOrphanColumn verifies that the migration
// reads the legacy artists.library_id column and creates a matching
// artist_libraries membership row, with source derived from the library's
// connection type.
func TestArtistLibrariesBackfillFromOrphanColumn(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	seedConnectionWithType(t, db, "conn-emby", "emby")
	seedConnectionWithType(t, db, "conn-jelly", "jellyfin")
	seedLibrary(t, db, "lib-fs", "filesystem", "")
	seedLibrary(t, db, "lib-emby", "import", "conn-emby")
	seedLibrary(t, db, "lib-jelly", "import", "conn-jelly")

	seedArtistWithLibrary(t, db, "a-fs", "Artist FS", "lib-fs", "2026-01-01T00:00:00Z")
	seedArtistWithLibrary(t, db, "a-emby", "Artist Emby", "lib-emby", "2026-01-02T00:00:00Z")
	seedArtistWithLibrary(t, db, "a-jelly", "Artist Jelly", "lib-jelly", "2026-01-03T00:00:00Z")

	// Re-run membership backfill (Migrate already ran during openMigratedDB,
	// but those artists were inserted after; rerun is idempotent and applies
	// the new rows).
	if err := ensureArtistLibrariesMembership(db); err != nil {
		t.Fatalf("ensureArtistLibrariesMembership: %v", err)
	}

	cases := []struct {
		artist, library, source string
	}{
		{"a-fs", "lib-fs", "filesystem"},
		{"a-emby", "lib-emby", "emby"},
		{"a-jelly", "lib-jelly", "jellyfin"},
	}
	for _, c := range cases {
		var got string
		err := db.QueryRowContext(ctx, `
			SELECT source FROM artist_libraries
			WHERE artist_id = ? AND library_id = ?
		`, c.artist, c.library).Scan(&got)
		if err != nil {
			t.Errorf("missing membership for %s/%s: %v", c.artist, c.library, err)
			continue
		}
		if got != c.source {
			t.Errorf("source for %s/%s = %q, want %q", c.artist, c.library, got, c.source)
		}
	}

	// Idempotency: running it again must not error or create extra rows.
	var before, after int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist_libraries`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if err := ensureArtistLibrariesMembership(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist_libraries`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Errorf("idempotent run changed row count: before=%d after=%d", before, after)
	}
}

// TestCollapseDuplicatesByMBID seeds two artist rows with the same MBID under
// different libraries (filesystem + emby) and asserts the migration:
// - keeps the filesystem row as canonical
// - re-points the loser's artist_provider_ids and other FK rows
// - inserts a membership row for the loser's library under the canonical
// - deletes the loser artist row
func TestCollapseDuplicatesByMBID(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	seedConnectionWithType(t, db, "conn-emby", "emby")
	seedLibrary(t, db, "lib-fs", "filesystem", "")
	seedLibrary(t, db, "lib-emby", "import", "conn-emby")

	// Filesystem row is older + filesystem-source -> wins canonical.
	seedArtistWithLibrary(t, db, "a-fs", "12 Stones", "lib-fs", "2026-01-01T00:00:00Z")
	// Emby row is the would-be loser.
	seedArtistWithLibrary(t, db, "a-emby", "12 Stones", "lib-emby", "2026-01-02T00:00:00Z")

	// Both rows carry the same MBID via artist_provider_ids.
	const mbid = "abcd-1234"
	for _, aid := range []string{"a-fs", "a-emby"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
			VALUES (?, 'musicbrainz', ?, datetime('now'))
		`, aid, mbid); err != nil {
			t.Fatalf("seeding mb provider id for %s: %v", aid, err)
		}
	}
	// Loser also has an alias the canonical doesn't.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_aliases (id, artist_id, alias, source) VALUES ('al-1', 'a-emby', 'Twelve Stones', 'emby')
	`); err != nil {
		t.Fatalf("seeding alias: %v", err)
	}

	if err := ensureArtistLibrariesMembership(db); err != nil {
		t.Fatalf("collapse: %v", err)
	}

	// Canonical artist remains.
	var canonical int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists WHERE id = 'a-fs'`).Scan(&canonical); err != nil {
		t.Fatalf("count canonical: %v", err)
	}
	if canonical != 1 {
		t.Errorf("canonical artist count = %d, want 1", canonical)
	}

	// Loser artist gone.
	var loser int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists WHERE id = 'a-emby'`).Scan(&loser); err != nil {
		t.Fatalf("count loser: %v", err)
	}
	if loser != 0 {
		t.Errorf("loser artist count = %d, want 0", loser)
	}

	// Canonical now has memberships in both libraries.
	wantMemberships := map[string]string{
		"lib-fs":   "filesystem",
		"lib-emby": "emby",
	}
	for libID, wantSrc := range wantMemberships {
		var got string
		err := db.QueryRowContext(ctx, `
			SELECT source FROM artist_libraries
			WHERE artist_id = 'a-fs' AND library_id = ?
		`, libID).Scan(&got)
		if err != nil {
			t.Errorf("missing canonical membership in %s: %v", libID, err)
			continue
		}
		if got != wantSrc {
			t.Errorf("source for canonical/%s = %q, want %q", libID, got, wantSrc)
		}
	}

	// Loser's alias re-pointed to canonical.
	var aliasArtist string
	if err := db.QueryRowContext(ctx, `SELECT artist_id FROM artist_aliases WHERE id = 'al-1'`).Scan(&aliasArtist); err != nil {
		t.Fatalf("query alias: %v", err)
	}
	if aliasArtist != "a-fs" {
		t.Errorf("alias re-pointed to %q, want a-fs", aliasArtist)
	}

	// Idempotent: second run finds no duplicates and changes nothing.
	if err := ensureArtistLibrariesMembership(db); err != nil {
		t.Fatalf("idempotent run: %v", err)
	}
	var artists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists`).Scan(&artists); err != nil {
		t.Fatalf("artist count: %v", err)
	}
	if artists != 1 {
		t.Errorf("artist count after idempotent run = %d, want 1", artists)
	}
}

// TestCollapseDuplicatesByName covers the name-based fallback for rows
// without an MBID. Case-insensitive matching collapses "VERIDIA" and
// "Veridia" into one canonical row (filesystem-source wins).
func TestCollapseDuplicatesByName(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	seedConnectionWithType(t, db, "conn-jelly", "jellyfin")
	seedLibrary(t, db, "lib-fs", "filesystem", "")
	seedLibrary(t, db, "lib-jelly", "import", "conn-jelly")

	seedArtistWithLibrary(t, db, "a-fs", "Veridia", "lib-fs", "2026-01-01T00:00:00Z")
	seedArtistWithLibrary(t, db, "a-jelly", "VERIDIA", "lib-jelly", "2026-01-02T00:00:00Z")

	if err := ensureArtistLibrariesMembership(db); err != nil {
		t.Fatalf("collapse: %v", err)
	}

	var canonical int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists`).Scan(&canonical); err != nil {
		t.Fatalf("count: %v", err)
	}
	if canonical != 1 {
		t.Errorf("artist count = %d, want 1 (collapsed)", canonical)
	}

	// Survivor is the filesystem row.
	var survivor string
	if err := db.QueryRowContext(ctx, `SELECT id FROM artists`).Scan(&survivor); err != nil {
		t.Fatalf("survivor: %v", err)
	}
	if survivor != "a-fs" {
		t.Errorf("survivor = %q, want a-fs", survivor)
	}

	// Survivor has both library memberships.
	var memberships int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artist_libraries WHERE artist_id = 'a-fs'
	`).Scan(&memberships); err != nil {
		t.Fatalf("memberships: %v", err)
	}
	if memberships != 2 {
		t.Errorf("memberships = %d, want 2 (lib-fs + lib-jelly)", memberships)
	}
}

// TestCollapseDuplicatesPreservesPlatformMappings covers the regression
// caught during UAT: artist_platform_ids has a secondary UNIQUE on
// (connection_id, platform_artist_id). The original collapse
// helper used INSERT OR IGNORE to move the loser's mapping onto canonical,
// which the unique index rejected, and the loser cascade-delete then dropped
// the mapping entirely. UPDATE OR IGNORE on the artist_id column moves the
// loser row onto canonical and only drops the row when canonical already
// has a mapping for the same connection.
func TestCollapseDuplicatesPreservesPlatformMappings(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	seedConnectionWithType(t, db, "conn-emby", "emby")
	seedConnectionWithType(t, db, "conn-jelly", "jellyfin")
	seedLibrary(t, db, "lib-fs", "filesystem", "")
	seedLibrary(t, db, "lib-emby", "import", "conn-emby")
	seedLibrary(t, db, "lib-jelly", "import", "conn-jelly")

	seedArtistWithLibrary(t, db, "a-fs", "12 Stones", "lib-fs", "2026-01-01T00:00:00Z")
	seedArtistWithLibrary(t, db, "a-emby", "12 Stones", "lib-emby", "2026-01-02T00:00:00Z")
	seedArtistWithLibrary(t, db, "a-jelly", "12 Stones", "lib-jelly", "2026-01-03T00:00:00Z")
	for _, aid := range []string{"a-fs", "a-emby", "a-jelly"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
			VALUES (?, 'musicbrainz', 'mbid-12-stones', datetime('now'))
		`, aid); err != nil {
			t.Fatalf("seed mbid for %s: %v", aid, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id, created_at, updated_at)
		VALUES ('a-emby', 'conn-emby', 'emby-12s-id',
			strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	`); err != nil {
		t.Fatalf("seed emby mapping: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id, created_at, updated_at)
		VALUES ('a-jelly', 'conn-jelly', 'jelly-12s-id',
			strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	`); err != nil {
		t.Fatalf("seed jellyfin mapping: %v", err)
	}

	if err := ensureArtistLibrariesMembership(db); err != nil {
		t.Fatalf("collapse: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists`).Scan(&n); err != nil {
		t.Fatalf("count artists: %v", err)
	}
	if n != 1 {
		t.Errorf("artist count after collapse = %d, want 1", n)
	}

	want := map[string]string{
		"conn-emby":  "emby-12s-id",
		"conn-jelly": "jelly-12s-id",
	}
	for connID, expectPID := range want {
		var got string
		err := db.QueryRowContext(ctx, `
			SELECT platform_artist_id FROM artist_platform_ids
			WHERE artist_id = 'a-fs' AND connection_id = ?
		`, connID).Scan(&got)
		if err != nil {
			t.Errorf("missing %s mapping on canonical: %v", connID, err)
			continue
		}
		if got != expectPID {
			t.Errorf("%s mapping = %q, want %q", connID, got, expectPID)
		}
	}
}

// TestCollapseDuplicatesPreferMBIDOverName covers the precedence rule: an
// artist already claimed by an MBID group is excluded from name-based
// grouping, even if its name matches another row.
func TestCollapseDuplicatesPreferMBIDOverName(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	seedConnectionWithType(t, db, "conn-emby", "emby")
	seedLibrary(t, db, "lib-fs", "filesystem", "")
	seedLibrary(t, db, "lib-emby", "import", "conn-emby")

	// Two artists with same MBID -> grouped by MBID. A third "Cher" with no
	// MBID exists on a different library; it must NOT be folded into either
	// group because (a) it has no MBID to match the MBID group, and (b) the
	// name-precedence branch excludes any artist already claimed by an MBID
	// group. Without this third row the test passes even when the
	// `claimed` filtering in findDuplicateGroupsByName is broken.
	seedLibrary(t, db, "lib-other", "filesystem", "")
	seedArtistWithLibrary(t, db, "a-fs", "Cher", "lib-fs", "2026-01-01T00:00:00Z")
	seedArtistWithLibrary(t, db, "a-emby", "Cher", "lib-emby", "2026-01-02T00:00:00Z")
	seedArtistWithLibrary(t, db, "a-other", "Cher", "lib-other", "2026-01-03T00:00:00Z")
	for _, aid := range []string{"a-fs", "a-emby"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
			VALUES (?, 'musicbrainz', 'mbid-cher-real', datetime('now'))
		`, aid); err != nil {
			t.Fatalf("seeding mb id %s: %v", aid, err)
		}
	}

	if err := ensureArtistLibrariesMembership(db); err != nil {
		t.Fatalf("collapse: %v", err)
	}

	// Two Chers remain: the MBID group's canonical (a-fs) and the
	// MBID-less standalone (a-other) which name-precedence must leave alone.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists WHERE name = 'Cher'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("Cher count = %d, want 2 (canonical + MBID-less standalone)", n)
	}
	// The MBID-less Cher must survive specifically.
	var stillThere int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists WHERE id = 'a-other'`).Scan(&stillThere); err != nil {
		t.Fatalf("count a-other: %v", err)
	}
	if stillThere != 1 {
		t.Errorf("a-other survived = %d, want 1 (MBID-less row must not be folded into MBID group)", stillThere)
	}
}

// TestCollapseDuplicates_FKDisabledExplicitChildCleanup covers the
// startup ordering where collapseDuplicateArtists runs BEFORE
// EnableForeignKeys turns SQLite FK enforcement on. The collapse code
// has an explicit per-table DELETE that defends against FK-OFF (where
// ON DELETE CASCADE would not fire and child rows would survive as
// orphans pointing at the deleted loser artist).
//
// openMigratedDB enables FKs eagerly, so the rest of the suite can
// rely on cascade rather than the explicit cleanup. This test
// deliberately bypasses that helper and uses the FK-OFF startup shape
// the production migration path actually runs under.
func TestCollapseDuplicates_FKDisabledExplicitChildCleanup(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// IMPORTANT: do NOT call EnableForeignKeys yet. We are simulating
	// the startup window in which collapseDuplicateArtists runs, so
	// CASCADE must not fire and the explicit child cleanup is the only
	// thing keeping orphans from surviving.
	ctx := context.Background()

	seedLibrary(t, db, "lib-fs", "filesystem", "")
	seedArtistWithLibrary(t, db, "a-keep", "Cher", "lib-fs", "2026-01-01T00:00:00Z")
	seedArtistWithLibrary(t, db, "a-loser", "Cher", "lib-fs", "2026-01-02T00:00:00Z")

	// Both rows share an MBID -> same group, a-keep wins on created_at.
	for _, aid := range []string{"a-keep", "a-loser"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
			VALUES (?, 'musicbrainz', 'mbid-fk-test', datetime('now'))
		`, aid); err != nil {
			t.Fatalf("seeding mb id %s: %v", aid, err)
		}
	}

	// Seed a dependent rule_violations row on the loser. With FK
	// enforcement ON, deleting the loser would CASCADE this away.
	// With FK enforcement OFF (this test), only the explicit per-table
	// DELETE in collapseDuplicateArtists can prevent it from becoming
	// an orphan.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rules (id, name, category, enabled, automation_mode, created_at, updated_at)
		VALUES ('rule-1', 'Test Rule', 'integrity', 1, 'manual',
			datetime('now'), datetime('now'))
	`); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, message, created_at)
		VALUES ('rv-1', 'rule-1', 'a-loser', 'Cher', 'test', datetime('now'))
	`); err != nil {
		t.Fatalf("seeding rule_violations: %v", err)
	}

	// Run the collapse path the production migrate startup uses.
	if err := ensureArtistLibrariesMembership(db); err != nil {
		t.Fatalf("collapse: %v", err)
	}

	// a-loser should be gone, a-keep should survive.
	var keepN, loserN int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'a-keep'`).Scan(&keepN); err != nil {
		t.Fatalf("count keeper: %v", err)
	}
	if keepN != 1 {
		t.Errorf("canonical artist count = %d, want 1", keepN)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'a-loser'`).Scan(&loserN); err != nil {
		t.Fatalf("count loser: %v", err)
	}
	if loserN != 0 {
		t.Errorf("loser count = %d, want 0", loserN)
	}

	// Critical assertion: rule_violations row pointing at a-loser must
	// NOT survive even though FK CASCADE was disabled when the collapse
	// ran. Without the explicit child-cleanup DELETE in
	// collapseDuplicateArtists, this row would still be present.
	var orphanN int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rule_violations WHERE artist_id = 'a-loser'`).Scan(&orphanN); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphanN != 0 {
		t.Errorf("orphaned rule_violations rows for a-loser = %d, want 0 (explicit child cleanup must run when FKs are off)", orphanN)
	}

	// Sanity: turning FK on after the collapse should not surface
	// any deferred constraint violations either.
	if err := EnableForeignKeys(db); err != nil {
		t.Errorf("EnableForeignKeys after collapse: %v", err)
	}
}

// TestCollapseDuplicates_CreatedAtMixedFormatsPickEarliest covers the
// pickCanonicalCTE datetime normalization. The artists table contains
// rows in two formats in the wild: SQLite's "YYYY-MM-DD HH:MM:SS" written
// by older code, and RFC3339 "YYYY-MM-DDTHH:MM:SSZ" written by newer Go
// callers. Without `datetime()` normalization, lexical TEXT ordering
// would treat 'T' (0x54) > ' ' (0x20), so a 2026-01-01T00:00:00Z RFC3339
// row would sort AFTER a 2026-02-01 00:00:00 SQLite row even though it
// is chronologically earlier. The canonical-pick rule is "lowest
// (source_rank, created_at, id) wins", so the wrong winner could be
// chosen on real-world DBs whose history mixes both formats.
func TestCollapseDuplicates_CreatedAtMixedFormatsPickEarliest(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	seedLibrary(t, db, "lib-fs", "filesystem", "")

	// Two filesystem rows with the same MBID, identical source_rank.
	// Tie-breaker is created_at_norm, then id. Format mismatch:
	//   a-rfc:    2026-01-01T00:00:00Z (RFC3339, chronologically earliest)
	//   a-sqlite: 2026-02-01 00:00:00  (SQLite text, chronologically later)
	// Lexically: 'T' > ' ', so without datetime() the SQLite row sorts
	// first and would be picked as canonical -- the wrong answer.
	seedArtistWithLibrary(t, db, "a-rfc", "TwinPeaks", "lib-fs", "2026-01-01T00:00:00Z")
	seedArtistWithLibrary(t, db, "a-sqlite", "TwinPeaks", "lib-fs", "2026-02-01 00:00:00")
	for _, aid := range []string{"a-rfc", "a-sqlite"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
			VALUES (?, 'musicbrainz', 'mbid-twinpeaks', datetime('now'))
		`, aid); err != nil {
			t.Fatalf("seeding mb id %s: %v", aid, err)
		}
	}

	if err := ensureArtistLibrariesMembership(db); err != nil {
		t.Fatalf("collapse: %v", err)
	}

	// The chronologically earliest row (a-rfc, January) must survive as
	// canonical; the later row (a-sqlite, February) must be the loser.
	var survivor string
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM artists WHERE name = 'TwinPeaks'`).Scan(&survivor); err != nil {
		t.Fatalf("query survivor: %v", err)
	}
	if survivor != "a-rfc" {
		t.Errorf("canonical = %q, want a-rfc (chronologically earliest after datetime() normalization)", survivor)
	}
}

// TestMigration002_LibraryNFOLockData_Idempotent verifies that the 002
// migration adds the nfo_lock_data column on a fresh DB and is a no-op on
// re-run (goose tracks 002 as applied; ALTER TABLE is not replayed). Re-
// running Migrate must succeed; the column must not be duplicated;
// pre-existing rows must retain their values.
func TestMigration002_LibraryNFOLockData_Idempotent(t *testing.T) {
	db := openMigratedDB(t)

	if _, err := db.Exec(`
		INSERT INTO libraries (id, name, path, type, source, external_id, fs_watch, fs_poll_interval, nfo_lock_data, created_at, updated_at)
		VALUES ('lib-a', 'Existing', '/music', 'regular', 'manual', '', 0, 60, 1, datetime('now'), datetime('now'))
	`); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate: %v", err)
	}

	var lock int
	if err := db.QueryRow(`SELECT nfo_lock_data FROM libraries WHERE id = 'lib-a'`).Scan(&lock); err != nil {
		t.Fatalf("querying back: %v", err)
	}
	if lock != 1 {
		t.Errorf("nfo_lock_data after re-migrate = %d, want 1 (existing value preserved)", lock)
	}

	rows, err := db.Query(`PRAGMA table_info(libraries)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scanning column info: %v", err)
		}
		if name == "nfo_lock_data" {
			count++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating column info: %v", err)
	}
	if count != 1 {
		t.Errorf("nfo_lock_data column count = %d, want 1 (idempotent re-run must not duplicate)", count)
	}
}

// TestMigrate_Pre002Shim_RecoversFromMissingTrackerRow simulates a database
// where libraries.nfo_lock_data was added by the now-retired ensureXColumns
// runtime helper before migration 002 existed: the column is present, but
// goose_db_version has no row for version_id=2. Without the shim, goose.Up
// would re-run 002 and abort startup with "duplicate column name". With the
// shim, Migrate inserts the marker row and returns cleanly.
func TestMigrate_Pre002Shim_RecoversFromMissingTrackerRow(t *testing.T) {
	db := openMigratedDB(t)
	ctx := context.Background()

	// Simulate the pre-002 state by deleting the goose tracker row for
	// version 2. The column itself remains in place, mirroring a DB that
	// got nfo_lock_data via the retired runtime helper instead of goose.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM goose_db_version WHERE version_id = 2`); err != nil {
		t.Fatalf("simulating pre-002 state: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after pre-002 simulation: %v", err)
	}

	var version int
	if err := db.QueryRowContext(ctx,
		`SELECT version_id FROM goose_db_version WHERE version_id = 2`).Scan(&version); err != nil {
		t.Fatalf("expected version_id=2 row after recovery: %v", err)
	}
	if version != 2 {
		t.Errorf("recovered version_id = %d, want 2", version)
	}

	// Capture the marker tstamp so we can prove the next Migrate call did
	// not re-insert (which would refresh the tstamp). Idempotency is carried
	// by the WHERE NOT EXISTS guard, not by a unique constraint, so this is
	// the only assertion that distinguishes "skipped" from "inserted again".
	var firstTstamp string
	if err := db.QueryRowContext(ctx,
		`SELECT tstamp FROM goose_db_version WHERE version_id = 2`).Scan(&firstTstamp); err != nil {
		t.Fatalf("reading initial tstamp: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate call after recovery: %v", err)
	}
	var rowCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goose_db_version WHERE version_id = 2`).Scan(&rowCount); err != nil {
		t.Fatalf("counting version_id=2 rows: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("version_id=2 row count = %d, want 1 (shim must be idempotent)", rowCount)
	}
	var secondTstamp string
	if err := db.QueryRowContext(ctx,
		`SELECT tstamp FROM goose_db_version WHERE version_id = 2`).Scan(&secondTstamp); err != nil {
		t.Fatalf("reading second tstamp: %v", err)
	}
	if secondTstamp != firstTstamp {
		t.Errorf("tstamp changed across re-runs: first=%q second=%q (shim must skip, not re-insert)",
			firstTstamp, secondTstamp)
	}
}

// TestMarkPre002Applied_TrackerExistsColumnAbsent covers the branch where
// goose_db_version is present but libraries.nfo_lock_data has not been
// created yet. The shim must not synthesize a marker row for a migration
// whose target column does not exist; it would mask a genuinely missing 002.
func TestMarkPre002Applied_TrackerExistsColumnAbsent(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()

	// Create only what the shim's pre-checks need: a tracker table (so
	// hasTracker is true) and a libraries table without nfo_lock_data (so
	// hasColumn is false). The schema does not need to match goose's exact
	// shape -- the shim only reads via sqlite_master / PRAGMA.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE goose_db_version (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			version_id INTEGER NOT NULL,
			is_applied INTEGER NOT NULL,
			tstamp TIMESTAMP DEFAULT (datetime('now'))
		)
	`); err != nil {
		t.Fatalf("creating tracker: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE libraries (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("creating libraries: %v", err)
	}

	if err := markPre002Applied(db); err != nil {
		t.Fatalf("markPre002Applied with column absent: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goose_db_version WHERE version_id = 2`).Scan(&n); err != nil {
		t.Fatalf("counting tracker rows: %v", err)
	}
	if n != 0 {
		t.Errorf("tracker rows for version_id=2 = %d, want 0 (shim must not synthesize a marker without the column)", n)
	}
}

// TestMarkPre002Applied_PropagatesErrorOnClosedDB verifies that errors from
// the underlying sqlite_master query propagate as wrapped errors rather than
// silently turning into a "no tracker" skip. A closed handle is the cheapest
// way to force the query to fail without mocks.
func TestMarkPre002Applied_PropagatesErrorOnClosedDB(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	err = markPre002Applied(db)
	if err == nil {
		t.Fatal("markPre002Applied on closed db returned nil; want wrapped error")
	}
	if !strings.Contains(err.Error(), "checking goose_db_version presence") {
		t.Errorf("error = %q, want wrap containing %q", err.Error(), "checking goose_db_version presence")
	}
}

// TestMarkPre002Applied_FreshDBSkips verifies that markPre002Applied is a
// no-op on a database that has no goose_db_version table yet. Fresh-install
// startup must not synthesize a tracker row before goose has had a chance to
// create the table itself.
func TestMarkPre002Applied_FreshDBSkips(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := markPre002Applied(db); err != nil {
		t.Fatalf("markPre002Applied on fresh db: %v", err)
	}

	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='goose_db_version'`).Scan(&n); err != nil {
		t.Fatalf("checking sqlite_master: %v", err)
	}
	if n != 0 {
		t.Errorf("goose_db_version table count after fresh-DB shim = %d, want 0 (shim must not create the tracker)", n)
	}
}
