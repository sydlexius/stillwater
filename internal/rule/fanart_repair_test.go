package rule

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// newPipelineWithArtistFanart builds a Pipeline wired against a real,
// migrated SQLite DB (mirrors newDupTestEngine) plus a single artist row
// with a real fanart directory on disk and four fanart slots (0-3) recorded
// in artist_images. Modeled on the ImageDuplicateFixer test setup
// (newDupTestEngine + insertTestArtist + insertTestImage in
// image_duplicate_exact_test.go / checkers_test.go); no scan test yet needed
// a Pipeline over that setup, so this is the thin new helper the brief calls
// for.
func newPipelineWithArtistFanart(t *testing.T) (p *Pipeline, artistID string, dir string) {
	t.Helper()
	e, db := newDupTestEngine(t)

	artistID = "art-fanart-scan"
	insertTestArtist(t, db, artistID, "Fanart Scan Artist")
	for i := 0; i < 4; i++ {
		insertTestImage(t, db, artistID, "fanart", i)
	}

	// Seed a libraries row and an explicit membership so List() hydrates a
	// non-empty Artist.LibraryID (the artists.library_id column was dropped
	// in migration 004; membership now lives in artist_libraries). Without
	// this, RemediateFanartDuplicates' ImageDuplicateFixer.IsShared check
	// fails closed on the empty LibraryID and treats every artist as a
	// shared-filesystem library, skipping every fix.
	if _, err := db.Exec(
		`INSERT INTO libraries (id, name, type, source, created_at, updated_at)
			VALUES ('lib-test', 'Test', 'regular', 'manual', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	dir = t.TempDir()
	svc := artist.NewService(db)
	if err := svc.AddLibraryMembership(context.Background(), artistID, "lib-test", "manual"); err != nil {
		t.Fatalf("adding library membership: %v", err)
	}
	a := &artist.Artist{
		ID: artistID, Name: "Fanart Scan Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 4,
	}
	if err := svc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating test artist path: %v", err)
	}

	fixer := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), svc, testLogger())
	p = NewPipeline(e, svc, nil, []Fixer{fixer}, nil, testLogger())
	return p, artistID, dir
}

// listFanartFiles returns the sorted basenames of every fanart*.jpg file
// remaining in dir, for asserting the on-disk state after a collapse.
func listFanartFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if matched, _ := filepath.Match("fanart*.jpg", e.Name()); matched {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// lockFanartSlot marks the given fanart slot as operator-protected (#2533),
// modeled on the carve-out tests in image_duplicate_carveout_test.go. A
// locked slot must never be deleted by RemediateFanartDuplicates even when it
// is a genuine byte-identical duplicate.
func lockFanartSlot(t *testing.T, p *Pipeline, artistID string, slot int) {
	t.Helper()
	if _, err := p.engine.db.Exec(
		`UPDATE artist_images SET locked = 1 WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = ?`,
		artistID, slot); err != nil {
		t.Fatalf("locking fanart slot %d: %v", slot, err)
	}
}

// readFixture returns a small, distinct, deterministically-generated JPEG for
// the given fixture name. There are no on-disk fixture files in this
// package; existing image_duplicates tests (createGradientJPEG) generate
// fixtures inline instead, so this does the same, keyed by name so callers
// can request two visibly-different images.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	variant := 0
	if name == "blue.jpg" {
		variant = 9
	}
	path := filepath.Join(t.TempDir(), name)
	createGradientJPEG(t, path, variant)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated fixture %s: %v", name, err)
	}
	return b
}

// writeFanartFiles writes the given filename->bytes map into dir.
func writeFanartFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		writeBytes(t, filepath.Join(dir, name), data)
	}
}

// TestScanFanartDuplicates_CountsExactDrops builds an artist whose fanart dir
// holds 3 byte-identical files (fanart, fanart2, fanart3) plus one distinct
// file (fanart4). The scan must report 2 exact drops (the two redundant copies
// of the identical group) and 0 for the distinct slot.
func TestScanFanartDuplicates_CountsExactDrops(t *testing.T) {
	p, artistID, dir := newPipelineWithArtistFanart(t)
	dup := readFixture(t, "red.jpg")
	distinct := readFixture(t, "blue.jpg")
	writeFanartFiles(t, dir, map[string][]byte{
		"fanart.jpg":  dup,
		"fanart2.jpg": dup,
		"fanart3.jpg": dup,
		"fanart4.jpg": distinct,
	})

	report, err := p.ScanFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("ScanFanartDuplicates: %v", err)
	}
	if report.ArtistsAffected != 1 {
		t.Fatalf("ArtistsAffected = %d, want 1", report.ArtistsAffected)
	}
	if report.ExactRedundantSlots != 2 {
		t.Fatalf("ExactRedundantSlots = %d, want 2", report.ExactRedundantSlots)
	}
	if len(report.PerArtist) != 1 || report.PerArtist[0].ArtistID != artistID {
		t.Fatalf("PerArtist = %+v, want single entry for %s", report.PerArtist, artistID)
	}
	if report.PerArtist[0].ExactDrops != 2 {
		t.Fatalf("PerArtist[0].ExactDrops = %d, want 2", report.PerArtist[0].ExactDrops)
	}
	if report.ScanErrors != 0 {
		t.Fatalf("ScanErrors = %d, want 0", report.ScanErrors)
	}
	_ = filepath.Join // keep import if unused after edits
}

// TestRemediateFanartDuplicates_CollapsesExactKeepsSurvivor sets up an artist
// with 3 byte-identical fanart + 1 distinct, remediates, and asserts: the
// survivor of the identical group remains, the 2 redundant files are gone, the
// distinct file remains, and a re-scan reports 0 exact drops. The precondition
// (dups existed before) is asserted so the test cannot pass vacuously.
func TestRemediateFanartDuplicates_CollapsesExactKeepsSurvivor(t *testing.T) {
	p, _, dir := newPipelineWithArtistFanart(t)
	dup := readFixture(t, "red.jpg")
	distinct := readFixture(t, "blue.jpg")
	writeFanartFiles(t, dir, map[string][]byte{
		"fanart.jpg":  dup,
		"fanart2.jpg": dup,
		"fanart3.jpg": dup,
		"fanart4.jpg": distinct,
	})

	pre, err := p.ScanFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("pre-scan: %v", err)
	}
	if pre.ExactRedundantSlots != 2 {
		t.Fatalf("precondition failed: ExactRedundantSlots = %d, want 2", pre.ExactRedundantSlots)
	}

	res, err := p.RemediateFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("RemediateFanartDuplicates: %v", err)
	}
	if res.SlotsRemoved != 2 {
		t.Fatalf("SlotsRemoved = %d, want 2", res.SlotsRemoved)
	}

	post, err := p.ScanFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("post-scan: %v", err)
	}
	if post.ExactRedundantSlots != 0 {
		t.Fatalf("post-scan ExactRedundantSlots = %d, want 0", post.ExactRedundantSlots)
	}
	// Survivor + distinct remain; two redundant collapsed away -> 2 files left,
	// contiguously numbered.
	remaining := listFanartFiles(t, dir)
	if len(remaining) != 2 {
		t.Fatalf("remaining fanart files = %v, want 2", remaining)
	}
}

// TestRemediateFanartDuplicates_SkipsLockedSlot locks the redundant slot and
// asserts it is NOT deleted (Fix filters protected slots, #2533).
func TestRemediateFanartDuplicates_SkipsLockedSlot(t *testing.T) {
	p, artistID, dir := newPipelineWithArtistFanart(t)
	dup := readFixture(t, "red.jpg")
	writeFanartFiles(t, dir, map[string][]byte{"fanart.jpg": dup, "fanart2.jpg": dup})
	lockFanartSlot(t, p, artistID, 1) // lock the redundant slot

	res, err := p.RemediateFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if got := len(listFanartFiles(t, dir)); got != 2 {
		t.Fatalf("locked duplicate was deleted: %d files remain, want 2", got)
	}
	_ = res
}

// TestFanartDuplicates_NilWiringGuard asserts both entry points hard-fail
// with an error, rather than panicking, when called on a zero-value Pipeline
// (no engine wired at all). This is the guard a caller trips if a Pipeline is
// ever constructed without going through NewPipeline.
func TestFanartDuplicates_NilWiringGuard(t *testing.T) {
	p := &Pipeline{}
	if _, err := p.ScanFanartDuplicates(context.Background()); err == nil {
		t.Fatal("ScanFanartDuplicates on unwired pipeline: want error, got nil")
	}
	if _, err := p.RemediateFanartDuplicates(context.Background()); err == nil {
		t.Fatal("RemediateFanartDuplicates on unwired pipeline: want error, got nil")
	}
}

// TestRemediateFanartDuplicates_NoFixerWired asserts the batch runner refuses
// to proceed when the pipeline has no ImageDuplicateFixer registered --
// otherwise it would silently no-op over every artist instead of surfacing
// the misconfiguration.
func TestRemediateFanartDuplicates_NoFixerWired(t *testing.T) {
	e, db := newDupTestEngine(t)
	svc := artist.NewService(db)
	p := NewPipeline(e, svc, nil, nil, nil, testLogger())

	_, err := p.RemediateFanartDuplicates(context.Background())
	if err == nil {
		t.Fatal("RemediateFanartDuplicates with no fixer wired: want error, got nil")
	}
}

// TestFanartDuplicates_EmptyLibrary asserts a wired pipeline over a library
// with zero artists returns clean zero-value results rather than an error --
// distinguishing "nothing to do" from "something went wrong".
func TestFanartDuplicates_EmptyLibrary(t *testing.T) {
	e, db := newDupTestEngine(t)
	svc := artist.NewService(db)
	fixer := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), svc, testLogger())
	p := NewPipeline(e, svc, nil, []Fixer{fixer}, nil, testLogger())

	report, err := p.ScanFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("ScanFanartDuplicates on empty library: %v", err)
	}
	if report.ArtistsAffected != 0 {
		t.Fatalf("ArtistsAffected = %d, want 0", report.ArtistsAffected)
	}

	res, err := p.RemediateFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("RemediateFanartDuplicates on empty library: %v", err)
	}
	if res.ArtistsProcessed != 0 {
		t.Fatalf("ArtistsProcessed = %d, want 0", res.ArtistsProcessed)
	}
}

// TestFanartDuplicates_SkipsPathlessArtist asserts an artist row with no
// filesystem Path is skipped by both entry points rather than causing a
// findImageDuplicates/Fix call against an empty path.
func TestFanartDuplicates_SkipsPathlessArtist(t *testing.T) {
	e, db := newDupTestEngine(t)
	artistID := "art-no-path"
	insertTestArtist(t, db, artistID, "Pathless Artist")
	// insertTestArtist leaves sort_name NULL; artist.Service.List scans it
	// into a non-nullable Go string field, so set it directly (bypassing the
	// artistService, which would also set Path).
	if _, err := db.Exec(`UPDATE artists SET sort_name = name WHERE id = ?`, artistID); err != nil {
		t.Fatalf("setting sort_name: %v", err)
	}
	svc := artist.NewService(db)
	fixer := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), svc, testLogger())
	p := NewPipeline(e, svc, nil, []Fixer{fixer}, nil, testLogger())

	report, err := p.ScanFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("ScanFanartDuplicates: %v", err)
	}
	if report.ArtistsAffected != 0 {
		t.Fatalf("ArtistsAffected = %d, want 0 (pathless artist must be skipped)", report.ArtistsAffected)
	}

	res, err := p.RemediateFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("RemediateFanartDuplicates: %v", err)
	}
	if res.ArtistsProcessed != 0 {
		t.Fatalf("ArtistsProcessed = %d, want 0 (pathless artist must be skipped)", res.ArtistsProcessed)
	}
}

// TestFanartDuplicates_ArtistListError asserts both entry points propagate
// the artist-listing error as a hard failure (rather than returning an
// empty, misleadingly-clean report) when the underlying DB is unusable. The
// DB is closed before the call to force artistService.List to fail.
func TestFanartDuplicates_ArtistListError(t *testing.T) {
	e, db := newDupTestEngine(t)
	svc := artist.NewService(db)
	fixer := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), svc, testLogger())
	p := NewPipeline(e, svc, nil, []Fixer{fixer}, nil, testLogger())

	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	if _, err := p.ScanFanartDuplicates(context.Background()); err == nil {
		t.Fatal("ScanFanartDuplicates over closed db: want error, got nil")
	}
	if _, err := p.RemediateFanartDuplicates(context.Background()); err == nil {
		t.Fatal("RemediateFanartDuplicates over closed db: want error, got nil")
	}
}

// TestRemediateFanartDuplicates_PartialLockReportsActualCount pins the honest
// count: a group of 3 byte-identical slots has 2 redundant copies, but ONE of
// them is #2533-locked, so Fix removes only the single unlocked redundant copy.
// SlotsRemoved must report 1 (what was actually deleted), not the 2 a fully
// unlocked group would have dropped. Under the earlier requested-count logic
// this reported 2, so the assertion fails if the honest-count wiring regresses.
func TestRemediateFanartDuplicates_PartialLockReportsActualCount(t *testing.T) {
	p, artistID, dir := newPipelineWithArtistFanart(t)
	dup := readFixture(t, "red.jpg")
	writeFanartFiles(t, dir, map[string][]byte{
		"fanart.jpg":  dup, // slot 0: survivor
		"fanart2.jpg": dup, // slot 1: redundant, LOCKED (must survive)
		"fanart3.jpg": dup, // slot 2: redundant, unlocked -> the only real deletion
	})
	lockFanartSlot(t, p, artistID, 1)

	res, err := p.RemediateFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if res.SlotsRemoved != 1 {
		t.Fatalf("SlotsRemoved = %d, want 1 (actual deletions, not the requested 2)", res.SlotsRemoved)
	}
	if got := len(listFanartFiles(t, dir)); got != 2 {
		t.Fatalf("files remaining = %d, want 2 (survivor + locked)", got)
	}
}
