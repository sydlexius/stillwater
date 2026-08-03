package artist

import (
	"context"
	"testing"
)

// TestUpdateImageProvenance_RecordsGeometry is the regression guard for #2713.
//
// Before this fix, artist_images.width/height were written by exactly one
// producer of FRESH values -- the scanner -- so a manual save left the row
// describing the PREVIOUS image until the next scan re-walked the artist. The
// rules read those columns through Engine.getImageDimensionsResolved, which
// prefers the DB and only falls back to the filesystem when the stored values
// are ZERO, so a stale-but-nonzero row always won. thumb_square and
// thumb_min_res therefore judged an image the operator had already replaced.
//
// Measured on production before the fix: a slot storing 450x600 whose file on
// disk was a 1000x1000 square, flagged both as "not square" and "low
// resolution" while being neither.
//
// The fixture deliberately differs along BOTH axes and in BOTH directions
// (600x400 landscape -> 1200x1600 portrait). A square-to-square or
// same-aspect change would let a fix that copies only one dimension, or that
// preserves aspect, pass vacuously.
func TestUpdateImageProvenance_RecordsGeometry(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Geometry Test", "/music/Geometry Test")
	a.ThumbExists = true
	a.ThumbWidth = 600
	a.ThumbHeight = 400
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	repo := newSQLiteImageRepo(db)

	// Precondition: the seeded geometry is actually stored. Without this the
	// test could pass against a row that never held the old values at all,
	// which would make the assertion below vacuous.
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image row, got %d", len(images))
	}
	if images[0].Width != 600 || images[0].Height != 400 {
		t.Fatalf("precondition: seeded geometry not stored, got %dx%d",
			images[0].Width, images[0].Height)
	}

	// A save records provenance for the file it just wrote. That file is a
	// different size from the one the row describes.
	if err := svc.UpdateImageProvenance(ctx, a.ID, "thumb", 0,
		"ff00ff00ff00ff00", "sha-thumb", "musicbrainz", "jpeg",
		"2026-03-21T15:30:00Z", 1200, 1600); err != nil {
		t.Fatalf("UpdateImageProvenance: %v", err)
	}

	images, err = repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist after update: %v", err)
	}
	if images[0].Width != 1200 || images[0].Height != 1600 {
		t.Errorf("stored geometry still describes the previous image: got %dx%d, want 1200x1600",
			images[0].Width, images[0].Height)
	}

	// Provenance itself must still land -- a fix that wrote geometry while
	// dropping the fields this function already carried would be a regression
	// that the geometry assertion alone would not catch.
	if images[0].PHash != "ff00ff00ff00ff00" {
		t.Errorf("PHash = %q, want ff00ff00ff00ff00", images[0].PHash)
	}
	if images[0].Source != "musicbrainz" {
		t.Errorf("Source = %q, want musicbrainz", images[0].Source)
	}
}

// TestInvalidateImageGeometry_SurvivesAStaleStructUpdate is the regression
// guard for the defect adversarial review caught before this shipped (#2713).
//
// Geometry lives in TWO places: the artist_images row and the in-memory Artist
// the caller is holding. Zeroing only the row looks correct at the call site
// and is silently undone a few lines later, because every rename path persists
// that same artist afterwards and persistNormalized rebuilds the row from the
// struct. writeAll's upsert keeps a non-zero incoming value
// (width = CASE WHEN excluded.width > 0 THEN excluded.width ELSE ... END), so
// the stale number wins.
//
// The original fix passed its own reorder test only by accident of ORDERING:
// that handler invalidates in a defer, which lands after the persist. Every
// other call site invalidated first and was undone. This asserts the property
// directly rather than through one handler's happens-to-work sequencing.
func TestInvalidateImageGeometry_SurvivesAStaleStructUpdate(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Stale Struct", "/music/Stale Struct")
	a.FanartExists = true
	a.FanartCount = 3
	a.FanartWidth = 1920
	a.FanartHeight = 1080
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	repo := newSQLiteImageRepo(db)

	// Precondition: the geometry really is stored, or the assertion below
	// could pass against a row that never held it.
	if got := fanartSlot0(t, ctx, repo, a.ID); got != [2]int{1920, 1080} {
		t.Fatalf("precondition: stored geometry is %v, want [1920 1080]", got)
	}

	// The safe form: zero the row AND the caller's artist.
	if err := svc.InvalidateImageGeometryOn(ctx, a, "fanart"); err != nil {
		t.Fatalf("InvalidateImageGeometryOn: %v", err)
	}
	if a.FanartWidth != 0 || a.FanartHeight != 0 {
		t.Errorf("the caller's artist still carries %dx%d; a later Update would restore it",
			a.FanartWidth, a.FanartHeight)
	}

	// The write that used to undo the invalidation.
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := fanartSlot0(t, ctx, repo, a.ID); got != [2]int{0, 0} {
		t.Errorf("a stale-struct Update resurrected geometry as %v; the invalidation must survive the caller's own persist", got)
	}
}

