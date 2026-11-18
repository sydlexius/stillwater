package rule

import (
	"bytes"
	"context"
	"fmt"
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
	// The one redundant slot is locked, so nothing is removed: the honest
	// count must be 0, not the 1 an unlocked group would have dropped.
	if res.SlotsRemoved != 0 {
		t.Fatalf("SlotsRemoved = %d, want 0 (the only redundant slot is locked)", res.SlotsRemoved)
	}
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

// TestScanFanartDuplicates_CountsPerceptualOnlyArtist is the regression pin
// for #2716's blind spot: an artist whose ONLY redundancy is perceptual
// (visually near-identical fanart that is byte-different, so the exact tier
// sees nothing) used to be skipped entirely by the old `if exact == 0 {
// continue }` gate -- zero ArtistsAffected, no PerArtist entry, nothing. This
// pins that such an artist is now counted.
//
// Slots 0 and 1 are the same gradient re-encoded at two different JPEG
// qualities (mirrors
// TestImageDuplicateExact_IgnoresVisuallyIdenticalButDifferentBytes): visually
// identical, byte-different. Slot 2 is a distinct gradient variant.
func TestScanFanartDuplicates_CountsPerceptualOnlyArtist(t *testing.T) {
	p, artistID, dir := newPipelineWithArtistFanart(t)
	// newPipelineWithArtistFanart seeds slots 0-3; this test only wants 3.
	if _, err := p.engine.db.Exec(
		`DELETE FROM artist_images WHERE artist_id = ? AND slot_index = 3`, artistID); err != nil {
		t.Fatalf("trimming seeded slot 3: %v", err)
	}
	createGradientJPEGQuality(t, filepath.Join(dir, "fanart.jpg"), 30, 90)
	createGradientJPEGQuality(t, filepath.Join(dir, "fanart2.jpg"), 30, 60)
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 0)

	// No non-fanart row is seeded here on purpose: totalFanartSlots' image-type
	// filter CANNOT be exercised through this path. ScanFanartDuplicates passes
	// fresh=true, which discards every stored hash, and imageDupRowPath resolves
	// a path only for fanart -- so a non-fanart row is unusable and never
	// reaches res.members regardless of how it is seeded. The filter is covered
	// directly by TestTotalFanartSlots_IgnoresNonFanartMembers instead.

	// Anti-vacuity: if the fixture generator ever produced byte-identical
	// files for slots 0/1, this degenerates into an ordinary exact-duplicate
	// case that would pass against the OLD code too. Assert the fixture is
	// actually byte-different before trusting anything the scan reports.
	if string(readBytes(t, filepath.Join(dir, "fanart.jpg"))) ==
		string(readBytes(t, filepath.Join(dir, "fanart2.jpg"))) {
		t.Fatal("fixture is wrong: slots 0 and 1 must be byte-different (perceptual-only case)")
	}

	report, err := p.ScanFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("ScanFanartDuplicates: %v", err)
	}
	if report.ArtistsAffected != 1 {
		t.Fatalf("ArtistsAffected = %d, want 1 (perceptual-only redundancy must still count as affected)", report.ArtistsAffected)
	}
	if report.ExactRedundantSlots != 0 {
		t.Fatalf("ExactRedundantSlots = %d, want 0 (no byte-identical pair exists)", report.ExactRedundantSlots)
	}
	if report.PerceptualRedundantSlots != 1 {
		t.Fatalf("PerceptualRedundantSlots = %d, want 1", report.PerceptualRedundantSlots)
	}
	if len(report.PerArtist) != 1 || report.PerArtist[0].ArtistID != artistID {
		t.Fatalf("PerArtist = %+v, want single entry for %s", report.PerArtist, artistID)
	}
	if report.PerArtist[0].PerceptualDrops != 1 {
		t.Fatalf("PerArtist[0].PerceptualDrops = %d, want 1", report.PerArtist[0].PerceptualDrops)
	}
	if report.PerArtist[0].ExactDrops != 0 {
		t.Fatalf("PerArtist[0].ExactDrops = %d, want 0", report.PerArtist[0].ExactDrops)
	}
	// The slot TOTALS are a separate axis from the redundancy counts, and
	// nothing above reads them: with only the drop assertions, deleting the
	// TotalSlots/TotalFanartSlots accumulation entirely leaves every test in
	// this package green. Both are pinned here (3 slots seeded, none removed
	// by a scan, which never deletes) so a regression in totalFanartSlots or
	// in either accumulation fails rather than slipping through.
	if report.PerArtist[0].TotalSlots != 3 {
		t.Fatalf("PerArtist[0].TotalSlots = %d, want 3", report.PerArtist[0].TotalSlots)
	}
	if report.TotalFanartSlots != 3 {
		t.Fatalf("TotalFanartSlots = %d, want 3 (library-level aggregate)", report.TotalFanartSlots)
	}
}

