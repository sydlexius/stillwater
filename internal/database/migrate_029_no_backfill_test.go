package database

// Migration 029 (metadata_changes.producer, issue #3078 PR 1) must add the
// column with NO backfill: every pre-existing row keeps producer = '', which
// reads as "unknown" and must stay unknown forever (#3078's central
// constraint). This test builds a database at the pre-029 schema, inserts
// rows the way v1.6.2 wrote them, migrates to 029, and asserts the migration
// wrote zero non-empty producers and changed no row count.
//
// This fails the instant anyone adds an UPDATE (or a DEFAULT other than '')
// to the migration -- see the mutation proof in the #3078 PR 1 report.

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigration029_NoBackfill(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pre029.db")
	ctx := context.Background()

	// 1. A database at the schema that shipped BEFORE this change.
	migrateUpTo(t, dbPath, 28)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer db.Close()

	// Sanity: producer must NOT exist yet, or this test proves nothing.
	if has, err := columnExists(db, "metadata_changes", "producer"); err != nil {
		t.Fatalf("checking column: %v", err)
	} else if has {
		t.Fatal("metadata_changes.producer already exists at v28; this test cannot " +
			"prove the migration adds no backfill")
	}

	// 2. Populate it the way a v1.6.2 build did: rows with only the pre-029
	//    columns, a mix of sources.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES ('a1', 'Pre029 Artist', '/music/Pre029 Artist')`); err != nil {
		t.Fatalf("inserting artist: %v", err)
	}
	const n = 5
	sources := []string{"manual", "manual", "scan", "rule:bio_exists", "revert"}
	for i := 0; i < n; i++ {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO metadata_changes
				(id, artist_id, field, old_value, new_value, source, created_at)
			VALUES (?, 'a1', 'biography', 'old', 'new', ?, '2026-01-01T00:00:00Z')`,
			"mc-"+string(rune('a'+i)), sources[i]); err != nil {
			t.Fatalf("inserting pre-existing metadata_changes row %d: %v", i, err)
		}
	}

	var preCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a1'`).Scan(&preCount); err != nil {
		t.Fatalf("counting pre-migration rows: %v", err)
	}
	if preCount != n {
		t.Fatalf("preCount = %d, want %d", preCount, n)
	}

	// 3. Upgrade: apply 029 (and the rest of Migrate's startup work) on top of
	//    the populated database.
	if err := Migrate(db); err != nil {
		t.Fatalf("migrating a POPULATED database to 029: %v", err)
	}

	// 4. Row count is unchanged.
	var postCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a1'`).Scan(&postCount); err != nil {
		t.Fatalf("counting post-migration rows: %v", err)
	}
	if postCount != preCount {
		t.Fatalf("postCount = %d, want %d (row count changed)", postCount, preCount)
	}

	// 5. No row was backfilled with a non-empty producer. This is the
	//    assertion the migration's no-backfill guarantee lives or dies on.
	var nonEmpty int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM metadata_changes WHERE artist_id = 'a1' AND producer != ''`).Scan(&nonEmpty); err != nil {
		t.Fatalf("counting non-empty producers: %v", err)
	}
	if nonEmpty != 0 {
		t.Fatalf("nonEmpty producer count = %d, want 0 -- migration 029 backfilled a value", nonEmpty)
	}

	// 6. Every row reads back producer = '' (never NULL) regardless of what
	//    source it carried before the migration.
	rows, err := db.QueryContext(ctx,
		`SELECT source, producer FROM metadata_changes WHERE artist_id = 'a1' ORDER BY id`)
	if err != nil {
		t.Fatalf("selecting migrated rows: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var source, producer string // a NULL producer errors here
		if err := rows.Scan(&source, &producer); err != nil {
			t.Fatalf("scanning migrated row (a NULL producer would fail here): %v", err)
		}
		if producer != "" {
			t.Errorf("source=%q: producer = %q, want '' (no backfill)", source, producer)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating migrated rows: %v", err)
	}
	if seen != n {
		t.Fatalf("seen %d rows, want %d", seen, n)
	}
}
