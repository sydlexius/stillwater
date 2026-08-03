package rule

// Additional coverage for ImageDuplicateFixer's early-exit / error branches and
// the resyncFanartFields helper (issue #2337). The happy path and the
// non-transitive deletion logic are exercised in image_duplicates_test.go;
// these tests pin the guard clauses (no path, no DB, platform-profile lookup
// success and failure, and re-detection failure) plus the post-mutation
// field resync, which are otherwise unreached.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
)

func TestImageDuplicateFixer_CanFix(t *testing.T) {
	f := NewImageDuplicateFixer(nil, nil, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	if !f.CanFix(&Violation{RuleID: RuleImageDuplicate}) {
		t.Error("CanFix should be true for the image_duplicate rule")
	}
	if f.CanFix(&Violation{RuleID: RuleBackdropSequencing}) {
		t.Error("CanFix should be false for an unrelated rule")
	}
}

func TestImageDuplicateFixer_Fix_NoPath(t *testing.T) {
	db := setupTestDB(t)
	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{ID: "art-nopath", Name: "No Path Artist", Path: "", LibraryID: "lib-test"}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicate})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true; want false for a pathless artist")
	}
	if !strings.Contains(res.Message, "no path") {
		t.Errorf("Message = %q; want it to mention the missing path", res.Message)
	}
}

func TestImageDuplicateFixer_Fix_NilDB(t *testing.T) {
	// A non-empty path but no DB handle: the fixer cannot re-detect duplicates,
	// so it must decline rather than proceed to (destructive) deletion.
	f := NewImageDuplicateFixer(nil, nil, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{ID: "art-nodb", Name: "No DB Artist", Path: t.TempDir(), LibraryID: "lib-test"}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicate})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true; want false when no database connection is available")
	}
	if !strings.Contains(res.Message, "no database connection") {
		t.Errorf("Message = %q; want it to mention the missing database connection", res.Message)
	}
}

func TestImageDuplicateFixer_Fix_GetActiveProfileError(t *testing.T) {
	// A platform service backed by a closed DB makes GetActive fail. The fixer
	// must ABORT with an error rather than silently falling back to the default
	// naming convention -- deleting files under the wrong convention is
	// destructive and not safely reversible.
	validDB := setupTestDB(t)

	closedDB := setupTestDB(t)
	if err := closedDB.Close(); err != nil {
		t.Fatalf("closing platform DB: %v", err)
	}
	platSvc := platform.NewService(closedDB)

	f := NewImageDuplicateFixer(validDB, platSvc, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{ID: "art-proferr", Name: "Profile Err Artist", Path: t.TempDir(), LibraryID: "lib-test"}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicate})
	if err == nil {
		t.Fatalf("Fix: expected an error when the active profile cannot load, got res=%+v", res)
	}
	if !strings.Contains(err.Error(), "active platform profile") {
		t.Errorf("error = %q; want it to mention the profile load failure", err.Error())
	}
}

func TestImageDuplicateFixer_Fix_WithPlatformProfile(t *testing.T) {
	// Exercises the profile != nil branch: naming comes from the active
	// profile's fanart list (not the default), and the within-type duplicate is
	// still removed. fanart2.jpg (slot 1) and fanart3.jpg (slot 2) are the same
	// gradient; the higher slot must be deleted and survivors renumbered.
	db := setupTestDB(t)
	ctx := context.Background()

	platSvc := platform.NewService(db)
	profile := &platform.Profile{
		Name:      "test-profile",
		NFOFormat: "kodi",
		ImageNaming: platform.ImageNaming{
			Thumb:  []string{"folder.jpg"},
			Fanart: []string{"fanart.jpg"},
			Logo:   []string{"logo.png"},
			Banner: []string{"banner.jpg"},
		},
		IsActive: true,
	}
	if err := platSvc.Create(ctx, profile); err != nil {
		t.Fatalf("creating profile: %v", err)
	}
	if err := platSvc.SetActive(ctx, profile.ID); err != nil {
		t.Fatalf("setting profile active: %v", err)
	}

	insertTestArtist(t, db, "art-profile-dup", "Profile Dup Artist")
	insertTestImage(t, db, "art-profile-dup", "fanart", 1)
	insertTestImage(t, db, "art-profile-dup", "fanart", 2)

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1) // duplicate of slot 1

	f := NewImageDuplicateFixer(db, platSvc, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{
		ID: "art-profile-dup", Name: "Profile Dup Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 3,
	}
	res, err := f.Fix(ctx, a, &Violation{RuleID: RuleImageDuplicate, Config: RuleConfig{Tolerance: 0.90}})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed {
		t.Fatalf("Fixed = false; want true. Message: %s", res.Message)
	}
	if !strings.Contains(res.Message, "fanart3.jpg") {
		t.Errorf("Message = %q; want it to name the deleted duplicate fanart3.jpg", res.Message)
	}
	// Two distinct fanart files remain after removing the duplicate.
	if a.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2 after removing one duplicate", a.FanartCount)
	}
}