// TestScanFanartDuplicates_ExactAndPerceptualCountsDoNotOverlap pins the
// subtraction in perceptualOnlyFanartDrops: exact and perceptual redundant
// slots must be non-overlapping counts, never double-counted.
//
// The fixture deliberately separates the two axes across FOUR slots forming
// two DISJOINT pairs -- slots 0/1 byte-identical (the exact pair), slots 2/3
// visually similar but byte-different (the perceptual-only pair). If the
// same slot pair were reused for both assertions, a bug that forgot the
// subtraction would still produce a correct-looking total (because
// pairImageDuplicates includes byte-identical pairs in the perceptual tier
// too) and this test would pass vacuously either way.
func TestScanFanartDuplicates_ExactAndPerceptualCountsDoNotOverlap(t *testing.T) {
	p, artistID, dir := newPipelineWithArtistFanart(t)
	dup := readFixture(t, "red.jpg")
	writeFanartFiles(t, dir, map[string][]byte{
		"fanart.jpg":  dup, // slot 0
		"fanart2.jpg": dup, // slot 1: byte-identical to slot 0 (exact pair)
	})
	createGradientJPEGQuality(t, filepath.Join(dir, "fanart3.jpg"), 30, 90) // slot 2
	createGradientJPEGQuality(t, filepath.Join(dir, "fanart4.jpg"), 30, 60) // slot 3: same picture as slot 2, re-encoded (perceptual-only pair)

	// Anti-vacuity: pin both axes independently. Slots 0/1 must actually be
	// byte-identical (or this is not testing the exact tier at all), and
	// slots 2/3 must actually be byte-different (or this collapses into the
	// exact case and proves nothing about the subtraction).
	if string(readBytes(t, filepath.Join(dir, "fanart.jpg"))) !=
		string(readBytes(t, filepath.Join(dir, "fanart2.jpg"))) {
		t.Fatal("fixture is wrong: slots 0 and 1 must be byte-identical")
	}
	if string(readBytes(t, filepath.Join(dir, "fanart3.jpg"))) ==
		string(readBytes(t, filepath.Join(dir, "fanart4.jpg"))) {
		t.Fatal("fixture is wrong: slots 2 and 3 must be byte-different")
	}

	report, err := p.ScanFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("ScanFanartDuplicates: %v", err)
	}
	if len(report.PerArtist) != 1 || report.PerArtist[0].ArtistID != artistID {
		t.Fatalf("PerArtist = %+v, want single entry for %s", report.PerArtist, artistID)
	}
	got := report.PerArtist[0]
	if got.TotalSlots != 4 {
		t.Fatalf("TotalSlots = %d, want 4", got.TotalSlots)
	}
	if got.ExactDrops != 1 {
		t.Fatalf("ExactDrops = %d, want 1", got.ExactDrops)
	}
	if got.PerceptualDrops != 1 {
		t.Fatalf("PerceptualDrops = %d, want 1", got.PerceptualDrops)
	}
	// The whole point: 1 exact + 1 perceptual = 2, never 3. A subtraction
	// bug would inflate PerceptualDrops to 2 (double-counting slot 1, which
	// is both byte-identical to slot 0 AND perceptually similar to it) and
	// this sum would read 3.
	if sum := got.ExactDrops + got.PerceptualDrops; sum != 2 {
		t.Fatalf("ExactDrops + PerceptualDrops = %d, want 2 (non-overlapping counts)", sum)
	}
	// Every assertion above reads report.PerArtist[0]; none reads the
	// library-level aggregates, so a broken accumulation (or one wrongly
	// applying the overlap subtraction a second time at library level) would
	// pass. On a single-artist fixture each aggregate must equal its
	// per-artist counterpart exactly, which pins the accumulation without
	// duplicating the per-artist reasoning.
	if report.TotalFanartSlots != got.TotalSlots {
		t.Fatalf("TotalFanartSlots = %d, want %d (must equal the sole artist's TotalSlots)",
			report.TotalFanartSlots, got.TotalSlots)
	}
	if report.ExactRedundantSlots != got.ExactDrops {
		t.Fatalf("ExactRedundantSlots = %d, want %d (must equal the sole artist's ExactDrops)",
			report.ExactRedundantSlots, got.ExactDrops)
	}
	if report.PerceptualRedundantSlots != got.PerceptualDrops {
		t.Fatalf("PerceptualRedundantSlots = %d, want %d (must equal the sole artist's PerceptualDrops)",
			report.PerceptualRedundantSlots, got.PerceptualDrops)
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
	dropFanartRowsAbove(t, p, artistID, 2) // 3 files, so exactly 3 rows
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

// dropFanartRowsAbove deletes fanart rows with slot_index > maxSlot so the
// fixture's row count matches the files a test writes. newPipelineWithArtistFanart
// seeds four rows; a test with fewer files must say so or it exercises the
// row/file count mismatch by accident.
func dropFanartRowsAbove(t *testing.T, p *Pipeline, artistID string, maxSlot int) {
	t.Helper()
	if _, err := p.engine.db.Exec(
		`DELETE FROM artist_images WHERE artist_id = ? AND image_type = 'fanart' AND slot_index > ?`,
		artistID, maxSlot); err != nil {
		t.Fatalf("dropping fanart rows: %v", err)
	}
}

// storedFanartHashes returns slot_index -> "phash|content_hash" for the artist.
func storedFanartHashes(t *testing.T, p *Pipeline, artistID string) map[int]string {
	t.Helper()
	rows, err := p.engine.db.Query(
		`SELECT slot_index, COALESCE(phash, ''), COALESCE(content_hash, '') FROM artist_images WHERE artist_id = ? AND image_type = 'fanart'`,
		artistID)
	if err != nil {
		t.Fatalf("reading stored hashes: %v", err)
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var slot int
		var ph, ch string
		if err := rows.Scan(&slot, &ph, &ch); err != nil {
			t.Fatal(err)
		}
		out[slot] = ph + "|" + ch
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// seedVanishedMiddleFanart builds the shape a vanished file leaves: three
// exists_flag=1 fanart rows (0,1,2) with sentinel stored hashes, three files
// written, then slot 1's file removed so only two remain. file0 and file2
// carry the given bytes; the removed file1 is a distinct image.
func seedVanishedMiddleFanart(t *testing.T, file0, file2 []byte) (*Pipeline, string, string, map[int]string) {
	t.Helper()
	p, artistID, dir := newPipelineWithArtistFanart(t)
	dropFanartRowsAbove(t, p, artistID, 2)
	for slot := 0; slot < 3; slot++ {
		if _, err := p.engine.db.Exec(
			`UPDATE artist_images SET phash = ?, content_hash = ? WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = ?`,
			fmt.Sprintf("%016x", 0x1111*(slot+1)), fmt.Sprintf("sentinel-%d", slot), artistID, slot); err != nil {
			t.Fatalf("seeding sentinel hashes: %v", err)
		}
	}
	writeFanartFiles(t, dir, map[string][]byte{
		"fanart.jpg":  file0,
		"fanart2.jpg": readFixture(t, "blue.jpg"),
		"fanart3.jpg": file2,
	})
	if err := os.Remove(filepath.Join(dir, "fanart2.jpg")); err != nil {
		t.Fatal(err)
	}
	before := storedFanartHashes(t, p, artistID)
	if len(before) != 3 {
		t.Fatalf("precondition: want 3 fanart rows, got %d", len(before))
	}
	return p, artistID, dir, before
}

// assertFixerDeletesNothing runs RemediateFanartDuplicates and asserts no slot
// was removed, every file is byte-unchanged, and no stored hash moved.
func assertFixerDeletesNothing(t *testing.T, p *Pipeline, artistID, dir string, before map[int]string) {
	t.Helper()
	files := listFanartFiles(t, dir)
	snap := map[string][]byte{}
	for _, n := range files {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		snap[n] = b
	}

	res, err := p.RemediateFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("RemediateFanartDuplicates: %v", err)
	}
	if res.SlotsRemoved != 0 {
		t.Errorf("SlotsRemoved = %d, want 0", res.SlotsRemoved)
	}
	got := listFanartFiles(t, dir)
	if len(got) != len(files) {
		t.Fatalf("files after = %v, want %v", got, files)
	}
	for _, n := range files {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("file %s missing after remediation: %v", n, err)
		}
		if !bytes.Equal(b, snap[n]) {
			t.Errorf("file %s changed on disk", n)
		}
	}
	after := storedFanartHashes(t, p, artistID)
	for slot, want := range before {
		if after[slot] != want {
			t.Errorf("slot %d stored hash changed %q -> %q", slot, want, after[slot])
		}
	}
}

// A file vanished after the rows were written, so rows no longer map to files
// by position. The scan must refuse to guess: report a scan error, report no
// duplicates, and persist nothing onto the wrong row.
func TestScanFanartDuplicates_VanishedFileExcludesTheArtistsFanart(t *testing.T) {
	distinct := filepath.Join(t.TempDir(), "g.jpg")
	createGradientJPEG(t, distinct, 5)
	gb, err := os.ReadFile(distinct)
	if err != nil {
		t.Fatal(err)
	}
	p, artistID, _, before := seedVanishedMiddleFanart(t, readFixture(t, "red.jpg"), gb)

	report, err := p.ScanFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("ScanFanartDuplicates: %v", err)
	}
	if report.ScanErrors != 1 {
		t.Errorf("ScanErrors = %d, want 1 (the artist's fanart was not examined)", report.ScanErrors)
	}
	if report.ArtistsAffected != 0 || report.ExactRedundantSlots != 0 || report.PerceptualRedundantSlots != 0 {
		t.Errorf("report = %+v, want no duplicates for the unexamined artist", report)
	}
	after := storedFanartHashes(t, p, artistID)
	for slot, want := range before {
		if after[slot] != want {
			t.Errorf("slot %d stored hash changed %q -> %q: a hash from the wrong file was persisted", slot, want, after[slot])
		}
	}
}

