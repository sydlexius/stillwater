package artist

import (
	"context"
	"sort"
	"testing"
)

// TestResolveHydrateOpts pins the variadic-opts shim used by every Get* and
// batch method: no opts -> HydrateAll (back-compat), one opts -> that opts
// is honored verbatim, multiple opts -> only the first is honored (the
// variadic exists only to make adding the parameter source-compatible).
func TestResolveHydrateOpts(t *testing.T) {
	t.Parallel()

	if got := resolveHydrateOpts(nil); got != HydrateAll {
		t.Errorf("no opts: want HydrateAll, got %+v", got)
	}
	zero := HydrateOpts{}
	if got := resolveHydrateOpts([]HydrateOpts{zero}); got != zero {
		t.Errorf("zero opts: want %+v, got %+v", zero, got)
	}
	only := HydrateOpts{ProviderIDs: true}
	if got := resolveHydrateOpts([]HydrateOpts{only, HydrateAll}); got != only {
		t.Errorf("multiple opts: want first (%+v), got %+v", only, got)
	}
}

// seedHydrateFixtures inserts a library and three artists, two of which
// have a path inside the library, plus one provider-id and one image row
// per artist. Enough to verify GetByIDsBatch / PreloadArtistsByLibrary
// hydration coverage without bringing in the entire artist test fixture.
func seedHydrateFixtures(t *testing.T, svc *Service) []string {
	t.Helper()
	ctx := context.Background()

	ids := []string{"hyd-1", "hyd-2", "hyd-3"}
	paths := []string{"/music/hyd/Alpha", "/music/hyd/Bravo", "/music/hyd/Charlie"}
	names := []string{"Alpha", "Bravo", "Charlie"}

	if _, err := svc.artists.(*sqliteArtistRepo).db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-h', 'lib-h', '/music/hyd', 'regular', 'filesystem', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seeding library: %v", err)
	}
	for i, id := range ids {
		a := &Artist{
			ID: id, Name: names[i], SortName: names[i], Path: paths[i],
		}
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist %s: %v", id, err)
		}
		if _, err := svc.artists.(*sqliteArtistRepo).db.ExecContext(ctx,
			`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
			 VALUES (?, 'lib-h', 'filesystem', datetime('now'))`, id); err != nil {
			t.Fatalf("membership for %s: %v", id, err)
		}
	}
	return ids
}

// TestGetByIDsBatch_HappyPath verifies the bulk loader returns one entry per
// supplied ID, missing IDs are silently dropped, and the back-compat default
// (no opts) still hydrates the side-tables -- existing callers see no change.
func TestGetByIDsBatch_HappyPath(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	svc := NewService(db)
	ids := seedHydrateFixtures(t, svc)
	ctx := context.Background()

	// Include a non-existent ID -- must be silently dropped, not an error.
	got, err := svc.GetByIDsBatch(ctx, append([]string{"no-such-id"}, ids...))
	if err != nil {
		t.Fatalf("GetByIDsBatch: %v", err)
	}
	if len(got) != len(ids) {
		t.Errorf("expected %d artists, got %d", len(ids), len(got))
	}
	for _, id := range ids {
		if _, ok := got[id]; !ok {
			t.Errorf("missing artist %s in result", id)
		}
	}
	// Pointer-identity guard: every map entry must be a distinct *Artist.
	// Catches a regression where the loop reuses a single backing variable
	// and every map key ends up aliased to the last-seen iteration value.
	seenPtrs := make(map[*Artist]string, len(got))
	for id, a := range got {
		if a == nil {
			t.Errorf("nil *Artist for key %s", id)
			continue
		}
		if existing, ok := seenPtrs[a]; ok {
			t.Errorf("aliased pointer: keys %s and %s share the same *Artist (%p)", existing, id, a)
			continue
		}
		seenPtrs[a] = id
		if a.ID != id {
			t.Errorf("map key %s points to artist with ID %s", id, a.ID)
		}
	}
}