// TestInvalidateImageGeometry_DBOnlyFormIsUndone documents WHY the -On variant
// exists, by pinning the trap the DB-only form leaves for a caller.
//
// This is not a bug report against InvalidateImageGeometry: it satisfies the
// image.HashInvalidator contract, which passes only an artist ID and so cannot
// reach the caller's struct. The test exists so that anyone tempted to use the
// DB-only form next to an Update sees, in a test name, that it does not hold.
func TestInvalidateImageGeometry_DBOnlyFormIsUndone(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("DB Only", "/music/DB Only")
	a.FanartExists = true
	a.FanartCount = 2
	a.FanartWidth = 800
	a.FanartHeight = 600
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	repo := newSQLiteImageRepo(db)

	if err := svc.InvalidateImageGeometry(ctx, a.ID, "fanart"); err != nil {
		t.Fatalf("InvalidateImageGeometry: %v", err)
	}
	if got := fanartSlot0(t, ctx, repo, a.ID); got != [2]int{0, 0} {
		t.Fatalf("precondition: the row was not zeroed, got %v", got)
	}

	// The caller's artist never learned, so its next persist restores it.
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := fanartSlot0(t, ctx, repo, a.ID); got != [2]int{800, 600} {
		t.Errorf("expected the DB-only form to be undone by the stale struct (got %v); "+
			"if this now survives, the two-store split was fixed and this test should be deleted", got)
	}
}

func fanartSlot0(t *testing.T, ctx context.Context, repo *sqliteImageRepo, artistID string) [2]int {
	t.Helper()
	images, err := repo.GetForArtist(ctx, artistID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	for _, im := range images {
		if im.ImageType == "fanart" && im.SlotIndex == 0 {
			return [2]int{im.Width, im.Height}
		}
	}
	t.Fatalf("no fanart slot 0 row for %s", artistID)
	return [2]int{}
}

// TestUpdateImageProvenance_ZeroPreservesStoredGeometry pins the load-bearing
// half of UpdateProvenance's CASE WHEN ? > 0 guard, which adversarial review
// found was asserted nowhere: weakening it to >= 0 left the entire suite green.
//
// Zero from CollectProvenance means "could not decode this file", not "this
// image is 0x0". Writing it through would replace a merely-stale number with a
// value the rule resolver reads as "measure the file" -- correct after a
// rename, wrong here, where the file is present and simply unreadable at this
// instant. The two cases must stay distinguishable, which is the whole reason
// the guard exists.
func TestUpdateImageProvenance_ZeroPreservesStoredGeometry(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Zero Guard", "/music/Zero Guard")
	a.ThumbExists = true
	a.ThumbWidth = 600
	a.ThumbHeight = 400
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	repo := newSQLiteImageRepo(db)

	// Precondition: without stored geometry the assertion is vacuous -- a row
	// that was always zero would "preserve" zero for the wrong reason.
	images, err := repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist: %v", err)
	}
	if images[0].Width != 600 || images[0].Height != 400 {
		t.Fatalf("precondition: seeded geometry not stored, got %dx%d",
			images[0].Width, images[0].Height)
	}

	// An undecodable file: provenance is still worth recording, geometry is not.
	if err := svc.UpdateImageProvenance(ctx, a.ID, "thumb", 0,
		"aabbccdd", "sha-undecodable", "user", "jpeg",
		"2026-04-01T00:00:00Z", 0, 0); err != nil {
		t.Fatalf("UpdateImageProvenance: %v", err)
	}

	images, err = repo.GetForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetForArtist after update: %v", err)
	}
	if images[0].Width != 600 || images[0].Height != 400 {
		t.Errorf("a zero (undecodable) measurement overwrote good stored geometry: got %dx%d, want 600x400",
			images[0].Width, images[0].Height)
	}
	// The provenance half must still have landed, or this would pass for a
	// function that did nothing at all.
	if images[0].PHash != "aabbccdd" {
		t.Errorf("PHash = %q, want aabbccdd -- the update did not run", images[0].PHash)
	}
}