// Same shape with file0 and file2 byte-identical: positional mapping would
// pair row 1 with file2 and call it a duplicate of file0, deleting a file
// that is not a duplicate of anything the operator can see. The fixer must
// delete nothing.
func TestRemediateFanartDuplicates_VanishedFileDeletesNothing(t *testing.T) {
	dup := readFixture(t, "red.jpg")
	p, artistID, dir, before := seedVanishedMiddleFanart(t, dup, dup)

	assertFixerDeletesNothing(t, p, artistID, dir, before)
}

// seedGappedSlotFanart builds the shape a count check alone misses: three
// exists_flag=1 fanart rows with slots 0, 2, 3 (slot 1's row is gone) against
// three files. The count matches, but slot 2 would map to the file at ordinal 2
// and slot 3 to nothing/the wrong file. file0 and file2 carry the given bytes;
// the middle file is a distinct image.
func seedGappedSlotFanart(t *testing.T, file0, file2 []byte) (*Pipeline, string, string, map[int]string) {
	t.Helper()
	p, artistID, dir := newPipelineWithArtistFanart(t)
	if _, err := p.engine.db.Exec(
		`DELETE FROM artist_images WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = 1`, artistID); err != nil {
		t.Fatalf("dropping slot 1 row: %v", err)
	}
	for _, slot := range []int{0, 2, 3} {
		if _, err := p.engine.db.Exec(
			`UPDATE artist_images SET phash = ?, content_hash = ? WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = ?`,
			fmt.Sprintf("%016x", 0x1111*(slot+1)), fmt.Sprintf("sentinel-%d", slot), artistID, slot); err != nil {
			t.Fatalf("seeding sentinel hashes: %v", err)
		}
	}
	writeFanartFiles(t, dir, map[string][]byte{
		"fanart.jpg":  file0,
		"fanart2.jpg": readFixture(t, "blue.jpg"),
		"fanart3.jpg": file2,
	})
	before := storedFanartHashes(t, p, artistID)
	if len(before) != 3 {
		t.Fatalf("precondition: want 3 fanart rows, got %d", len(before))
	}
	if _, ok := before[1]; ok {
		t.Fatalf("precondition: slot 1 must have no row, got %v", before)
	}
	return p, artistID, dir, before
}

