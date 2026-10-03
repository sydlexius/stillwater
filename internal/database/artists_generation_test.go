package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestOpen_PoolIsCappedAtOneConnection pins the cap the artists write
// generation (artists_generation.go) depends on. The generation bumps when a
// row is WRITTEN, not when its transaction COMMITS. With one connection a
// reader cannot query until the writer is done. With a second, a reader could
// run mid-transaction, see the already-bumped generation next to the
// pre-commit rows, and cache that old count as fresh.
func TestOpen_PoolIsCappedAtOneConnection(t *testing.T) {
	t.Parallel()
	for name, open := range map[string]func(string) (*sql.DB, error){"Open": Open, "OpenRuntime": OpenRuntime} {
		db, err := open(filepath.Join(t.TempDir(), name+".db"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if got := db.Stats().MaxOpenConnections; got != 1 {
			t.Errorf("%s: MaxOpenConnections = %d, want 1 (see the comment on this test before raising it)", name, got)
		}
	}
}

// TestArtistsGeneration_TracksEveryRowWrite pins which SQL moves the artists
// write generation (#2395). Each case is a statement shape some production
// writer really issues; the last two are the controls that keep the signal
// from firing on everything.
//
// Not parallel: the generation is process-wide, so a concurrent test writing
// artists rows would move it underneath the "must not move" cases.
func TestArtistsGeneration_TracksEveryRowWrite(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "gen.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	insert := `INSERT INTO artists (id, name, sort_name, path, health_score, created_at, updated_at)
	           VALUES (?, 'A', 'A', ?, 50, datetime('now'), datetime('now'))`
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	// moved runs fn and reports whether the generation changed across it.
	moved := func(fn func()) bool {
		t.Helper()
		before := ArtistsGeneration()
		fn()
		return ArtistsGeneration() != before
	}

	if !moved(func() { exec(insert, "a1", "/m/a1") }) {
		t.Error("INSERT INTO artists did not move the generation")
	}
	if !moved(func() { exec(`UPDATE artists SET health_score = 100 WHERE id = 'a1'`) }) {
		t.Error("UPDATE artists did not move the generation")
	}
	if !moved(func() { exec(`DELETE FROM artists WHERE id = 'a1'`) }) {
		t.Error("DELETE FROM artists WHERE ... did not move the generation")
	}

	// A write inside a transaction (library removal and artist merge both
	// delete artists this way) must be seen by the time Commit returns.
	if !moved(func() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, insert, "a2", "/m/a2"); err != nil {
			t.Fatalf("tx insert: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}) {
		t.Error("INSERT inside a committed transaction did not move the generation")
	}

	// DELETE with no WHERE: SQLite normally empties the table without
	// visiting rows (the "truncate optimization"), which row-level hooks never
	// see. It turns that shortcut off while a pre-update hook is installed;
	// this case fails if a driver upgrade ever changes that.
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists`).Scan(&rows); err != nil || rows == 0 {
		t.Fatalf("precondition: want at least one artists row to delete, got %d (err %v)", rows, err)
	}
	if !moved(func() { exec(`DELETE FROM artists`) }) {
		t.Error("DELETE FROM artists (no WHERE) did not move the generation")
	}

	// Controls. A write to another table must NOT move it, or every settings
	// save and session touch would throw the cached counts away.
	if moved(func() {
		exec(`INSERT INTO settings (key, value, updated_at) VALUES ('gen.test', '1', datetime('now'))`)
	}) {
		t.Error("a write to the settings table moved the artists generation")
	}
	// An UPDATE that matches no row writes nothing, so it must not move it.
	if moved(func() { exec(`UPDATE artists SET health_score = 1 WHERE id = 'no-such-artist'`) }) {
		t.Error("an UPDATE matching zero rows moved the generation")
	}
}