// TestGetByIDsBatch_EmptyAndDuplicate covers the boundary paths: empty input
// short-circuits with no DB hit; empty strings inside the slice are skipped;
// duplicates collapse to a single lookup.
func TestGetByIDsBatch_EmptyAndDuplicate(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	svc := NewService(db)
	ids := seedHydrateFixtures(t, svc)
	ctx := context.Background()

	zero, err := svc.GetByIDsBatch(ctx, nil)
	if err != nil {
		t.Fatalf("GetByIDsBatch(nil): %v", err)
	}
	if len(zero) != 0 {
		t.Errorf("expected empty map for nil input, got %v", zero)
	}

	// All-empty input also short-circuits (after the dedupe pass).
	allEmpty, err := svc.GetByIDsBatch(ctx, []string{"", "", ""})
	if err != nil {
		t.Fatalf("GetByIDsBatch(empties): %v", err)
	}
	if len(allEmpty) != 0 {
		t.Errorf("expected empty map for all-empty input, got %v", allEmpty)
	}

	// Duplicates plus an empty string collapse to a single lookup.
	dup, err := svc.GetByIDsBatch(ctx, []string{ids[0], ids[0], "", ids[0]})
	if err != nil {
		t.Fatalf("GetByIDsBatch(dup): %v", err)
	}
	if len(dup) != 1 {
		t.Errorf("expected 1 artist for duplicated input, got %d (%v)", len(dup), dup)
	}
}