// Rows 0, 2, 3 against three files: the count matches but the slots are not
// {0,1,2}, so positional mapping is unsound. The scan must refuse to guess.
func TestScanFanartDuplicates_GappedSlotsExcludeTheArtistsFanart(t *testing.T) {
	distinct := filepath.Join(t.TempDir(), "g.jpg")
	createGradientJPEG(t, distinct, 5)
	gb, err := os.ReadFile(distinct)
	if err != nil {
		t.Fatal(err)
	}
	p, artistID, _, before := seedGappedSlotFanart(t, readFixture(t, "red.jpg"), gb)

	report, err := p.ScanFanartDuplicates(context.Background())
	if err != nil {
		t.Fatalf("ScanFanartDuplicates: %v", err)
	}
	if report.ScanErrors != 1 {
		t.Errorf("ScanErrors = %d, want 1 (the artist's fanart was not examined)", report.ScanErrors)
	}
	if report.ArtistsAffected != 0 || report.ExactRedundantSlots != 0 || report.PerceptualRedundantSlots != 0 {
		t.Errorf("report = %+v, want no duplicates for the unexamined artist", report)
	}
	after := storedFanartHashes(t, p, artistID)
	for slot, want := range before {
		if after[slot] != want {
			t.Errorf("slot %d stored hash changed %q -> %q: a hash from the wrong file was persisted", slot, want, after[slot])
		}
	}
}

// Same gapped shape with file0 and file2 byte-identical: the fixer must delete
// nothing rather than act on a positional guess.
func TestRemediateFanartDuplicates_GappedSlotsDeletesNothing(t *testing.T) {
	dup := readFixture(t, "red.jpg")
	p, artistID, dir, before := seedGappedSlotFanart(t, dup, dup)
	assertFixerDeletesNothing(t, p, artistID, dir, before)
}
