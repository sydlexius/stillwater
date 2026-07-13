package database

// Migration 023 (artist_images.content_hash) has to be safe on a REAL,
// already-populated library, not just on an empty database created fresh by
// the test suite. Every other migration test starts from nothing, which would
// happily pass even if 023 destroyed or NULL-ed existing rows.
//
// This test builds a database at the PRE-023 schema, fills it with
// artist_images rows written the way the old code wrote them, and only then
// applies 023 -- which is the exact path every existing install takes on
// upgrade.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// migrateUpTo brings a database to a specific schema version, so the state
// before the migration under test can be reconstructed faithfully.
func migrateUpTo(t *testing.T, dbPath string, version int64) {
	t.Helper()
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("setting goose dialect: %v", err)
	}
	if err := goose.UpTo(db, "migrations", version); err != nil {
		t.Fatalf("migrating up to %d: %v", version, err)
	}
}

func TestMigration023_SafeOnPopulatedDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "populated.db")
	ctx := context.Background()

	// 1. A database at the schema that shipped BEFORE this change.
	migrateUpTo(t, dbPath, 22)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer db.Close()

	// Sanity: content_hash must NOT exist yet, or the test proves nothing.
	if has, err := columnExists(db, "artist_images", "content_hash"); err != nil {
		t.Fatalf("checking column: %v", err)
	} else if has {
		t.Fatal("artist_images.content_hash already exists at v22; this test cannot " +
			"prove the migration is safe on a pre-023 database")
	}

	// 2. Populate it the way the OLD code did: rows with a phash and real
	//    provenance, and no notion of a content hash.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES ('a1', 'Populated Artist', '/music/Populated Artist')`); err != nil {
		t.Fatalf("inserting artist: %v", err)
	}
	for slot := 0; slot < 3; slot++ {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO artist_images
				(id, artist_id, image_type, slot_index, exists_flag, phash, source, file_format, last_written_at)
			VALUES (?, 'a1', 'fanart', ?, 1, ?, 'fanarttv', 'jpeg', '2026-01-01T00:00:00Z')`,
			"img-"+string(rune('a'+slot)), slot, "00000000000000ff"); err != nil {
			t.Fatalf("inserting pre-existing image row %d: %v", slot, err)
		}
	}

	// 3. Upgrade: apply 023 (and the rest of Migrate's startup work) on top of
	//    the populated database.
	if err := Migrate(db); err != nil {
		t.Fatalf("migrating a POPULATED database to 023: %v", err)
	}

	// 4. The pre-existing rows must survive intact.
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_images WHERE artist_id = 'a1'`).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 3 {
		t.Fatalf("%d artist_images rows survived the migration, want 3", count)
	}

	// 5. content_hash must read back as '' (the NOT NULL default), never NULL.
	//    Scanning into a plain string is the point: every existing reader does
	//    exactly this, and a NULL would make them all fail at runtime.
	rows, err := db.QueryContext(ctx,
		`SELECT slot_index, phash, content_hash, source, file_format
		 FROM artist_images WHERE artist_id = 'a1' ORDER BY slot_index`)
	if err != nil {
		t.Fatalf("selecting migrated rows: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var slot int
		var phash, contentHash, source, format string // a NULL content_hash errors here
		if err := rows.Scan(&slot, &phash, &contentHash, &source, &format); err != nil {
			t.Fatalf("scanning migrated row (a NULL content_hash would fail here): %v", err)
		}
		if contentHash != "" {
			t.Errorf("slot %d: content_hash = %q, want '' (unhashed)", slot, contentHash)
		}
		// The migration must not disturb data that was already there.
		if phash != "00000000000000ff" {
			t.Errorf("slot %d: phash = %q; the migration altered pre-existing data", slot, phash)
		}
		if source != "fanarttv" || format != "jpeg" {
			t.Errorf("slot %d: provenance lost (source=%q format=%q)", slot, source, format)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating migrated rows: %v", err)
	}
	if seen != 3 {
		t.Fatalf("read back %d rows, want 3", seen)
	}

	// 6. The lazy backfill must be able to populate those pre-existing rows.
	if _, err := db.ExecContext(ctx,
		`UPDATE artist_images SET content_hash = 'deadbeef' WHERE artist_id = 'a1' AND slot_index = 1`); err != nil {
		t.Fatalf("backfilling content_hash on a pre-existing row: %v", err)
	}
	var got string
	if err := db.QueryRowContext(ctx,
		`SELECT content_hash FROM artist_images WHERE artist_id = 'a1' AND slot_index = 1`).Scan(&got); err != nil {
		t.Fatalf("reading back the backfilled hash: %v", err)
	}
	if got != "deadbeef" {
		t.Errorf("backfilled content_hash = %q, want %q", got, "deadbeef")
	}

	// 7. Re-running migrations must be a no-op, not a second ALTER TABLE.
	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the upgraded database: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_images WHERE artist_id = 'a1'`).Scan(&count); err != nil {
		t.Fatalf("counting rows after re-run: %v", err)
	}
	if count != 3 {
		t.Errorf("row count changed to %d on migration re-run; 023 is not idempotent", count)
	}

	// 8. The lookup index must exist.
	var idxName string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_artist_images_content_hash'`).
		Scan(&idxName); err != nil {
		t.Fatalf("idx_artist_images_content_hash was not created: %v", err)
	}
}
