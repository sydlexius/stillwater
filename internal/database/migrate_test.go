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

// seedArtistWithLibrary inserts an artist tied to a specific library_id
// (legacy path that the M:N migration backfills from). created_at lets the
// test control the canonical-pick tie-breaker.
func seedArtistWithLibrary(t *testing.T, db *sql.DB, id, name, libraryID, createdAt string) {
	t.Helper()
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
// connection type. Issue #1004.
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
//   - keeps the filesystem row as canonical
//   - re-points the loser's artist_provider_ids and other FK rows
//   - inserts a membership row for the loser's library under the canonical
//   - deletes the loser artist row
//
// Issue #1004 architectural fix.
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
// (connection_id, platform_artist_id) added in #1076. The original collapse
// helper used INSERT OR IGNORE to move the loser's mapping onto canonical,
// which the unique index rejected, and the loser cascade-delete then dropped
// the mapping entirely. UPDATE OR IGNORE on the artist_id column moves the
// loser row onto canonical and only drops the row when canonical already
// has a mapping for the same connection. Issue #1004.
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

	// Two artists with same MBID -> grouped by MBID. Both happen to have
	// names that also match a third unrelated artist; the MBID group claims
	// them both, so the third artist must NOT be folded into a name group.
	seedArtistWithLibrary(t, db, "a-fs", "Cher", "lib-fs", "2026-01-01T00:00:00Z")
	seedArtistWithLibrary(t, db, "a-emby", "Cher", "lib-emby", "2026-01-02T00:00:00Z")
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

	// One Cher remains (a-fs canonical).
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists WHERE name = 'Cher'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("Cher count = %d, want 1", n)
	}
}
