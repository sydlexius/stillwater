package rule

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
)

// storedFanartSlots reads the artist's fanart slot indices back out of the
// database. Every assertion in this file goes through it: the outcome that
// matters is which rows SURVIVED, never a counter, never a returned error.
func storedFanartSlots(t *testing.T, svc *artist.Service, artistID string) []int {
	t.Helper()
	imgs, err := svc.GetImagesForArtist(context.Background(), artistID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	var out []int
	for _, im := range imgs {
		if im.ImageType == "fanart" {
			out = append(out, im.SlotIndex)
		}
	}
	return out
}

// newReconcilePipeline builds a Pipeline wired with a real artist service and a
// real platform service, which is the minimum reconcileAfterFix needs: it
// resolves the fanart naming convention through the engine's platform service
// and persists through the artist service.
func newReconcilePipeline(t *testing.T) (*Pipeline, *artist.Service) {
	t.Helper()
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(t.Context()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	artistSvc := artist.NewService(db)
	engine := NewEngine(ruleSvc, db, platform.NewService(db), nil, testLogger())
	return NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger()), artistSvc
}

// seedArtistWithFanart creates an artist whose directory holds the named fanart
// files, with a stored exists=true row per file.
func seedArtistWithFanart(t *testing.T, svc *artist.Service, name string, files ...string) (*artist.Artist, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	a := &artist.Artist{Name: name, SortName: name, Path: dir}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	for i, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("fake-image"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", f, err)
		}
		if err := svc.UpsertImage(ctx, &artist.ArtistImage{
			ArtistID: a.ID, ImageType: "fanart", SlotIndex: i, Exists: true,
		}); err != nil {
			t.Fatalf("seeding fanart row %d: %v", i, err)
		}
	}
	if got := storedFanartSlots(t, svc, a.ID); len(got) != len(files) {
		t.Fatalf("precondition: want %d stored fanart slots, got %v", len(files), got)
	}
	return a, dir
}

// TestReconcileAfterFix_UsesFreshWalkNotAMidRunCount is the stale-enumeration
// guard (#2635).
//
// Fixers run in SEQUENCE against the same artist, and each used to hand back
// the count it measured during its own walk. Those counts are stale by
// construction: fixer A walks and sees two files, fixer B later adds a third,
// and the run then persists using A's count of two. deleteStaleSlots keeps rows
// where slot_index < FoundSlots, so a count of two condemns slot 2 -- whose
// file fixer B just created and which is sitting on disk right now.
//
// The old fold made this worse rather than better. It kept the SMALLEST count
// per type, believing smaller was conservative. It is the opposite: a lower
// count deletes MORE rows. So the fold actively selected the most destructive
// of the disagreeing counts.
//
// The bound therefore has to come from a walk performed at PERSIST time, so the
// enumeration and the persist describe the same instant.
//
// This test reproduces the sequence exactly and asserts on the surviving rows.
func TestReconcileAfterFix_UsesFreshWalkNotAMidRunCount(t *testing.T) {
	p, svc := newReconcilePipeline(t)
	ctx := t.Context()

	a, dir := seedArtistWithFanart(t, svc, "Stale Count",
		"fanart.jpg", "fanart1.jpg", "fanart2.jpg")

	// Fixer A removes a file. Anything that captured the enumeration HERE would
	// carry a count of 2 to the persist.
	if err := os.Remove(filepath.Join(dir, "fanart2.jpg")); err != nil {
		t.Fatalf("fixer A removing fanart2.jpg: %v", err)
	}

	// Fixer B, later in the same run, adds a file back at that same ordinal.
	if err := os.WriteFile(filepath.Join(dir, "fanart2.jpg"), []byte("new-artwork"), 0o600); err != nil {
		t.Fatalf("fixer B writing fanart2.jpg: %v", err)
	}

	// PRECONDITION: three files are on disk at persist time, so slot 2's row is
	// backed by a file that genuinely exists. Without this the assertion below
	// could pass against a directory that legitimately holds only two.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 3 {
		t.Fatalf("precondition: want 3 files on disk at persist time, got %d (err %v)",
			len(entries), err)
	}

	p.reconcileAfterFix(ctx, a, true)

	got := storedFanartSlots(t, svc, a.ID)
	if len(got) != 3 {
		t.Errorf("stored fanart slots = %v, want all of [0 1 2] to survive: slot 2's "+
			"file was removed and then re-created before the persist, so a count "+
			"captured mid-run condemns a row whose file is on disk. The bound must "+
			"come from a walk taken at persist time", got)
	}
}

