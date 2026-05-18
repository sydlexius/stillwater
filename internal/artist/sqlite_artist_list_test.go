package artist

import (
	"context"
	"database/sql"
	"sort"
	"testing"
)

// seedListFixtures inserts two libraries and a small artist set with
// per-library membership, so the ListRefsByLibrary / ListByIDs /
// ListByLibrary tests below can verify the membership filter,
// path-non-empty filter, and IN-list ordering semantics.
func seedListFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	rows := []string{
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-a', 'lib-a', '/music/a', 'regular', 'filesystem', datetime('now'), datetime('now'))`,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-b', 'lib-b', '/music/b', 'regular', 'filesystem', datetime('now'), datetime('now'))`,
		// artist-1 in lib-a, with path
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('a-1', 'Alpha', 'Alpha', '/music/a/Alpha', datetime('now'), datetime('now'))`,
		// artist-2 in lib-a, with path
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('a-2', 'Bravo', 'Bravo', '/music/a/Bravo', datetime('now'), datetime('now'))`,
		// artist-3 in lib-a, empty path -- must be filtered out by ListRefsByLibrary
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('a-3', 'NoPath', 'NoPath', '', datetime('now'), datetime('now'))`,
		// artist-4 in lib-b only
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('a-4', 'Charlie', 'Charlie', '/music/b/Charlie', datetime('now'), datetime('now'))`,
		// artist-5 with no library membership at all
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('a-5', 'Orphan', 'Orphan', '/music/orphan/Orphan', datetime('now'), datetime('now'))`,
		`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		 VALUES ('a-1', 'lib-a', 'filesystem', datetime('now'))`,
		`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		 VALUES ('a-2', 'lib-a', 'filesystem', datetime('now'))`,
		`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		 VALUES ('a-3', 'lib-a', 'filesystem', datetime('now'))`,
		`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		 VALUES ('a-4', 'lib-b', 'filesystem', datetime('now'))`,
	}
	for _, q := range rows {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seeding fixture %q: %v", q, err)
		}
	}
}

// TestSqliteListRefsByLibrary pins three contracts of the lightweight
// ListRefsByLibrary helper added in #1409: membership scope, path filter,
// and shape of the returned ArtistRef slice.
func TestSqliteListRefsByLibrary(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedListFixtures(t, db)
	repo := &sqliteArtistRepo{db: db}
	ctx := context.Background()

	refs, err := repo.ListRefsByLibrary(ctx, "lib-a")
	if err != nil {
		t.Fatalf("ListRefsByLibrary(lib-a): %v", err)
	}
	got := make(map[string]ArtistRef, len(refs))
	for _, r := range refs {
		got[r.ID] = r
	}
	// a-1 and a-2 must be returned. a-3 has an empty path so the helper
	// must filter it out (the scanner's removal sweep relies on this).
	// a-4 is in lib-b, a-5 has no membership; neither should appear.
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 refs for lib-a; got %d (%v)", len(got), refs)
	}
	if _, ok := got["a-1"]; !ok {
		t.Errorf("a-1 missing from lib-a refs")
	}
	if got["a-1"].Name != "Alpha" || got["a-1"].Path != "/music/a/Alpha" {
		t.Errorf("a-1 ref shape wrong: %+v", got["a-1"])
	}
	if _, ok := got["a-2"]; !ok {
		t.Errorf("a-2 missing from lib-a refs")
	}
	if _, ok := got["a-3"]; ok {
		t.Errorf("a-3 has empty path; must be excluded but appeared in result")
	}

	empty, err := repo.ListRefsByLibrary(ctx, "lib-empty")
	if err != nil {
		t.Fatalf("ListRefsByLibrary(lib-empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected empty refs for nonexistent library; got %v", empty)
	}
}

// TestSqliteListByIDs pins the IN-clause batch loader contract: empty input
// is a no-op (no DB hit, nil slice), the returned rows match exactly the
// requested IDs, and missing IDs are silently skipped (not an error).
func TestSqliteListByIDs(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedListFixtures(t, db)
	repo := &sqliteArtistRepo{db: db}
	ctx := context.Background()

	// Empty input: must short-circuit without touching the DB.
	zero, err := repo.ListByIDs(ctx, nil)
	if err != nil {
		t.Fatalf("ListByIDs(nil): %v", err)
	}
	if zero != nil {
		t.Errorf("expected nil slice for empty input; got %v", zero)
	}

	// Mixed-existence input: a-1 + a-4 exist, no-such-id does not.
	// The function must return the two that exist without erroring on
	// the missing one.
	rows, err := repo.ListByIDs(ctx, []string{"a-1", "a-4", "no-such-id"})
	if err != nil {
		t.Fatalf("ListByIDs(mixed): %v", err)
	}
	ids := make([]string, len(rows))
	for i, a := range rows {
		ids[i] = a.ID
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "a-1" || ids[1] != "a-4" {
		t.Errorf("expected [a-1 a-4]; got %v", ids)
	}
}

// TestSqliteListByLibrary pins the bulk loader contract used by the scanner
// pre-load (#1411): every artist whose membership includes the library is
// returned, regardless of path-non-empty (the scanner needs the empty-path
// entries too so it can decide whether to populate them on first scan).
func TestSqliteListByLibrary(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedListFixtures(t, db)
	repo := &sqliteArtistRepo{db: db}
	ctx := context.Background()

	rows, err := repo.ListByLibrary(ctx, "lib-a")
	if err != nil {
		t.Fatalf("ListByLibrary(lib-a): %v", err)
	}
	ids := make([]string, len(rows))
	for i, a := range rows {
		ids[i] = a.ID
	}
	sort.Strings(ids)
	// All three lib-a artists must be returned, including a-3 (empty path).
	// a-4 is in lib-b only; a-5 has no membership.
	if len(ids) != 3 || ids[0] != "a-1" || ids[1] != "a-2" || ids[2] != "a-3" {
		t.Errorf("expected [a-1 a-2 a-3]; got %v", ids)
	}

	empty, err := repo.ListByLibrary(ctx, "lib-empty")
	if err != nil {
		t.Fatalf("ListByLibrary(lib-empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected empty slice for nonexistent library; got %v", empty)
	}
}
