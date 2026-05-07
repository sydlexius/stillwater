package artist

import (
	"context"
	"testing"
	"time"
)

// TestHydratePrimaryLibrary_NoMembership covers the artist-with-zero-rows
// branch: the artist exists but has no artist_libraries entry. Per the
// OpenAPI contract on Artist.library_id ("empty when the artist has no
// library memberships"), the helper must CLEAR LibraryID rather than
// preserve any caller-set value, so orphaned rows never leak a stale
// library reference into API responses.
func TestHydratePrimaryLibrary_NoMembership(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Insert an artist row directly without going through Service.Create
	// so no membership row is recorded.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES (?, ?, ?, ?, datetime('now'), datetime('now'))`,
		"orphan-1", "Orphan", "Orphan", "/music/Orphan"); err != nil {
		t.Fatalf("seeding orphan artist: %v", err)
	}

	a := &Artist{ID: "orphan-1", LibraryID: "stale-caller-value"}
	if err := svc.hydratePrimaryLibrary(ctx, a); err != nil {
		t.Fatalf("hydratePrimaryLibrary: %v", err)
	}
	if a.LibraryID != "" {
		t.Errorf("LibraryID = %q, want \"\" (zero memberships must clear LibraryID per OpenAPI contract)",
			a.LibraryID)
	}
}

