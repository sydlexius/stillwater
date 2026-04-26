package artist

import (
	"context"
	"database/sql"
	"testing"
)

// seedLibForMNTests inserts a single library row used as a home for
// helper-created artists below. Each test that needs more libraries seeds
// them inline.
func seedLibForMNTests(t *testing.T, ctx context.Context, db *sql.DB, id string, source string) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES (?, ?, '/music', 'regular', ?, datetime('now'), datetime('now'))`,
		id, id, source)
	if err != nil {
		t.Fatalf("seed library %s: %v", id, err)
	}
}

func TestFindByMBIDOrNameUnscoped_ByMBID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibForMNTests(t, ctx, db, "lib-fs", "manual")
	seedLibForMNTests(t, ctx, db, "lib-emby", "emby")

	mbid := "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	a := testArtist("Nirvana", "/music/Nirvana")
	a.MusicBrainzID = mbid
	a.LibraryID = "lib-emby"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Unscoped lookup should find the artist regardless of which library
	// the caller has in mind. This is the populate-path dedupe contract.
	got, err := svc.FindByMBIDOrNameUnscoped(ctx, mbid, "Nirvana")
	if err != nil {
		t.Fatalf("FindByMBIDOrNameUnscoped: %v", err)
	}
	if got == nil {
		t.Fatal("expected artist, got nil")
	}
	if got.ID != a.ID {
		t.Errorf("ID = %q, want %q", got.ID, a.ID)
	}
}

func TestFindByMBIDOrNameUnscoped_ByNameCaseInsensitive(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibForMNTests(t, ctx, db, "lib-fs", "manual")

	a := testArtist("Veridia", "/music/Veridia")
	a.LibraryID = "lib-fs"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.FindByMBIDOrNameUnscoped(ctx, "", "VERIDIA")
	if err != nil {
		t.Fatalf("FindByMBIDOrNameUnscoped: %v", err)
	}
	if got == nil {
		t.Fatal("expected artist, got nil")
	}
	if got.ID != a.ID {
		t.Errorf("ID = %q, want %q", got.ID, a.ID)
	}
}

func TestFindByMBIDOrNameUnscoped_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	got, err := svc.FindByMBIDOrNameUnscoped(ctx, "no-such-mbid", "No Such Artist")
	if err != nil {
		t.Fatalf("FindByMBIDOrNameUnscoped: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestFindByMBIDOrNameUnscoped_CrossesLibraries(t *testing.T) {
	// The unscoped variant exists specifically to find an artist regardless
	// of library context. Seed an artist on one library and look it up
	// without a library scope.
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibForMNTests(t, ctx, db, "lib-fs", "manual")
	seedLibForMNTests(t, ctx, db, "lib-emby", "emby")

	mbid := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	a := testArtist("Muse", "/music/emby/Muse")
	a.MusicBrainzID = mbid
	a.LibraryID = "lib-emby"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.FindByMBIDOrNameUnscoped(ctx, mbid, "Muse")
	if err != nil {
		t.Fatalf("FindByMBIDOrNameUnscoped: %v", err)
	}
	if got == nil || got.ID != a.ID {
		t.Errorf("got %+v, want id=%s", got, a.ID)
	}
}

// MBID precedence: when an artist with a matching MBID exists alongside a
// different artist sharing the same name, the MBID match must win.
// Without this guarantee a regression that fell back to case-insensitive
// name lookup before MBID would still pass the happy-path tests.
func TestFindByMBIDOrNameUnscoped_MBIDPrecedence(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibForMNTests(t, ctx, db, "lib-fs", "manual")
	seedLibForMNTests(t, ctx, db, "lib-emby", "emby")

	mbid := "11111111-2222-3333-4444-555555555555"

	aMBID := testArtist("TieName", "/music/emby/TieName")
	aMBID.MusicBrainzID = mbid
	aMBID.LibraryID = "lib-emby"
	if err := svc.Create(ctx, aMBID); err != nil {
		t.Fatalf("Create aMBID: %v", err)
	}

	aName := testArtist("TieName", "/music/fs/TieName")
	aName.LibraryID = "lib-fs"
	if err := svc.Create(ctx, aName); err != nil {
		t.Fatalf("Create aName: %v", err)
	}

	got, err := svc.FindByMBIDOrNameUnscoped(ctx, mbid, "TieName")
	if err != nil {
		t.Fatalf("FindByMBIDOrNameUnscoped: %v", err)
	}
	if got == nil {
		t.Fatal("expected artist, got nil")
	}
	if got.ID != aMBID.ID {
		t.Errorf("ID = %q, want %q (MBID match must win over name match)", got.ID, aMBID.ID)
	}
}

func TestGetByName_CaseInsensitive(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibForMNTests(t, ctx, db, "lib-fs", "manual")

	a := testArtist("Portishead", "/music/Portishead")
	a.LibraryID = "lib-fs"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.GetByName(ctx, "PORTISHEAD")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got == nil || got.ID != a.ID {
		t.Errorf("got %+v, want id=%s", got, a.ID)
	}
}

func TestGetByName_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	got, err := svc.GetByName(ctx, "Nobody Is Here")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// TestMembershipServicePassthroughs covers the four membership Service
// methods (Add / Remove / List / Count) end-to-end through the service shell
// rather than against the repo directly. These methods are thin wrappers
// today but constitute the public M:N surface, so a test that exercises them
// via the Service guards against a future refactor that would silently break
// the wrapper layer.
func TestMembershipServicePassthroughs(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibForMNTests(t, ctx, db, "lib-fs", "manual")
	seedLibForMNTests(t, ctx, db, "lib-emby", "emby")

	a := testArtist("Bjork", "/music/Bjork")
	a.LibraryID = "lib-fs"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Service.Create auto-derives the initial filesystem membership; baseline
	// count after creation should be 1.
	if got, err := svc.CountLibrariesForArtist(ctx, a.ID); err != nil {
		t.Fatalf("CountLibrariesForArtist baseline: %v", err)
	} else if got != 1 {
		t.Errorf("baseline count = %d, want 1", got)
	}

	if err := svc.AddLibraryMembership(ctx, a.ID, "lib-emby", "emby"); err != nil {
		t.Fatalf("AddLibraryMembership: %v", err)
	}

	// Idempotent on duplicate add.
	if err := svc.AddLibraryMembership(ctx, a.ID, "lib-emby", "emby"); err != nil {
		t.Fatalf("AddLibraryMembership (duplicate): %v", err)
	}

	libs, err := svc.LibrariesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("LibrariesForArtist: %v", err)
	}
	if len(libs) != 2 {
		t.Fatalf("LibrariesForArtist returned %d rows, want 2", len(libs))
	}
	bySource := map[string]string{}
	for _, m := range libs {
		bySource[m.Source] = m.LibraryID
	}
	if bySource["filesystem"] != "lib-fs" {
		t.Errorf("filesystem -> %q, want lib-fs", bySource["filesystem"])
	}
	if bySource["emby"] != "lib-emby" {
		t.Errorf("emby -> %q, want lib-emby", bySource["emby"])
	}

	if got, err := svc.CountLibrariesForArtist(ctx, a.ID); err != nil {
		t.Fatalf("CountLibrariesForArtist post-add: %v", err)
	} else if got != 2 {
		t.Errorf("post-add count = %d, want 2", got)
	}

	if err := svc.RemoveLibraryMembership(ctx, a.ID, "lib-emby"); err != nil {
		t.Fatalf("RemoveLibraryMembership: %v", err)
	}

	// Removing again is a no-op, not an error.
	if err := svc.RemoveLibraryMembership(ctx, a.ID, "lib-emby"); err != nil {
		t.Fatalf("RemoveLibraryMembership (no-op): %v", err)
	}

	if got, err := svc.CountLibrariesForArtist(ctx, a.ID); err != nil {
		t.Fatalf("CountLibrariesForArtist post-remove: %v", err)
	} else if got != 1 {
		t.Errorf("post-remove count = %d, want 1", got)
	}
}

// TestMembershipMethods_NoMembershipRepo covers the nil-repo guard on the
// Service: when membership repo wiring is absent, the methods return zero
// values without erroring, so callers never need to gate on availability.
func TestMembershipMethods_NoMembershipRepo(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	svc.memberships = nil
	ctx := context.Background()

	if err := svc.AddLibraryMembership(ctx, "any", "any", "filesystem"); err != nil {
		t.Errorf("AddLibraryMembership returned err with nil repo: %v", err)
	}
	if err := svc.RemoveLibraryMembership(ctx, "any", "any"); err != nil {
		t.Errorf("RemoveLibraryMembership returned err with nil repo: %v", err)
	}
	libs, err := svc.LibrariesForArtist(ctx, "any")
	if err != nil {
		t.Errorf("LibrariesForArtist returned err with nil repo: %v", err)
	}
	if libs != nil {
		t.Errorf("LibrariesForArtist with nil repo returned %v, want nil", libs)
	}
	got, err := svc.CountLibrariesForArtist(ctx, "any")
	if err != nil {
		t.Errorf("CountLibrariesForArtist returned err with nil repo: %v", err)
	}
	if got != 0 {
		t.Errorf("CountLibrariesForArtist with nil repo = %d, want 0", got)
	}
}
