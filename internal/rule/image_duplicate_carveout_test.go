package rule

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestImageDuplicateFixer_Fix_SkipsProtectedFanartSlot is the #2533 carve-out
// for the duplicate fixer, which deletes fanart by slot index and so bypasses
// the ruleToImageType guard in attemptFix. A locked or "user"-provenance slot
// must never be deleted, even when it is a genuine byte-identical duplicate.
//
// Setup: slot 0 and slot 1 are byte-identical, so the exact fixer would
// normally delete slot 1 (keep the lowest). Slot 1 is marked protected. The
// fixer must delete nothing and leave the file on disk. Without the filter the
// duplicate is deleted (proven by the sibling
// TestImageDuplicateExactFixer_RemovesCopiesKeepsLowestAndRenumbers).
func TestImageDuplicateFixer_Fix_SkipsProtectedFanartSlot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		locked bool
		source string
	}{
		{name: "locked slot", locked: true, source: ""},
		{name: "user provenance", locked: false, source: "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db := newDupTestEngine(t)
			insertTestArtist(t, db, "art-prot", "Protected Artist")

			dir := t.TempDir()
			// slot 0 and slot 1 are byte-identical duplicates.
			createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
			writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))

			insertTestImage(t, db, "art-prot", "fanart", 0)
			insertTestImage(t, db, "art-prot", "fanart", 1)

			// Mark slot 1 (the deletion target) as operator-protected.
			if tc.locked {
				if _, err := db.Exec(
					`UPDATE artist_images SET locked = 1 WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = 1`,
					"art-prot"); err != nil {
					t.Fatalf("locking slot 1: %v", err)
				}
			}
			if tc.source != "" {
				if _, err := db.Exec(
					`UPDATE artist_images SET source = ? WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = 1`,
					tc.source, "art-prot"); err != nil {
					t.Fatalf("setting source on slot 1: %v", err)
				}
			}

			f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artist.NewService(db), testLogger())
			a := &artist.Artist{
				ID: "art-prot", Name: "Protected Artist", Path: dir, LibraryID: "lib-test",
				FanartExists: true, FanartCount: 2,
			}
			res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
			if err != nil {
				t.Fatalf("Fix: %v", err)
			}
			if res.Fixed {
				t.Errorf("Fixed = true, want false: the only duplicate slot was protected and must not be deleted")
			}

			// The protected duplicate must still be on disk, byte-for-byte.
			if _, err := os.Stat(filepath.Join(dir, "fanart2.jpg")); err != nil {
				t.Errorf("protected fanart slot 1 (fanart2.jpg) was deleted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "fanart.jpg")); err != nil {
				t.Errorf("fanart.jpg (slot 0) unexpectedly gone: %v", err)
			}
		})
	}
}

// TestImageDuplicateFixer_Fix_PartialProtection_DeletesUnprotectedDuplicate
// exercises the reduced-set path: with three byte-identical fanart slots and
// only the middle one protected, the fixer must keep slots 0 (lowest) and 1
// (protected) but still delete slot 2 (an unprotected duplicate). This proves
// the carve-out narrows the deletion set rather than aborting it wholesale.
func TestImageDuplicateFixer_Fix_PartialProtection_DeletesUnprotectedDuplicate(t *testing.T) {
	_, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-part", "Partial Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	writeBytes(t, filepath.Join(dir, "fanart3.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	for _, slot := range []int{0, 1, 2} {
		insertTestImage(t, db, "art-part", "fanart", slot)
	}
	// Protect the middle slot only.
	if _, err := db.Exec(
		`UPDATE artist_images SET locked = 1 WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = 1`,
		"art-part"); err != nil {
		t.Fatalf("locking slot 1: %v", err)
	}

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artist.NewService(db), testLogger())
	a := &artist.Artist{
		ID: "art-part", Name: "Partial Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 3,
	}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed {
		t.Errorf("Fixed = false, want true: an unprotected duplicate (slot 2) should still be deleted. Message: %s", res.Message)
	}
	// The protected slot survives; an unprotected duplicate is removed (three
	// files down to two).
	if _, err := os.Stat(filepath.Join(dir, "fanart2.jpg")); err != nil {
		t.Errorf("protected fanart slot 1 was deleted: %v", err)
	}
	remaining := 0
	for _, name := range []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			remaining++
		}
	}
	if remaining != 2 {
		t.Errorf("expected 2 fanart files after deleting one unprotected duplicate, found %d", remaining)
	}
}
