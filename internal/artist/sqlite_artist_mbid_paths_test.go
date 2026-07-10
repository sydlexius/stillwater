package artist

import (
	"context"
	"database/sql"
	"testing"
)

// seedMBIDPathFixtures inserts artists in each relevant state for
// ListMBIDPaths: both MBID + path set (returned), MBID but no path, path but
// no MBID row, an MBID row whose provider_id is empty, and a non-musicbrainz
// provider row (must be ignored). Only the first should survive the filter.
func seedMBIDPathFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	rows := []string{
		// m-1: MBID + path -> the one row ListMBIDPaths must return.
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('m-1', 'Alpha', 'Alpha', '/music/Alpha', datetime('now'), datetime('now'))`,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
		 VALUES ('m-1', 'musicbrainz', 'mbid-alpha')`,
		// m-2: MBID present but empty path -> excluded by the a.path != '' filter.
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('m-2', 'Bravo', 'Bravo', '', datetime('now'), datetime('now'))`,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
		 VALUES ('m-2', 'musicbrainz', 'mbid-bravo')`,
		// m-3: path present but no musicbrainz provider row -> excluded by the JOIN.
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('m-3', 'Charlie', 'Charlie', '/music/Charlie', datetime('now'), datetime('now'))`,
		// m-4: musicbrainz row present but provider_id is empty -> excluded by
		// the p.provider_id != '' filter (a stub row from a failed lookup).
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('m-4', 'Delta', 'Delta', '/music/Delta', datetime('now'), datetime('now'))`,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
		 VALUES ('m-4', 'musicbrainz', '')`,
		// m-5: path present + a NON-musicbrainz provider id -> excluded by the
		// provider='musicbrainz' filter (must not leak a discogs id as an MBID).
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('m-5', 'Echo', 'Echo', '/music/Echo', datetime('now'), datetime('now'))`,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
		 VALUES ('m-5', 'discogs', 'discogs-echo')`,
	}
	for _, q := range rows {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seeding fixture %q: %v", q, err)
		}
	}
}

// TestSqliteListMBIDPaths pins the ListMBIDPaths contract used by Lidarr
// path-mapping inference (#2329): only artists with BOTH a non-empty
// musicbrainz provider id AND a non-empty path are returned, with the correct
// (MBID, path) pairing. It exercises the repo directly.
func TestSqliteListMBIDPaths(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedMBIDPathFixtures(t, db)
	repo := &sqliteArtistRepo{db: db}
	ctx := context.Background()

	got, err := repo.ListMBIDPaths(ctx)
	if err != nil {
		t.Fatalf("ListMBIDPaths: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 row (m-1); got %d (%+v)", len(got), got)
	}
	if got[0].MBID != "mbid-alpha" || got[0].Path != "/music/Alpha" {
		t.Errorf("row = %+v, want {MBID:mbid-alpha Path:/music/Alpha}", got[0])
	}
}

// TestSqliteListMBIDPaths_Empty confirms a database with no qualifying rows
// yields an empty (nil) slice and no error, not a spurious row.
func TestSqliteListMBIDPaths_Empty(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	repo := &sqliteArtistRepo{db: db}

	got, err := repo.ListMBIDPaths(context.Background())
	if err != nil {
		t.Fatalf("ListMBIDPaths: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no rows on an empty DB; got %+v", got)
	}
}

// TestServiceListMBIDPaths covers the service-level wrapper, asserting it
// passes the repository result through unchanged.
func TestServiceListMBIDPaths(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedMBIDPathFixtures(t, db)
	svc := NewService(db)

	got, err := svc.ListMBIDPaths(context.Background())
	if err != nil {
		t.Fatalf("ListMBIDPaths: %v", err)
	}
	if len(got) != 1 || got[0].MBID != "mbid-alpha" || got[0].Path != "/music/Alpha" {
		t.Fatalf("service result = %+v, want one {mbid-alpha,/music/Alpha}", got)
	}
}
