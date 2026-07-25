package artist

// name_collision_test.go -- guards for the #2730 pre-write rename check.
//
// The scenario every test here is built around is the one the issue reports:
// two artist rows, one with a filesystem path and one platform-only (no path),
// and an operator renaming one onto the other's name. Before the fix nothing
// refused that write, so a second same-named record appeared.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// seedCollisionPair creates a library, a path-bearing artist, and a
// platform-only artist with DIFFERENT names. The names differ so that a test
// can drive a rename that CAUSES the collision, rather than starting from a
// state that is already colliding -- otherwise the "no collision" control
// cases would have nothing to distinguish.
//
// Returns the service plus both artist IDs.
func seedCollisionPair(t *testing.T, localName, platformName string) (svc *Service, localID, platformOnlyID string) {
	t.Helper()
	db := newTestDB(t)
	svc = NewService(db)
	ctx := context.Background()
	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-collision', 'lib-collision', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	localPath := filepath.Join(root, localName)
	if err := os.MkdirAll(localPath, 0o755); err != nil {
		t.Fatalf("mkdir local artist dir: %v", err)
	}

	local := &Artist{Name: localName, SortName: localName, Path: localPath, LibraryID: "lib-collision"}
	if err := svc.Create(ctx, local); err != nil {
		t.Fatalf("Create local: %v", err)
	}
	// Platform-only row: no path at all, exactly what an Emby/Jellyfin
	// populate creates for an artist with no directory on disk.
	platformOnly := &Artist{Name: platformName, SortName: platformName, Path: ""}
	if err := svc.Create(ctx, platformOnly); err != nil {
		t.Fatalf("Create platform-only: %v", err)
	}

	return svc, local.ID, platformOnly.ID
}

// TestFindNameCollision_DetectsRenameOntoExisting is the direct #2730 case:
// renaming the platform-only row onto the path-bearing row's name must be
// reported as a collision, naming that row.
//
// The precondition assertion matters: it confirms the two rows really do start
// with DIFFERENT identity keys. Without it, a bug that made every call report
// a collision would still pass the main assertion.
func TestFindNameCollision_DetectsRenameOntoExisting(t *testing.T) {
	t.Parallel()
	svc, localID, platformOnlyID := seedCollisionPair(t, "Northfield Chorale", "Northfield Chorale Live")
	ctx := context.Background()

	// Precondition: no collision exists yet, so anything the main assertion
	// finds was genuinely caused by the requested rename.
	pre, err := svc.FindNameCollision(ctx, platformOnlyID, "Northfield Chorale Live")
	if err != nil {
		t.Fatalf("precondition FindNameCollision: %v", err)
	}
	if pre != nil {
		t.Fatalf("precondition: the seeded pair already collides (%+v); the fixture is not exercising a rename", pre)
	}

	got, err := svc.FindNameCollision(ctx, platformOnlyID, "Northfield Chorale")
	if err != nil {
		t.Fatalf("FindNameCollision: %v", err)
	}
	if got == nil {
		t.Fatal("FindNameCollision = nil, want a collision: renaming onto an existing artist's name is exactly the #2730 defect")
	}
	if got.ArtistID != localID {
		t.Errorf("collision.ArtistID = %q, want the path-bearing artist %q", got.ArtistID, localID)
	}
	if got.Name != "Northfield Chorale" {
		t.Errorf("collision.Name = %q, want %q", got.Name, "Northfield Chorale")
	}
	if got.PlatformOnly() {
		t.Error("collision.PlatformOnly() = true, want false: the colliding artist has a filesystem path")
	}
}

// TestFindNameCollision_DetectsNormalizedVariant proves the guard is keyed on
// NormalizeIdentityKey rather than raw string equality. This is the property
// that makes the guard agree with the duplicates report: a rename the guard
// allows must not immediately show up as a detected duplicate.
//
// "the northfield chorale" differs from the stored "Northfield Chorale" in
// case AND in a leading article, so a raw == comparison would let it through.
func TestFindNameCollision_DetectsNormalizedVariant(t *testing.T) {
	t.Parallel()
	svc, localID, platformOnlyID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	got, err := svc.FindNameCollision(ctx, platformOnlyID, "the northfield chorale")
	if err != nil {
		t.Fatalf("FindNameCollision: %v", err)
	}
	if got == nil {
		t.Fatal("FindNameCollision = nil for a case-and-article variant, want a collision: " +
			"a raw string comparison would let this through and DetectDuplicates would then group the two rows")
	}
	if got.ArtistID != localID {
		t.Errorf("collision.ArtistID = %q, want %q", got.ArtistID, localID)
	}
}

