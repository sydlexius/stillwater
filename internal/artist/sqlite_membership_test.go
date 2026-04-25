package artist

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
)

// openTestDB sets up an in-memory SQLite database with the production
// migration applied + foreign keys enabled, matching the convention used in
// internal/database/migrate_test.go. Tests share this helper so they get the
// same schema as production startup, including the artist_libraries table
// .
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("foreign keys: %v", err)
	}
	return db
}

// seedMembershipFixtures inserts the libraries and artists referenced by
// every membership test. Returns the set of seeded library IDs for
// convenience. The artist_libraries table is intentionally left empty so
// each test starts with a clean membership ledger.
func seedMembershipFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	// Two libraries: one filesystem (no connection_id) and one Emby
	// connection-library. The 'source' column on libraries is informational;
	// the membership 'source' is provided per-call by the caller.
	_, err := db.ExecContext(ctx, `
		INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		VALUES ('lib-fs', 'lib-fs', '/music', 'regular', 'filesystem',
			datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("seeding fs library: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		VALUES ('conn-emby', 'Emby', 'emby', 'http://t', 'k', 1, 'ok',
			datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("seeding emby connection: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO libraries (id, name, path, type, source, connection_id, created_at, updated_at)
		VALUES ('lib-emby', 'lib-emby', '/music', 'regular', 'import', 'conn-emby',
			datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("seeding emby library: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		VALUES ('a-1', 'TestArtist', 'TestArtist', '/music/TestArtist',
			datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
}

// TestMembershipAddIsIdempotent verifies Add inserts a row on first call
// and is a no-op on repeat.
func TestMembershipAddIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	seedMembershipFixtures(t, db)
	repo := newSQLiteMembershipRepo(db)
	ctx := context.Background()

	if err := repo.Add(ctx, "a-1", "lib-fs", "filesystem"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := repo.Add(ctx, "a-1", "lib-fs", "filesystem"); err != nil {
		t.Fatalf("second add: %v", err)
	}

	got, err := repo.CountForArtist(ctx, "a-1")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 1 {
		t.Errorf("count = %d, want 1 (idempotent add must not duplicate)", got)
	}
}

// TestMembershipMultipleLibraries verifies an artist can be a member of
// many libraries with different sources and that ListForArtist returns
// them all.
func TestMembershipMultipleLibraries(t *testing.T) {
	db := openTestDB(t)
	seedMembershipFixtures(t, db)
	repo := newSQLiteMembershipRepo(db)
	ctx := context.Background()

	if err := repo.Add(ctx, "a-1", "lib-fs", "filesystem"); err != nil {
		t.Fatalf("add fs: %v", err)
	}
	if err := repo.Add(ctx, "a-1", "lib-emby", "emby"); err != nil {
		t.Fatalf("add emby: %v", err)
	}

	memberships, err := repo.ListForArtist(ctx, "a-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(memberships) != 2 {
		t.Fatalf("memberships = %d, want 2", len(memberships))
	}

	bySource := map[string]string{}
	for _, m := range memberships {
		bySource[m.Source] = m.LibraryID
	}
	if bySource["filesystem"] != "lib-fs" {
		t.Errorf("filesystem -> %q, want lib-fs", bySource["filesystem"])
	}
	if bySource["emby"] != "lib-emby" {
		t.Errorf("emby -> %q, want lib-emby", bySource["emby"])
	}
}

// TestMembershipRemove verifies Remove deletes only the targeted pair and
// is a no-op for nonexistent pairs.
func TestMembershipRemove(t *testing.T) {
	db := openTestDB(t)
	seedMembershipFixtures(t, db)
	repo := newSQLiteMembershipRepo(db)
	ctx := context.Background()

	_ = repo.Add(ctx, "a-1", "lib-fs", "filesystem")
	_ = repo.Add(ctx, "a-1", "lib-emby", "emby")

	if err := repo.Remove(ctx, "a-1", "lib-emby"); err != nil {
		t.Fatalf("remove emby: %v", err)
	}
	got, _ := repo.CountForArtist(ctx, "a-1")
	if got != 1 {
		t.Errorf("after remove count = %d, want 1", got)
	}

	// Nonexistent pair: no error.
	if err := repo.Remove(ctx, "a-1", "lib-emby"); err != nil {
		t.Errorf("remove nonexistent: %v", err)
	}
}

// TestMembershipCascadeOnArtistDelete verifies that deleting the parent
// artist cascades to artist_libraries via the FK ON DELETE CASCADE.
func TestMembershipCascadeOnArtistDelete(t *testing.T) {
	db := openTestDB(t)
	seedMembershipFixtures(t, db)
	repo := newSQLiteMembershipRepo(db)
	ctx := context.Background()

	_ = repo.Add(ctx, "a-1", "lib-fs", "filesystem")

	if _, err := db.ExecContext(ctx, `DELETE FROM artists WHERE id = 'a-1'`); err != nil {
		t.Fatalf("delete artist: %v", err)
	}
	got, err := repo.CountForArtist(ctx, "a-1")
	if err != nil {
		t.Fatalf("count after cascade: %v", err)
	}
	if got != 0 {
		t.Errorf("post-cascade count = %d, want 0", got)
	}
}

// TestArtistGetByName verifies the unscoped name lookup is case-insensitive
// and returns nil when no match exists.
func TestArtistGetByName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		VALUES ('a-veridia', 'VERIDIA', 'VERIDIA', '/music/VERIDIA',
			datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	repo := newSQLiteArtistRepo(db)
	a, err := repo.GetByName(ctx, "veridia")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if a == nil {
		t.Fatal("GetByName returned nil for case-insensitive match")
	}
	if a.ID != "a-veridia" {
		t.Errorf("id = %q, want a-veridia", a.ID)
	}

	none, err := repo.GetByName(ctx, "Nonexistent")
	if err != nil {
		t.Fatalf("GetByName miss: %v", err)
	}
	if none != nil {
		t.Errorf("expected nil for unknown name, got %+v", none)
	}
}

// TestArtistFindByMBIDOrNameUnscoped covers the dedupe priority: MBID
// match wins when present; falls through to case-insensitive name
// otherwise.
func TestArtistFindByMBIDOrNameUnscoped(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		VALUES ('a-byname', 'NameOnly', 'NameOnly', '/music/NameOnly',
			datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("seed name-only: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		VALUES ('a-bymbid', 'MBIDArtist', 'MBIDArtist', '/music/MBIDArtist',
			datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("seed mbid: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
		VALUES ('a-bymbid', 'musicbrainz', 'mb-1', datetime('now'))
	`)
	if err != nil {
		t.Fatalf("seed mbid provider id: %v", err)
	}

	repo := newSQLiteArtistRepo(db)

	a, err := repo.FindByMBIDOrNameUnscoped(ctx, "mb-1", "")
	if err != nil || a == nil {
		t.Fatalf("mbid path: a=%v err=%v", a, err)
	}
	if a.ID != "a-bymbid" {
		t.Errorf("mbid match id = %q, want a-bymbid", a.ID)
	}

	a, err = repo.FindByMBIDOrNameUnscoped(ctx, "", "nameonly")
	if err != nil || a == nil {
		t.Fatalf("name path: a=%v err=%v", a, err)
	}
	if a.ID != "a-byname" {
		t.Errorf("name match id = %q, want a-byname", a.ID)
	}

	// MBID present but unmatched: must fall through to name. The MBID
	// belongs to a different artist, so the name match returns the
	// name-only row, not the MBID-bearing row.
	a, err = repo.FindByMBIDOrNameUnscoped(ctx, "mb-missing", "nameonly")
	if err != nil || a == nil {
		t.Fatalf("fallback path: a=%v err=%v", a, err)
	}
	if a.ID != "a-byname" {
		t.Errorf("fallback id = %q, want a-byname", a.ID)
	}
}