// TestReconcileAfterFix_StillRetiresGenuinelyAbsentSlots is the positive
// control. Taking the count fresh must not make the reconcile toothless: a file
// that really was deleted and never replaced still has to lose its row, or the
// artist reports artwork that is gone forever.
func TestReconcileAfterFix_StillRetiresGenuinelyAbsentSlots(t *testing.T) {
	p, svc := newReconcilePipeline(t)
	ctx := t.Context()

	a, dir := seedArtistWithFanart(t, svc, "Genuine Shrink",
		"fanart.jpg", "fanart1.jpg", "fanart2.jpg")

	for _, f := range []string{"fanart1.jpg", "fanart2.jpg"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatalf("removing %s: %v", f, err)
		}
	}

	p.reconcileAfterFix(ctx, a, true)

	got := storedFanartSlots(t, svc, a.ID)
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("stored fanart slots = %v, want exactly [0]: two files were deleted "+
			"and not replaced, so their rows must be retired", got)
	}
}

// TestReconcileAfterFix_NoRemovalIsANoOp is the safety default. A fix that
// removed nothing must not reach the destructive path at all -- not even to be
// refused there. Most fixers never touch the filesystem, and the flag is what
// keeps them structurally incapable of deleting a row.
func TestReconcileAfterFix_NoRemovalIsANoOp(t *testing.T) {
	p, svc := newReconcilePipeline(t)
	ctx := t.Context()

	// A directory holding NO fanart at all, against stored rows. If the guard
	// were absent, the walk would enumerate zero and condemn every row.
	a, dir := seedArtistWithFanart(t, svc, "No Removal", "fanart.jpg", "fanart1.jpg")
	for _, f := range []string{"fanart.jpg", "fanart1.jpg"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatalf("removing %s: %v", f, err)
		}
	}

	p.reconcileAfterFix(ctx, a, false)

	if got := storedFanartSlots(t, svc, a.ID); len(got) != 2 {
		t.Errorf("stored fanart slots = %v, want both rows untouched: no fix in this "+
			"run removed anything, so nothing licenses a delete regardless of what a "+
			"walk would have found", got)
	}
}

// TestReconcileAfterFix_PersistErrorLeavesRegistryIntact covers the swallow
// path (#2635): when the registry write itself fails, reconcileAfterFix
// warn-logs and returns rather than propagating. The fix is already committed
// to disk and to the artist row by the time this runs, so surfacing the error
// would report a successful repair as a failure and invite a re-run; the next
// scan re-derives the registry regardless.
//
// The persist is forced to fail with a reachable input: ReconcileImages returns
// "artist ID is required" for an artist with an empty ID. The assertion is on
// the OUTCOME -- the seeded artist's stored rows are untouched -- proving the
// failed reconcile neither propagated nor disturbed the registry.
func TestReconcileAfterFix_PersistErrorLeavesRegistryIntact(t *testing.T) {
	p, svc := newReconcilePipeline(t)
	ctx := t.Context()

	a, dir := seedArtistWithFanart(t, svc, "Persist Error", "fanart.jpg", "fanart1.jpg")

	// A stand-in that shares the artist's real directory (so the walk succeeds)
	// but carries no ID, which is the reachable trigger for a ReconcileImages
	// persist error.
	broken := &artist.Artist{Name: a.Name, SortName: a.SortName, Path: dir}

	// Must not panic and must not propagate: the call returns normally.
	p.reconcileAfterFix(ctx, broken, true)

	if got := storedFanartSlots(t, svc, a.ID); len(got) != 2 {
		t.Errorf("stored fanart slots = %v, want both rows untouched: a reconcile whose "+
			"registry write fails must swallow the error and leave the stored rows "+
			"exactly as they were", got)
	}
}

// TestReconcileAfterFix_UnreadableDirectoryDeletesNothing pins the distinction
// the whole mechanism rests on. A failed walk knows nothing; reporting that as
// a count of zero would delete every fanart row on the strength of an I/O
// error. "Found no files" and "could not look" are opposite claims and only the
// first licenses a delete.
func TestReconcileAfterFix_UnreadableDirectoryDeletesNothing(t *testing.T) {
	p, svc := newReconcilePipeline(t)
	ctx := t.Context()

	a, dir := seedArtistWithFanart(t, svc, "Unreadable", "fanart.jpg", "fanart1.jpg")

	// Point the artist at a directory that does not exist, so the walk errors
	// rather than returning an empty listing.
	a.Path = filepath.Join(dir, "vanished")

	p.reconcileAfterFix(ctx, a, true)

	if got := storedFanartSlots(t, svc, a.ID); len(got) != 2 {
		t.Errorf("stored fanart slots = %v, want both rows to survive: the directory "+
			"could not be read, so nothing was proven absent", got)
	}
}