// TestClearGeometryFields_ClearsOnlyTheNamedType pins that the type switch
// touches exactly one image type and leaves the others alone.
//
// Only the fanart branch was exercised before, by the handler tests. A switch
// whose other three arms are unexecuted is a switch whose other three arms
// could be wrong -- a copy-paste that cleared ThumbWidth from the "banner"
// case would have shipped, and would silently blank a slot the operator never
// touched. Each subtest asserts BOTH halves: the named type went to zero AND
// every other type kept its value.
func TestClearGeometryFields_ClearsOnlyTheNamedType(t *testing.T) {
	t.Parallel()

	// Distinct values per type, so a branch clearing the wrong field cannot
	// coincide with the expected result.
	seed := func() *Artist {
		return &Artist{
			ThumbWidth: 100, ThumbHeight: 101,
			FanartWidth: 200, FanartHeight: 201,
			LogoWidth: 300, LogoHeight: 301,
			BannerWidth: 400, BannerHeight: 401,
		}
	}

	type dims struct{ w, h int }
	got := func(a *Artist) map[string]dims {
		return map[string]dims{
			"thumb":  {a.ThumbWidth, a.ThumbHeight},
			"fanart": {a.FanartWidth, a.FanartHeight},
			"logo":   {a.LogoWidth, a.LogoHeight},
			"banner": {a.BannerWidth, a.BannerHeight},
		}
	}
	want := map[string]dims{
		"thumb":  {100, 101},
		"fanart": {200, 201},
		"logo":   {300, 301},
		"banner": {400, 401},
	}

	for _, imageType := range []string{"thumb", "fanart", "logo", "banner"} {
		t.Run(imageType, func(t *testing.T) {
			a := seed()
			ClearGeometryFields(a, imageType)

			for typ, d := range got(a) {
				if typ == imageType {
					if d.w != 0 || d.h != 0 {
						t.Errorf("%s: got %dx%d, want 0x0 -- the named type must be cleared", typ, d.w, d.h)
					}
					continue
				}
				if d != want[typ] {
					t.Errorf("%s: got %dx%d, want %dx%d -- clearing %s must not touch it",
						typ, d.w, d.h, want[typ].w, want[typ].h, imageType)
				}
			}
		})
	}

	// An unrecognized type is a no-op rather than a silent wipe. This is the
	// direction that matters: a typo'd caller must lose nothing.
	t.Run("unknown type touches nothing", func(t *testing.T) {
		a := seed()
		ClearGeometryFields(a, "poster")
		for typ, d := range got(a) {
			if d != want[typ] {
				t.Errorf("%s: got %dx%d, want %dx%d -- an unknown image type must clear nothing",
					typ, d.w, d.h, want[typ].w, want[typ].h)
			}
		}
	})
}

// TestGeometryHelpers_NilArtistIsSafe pins that neither helper panics on a nil
// artist. Both are called from cleanup and error paths where the artist may not
// have loaded, and a panic there would take down a request that was already
// handling a failure.
func TestGeometryHelpers_NilArtistIsSafe(t *testing.T) {
	t.Parallel()

	ClearGeometryFields(nil, "fanart") // must not panic

	db := setupTestDB(t)
	svc := NewService(db)
	if err := svc.InvalidateImageGeometryOn(context.Background(), nil, "fanart"); err == nil {
		t.Error("InvalidateImageGeometryOn(nil) must return an error rather than panicking or silently succeeding")
	}
}