// TestFindNameCollision_ReportsPlatformOnlyPartner covers the reverse
// direction: renaming the path-bearing artist onto the PLATFORM-ONLY row's
// name. The collision must be reported with PlatformOnly true so the operator
// message can say the other record has no folder on disk.
func TestFindNameCollision_ReportsPlatformOnlyPartner(t *testing.T) {
	t.Parallel()
	svc, localID, platformOnlyID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	got, err := svc.FindNameCollision(ctx, localID, "Southgate Winds")
	if err != nil {
		t.Fatalf("FindNameCollision: %v", err)
	}
	if got == nil {
		t.Fatal("FindNameCollision = nil, want a collision with the platform-only row")
	}
	if got.ArtistID != platformOnlyID {
		t.Errorf("collision.ArtistID = %q, want the platform-only artist %q", got.ArtistID, platformOnlyID)
	}
	if !got.PlatformOnly() {
		t.Errorf("collision.PlatformOnly() = false, want true: the colliding artist has Path %q, expected empty", got.Path)
	}
}

// TestFindNameCollision_AllowsSelfAndUnusedNames covers the cases that must
// NOT be blocked. Over-blocking is a real failure mode here: a guard that
// refused an artist's own name, or refused a cosmetic tidy-up of it, would
// make the name field uneditable.
func TestFindNameCollision_AllowsSelfAndUnusedNames(t *testing.T) {
	t.Parallel()
	svc, localID, _ := seedCollisionPair(t, "The Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	cases := []struct {
		name    string
		newName string
		why     string
	}{
		{
			name:    "unused name",
			newName: "Brackenmoor Ensemble",
			why:     "no other artist holds this identity",
		},
		{
			name:    "its own current name",
			newName: "The Northfield Chorale",
			why:     "an artist never collides with itself",
		},
		{
			name:    "cosmetic variant of its own name",
			newName: "Northfield Chorale",
			why:     "dropping a leading article keeps the same identity key, so no second identity is created",
		},
		{
			name:    "empty name",
			newName: "   ",
			why:     "field validation rejects a blank name separately; a collision message here would be misleading",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.FindNameCollision(ctx, localID, tc.newName)
			if err != nil {
				t.Fatalf("FindNameCollision(%q): %v", tc.newName, err)
			}
			if got != nil {
				t.Errorf("FindNameCollision(%q) = %+v, want nil: %s", tc.newName, got, tc.why)
			}
		})
	}
}

// TestFindNameCollision_AgreesWithDetectDuplicates ties the guard to the
// report it defers to. A rename the guard REFUSES must be one that, had it
// been allowed, DetectDuplicates would have grouped -- otherwise the 409
// message ("merge them on the duplicates report") sends the operator to a page
// that shows nothing.
//
// The test performs the refused write anyway, then runs detection, so it
// measures the actual consequence rather than asserting the two code paths
// share a helper.
func TestFindNameCollision_AgreesWithDetectDuplicates(t *testing.T) {
	t.Parallel()
	svc, _, platformOnlyID := seedCollisionPair(t, "Northfield Chorale", "Southgate Winds")
	ctx := context.Background()

	const target = "the northfield chorale"
	got, err := svc.FindNameCollision(ctx, platformOnlyID, target)
	if err != nil {
		t.Fatalf("FindNameCollision: %v", err)
	}
	if got == nil {
		t.Fatalf("precondition: FindNameCollision(%q) = nil, expected a collision to verify against detection", target)
	}

	// Apply the write the guard would have refused, then confirm detection
	// really does surface the resulting pair.
	if err := svc.artists.UpdateField(ctx, platformOnlyID, "name", target); err != nil {
		t.Fatalf("applying the refused rename: %v", err)
	}

	db, err := svc.artistDB()
	if err != nil {
		t.Fatalf("artistDB: %v", err)
	}
	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}

	found := false
	for _, g := range groups {
		ids := make(map[string]bool, len(g.Members))
		for _, m := range g.Members {
			ids[m.ID] = true
		}
		if ids[platformOnlyID] && ids[got.ArtistID] {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DetectDuplicates did not group %s with %s after the colliding rename; "+
			"the 409 points the operator at the duplicates report, so the guard and the report must agree",
			platformOnlyID, got.ArtistID)
	}
}