func TestImageDuplicateFixer_Fix_DetectionError(t *testing.T) {
	// A closed (but non-nil) DB passes the nil-DB guard yet makes the
	// re-detection query fail; the fixer must surface that as an error rather
	// than proceed to deletion with an empty group set.
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-det-err", "Detection Err Artist")
	if err := db.Close(); err != nil {
		t.Fatalf("closing DB: %v", err)
	}

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{ID: "art-det-err", Name: "Detection Err Artist", Path: t.TempDir(), LibraryID: "lib-test"}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicate, Config: RuleConfig{Tolerance: 0.90}})
	if err == nil {
		t.Fatalf("Fix: expected a re-detection error on a closed DB, got res=%+v", res)
	}
	if !strings.Contains(err.Error(), "re-detecting image duplicates") {
		t.Errorf("error = %q; want it to mention re-detection failure", err.Error())
	}
}

func TestResyncFanartFields_NoFanart(t *testing.T) {
	// An artist directory with no fanart files: the count-zero branch must clear
	// the fanart fields (Exists=false, Count=0, LowRes=false) without opening
	// any file.
	a := &artist.Artist{
		Name: "Empty Fanart Artist", Path: t.TempDir(),
		FanartExists: true, FanartCount: 5, FanartLowRes: true,
	}
	resyncFanartFields(context.Background(), a, []string{"fanart.jpg"})
	if a.FanartExists {
		t.Error("FanartExists = true; want false when no fanart files exist")
	}
	if a.FanartCount != 0 {
		t.Errorf("FanartCount = %d, want 0", a.FanartCount)
	}
	if a.FanartLowRes {
		t.Error("FanartLowRes = true; want false when no fanart files exist")
	}
}

func TestResyncFanartFields_CountsAndReadsSlot0(t *testing.T) {
	// Two fanart files present: the helper must set Count=2 and Exists=true and
	// open slot 0 to read its dimensions (the LowRes-computation path). The
	// LowRes verdict itself depends on the fixture size and is not asserted
	// here; the count/exists resync is the load-bearing behavior.
	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 3)

	a := &artist.Artist{Name: "Fanart Artist", Path: dir}
	resyncFanartFields(context.Background(), a, []string{"fanart.jpg"})
	if !a.FanartExists {
		t.Error("FanartExists = false; want true when fanart files exist")
	}
	if a.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2", a.FanartCount)
	}
}

// TestResyncFanartFields_RecordsGeometryOfTheNewSlotZero closes the rule-engine
// half of the two-store defect adversarial review found on the API handlers
// (#2713).
//
// All three rule paths that renumber fanart -- backdrop sequencing, duplicate
// collapse, and pHash pollution back-out -- reach the artist struct through
// this one function, then persist it. RenumberFanart zeroes the stored row
// through the invalidator, but ReconcileImages and persistNormalized both
// rebuild that row from this struct, and a surviving non-zero value wins the
// upsert. So leaving geometry here did not merely go stale, it UNDID the
// invalidation on the very next write.
//
// The function was already measuring the dimensions to derive low_res and
// discarding them -- the identical discard that produced the original bug in
// setArtistImageFlag. It now keeps them.
//
// The seeded geometry differs from the fixture's real size on BOTH axes, so a
// no-op cannot pass.
func TestResyncFanartFields_RecordsGeometryOfTheNewSlotZero(t *testing.T) {
	dir := t.TempDir()
	// A real decodable image, deliberately not square.
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1280, 720)

	a := &artist.Artist{
		ID:   "resync-geom",
		Path: dir,
		// What the struct carried before the renumber: a different file.
		FanartWidth:  3840,
		FanartHeight: 2160,
	}

	resyncFanartFields(context.Background(), a, []string{"fanart.jpg"})

	if a.FanartWidth != 1280 || a.FanartHeight != 720 {
		t.Errorf("resync left geometry at %dx%d; it must describe the file now in slot 0 (1280x720), "+
			"otherwise the caller's next persist writes the old file's size back over the invalidated row",
			a.FanartWidth, a.FanartHeight)
	}
}

// TestResyncFanartFields_ZeroesWhenItCannotMeasure pins the other half: when
// this function cannot establish the geometry (no fanart left after a collapse
// that removed everything), it must leave "unknown" rather than the previous
// file's size. Zero routes readers to the filesystem; a stale number does not.
func TestResyncFanartFields_ZeroesWhenItCannotMeasure(t *testing.T) {
	dir := t.TempDir() // deliberately empty: every fanart file was removed

	a := &artist.Artist{
		ID:           "resync-empty",
		Path:         dir,
		FanartWidth:  3840,
		FanartHeight: 2160,
	}

	resyncFanartFields(context.Background(), a, []string{"fanart.jpg"})

	if a.FanartWidth != 0 || a.FanartHeight != 0 {
		t.Errorf("with no fanart on disk the geometry must read unknown (0x0), got %dx%d",
			a.FanartWidth, a.FanartHeight)
	}
}
