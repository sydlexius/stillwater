package rule

// Regression coverage for issue #2337: makeImageDuplicateChecker was blind to
// within-type fanart duplicates (e.g. fanart slot 1 vs fanart slot 2) because
// it only ever compared one hash per image_type at slot_index = 0, and always
// reported Fixable: false. These tests pin the fix: within-type fanart
// duplicates are now detected (via on-demand perceptual hashing of numbered
// slots) and marked Fixable, and ImageDuplicateFixer can actually remove
// them.

import (
	"fmt"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"

	"database/sql"
)

// insertTestImageWithHash inserts an artist_images row with an explicit
// stored phash, for slot-0 rows where resolveImageDupHash uses the stored
// value directly rather than resolving a numbered fanart slot from disk.
func insertTestImageWithHash(t *testing.T, db *sql.DB, artistID, imageType string, slotIndex int, hash uint64) {
	t.Helper()
	id := fmt.Sprintf("%s-%s-%d", artistID, imageType, slotIndex)
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash) VALUES (?, ?, ?, ?, 1, ?)`,
		id, artistID, imageType, slotIndex, image.HashHex(hash))
	if err != nil {
		t.Fatalf("inserting test image row with hash: %v", err)
	}
}

// createGradientJPEG writes a JPEG with a horizontal color gradient. Unlike
// createTestJPEG's flat solid color (whose dHash is always the zero value,
// since every pixel equals its neighbor), a gradient produces a non-trivial
// perceptual hash: identical variants hash identically, and distinct
// variants hash differently. Used to build within-type fanart fixtures where
// some slots must be genuinely distinct and others must be genuine
// duplicates.
func createGradientJPEG(t *testing.T, path string, variant int) {
	t.Helper()
	const width, height = 400, 300
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := uint8((x*255/width + variant*37) % 256)
			img.Set(x, y, color.RGBA{R: v, G: 255 - v, B: 128, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating gradient test image: %v", err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding gradient jpeg: %v", err)
	}
}

// --------------------------------------------------------------------------
// makeImageDuplicateChecker -- within-type fanart detection
// --------------------------------------------------------------------------

func TestMakeImageDuplicateChecker_WithinTypeFanartDuplicateIsFixable(t *testing.T) {
	// fanart2.jpg (slot 1) and fanart3.jpg (slot 2) are the same gradient;
	// neither has a stored phash (numbered slots never do), so the checker
	// must hash them on demand from disk to notice the duplicate.
	e, db := newRuleCovTestEngine(t)
	insertTestArtist(t, db, "art-fanart-dup", "Fanart Dup Artist")
	insertTestImage(t, db, "art-fanart-dup", "fanart", 1)
	insertTestImage(t, db, "art-fanart-dup", "fanart", 2)

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1)

	checker := e.makeImageDuplicateChecker()
	a := &artist.Artist{ID: "art-fanart-dup", Name: "Fanart Dup Artist", Path: dir}
	v := checker(t.Context(), a, RuleConfig{})
	if v == nil {
		t.Fatal("expected within-type fanart duplicate violation, got nil")
	}
	if v.RuleID != RuleImageDuplicate {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleImageDuplicate)
	}
	if !v.Fixable {
		t.Error("Fixable = false; within-type fanart duplicates should be fixable")
	}
	if !strings.Contains(v.Message, "fanart slot 1") || !strings.Contains(v.Message, "fanart slot 2") {
		t.Errorf("Message = %q; want it to name both fanart slots", v.Message)
	}
}

func TestMakeImageDuplicateChecker_WithinTypeFanartDistinctNoViolation(t *testing.T) {
	// fanart2.jpg (slot 1) and fanart3.jpg (slot 2) are visually distinct
	// gradients -- no duplicate should be reported.
	e, db := newRuleCovTestEngine(t)
	insertTestArtist(t, db, "art-fanart-distinct", "Fanart Distinct Artist")
	insertTestImage(t, db, "art-fanart-distinct", "fanart", 1)
	insertTestImage(t, db, "art-fanart-distinct", "fanart", 2)

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 5)

	checker := e.makeImageDuplicateChecker()
	a := &artist.Artist{ID: "art-fanart-distinct", Name: "Fanart Distinct Artist", Path: dir}
	if v := checker(t.Context(), a, RuleConfig{}); v != nil {
		t.Errorf("expected nil violation for distinct fanart slots, got %q (fixable=%v)", v.Message, v.Fixable)
	}
}

// TestMakeImageDuplicateChecker_CrossTypeOrderDoesNotMaskFixability pins the
// review fix for issue #2337's P2 finding: queryImageDupRows has no ORDER
// BY, so a cross-type duplicate (thumb vs fanart, never fixable) can sort
// ahead of a genuine within-type fanart duplicate (fixable). The old
// checker took groups[0] unconditionally, so whichever pair happened to
// sort first decided Fixable -- masking a real fixable violation behind a
// non-fixable one whenever the cross-type pair came first. Thumb slot 0 and
// fanart slot 0 are given an identical explicit stored hash here (a
// cross-type dup with no on-disk dependency, since slot-0 rows use their
// stored phash directly) and inserted before the within-type fanart slots 1
// and 2, which are genuine on-disk duplicate gradients -- reproducing the
// unfavorable insertion order. The violation must still report Fixable.
func TestMakeImageDuplicateChecker_CrossTypeOrderDoesNotMaskFixability(t *testing.T) {
	e, db := newRuleCovTestEngine(t)
	insertTestArtist(t, db, "art-order", "Order Artist")

	// Cross-type dup, inserted first so it is the first row queryImageDupRows
	// returns (no ORDER BY -> insertion/rowid order in SQLite).
	const sharedHash uint64 = 0x00000000FFFFFFFF
	insertTestImageWithHash(t, db, "art-order", "thumb", 0, sharedHash)
	insertTestImageWithHash(t, db, "art-order", "fanart", 0, sharedHash)

	// Within-type fanart duplicate, inserted after -- would previously be
	// masked by the cross-type pair above.
	insertTestImage(t, db, "art-order", "fanart", 1)
	insertTestImage(t, db, "art-order", "fanart", 2)

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 9)  // slot 0: hash unused, comes from DB row
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1) // slot 1
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1) // slot 2: duplicate of slot 1

	checker := e.makeImageDuplicateChecker()
	a := &artist.Artist{ID: "art-order", Name: "Order Artist", Path: dir}
	v := checker(t.Context(), a, RuleConfig{})
	if v == nil {
		t.Fatal("expected a duplicate violation, got nil")
	}
	if !v.Fixable {
		t.Errorf("Fixable = false; want true -- a within-type fanart duplicate exists and must not be masked by cross-type sort order. Message: %s", v.Message)
	}
	if !strings.Contains(v.Message, "fanart slot 1") || !strings.Contains(v.Message, "fanart slot 2") {
		t.Errorf("Message = %q; want it to name the within-type fanart pair (slot 1, slot 2), not the cross-type pair", v.Message)
	}
}

// --------------------------------------------------------------------------
// nonTransitiveFanartDeletionSet -- dHash similarity is not transitive
// --------------------------------------------------------------------------

// TestNonTransitiveFanartDeletionSet_KeepsDistinctThirdSlot pins the review
// fix for issue #2337's P1 finding: perceptual-hash similarity is pairwise,
// not transitive. Given three fanart slots where sim(0,1) and sim(1,2) both
// clear tolerance but sim(0,2) does not, the old logic collected the higher
// slot of every pair as a deletion candidate (0,1)->delete 1, (1,2)->delete
// 2, destroying slot 2's genuinely distinct artwork even though it was
// never directly similar to the surviving representative (slot 0). Only
// slot 1 -- directly paired with the surviving slot 0 -- may be deleted;
// slot 2 must survive.
func TestNonTransitiveFanartDeletionSet_KeepsDistinctThirdSlot(t *testing.T) {
	slot0 := imageDupMember{imageType: "fanart", slotIndex: 0, slotName: "fanart slot 0"}
	slot1 := imageDupMember{imageType: "fanart", slotIndex: 1, slotName: "fanart slot 1"}
	slot2 := imageDupMember{imageType: "fanart", slotIndex: 2, slotName: "fanart slot 2"}

	// sim(0,1) and sim(1,2) both meet tolerance; sim(0,2) is below it (no
	// group emitted for that pair) -- exactly the non-transitive scenario.
	groups := []imageDupGroup{
		{a: slot0, b: slot1, similarity: 0.95, withinTypeFanart: true},
		{a: slot1, b: slot2, similarity: 0.92, withinTypeFanart: true},
	}

	toDelete := nonTransitiveFanartDeletionSet(groups)

	if !toDelete[1] {
		t.Error("slot 1 should be deleted: directly paired with surviving slot 0")
	}
	if toDelete[2] {
		t.Error("slot 2 must survive: never directly paired with a surviving representative (only chained through deleted slot 1) -- this is the P1 data-loss regression")
	}
	if toDelete[0] {
		t.Error("slot 0 must survive: it is the lowest slot and the representative")
	}
	if len(toDelete) != 1 {
		t.Errorf("toDelete = %v; want exactly {1: true}", toDelete)
	}
}

// TestNonTransitiveFanartDeletionSet_PreOldBehaviorWouldDeleteBoth documents
// (without reintroducing) the pre-fix behavior for contrast: the naive
// "delete the higher member of every pair" approach used to compute this
// same toDelete map inline in ImageDuplicateFixer.Fix. Manually reverting
// nonTransitiveFanartDeletionSet to that naive form and re-running
// TestNonTransitiveFanartDeletionSet_KeepsDistinctThirdSlot turns it RED
// (toDelete[2] becomes true, deleting the distinct slot); restoring the
// current implementation turns it GREEN again. This test independently
// re-derives the naive result inline to keep that comparison mechanical
// and reviewable without requiring an actual revert.
func TestNonTransitiveFanartDeletionSet_PreOldBehaviorWouldDeleteBoth(t *testing.T) {
	slot0 := imageDupMember{imageType: "fanart", slotIndex: 0}
	slot1 := imageDupMember{imageType: "fanart", slotIndex: 1}
	slot2 := imageDupMember{imageType: "fanart", slotIndex: 2}
	groups := []imageDupGroup{
		{a: slot0, b: slot1, similarity: 0.95, withinTypeFanart: true},
		{a: slot1, b: slot2, similarity: 0.92, withinTypeFanart: true},
	}

	naiveToDelete := make(map[int]bool)
	for i := range groups {
		g := &groups[i]
		if !g.withinTypeFanart {
			continue
		}
		remove := g.a.slotIndex
		if g.b.slotIndex > remove {
			remove = g.b.slotIndex
		}
		naiveToDelete[remove] = true
	}
	if !naiveToDelete[1] || !naiveToDelete[2] {
		t.Fatalf("naive baseline sanity check failed: %v", naiveToDelete)
	}

	fixed := nonTransitiveFanartDeletionSet(groups)
	if fixed[2] {
		t.Error("fixed deletion set must not match the naive baseline's data-loss behavior")
	}
}

// --------------------------------------------------------------------------
// ImageDuplicateFixer.Fix
// --------------------------------------------------------------------------

func TestImageDuplicateFixer_Fix_SharedFSSkip(t *testing.T) {
	sharedCheck := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSConfirmed},
	}, testLogger())
	f := NewImageDuplicateFixer(nil, nil, sharedCheck, testLogger())
	a := &artist.Artist{Name: "Shared Artist", Path: t.TempDir(), LibraryID: "lib-test"}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicate})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true; want false on shared-filesystem library")
	}
	if !strings.Contains(res.Message, "shared-filesystem") {
		t.Errorf("Message = %q; want it to mention shared-filesystem", res.Message)
	}
}

func TestImageDuplicateFixer_Fix_NoRemovableDuplicates(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-fix-none", "No Dup Artist")
	insertTestImage(t, db, "art-fix-none", "fanart", 1)

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 9)

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), testLogger())
	a := &artist.Artist{ID: "art-fix-none", Name: "No Dup Artist", Path: dir, LibraryID: "lib-test"}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicate, Config: RuleConfig{Tolerance: 0.90}})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true; want false when no removable duplicates exist")
	}
	if !strings.Contains(res.Message, "no removable") {
		t.Errorf("Message = %q; want 'no removable ...'", res.Message)
	}
}

func TestImageDuplicateFixer_Fix_RemovesDuplicateAndRenumbersSurvivors(t *testing.T) {
	// fanart.jpg=slot0 (unique A), fanart2.jpg=slot1 (unique B),
	// fanart3.jpg=slot2 (duplicate of slot1's B), fanart4.jpg=slot3 (unique C).
	// The fixer must delete the higher-numbered duplicate (slot 2 ==
	// fanart3.jpg) and renumber the survivor at slot 3 (fanart4.jpg) down to
	// close the gap, ending as fanart.jpg, fanart2.jpg, fanart3.jpg (unique C).
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-fix-dup", "Fix Dup Artist")
	insertTestImage(t, db, "art-fix-dup", "fanart", 1)
	insertTestImage(t, db, "art-fix-dup", "fanart", 2)
	insertTestImage(t, db, "art-fix-dup", "fanart", 3)

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1)
	createGradientJPEG(t, filepath.Join(dir, "fanart4.jpg"), 2)

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), testLogger())
	a := &artist.Artist{
		ID: "art-fix-dup", Name: "Fix Dup Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 4,
	}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicate, Config: RuleConfig{Tolerance: 0.90}})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed {
		t.Fatalf("Fixed = false; want true. Message: %s", res.Message)
	}
	if !strings.Contains(res.Message, "fanart3.jpg") {
		t.Errorf("Message = %q; want it to name the deleted file fanart3.jpg", res.Message)
	}

	// fanart2.jpg (kept, lowest slot of the duplicate group) is unchanged.
	if _, statErr := os.Stat(filepath.Join(dir, "fanart2.jpg")); statErr != nil {
		t.Errorf("fanart2.jpg should still exist: %v", statErr)
	}
	// The original duplicate's slot (fanart3.jpg) was deleted, then
	// RenumberFanart closed the gap by renaming the surviving fanart4.jpg
	// down into that slot -- so fanart3.jpg exists again (now holding the
	// former fanart4.jpg content) while fanart4.jpg itself is gone.
	if _, statErr := os.Stat(filepath.Join(dir, "fanart3.jpg")); statErr != nil {
		t.Errorf("fanart3.jpg should exist (renumbered survivor): %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "fanart4.jpg")); !os.IsNotExist(statErr) {
		t.Errorf("fanart4.jpg should have been renumbered away; stat err = %v", statErr)
	}

	// Resync: three fanart files remain on disk after the fix.
	if a.FanartCount != 3 {
		t.Errorf("FanartCount = %d, want 3 after removing one duplicate", a.FanartCount)
	}
	if !a.FanartExists {
		t.Error("FanartExists = false; want true")
	}
}

// TestImageDuplicateFixer_Fix_RestoresStagedTombsOnRenumberFailure pins F2's
// crash-safe deletion contract (issue #2351): ImageDuplicateFixer.Fix STAGES
// each duplicate to a tomb (never an immediate unlink) and only commits the
// deletion once RenumberFanart succeeds. If renumbering fails, every staged
// tomb must be RESTORED to its original path so no distinct artwork is lost.
//
// The failure is forced deterministically and host-independently: RenumberFanart
// clears any stale temp file named fanart_renumber_0.jpg.tmp before staging its
// first survivor; pre-creating a NON-EMPTY directory at that path makes its
// os.Remove return ENOTEMPTY, aborting the renumber -- but only after the fixer
// has already staged the duplicate, exercising the rollback path.
//
// Revert-and-rerun proof (#210): reverting F2's staging back to an immediate
// os.Remove(p) makes this test RED (fanart3.jpg is gone, never restored);
// restoring the staging turns it GREEN. Measured RED/GREEN reported in the PR.
func TestImageDuplicateFixer_Fix_RestoresStagedTombsOnRenumberFailure(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-rollback", "Rollback Artist")
	insertTestImage(t, db, "art-rollback", "fanart", 1)
	insertTestImage(t, db, "art-rollback", "fanart", 2)

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1) // slot 2: duplicate of slot 1

	// Force RenumberFanart to fail on its very first survivor: it clears a
	// stale temp file named fanart_renumber_0.jpg.tmp before staging survivor 0
	// (fanart.jpg, .jpg ext). A non-empty directory at that path makes the
	// clearing os.Remove return ENOTEMPTY, so RenumberFanart returns an error
	// after the fixer has already staged the duplicate (fanart3.jpg).
	blockDir := filepath.Join(dir, "fanart_renumber_0.jpg.tmp")
	if err := os.Mkdir(blockDir, 0o755); err != nil {
		t.Fatalf("creating block dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blockDir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("populating block dir: %v", err)
	}

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), testLogger())
	a := &artist.Artist{
		ID: "art-rollback", Name: "Rollback Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 3,
	}
	_, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicate, Config: RuleConfig{Tolerance: 0.90}})
	if err == nil {
		t.Fatal("expected Fix to fail when RenumberFanart fails, got nil error")
	}

	// Rollback proof: the staged duplicate (fanart3.jpg) is restored to its
	// original path, and no tomb file is left behind.
	if _, statErr := os.Stat(filepath.Join(dir, "fanart3.jpg")); statErr != nil {
		t.Errorf("fanart3.jpg must be RESTORED after rollback: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "fanart3.jpg.dup_pending_delete.tmp")); !os.IsNotExist(statErr) {
		t.Errorf("staged tomb must not remain after rollback; stat err = %v", statErr)
	}
	// The lower duplicate slot's file is untouched.
	if _, statErr := os.Stat(filepath.Join(dir, "fanart2.jpg")); statErr != nil {
		t.Errorf("fanart2.jpg should be untouched: %v", statErr)
	}
}