// TestHydratePrimaryLibrary_PicksOldestMembership covers the "oldest
// added_at wins" rule with explicit chronological ordering. The helper
// must select the earliest membership even when timestamps are stored in
// mixed SQLite + RFC3339 formats (the wrap-with-datetime() fix).
func TestHydratePrimaryLibrary_PicksOldestMembership(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibraries(t, db, "lib-old", "lib-mid", "lib-new")

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('a-multi', 'Multi', 'Multi', '/music/Multi', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}

	// Three memberships, mixed timestamp formats. lib-old is the
	// chronologically earliest; raw TEXT ordering would put 'T'-separated
	// RFC3339 rows after ' '-separated SQLite ones, so without datetime()
	// normalization the wrong row could win.
	rows := []struct {
		libID, addedAt string
	}{
		{"lib-mid", "2026-02-01T00:00:00Z"}, // RFC3339, middle
		{"lib-old", "2026-01-01 00:00:00"},  // SQLite, earliest
		{"lib-new", "2026-03-01T00:00:00Z"}, // RFC3339, latest
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
			 VALUES ('a-multi', ?, 'manual', ?)`,
			r.libID, r.addedAt); err != nil {
			t.Fatalf("seeding membership %s: %v", r.libID, err)
		}
	}

	a := &Artist{ID: "a-multi"}
	if err := svc.hydratePrimaryLibrary(ctx, a); err != nil {
		t.Fatalf("hydratePrimaryLibrary: %v", err)
	}
	if a.LibraryID != "lib-old" {
		t.Errorf("LibraryID = %q, want lib-old (datetime() must normalize mixed formats)", a.LibraryID)
	}
}

// TestHydratePrimaryLibrariesBatch covers the batch hydration path: a
// mix of artists with zero, one, and many memberships. Each artist's
// LibraryID must reflect its own oldest membership independently.
func TestHydratePrimaryLibrariesBatch(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibraries(t, db, "lib-a", "lib-b", "lib-c")

	// Three artists:
	//   a-1: single membership in lib-a.
	//   a-2: two memberships, lib-b should win (older).
	//   a-3: zero memberships (orphan).
	for _, id := range []string{"a-1", "a-2", "a-3"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
			 VALUES (?, ?, ?, ?, datetime('now'), datetime('now'))`,
			id, id, id, "/music/"+id); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
	memberships := []struct{ artist, lib, addedAt string }{
		{"a-1", "lib-a", "2026-02-01T00:00:00Z"},
		// a-2 in two formats again to keep batch path covered for the
		// same datetime() normalization concern.
		{"a-2", "lib-c", "2026-03-01T00:00:00Z"}, // RFC3339, later
		{"a-2", "lib-b", "2026-01-01 00:00:00"},  // SQLite, earlier -> wins
	}
	for _, m := range memberships {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
			 VALUES (?, ?, 'manual', ?)`,
			m.artist, m.lib, m.addedAt); err != nil {
			t.Fatalf("seeding membership for %s: %v", m.artist, err)
		}
	}

	artists := []Artist{
		{ID: "a-1"},
		{ID: "a-2"},
		{ID: "a-3", LibraryID: "stale-caller-value"},
	}
	if err := svc.hydratePrimaryLibrariesBatch(ctx, artists); err != nil {
		t.Fatalf("hydratePrimaryLibrariesBatch: %v", err)
	}
	if artists[0].LibraryID != "lib-a" {
		t.Errorf("a-1 LibraryID = %q, want lib-a", artists[0].LibraryID)
	}
	if artists[1].LibraryID != "lib-b" {
		t.Errorf("a-2 LibraryID = %q, want lib-b (oldest by datetime() across formats)", artists[1].LibraryID)
	}
	// a-3 has no memberships: the batch path must CLEAR LibraryID per the
	// OpenAPI contract, matching the single-artist hydration behavior.
	if artists[2].LibraryID != "" {
		t.Errorf("a-3 LibraryID = %q, want \"\" (zero memberships must clear LibraryID per OpenAPI contract)",
			artists[2].LibraryID)
	}
}

// TestHydratePrimaryLibrariesBatch_Empty is the trivial guard: passing
// no artists must be a no-op that never touches the DB.
func TestHydratePrimaryLibrariesBatch_Empty(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)

	if err := svc.hydratePrimaryLibrariesBatch(context.Background(), nil); err != nil {
		t.Fatalf("nil slice: %v", err)
	}
	if err := svc.hydratePrimaryLibrariesBatch(context.Background(), []Artist{}); err != nil {
		t.Fatalf("empty slice: %v", err)
	}
}

// TestHydratePrimaryLibrary_DecoratedRepo verifies the dbProvider interface
// path: a repo wrapper that EMBEDS *sqliteArtistRepo (the decorator
// pattern used by NewServiceWithRepos callers) must continue to expose
// the underlying *sql.DB and let hydration succeed. The previous
// concrete-type assertion silently disabled hydration here, leaving
// LibraryID empty.
func TestHydratePrimaryLibrary_DecoratedRepo(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	seedLibraries(t, db, "lib-dec")

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		 VALUES ('a-dec', 'Dec', 'Dec', '/music/Dec', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		 VALUES ('a-dec', 'lib-dec', 'manual', ?)`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seeding membership: %v", err)
	}

	// Build a Service with a decorator that wraps the real
	// sqliteArtistRepo. Method promotion exposes DB() so the dbProvider
	// type assertion in hydratePrimaryLibrary still succeeds even though
	// the field type is no longer the concrete *sqliteArtistRepo.
	inner := newSQLiteArtistRepo(db)
	svc := NewService(db)
	svc.artists = &decoratorRepo{sqliteArtistRepo: inner}

	a := &Artist{ID: "a-dec"}
	if err := svc.hydratePrimaryLibrary(ctx, a); err != nil {
		t.Fatalf("hydratePrimaryLibrary: %v", err)
	}
	if a.LibraryID != "lib-dec" {
		t.Errorf("decorated repo LibraryID = %q, want lib-dec (interface-based hydration must work through wrappers)",
			a.LibraryID)
	}
}

// decoratorRepo embeds *sqliteArtistRepo so it both satisfies Repository
// (via method promotion) and inherits the DB() accessor used by the
// unexported dbProvider interface in service.go.
type decoratorRepo struct {
	*sqliteArtistRepo
}

// Compile-time guard: decoratorRepo satisfies Repository through promotion.
var _ Repository = (*decoratorRepo)(nil)