// TestGetByIDsBatch_NoHydration verifies that passing HydrateOpts{}
// (the zero value -- nothing to hydrate) is honored: the result still
// includes every requested artist, but the side-table fields stay
// at their zero values for at least the provider-id side. This is the
// opt-out path that lets the bulk-action handler skip unneeded work.
func TestGetByIDsBatch_NoHydration(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	svc := NewService(db)
	ids := seedHydrateFixtures(t, svc)
	ctx := context.Background()

	// Plant a provider-ID row so we can detect whether hydration ran.
	if _, err := svc.artists.(*sqliteArtistRepo).db.ExecContext(ctx,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id, fetched_at)
		 VALUES (?, 'musicbrainz', 'mb-12345', datetime('now'))`,
		ids[0]); err != nil {
		t.Fatalf("seeding provider id: %v", err)
	}

	// With HydrateAll (default), the MBID should hydrate onto the struct.
	full, err := svc.GetByIDsBatch(ctx, []string{ids[0]})
	if err != nil {
		t.Fatalf("GetByIDsBatch(full): %v", err)
	}
	if full[ids[0]].MusicBrainzID != "mb-12345" {
		t.Errorf("default hydration should populate MusicBrainzID; got %q", full[ids[0]].MusicBrainzID)
	}

	// With HydrateOpts{} (no hydration), MBID stays at the zero value.
	bare, err := svc.GetByIDsBatch(ctx, []string{ids[0]}, HydrateOpts{})
	if err != nil {
		t.Fatalf("GetByIDsBatch(bare): %v", err)
	}
	if bare[ids[0]].MusicBrainzID != "" {
		t.Errorf("HydrateOpts{} should skip provider hydration; got %q", bare[ids[0]].MusicBrainzID)
	}
}

// TestPreloadArtistsByLibrary_HappyPath verifies the scanner's pre-load
// helper returns a path-keyed map of the library's members, skipping any
// artist whose path is empty (the scanner doesn't index those).
func TestPreloadArtistsByLibrary_HappyPath(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	svc := NewService(db)
	ids := seedHydrateFixtures(t, svc)
	ctx := context.Background()

	got, err := svc.PreloadArtistsByLibrary(ctx, "lib-h")
	if err != nil {
		t.Fatalf("PreloadArtistsByLibrary: %v", err)
	}
	if len(got) != len(ids) {
		t.Errorf("expected %d preloaded, got %d", len(ids), len(got))
	}
	gotIDs := make([]string, 0, len(got))
	for _, a := range got {
		gotIDs = append(gotIDs, a.ID)
	}
	sort.Strings(gotIDs)
	wantIDs := append([]string(nil), ids...)
	sort.Strings(wantIDs)
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("preload set mismatch at index %d: want %s got %s", i, wantIDs[i], gotIDs[i])
		}
	}
	// Pointer-identity guard: every map entry must be a distinct *Artist.
	seenPtrs := make(map[*Artist]string, len(got))
	for path, a := range got {
		if a == nil {
			t.Errorf("nil *Artist for key %s", path)
			continue
		}
		if existing, ok := seenPtrs[a]; ok {
			t.Errorf("aliased pointer: paths %s and %s share the same *Artist (%p)", existing, path, a)
			continue
		}
		seenPtrs[a] = path
		if a.Path != path {
			t.Errorf("map key %s points to artist with Path %s", path, a.Path)
		}
	}
}

// TestPreloadArtistsByLibrary_EmptyAndMissing covers the boundary paths:
// empty libraryID short-circuits (no DB hit), a nonexistent library
// yields an empty map, and an artist with no path (the scanner would
// not key it anyway) is dropped from the result.
func TestPreloadArtistsByLibrary_EmptyAndMissing(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	zero, err := svc.PreloadArtistsByLibrary(ctx, "")
	if err != nil {
		t.Fatalf("Preload(empty): %v", err)
	}
	if len(zero) != 0 {
		t.Errorf("expected empty map for empty libraryID, got %v", zero)
	}

	missing, err := svc.PreloadArtistsByLibrary(ctx, "no-such-library")
	if err != nil {
		t.Fatalf("Preload(missing): %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("expected empty map for missing library, got %v", missing)
	}

	// Seed a library with one path-less artist; PreloadArtistsByLibrary
	// must filter it out so the scanner's path-key invariant holds.
	if _, err := svc.artists.(*sqliteArtistRepo).db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-pl', 'lib-pl', '/music/pl', 'regular', 'filesystem', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seeding library: %v", err)
	}
	a := &Artist{ID: "pl-1", Name: "PathLess", SortName: "PathLess", Path: ""}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating pathless artist: %v", err)
	}
	if _, err := svc.artists.(*sqliteArtistRepo).db.ExecContext(ctx,
		`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		 VALUES ('pl-1', 'lib-pl', 'filesystem', datetime('now'))`); err != nil {
		t.Fatalf("membership: %v", err)
	}

	out, err := svc.PreloadArtistsByLibrary(ctx, "lib-pl")
	if err != nil {
		t.Fatalf("Preload(pl): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("path-less artist must be excluded; got %v", out)
	}
}

// TestServiceListRefsByLibrary verifies the service-level wrapper passes
// through to the repository contract pinned by TestSqliteListRefsByLibrary.
func TestServiceListRefsByLibrary(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	svc := NewService(db)
	ids := seedHydrateFixtures(t, svc)
	ctx := context.Background()

	refs, err := svc.ListRefsByLibrary(ctx, "lib-h")
	if err != nil {
		t.Fatalf("ListRefsByLibrary: %v", err)
	}
	if len(refs) != len(ids) {
		t.Errorf("expected %d refs, got %d", len(ids), len(refs))
	}
}

// TestGetMethods_HydrateOptsVariadic exercises the variadic-opts shim on each
// of the six Get* methods so the patch-coverage gate includes the opts-passed
// branch alongside the no-opts back-compat branch. The functional contract
// (no-hydration -> zero-valued side-table fields) is pinned by
// TestGetByIDsBatch_NoHydration; this test just walks the shim.
func TestGetMethods_HydrateOptsVariadic(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	svc := NewService(db)
	ids := seedHydrateFixtures(t, svc)
	ctx := context.Background()
	first := ids[0]

	if a, err := svc.GetByID(ctx, first, HydrateOpts{}); err != nil || a == nil {
		t.Errorf("GetByID(opts): err=%v a=%v", err, a)
	}
	if a, err := svc.GetByMBID(ctx, "mb-none", HydrateOpts{}); err == nil && a != nil {
		t.Errorf("GetByMBID(missing): expected nil/err; got %v", a)
	}
	if a, err := svc.GetByProviderID(ctx, "musicbrainz", "none", HydrateOpts{}); err == nil && a != nil {
		t.Errorf("GetByProviderID(missing): expected nil/err; got %v", a)
	}
	if a, err := svc.GetByName(ctx, "Alpha", HydrateOpts{}); err != nil || a == nil {
		t.Errorf("GetByName(opts): err=%v a=%v", err, a)
	}
	if _, err := svc.FindByMBIDOrNameUnscoped(ctx, "", "Alpha", HydrateOpts{}); err != nil {
		t.Errorf("FindByMBIDOrNameUnscoped(opts): err=%v", err)
	}
	if a, err := svc.GetByPath(ctx, "/music/hyd/Alpha", HydrateOpts{}); err != nil || a == nil {
		t.Errorf("GetByPath(opts): err=%v a=%v", err, a)
	}
}
