package database

// Migration 027 clears every stored image dimension and every last_scanned_at
// stamp, so that rows describing a REPLACED image stop being read as truth and
// the next scan re-measures the files (issue #2713).
//
// It gets a populated-database test because it is the only unconditional,
// library-wide, two-table UPDATE in the migration set, and because the second
// statement is the one that is easy to lose: without the last_scanned_at clear
// the scanner's mtime fast path carries the zeros forward forever and the
// migration silently REMOVES geometry rather than refreshing it. Adversarial
// review flagged that nothing asserted the second statement ran at all.
//
// The test builds a database at the PRE-027 schema, seeds it the way a real
// library looks on upgrade day, and only then applies 027.

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigration027_ClearsGeometryAndScanStamps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "populated.db")
	ctx := context.Background()

	// 1. A database at the schema that shipped BEFORE this change.
	migrateUpTo(t, dbPath, 26)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer db.Close()

	// 2. Seed it the way a real library looks: artists carrying scan stamps,
	//    image rows carrying geometry, and -- deliberately -- one row that
	//    already reads zero, so the migration's WHERE clause is exercised
	//    rather than assumed to match everything.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, type, path, last_scanned_at)
		VALUES ('a-stamped', 'Stamped', 'Stamped', 'group', '/music/Stamped', '2026-01-01T00:00:00Z'),
		       ('a-never',   'Never',   'Never',   'group', '/music/Never',   NULL)`); err != nil {
		t.Fatalf("seeding artists: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, width, height)
		VALUES ('i-thumb',  'a-stamped', 'thumb',  0, 1, 1920, 1080),
		       ('i-fanart', 'a-stamped', 'fanart', 0, 1,  800,  600),
		       ('i-zero',   'a-never',   'thumb',  0, 1,    0,    0)`); err != nil {
		t.Fatalf("seeding images: %v", err)
	}

	// Sanity: the pre-state must actually hold the values the migration is
	// supposed to clear, or the assertions below prove nothing.
	var nonZero, stamped int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_images WHERE width > 0 OR height > 0`).Scan(&nonZero); err != nil {
		t.Fatalf("counting geometry: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE last_scanned_at IS NOT NULL`).Scan(&stamped); err != nil {
		t.Fatalf("counting stamps: %v", err)
	}
	if nonZero != 2 || stamped != 1 {
		t.Fatalf("precondition: seeded %d geometry rows and %d stamps, want 2 and 1", nonZero, stamped)
	}

	// 3. Apply 027.
	if err := Migrate(db); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	// 4a. Every stored dimension is zero.
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_images WHERE width > 0 OR height > 0`).Scan(&nonZero); err != nil {
		t.Fatalf("counting geometry after: %v", err)
	}
	if nonZero != 0 {
		t.Errorf("%d image row(s) still carry geometry; every stored dimension must be cleared", nonZero)
	}

	// 4b. Every scan stamp is NULL. This is the assertion that was missing:
	//     dropping the second statement leaves the rows zeroed but the fast
	//     path still engaged, so they never refill.
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE last_scanned_at IS NOT NULL`).Scan(&stamped); err != nil {
		t.Fatalf("counting stamps after: %v", err)
	}
	if stamped != 0 {
		t.Errorf("%d artist(s) still carry a last_scanned_at stamp; the scanner's fast path would "+
			"carry the zeroed geometry forward and it would never be re-measured", stamped)
	}

	// 4c. The rows themselves survive. A migration that fixed the numbers by
	//     deleting the registry would satisfy 4a and be catastrophic.
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist_images`).Scan(&rows); err != nil {
		t.Fatalf("counting rows after: %v", err)
	}
	if rows != 3 {
		t.Errorf("artist_images holds %d rows after the migration, want 3 -- rows must be updated, never removed", rows)
	}

	// 4d. Nothing else on the row was touched. exists_flag in particular
	//     decides whether the UI shows artwork at all.
	var existsFlag int
	if err := db.QueryRowContext(ctx,
		`SELECT exists_flag FROM artist_images WHERE id = 'i-thumb'`).Scan(&existsFlag); err != nil {
		t.Fatalf("reading exists_flag: %v", err)
	}
	if existsFlag != 1 {
		t.Errorf("exists_flag = %d, want 1 -- the migration must clear geometry only", existsFlag)
	}
}
