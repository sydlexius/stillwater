package database

// Migration 029's Down path (drop metadata_changes.producer) is claimed safe
// in the migration's own header, but nothing exercised it until now (#3078
// PR 1 hostile review, F2). This test proves the claim: build a database at
// the pre-029 schema (027, matching what production actually runs today --
// TestMigration029_NoBackfill starts one version later, at 28), migrate up
// through 029 the way a real deploy does, roll back with goose.Down, and
// assert the table survives -- column gone, row count unchanged, pre-029
// columns still readable -- then migrate back Up and confirm producer
// reappears empty (the rollback's own data loss is expected and scoped to
// only what this migration introduced).
//
// Worth having on its own: migration 029's header commits to shipping NO
// index on producer specifically so Down stays a single statement forever.
// SQLite refuses to DROP a COLUMN that an index still covers, so a future
// migration adding one without also updating this Down would break the
// rollback silently -- this test is what would catch that.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestMigration029_DownRoundTrips(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "down029.db")
	ctx := context.Background()

	// Production is currently at 027 (see docs/milestone-protocol.md-adjacent
	// history: 028/029 are the newest two). Build there, not at 028, so this
	// exercises the real upgrade path.
	migrateUpTo(t, dbPath, 27)

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path) VALUES ('a1', 'A', 'A', '/music/A')`); err != nil {
		t.Fatalf("inserting artist: %v", err)
	}
	sources := []string{"manual", "scan", "rule:bio_exists"}
	for i, src := range sources {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
			VALUES (?, 'a1', 'biography', 'old', 'new', ?, '2026-01-01T00:00:00Z')`,
			"pre029-"+string(rune('a'+i)), src); err != nil {
			t.Fatalf("inserting pre-029 row %d: %v", i, err)
		}
	}

	// Full upgrade from 027 to head -- the real path a running install takes.
	if err := Migrate(db); err != nil {
		t.Fatalf("migrating 027 -> head: %v", err)
	}
	if has, err := columnExists(db, "metadata_changes", "producer"); err != nil {
		t.Fatalf("columnExists after upgrade: %v", err)
	} else if !has {
		t.Fatal("producer missing after 027 -> head; migration 029 did not apply")
	}

	// A post-029 row carrying a producer, to confirm the Down loses exactly
	// this kind of data and nothing else.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, producer, created_at)
		VALUES ('post029-a', 'a1', 'origin', '', 'X', 'manual', 'operator', '2026-02-01T00:00:00Z')`); err != nil {
		t.Fatalf("inserting post-029 row: %v", err)
	}

	var before int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metadata_changes`).Scan(&before); err != nil {
		t.Fatalf("counting rows before down: %v", err)
	}

	// --- Down ---
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("setting goose dialect: %v", err)
	}
	// DownTo 28, not a bare Down: a bare Down rolls back whatever migration is
	// newest, so this test asserted 029's rollback only while 029 happened to
	// be the last file in the set. #2897 added 030 and the bare call started
	// rolling that back instead, leaving 029's column in place and failing
	// here. Naming the target version makes the assertion independent of how
	// many migrations land later.
	if err := goose.DownTo(db, "migrations", 28); err != nil {
		t.Fatalf("goose.DownTo(28): %v", err)
	}

	if has, err := columnExists(db, "metadata_changes", "producer"); err != nil {
		t.Fatalf("columnExists after down: %v", err)
	} else if has {
		t.Fatal("producer column still present after goose.Down -- rollback no-opped")
	}

	var after int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metadata_changes`).Scan(&after); err != nil {
		t.Fatalf("table unqueryable after down: %v", err)
	}
	if after != before {
		t.Fatalf("row count moved across down: %d -> %d", before, after)
	}

	// Pre-029 columns must still be readable -- proves the rollback rebuilt
	// the table (SQLite's DROP COLUMN path) without losing any of them.
	rows, err := db.QueryContext(ctx,
		`SELECT id, artist_id, field, old_value, new_value, source, created_at FROM metadata_changes ORDER BY id`)
	if err != nil {
		t.Fatalf("querying pre-029 columns after down: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var a, b, c, d, e, f, g string
		if err := rows.Scan(&a, &b, &c, &d, &e, &f, &g); err != nil {
			t.Fatalf("scanning row after down: %v", err)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating rows after down: %v", err)
	}
	if seen != before {
		t.Fatalf("seen %d rows after down, want %d", seen, before)
	}

	// --- Re-Up: confirm the column comes back clean (empty for every row,
	// since Down destroyed the one non-empty value by design). ---
	if err := goose.Up(db, "migrations"); err != nil {
		t.Fatalf("re-Up after down: %v", err)
	}
	if has, err := columnExists(db, "metadata_changes", "producer"); err != nil {
		t.Fatalf("columnExists after re-up: %v", err)
	} else if !has {
		t.Fatal("producer missing after re-up")
	}
	var nonEmptyAfterReUp int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM metadata_changes WHERE producer != ''`).Scan(&nonEmptyAfterReUp); err != nil {
		t.Fatalf("counting non-empty producers after re-up: %v", err)
	}
	if nonEmptyAfterReUp != 0 {
		t.Fatalf("nonEmptyAfterReUp = %d, want 0 (the operator-authored row's producer is lost by the down/up cycle, by design)",
			nonEmptyAfterReUp)
	}

	var finalCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metadata_changes`).Scan(&finalCount); err != nil {
		t.Fatalf("counting rows after re-up: %v", err)
	}
	if finalCount != before {
		t.Fatalf("row count moved across down+up: %d -> %d", before, finalCount)
	}
}
