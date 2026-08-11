package artist

import (
	"context"
	"database/sql"
	"strings"
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

// TestSqliteListMBIDPaths_QueryError covers the error path: a closed DB makes
// QueryContext fail, and the wrapped error must propagate (org test-quality
// guideline requires error-path coverage).
func TestSqliteListMBIDPaths_QueryError(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	repo := &sqliteArtistRepo{db: db}
	// Close the DB so the query fails on execution. The test-DB cleanup closes
	// idempotently, so a second close is harmless.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_, err := repo.ListMBIDPaths(context.Background())
	if err == nil {
		t.Fatal("expected an error from ListMBIDPaths on a closed DB")
	}
	// queryMBIDPaths is shared with ListMBIDPopulation, so an error must name
	// the caller that actually failed. A hard-coded message would send whoever
	// reads the log to the wrong query.
	if !strings.Contains(err.Error(), "listing artist MBID paths") {
		t.Errorf("error = %q, want it to name the ListMBIDPaths caller", err)
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

// TestSqliteListMBIDPopulation pins the #2810 sweep population: EVERY artist
// with a non-empty musicbrainz provider id, INCLUDING one with no path.
//
// The pathless row (m-2) is the whole point. It is the row ListMBIDPaths
// deliberately drops, and dropping it here would silently remove
// platform-only artists from the re-validation ledger -- leaving ids nobody
// has ever checked reading as clean.
func TestSqliteListMBIDPopulation(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedMBIDPathFixtures(t, db)
	repo := &sqliteArtistRepo{db: db}
	ctx := context.Background()

	// PRECONDITION: the fixture really does contain an artist with an MBID and
	// no path, and really does contain one that ListMBIDPaths keeps. Without
	// this, a fixture that silently lost m-2 would make the assertion below
	// pass vacuously (one row in, one row out).
	var pathless int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists a JOIN artist_provider_ids p ON p.artist_id = a.id
		 WHERE p.provider = 'musicbrainz' AND p.provider_id != '' AND a.path = ''`).Scan(&pathless); err != nil {
		t.Fatalf("precondition query: %v", err)
	}
	if pathless != 1 {
		t.Fatalf("precondition: fixture must hold exactly 1 pathless MBID artist, has %d", pathless)
	}

	got, err := repo.ListMBIDPopulation(ctx)
	if err != nil {
		t.Fatalf("ListMBIDPopulation: %v", err)
	}

	byID := make(map[string]MBIDPath, len(got))
	for _, mp := range got {
		byID[mp.ArtistID] = mp
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 rows (m-1, m-2); got %d (%+v)", len(got), got)
	}

	alpha, ok := byID["m-1"]
	if !ok {
		t.Fatal("m-1 (MBID + path) missing from the population")
	}
	if alpha.MBID != "mbid-alpha" || alpha.Path != "/music/Alpha" {
		t.Errorf("m-1 = %+v, want {m-1 mbid-alpha /music/Alpha}", alpha)
	}

	bravo, ok := byID["m-2"]
	if !ok {
		t.Fatal("m-2 (MBID, NO path) missing: a pathless artist must stay in the sweep population")
	}
	if bravo.MBID != "mbid-bravo" || bravo.Path != "" {
		t.Errorf("m-2 = %+v, want {m-2 mbid-bravo <empty path>}", bravo)
	}

	// The three exclusions still hold: no musicbrainz row (m-3), an empty
	// provider_id (m-4), and a non-musicbrainz provider (m-5) are all out.
	for _, id := range []string{"m-3", "m-4", "m-5"} {
		if mp, present := byID[id]; present {
			t.Errorf("%s must be excluded from the population, got %+v", id, mp)
		}
	}
}

// TestSqliteListMBIDPaths_CarriesArtistID pins the field ListMBIDPaths' own
// caller does not use but the ledger upsert requires: the artist id is
// populated on both producers, so a consumer never needs a second lookup that
// a duplicate MBID would make ambiguous.
func TestSqliteListMBIDPaths_CarriesArtistID(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedMBIDPathFixtures(t, db)
	repo := &sqliteArtistRepo{db: db}

	got, err := repo.ListMBIDPaths(context.Background())
	if err != nil {
		t.Fatalf("ListMBIDPaths: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("precondition: expected 1 row, got %d", len(got))
	}
	if got[0].ArtistID != "m-1" {
		t.Errorf("ArtistID = %q, want %q", got[0].ArtistID, "m-1")
	}
}

// TestSqliteListMBIDPopulation_Ordered asserts the documented ordering, which
// is what makes paging the population stable across calls.
func TestSqliteListMBIDPopulation_Ordered(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedMBIDPathFixtures(t, db)
	repo := &sqliteArtistRepo{db: db}

	got, err := repo.ListMBIDPopulation(context.Background())
	if err != nil {
		t.Fatalf("ListMBIDPopulation: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("precondition: need at least 2 rows to test ordering, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ArtistID >= got[i].ArtistID {
			t.Fatalf("rows not ordered by artist id: %q then %q", got[i-1].ArtistID, got[i].ArtistID)
		}
	}
}

// TestSqliteListMBIDPopulation_QueryError covers the error path.
func TestSqliteListMBIDPopulation_QueryError(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	repo := &sqliteArtistRepo{db: db}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_, err := repo.ListMBIDPopulation(context.Background())
	if err == nil {
		t.Fatal("expected an error from ListMBIDPopulation on a closed DB")
	}
	// The other half of the shared-helper guard: this caller's error must name
	// THIS caller, not the ListMBIDPaths one it borrows the helper from.
	if !strings.Contains(err.Error(), "listing artist MBID population") {
		t.Errorf("error = %q, want it to name the ListMBIDPopulation caller", err)
	}
}

// TestServiceListMBIDPopulation covers the service-level wrapper.
func TestServiceListMBIDPopulation(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedMBIDPathFixtures(t, db)
	svc := NewService(db)

	got, err := svc.ListMBIDPopulation(context.Background())
	if err != nil {
		t.Fatalf("ListMBIDPopulation: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("service result = %+v, want 2 rows", got)
	}

	// A count alone would pass on two entirely wrong records. The identity of
	// each row is the contract: m-2 is the pathless artist this query exists to
	// keep, and it must arrive with its id and MBID intact rather than merely
	// being counted. Keyed by id because the query guarantees no order.
	byID := make(map[string]MBIDPath, len(got))
	for _, mp := range got {
		if _, dup := byID[mp.ArtistID]; dup {
			t.Fatalf("artist %q appears more than once in %+v", mp.ArtistID, got)
		}
		byID[mp.ArtistID] = mp
	}

	for _, want := range []MBIDPath{
		{ArtistID: "m-1", MBID: "mbid-alpha", Path: "/music/Alpha"},
		{ArtistID: "m-2", MBID: "mbid-bravo", Path: ""},
	} {
		found, ok := byID[want.ArtistID]
		if !ok {
			t.Fatalf("artist %q missing from the population: %+v", want.ArtistID, got)
		}
		if found != want {
			t.Errorf("population row for %q = %+v, want %+v", want.ArtistID, found, want)
		}
	}
}
