package rule

// Caller-level coverage for #2618.
//
// image.HashFile used to read whole files into memory with no bound, so an
// oversized file in an operator's library directory took the process down
// rather than returning an error. The bound now makes it an error, and this
// test pins the thing that actually matters at this layer: duplicate detection
// DEGRADES on that file (skips it, keeps going) instead of aborting the pass.
//
// Note what is deliberately NOT asserted here: that HashFile returns the
// sentinel. That is the unit test's job. This test asserts the operator-facing
// consequence -- the other artists' slots still get hashed and persisted.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
)

// writeOversizedSparse creates a file just past image.HashFile's read bound
// using a sparse hole, so the fixture is instantaneous and costs no real disk.
func writeOversizedSparse(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating oversized fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	// 25 MB is image.MaxDecodeBytes; +1 puts it just past the bound. The
	// literal is unavoidable here -- the constant is unexported in package
	// image -- but it is pinned by TestOversizedBoundMatchesHashFile below,
	// which fails if the two ever drift.
	if err := f.Truncate(25<<20 + 1); err != nil {
		t.Fatalf("truncating oversized fixture: %v", err)
	}
}

// Guards the hardcoded size in writeOversizedSparse against drift in
// image.MaxDecodeBytes. If the bound moves, this fails loudly here rather than
// silently turning the degradation test below into a no-op that hashes a
// merely-large file successfully.
func TestOversizedBoundMatchesHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.jpg")
	writeOversizedSparse(t, path)
	if _, err := image.HashFile(path, true); !errors.Is(err, image.ErrImageTooLarge) {
		t.Fatalf("oversized fixture no longer exceeds image.HashFile's bound "+
			"(got %v) -- update writeOversizedSparse to match image.MaxDecodeBytes", err)
	}
}

// The pass must survive an oversized file: the good slots are still read,
// hashed, and persisted. Before #2618 this input did not produce a failed
// evaluation -- it produced a dead process.
func TestImageDuplicate_OversizedFile_SkippedNotFatal(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-big", "Big Artist")

	dir := t.TempDir()
	// Slots 0 and 1 are ordinary images; slot 2 is past the read bound.
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	insertTestImage(t, db, "art-big", "fanart", 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)
	insertTestImage(t, db, "art-big", "fanart", 1)
	writeOversizedSparse(t, filepath.Join(dir, "fanart3.jpg"))
	insertTestImage(t, db, "art-big", "fanart", 2)

	a := &artist.Artist{ID: "art-big", Name: "Big Artist", Path: dir}
	checker := e.makeImageDuplicateChecker()

	// The pass completes. If the bound were absent this would not return at
	// all on a genuinely huge file; if the error were propagated instead of
	// absorbed, the good slots below would have no hashes.
	checker(t.Context(), a, RuleConfig{})

	for _, slot := range []int{0, 1} {
		phash, contentHash := storedHashes(t, db, "art-big", "fanart", slot)
		if phash == "" || contentHash == "" {
			t.Errorf("slot %d: phash=%q content_hash=%q -- an oversized sibling slot "+
				"aborted the pass instead of being skipped", slot, phash, contentHash)
		}
	}

	// The oversized slot itself stores nothing. Persisting a hash for a file
	// that was never fully read would be a hash that does not identify it.
	phash, contentHash := storedHashes(t, db, "art-big", "fanart", 2)
	if phash != "" || contentHash != "" {
		t.Errorf("oversized slot 2: phash=%q content_hash=%q, want both empty -- "+
			"a file past the read bound must not yield a hash", phash, contentHash)
	}
}
