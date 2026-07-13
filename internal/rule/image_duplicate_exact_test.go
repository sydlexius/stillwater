package rule

// Coverage for issues #2349 and #2341.
//
// #2349 is a recomputation bug: the perceptual hash of every numbered fanart
// slot was recomputed from disk on EVERY rule evaluation, because nothing ever
// wrote the computed hash back. The tests here measure that directly -- they
// count the reads and decodes the checker actually performs -- rather than
// asserting that some call returned without error. A test that only checked
// for a violation would pass just as happily against the unfixed code.
//
// #2341 adds the exact byte-hash tier. Its tests pin the two properties that
// make it worth having next to the perceptual tier: it never fires on files
// that merely look alike (no false positives), and it does fire on files that
// are byte-identical regardless of what they depict.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"

	"database/sql"
)

// hashCallLog records what the duplicate checker actually did to the
// filesystem. reads counts files opened and hashed; decodes counts the subset
// of those that were also fully decoded for a perceptual hash (the expensive
// half).
type hashCallLog struct {
	reads   int
	decodes int
	paths   []string
}

// installHashCounter swaps the package's file-hashing seam for one that counts
// calls and delegates to the real implementation, so the counts describe real
// work rather than a mock's idea of it. Restored at test end.
func installHashCounter(t *testing.T) *hashCallLog {
	t.Helper()
	log := &hashCallLog{}
	original := hashImageFile
	hashImageFile = func(path string, needPerceptual bool) (image.FileHashes, error) {
		log.reads++
		if needPerceptual {
			log.decodes++
		}
		log.paths = append(log.paths, path)
		return original(path, needPerceptual)
	}
	t.Cleanup(func() { hashImageFile = original })
	return log
}

// storedHashes reads back what was actually persisted for a slot. The exit
// code of a fix proves nothing; the hash being readable from the DB afterwards
// is the outcome that matters.
func storedHashes(t *testing.T, db *sql.DB, artistID, imageType string, slot int) (phash, contentHash string) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		`SELECT phash, content_hash FROM artist_images
		 WHERE artist_id = ? AND image_type = ? AND slot_index = ?`,
		artistID, imageType, slot).Scan(&phash, &contentHash)
	if err != nil {
		t.Fatalf("reading stored hashes for %s/%d: %v", imageType, slot, err)
	}
	return phash, contentHash
}

// writeBytes writes exact bytes, used to create genuinely byte-identical files.
func writeBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// readBytes reads a file's bytes so they can be duplicated verbatim.
func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// newDupTestEngine builds an engine wired to the real artist service, so the
// hash persistence under test goes through the production repository code
// rather than a stand-in.
func newDupTestEngine(t *testing.T) (*Engine, *sql.DB) {
	t.Helper()
	e, db := newRuleCovTestEngine(t)
	e.SetImageHashRecorder(artist.NewService(db))
	return e, db
}

// --------------------------------------------------------------------------
// #2349 -- the recomputation bug. These are the measurements, not assertions
// that something merely "worked".
// --------------------------------------------------------------------------

// TestImageDuplicate_HashesComputedOncePerFile_NotPerEvaluation is the core
// #2349 proof. Four fanart slots start with no hashes at all. The first
// evaluation must read and decode each file exactly once; the SECOND
// evaluation must touch the filesystem zero times, because the first one
// persisted what it computed.
//
// Before the fix, the second evaluation re-read and re-decoded all four files,
// and so did every evaluation after it, forever.
func TestImageDuplicate_HashesComputedOncePerFile_NotPerEvaluation(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-perf", "Perf Artist")

	dir := t.TempDir()
	// Four distinct images: distinct so that no fix is triggered and the only
	// thing being measured is the cost of hashing.
	names := []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg", "fanart4.jpg"}
	for i, n := range names {
		createGradientJPEG(t, filepath.Join(dir, n), i)
		insertTestImage(t, db, "art-perf", "fanart", i)
	}

	a := &artist.Artist{ID: "art-perf", Name: "Perf Artist", Path: dir}
	checker := e.makeImageDuplicateChecker()

	// --- Evaluation 1: cold. Every file must be read and decoded once. ---
	log := installHashCounter(t)
	checker(t.Context(), a, RuleConfig{})

	if log.reads != len(names) {
		t.Errorf("cold evaluation: read %d files, want %d (one read per file)", log.reads, len(names))
	}
	if log.decodes != len(names) {
		t.Errorf("cold evaluation: decoded %d files, want %d", log.decodes, len(names))
	}

	// The hashes must actually be in the database now. This is the assertion
	// that separates a real fix from one that computes and throws away.
	for i := range names {
		phash, contentHash := storedHashes(t, db, "art-perf", "fanart", i)
		if phash == "" {
			t.Errorf("slot %d: phash was not persisted after evaluation", i)
		}
		if contentHash == "" {
			t.Errorf("slot %d: content_hash was not persisted after evaluation", i)
		}
	}

	// --- Evaluation 2: warm. Nothing may be read or decoded. ---
	warm := installHashCounter(t)
	checker(t.Context(), a, RuleConfig{})

	if warm.reads != 0 {
		t.Errorf("warm evaluation: read %d files, want 0 -- hashes are persisted, "+
			"so re-evaluation must do no filesystem work. Re-read: %v", warm.reads, warm.paths)
	}
	if warm.decodes != 0 {
		t.Errorf("warm evaluation: decoded %d files, want 0 -- this is the #2349 "+
			"recomputation bug: the dHash is being recomputed per evaluation", warm.decodes)
	}
}

// installDupCallCounter swaps findImageDuplicates for one that counts calls
// and delegates to the real implementation. Restored at test end. Mirrors
// installHashCounter one level up: the file-hashing seam counts per-file
// work, this seam counts per-ARTIST detection passes, which is what
// getCachedImageDuplicates's cross-checker sharing actually saves.
func installDupCallCounter(t *testing.T) *int {
	t.Helper()
	calls := 0
	original := findImageDuplicates
	findImageDuplicates = func(
		ctx context.Context,
		db *sql.DB,
		a *artist.Artist,
		fanartPrimaryName string,
		tolerance float64,
		persist imageHashRecorder,
		fresh bool,
		logger *slog.Logger,
	) (imageDupResult, error) {
		calls++
		return original(ctx, db, a, fanartPrimaryName, tolerance, persist, fresh, logger)
	}
	t.Cleanup(func() { findImageDuplicates = original })
	return &calls
}

// TestImageDuplicate_ExactAndPerceptualCheckersShareOneDetectionPass is the F3
// regression test (PR #2458 review, CodeRabbit MEDIUM). RuleImageDuplicate
// (perceptual) and RuleImageDuplicateExact (exact) are separate Checkers, and
// each independently called findImageDuplicates for the same artist -- so a
// single Evaluate pass paid for the full perceptual grouping TWICE, undoing
// half of #2349's "hash each file once" guarantee at the rule level even
// though findImageDuplicates itself still only reads/decodes each FILE once.
// getCachedImageDuplicates makes the two checkers share one computation.
func TestImageDuplicate_ExactAndPerceptualCheckersShareOneDetectionPass(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := t.Context()
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	// RuleImageDuplicate ships Disabled by default; enable it so both
	// duplicate checkers actually run in this Evaluate pass.
	r, err := svc.GetByID(ctx, RuleImageDuplicate)
	if err != nil {
		t.Fatalf("GetByID(%s): %v", RuleImageDuplicate, err)
	}
	r.Enabled = true
	if err := svc.Update(ctx, r); err != nil {
		t.Fatalf("enabling %s: %v", RuleImageDuplicate, err)
	}

	e := NewEngine(svc, db, nil, nil, testLogger())
	e.SetImageHashRecorder(artist.NewService(db))

	insertTestArtist(t, db, "art-shared", "Shared Artist")
	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 5)
	insertTestImage(t, db, "art-shared", "fanart", 0)
	insertTestImage(t, db, "art-shared", "fanart", 1)

	a := &artist.Artist{ID: "art-shared", Name: "Shared Artist", Path: dir, LibraryID: "lib-test"}

	calls := installDupCallCounter(t)
	if _, err := e.Evaluate(ctx, a); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if *calls != 1 {
		t.Errorf("findImageDuplicates was called %d times in one Evaluate pass, want 1 -- the exact "+
			"and perceptual duplicate checkers are not sharing their detection pass", *calls)
	}
}

// TestImageDuplicate_ToleranceMismatchForcesRecompute pins the correctness
// side of the same cache: a cached result computed at one tolerance must
// never be served to a caller that asked for a different one, since the
// perceptual grouping depends on tolerance. Evaluate a second time
// immediately after the first, at a different tolerance (as a custom
// RuleImageDuplicate threshold would produce), and confirm a second detection
// pass actually ran rather than reusing the first tolerance's grouping.
func TestImageDuplicate_ToleranceMismatchForcesRecompute(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-tol", "Tolerance Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 5)
	a := &artist.Artist{ID: "art-tol", Name: "Tolerance Artist", Path: dir}

	primaryName := resolveFanartPrimaryName(t.Context(), e.platformService)

	calls := installDupCallCounter(t)
	if _, err := e.getCachedImageDuplicates(t.Context(), a, primaryName, 0.90, testLogger()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := e.getCachedImageDuplicates(t.Context(), a, primaryName, 0.75, testLogger()); err != nil {
		t.Fatalf("second call (different tolerance): %v", err)
	}

	if *calls != 2 {
		t.Errorf("findImageDuplicates was called %d times for two DIFFERENT tolerances, want 2 -- a "+
			"tolerance mismatch must never be served a cached result computed at the wrong threshold",
			*calls)
	}
}

// TestImageDuplicate_EvaluateClearsCacheBetweenCalls is the M3 regression test
// (PR #2458 review round 2, adversarial-review finding). The adversarial
// review deleted the `e.imageDupCache = nil` clear at the top of Evaluate and
// the entire internal/rule suite stayed green -- a surviving mutant. The
// memo's ENTIRE safety story is "cleared at the start of every Evaluate
// call"; nothing else guarded that one line.
//
// Not destructive today: getCachedImageDuplicates is reachable only from the
// two checkers (fresh=false, hardcoded), never from the destructive fixer
// (fresh=true, calls the raw findImageDuplicates directly, confirmed in
// F3/M4). But a stale memo one Evaluate call away from the checkers is one
// wiring change away from becoming the "cached result feeds the fixer"
// scenario this whole PR exists to prevent, and had no test.
//
// This evaluates the SAME artist twice with a NEW fanart slot added in
// between -- not a mutation of an existing file's bytes, which would be
// confounded by the STORED hash's own steady-state caching (#2349's separate,
// intentional mechanism: a row whose content_hash is already persisted is
// never re-read regardless of the imageDupCache). Only one fanart file exists
// for the first Evaluate call (no duplicate is even possible with a single
// slot); a second, byte-identical file and its brand-new, never-hashed DB row
// are added before the second call. A stale imageDupCache entry from the
// first call describes only the ORIGINAL one-row query result and would never
// see the new row at all, regardless of what its stored hash says -- so this
// isolates the memo specifically, not the separate stored-hash cache.
func TestImageDuplicate_EvaluateClearsCacheBetweenCalls(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := t.Context()
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	e := NewEngine(svc, db, nil, nil, testLogger())
	e.SetImageHashRecorder(artist.NewService(db))

	insertTestArtist(t, db, "art-m3", "M3 Artist")
	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	insertTestImage(t, db, "art-m3", "fanart", 0)

	a := &artist.Artist{ID: "art-m3", Name: "M3 Artist", Path: dir, LibraryID: "lib-test"}

	first, err := e.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("first Evaluate: %v", err)
	}
	for _, v := range first.Violations {
		if v.RuleID == RuleImageDuplicateExact {
			t.Fatalf("fixture bug: first Evaluate already reports a duplicate violation (%s); "+
				"only one fanart slot exists, so no duplicate is possible yet", v.Message)
		}
	}

	// Add a second fanart file, byte-identical to the first, with a
	// brand-new DB row that has never been hashed.
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	insertTestImage(t, db, "art-m3", "fanart", 1)

	second, err := e.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("second Evaluate: %v", err)
	}
	found := false
	for _, v := range second.Violations {
		if v.RuleID == RuleImageDuplicateExact {
			found = true
		}
	}
	if !found {
		t.Error("second Evaluate did not report the new exact-duplicate violation -- the " +
			"per-Evaluate cache served a stale result from the first call instead of recomputing")
	}
}

// TestImageDuplicate_FixerNeverSeesTheCheckerMemo is the M4 regression test
// (PR #2458 review round 2, adversarial-review finding). This is the test the
// F3 round should have shipped with and did not.
//
// HONEST SCOPE, stated up front because a prior comment in this file overclaimed
// what a passing test proves and that is exactly the failure mode this whole
// review round exists to stop: the adversarial review confirmed
// ImageDuplicateFixer has NO *Engine field at all -- it is a wholly separate
// type with its own db/imageHashRecorder/logger, and cannot reach
// Engine.imageDupCache even in principle. So THIS TEST PASSES TRIVIALLY
// TODAY. It is not evidence that today's code is correct (the structural
// separation already proves that, independent of any test). It exists as a
// REGRESSION GUARD against a future refactor that hands ImageDuplicateFixer
// an *Engine reference and wires its detection call through
// getCachedImageDuplicates instead of the raw fresh=true findImageDuplicates
// call -- the exact trap the lead's addendum warned about. If that refactor
// ever happens without also preserving fresh=true semantics, this test is
// designed to catch it: it runs a checker (populating the memo with a
// deliberately WRONG cached grouping), then mutates the artist's fanart to
// disagree with that grouping, then asserts the FIXER acts on the fresh,
// correct state -- which only stays true if the fixer's detection pass is
// never served the checker's stale memo entry.
func TestImageDuplicate_FixerNeverSeesTheCheckerMemo(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-m4", "M4 Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	insertTestImage(t, db, "art-m4", "fanart", 0)
	insertTestImage(t, db, "art-m4", "fanart", 1)

	a := &artist.Artist{
		ID: "art-m4", Name: "M4 Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 2,
	}

	// Run a checker: slot 0 and slot 1 are byte-identical right now, so this
	// populates the memo with a grouping that says "slot 1 is a duplicate of
	// slot 0, removable."
	primaryName := resolveFanartPrimaryName(t.Context(), e.platformService)
	memoed, err := e.getCachedImageDuplicates(t.Context(), a, primaryName, defaultImageDupTolerance, testLogger())
	if err != nil {
		t.Fatalf("populating the memo: %v", err)
	}
	if len(memoed.exactFanartToDelete) == 0 {
		t.Fatal("fixture bug: the memoed grouping does not show slot 1 as removable")
	}

	// Now make the memoed grouping WRONG: overwrite slot 1 with a genuinely
	// DISTINCT image. If the fixer were ever wired to consult this Engine's
	// memo instead of its own fresh=true call, it would still see slot 0 and
	// slot 1 as duplicates and delete slot 1 -- destroying distinct artwork
	// on the strength of a stale grouping computed before the file changed.
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 9)
	distinct := readBytes(t, filepath.Join(dir, "fanart2.jpg"))

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artist.NewService(db), testLogger())
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Errorf("fixer reported Fixed=true (%q) -- it deleted a file based on a stale grouping "+
			"instead of re-deriving from the current, now-distinct disk state", res.Message)
	}

	got := readBytes(t, filepath.Join(dir, "fanart2.jpg"))
	if string(got) != string(distinct) {
		t.Error("fanart2.jpg's distinct bytes were destroyed -- the fixer acted on the checker's " +
			"stale memoed grouping instead of a fresh read")
	}
}

// TestImageDuplicate_StoredPerceptualHashSkipsTheDecode pins the "exact-first
// saves the decode, not the read" property. A slot that already has a stored
// perceptual hash but no content hash still has to be read (both tiers need
// the bytes), but it must NOT be decoded again -- the decode is the expensive
// half and its answer is already known.
func TestImageDuplicate_StoredPerceptualHashSkipsTheDecode(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-nodecode", "No Decode Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)

	// Slot 0 has a stored phash but no content hash; slot 1 has neither.
	insertTestImageWithHash(t, db, "art-nodecode", "fanart", 0, 0xABCDEF0123456789)
	insertTestImage(t, db, "art-nodecode", "fanart", 1)

	a := &artist.Artist{ID: "art-nodecode", Name: "No Decode Artist", Path: dir}
	log := installHashCounter(t)
	e.makeImageDuplicateChecker()(t.Context(), a, RuleConfig{})

	if log.reads != 2 {
		t.Errorf("read %d files, want 2 (both need a content hash)", log.reads)
	}
	if log.decodes != 1 {
		t.Errorf("decoded %d files, want 1 -- only the slot lacking a stored "+
			"perceptual hash should be decoded", log.decodes)
	}
}

// TestImageDuplicate_HashPersistToleratesVanishedSlot covers the race where a
// concurrent scan removes or renumbers a slot between the read and the write.
// The update affects zero rows; that is benign and must not fail evaluation.
func TestImageDuplicate_HashPersistToleratesVanishedSlot(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-race", "Race Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)
	insertTestImage(t, db, "art-race", "fanart", 0)
	insertTestImage(t, db, "art-race", "fanart", 1)

	// Simulate the scan winning the race: the rows are gone by the time the
	// checker tries to persist, but the files are still on disk.
	if _, err := db.ExecContext(t.Context(),
		`DELETE FROM artist_images WHERE artist_id = ?`, "art-race"); err != nil {
		t.Fatalf("deleting rows to simulate race: %v", err)
	}

	a := &artist.Artist{ID: "art-race", Name: "Race Artist", Path: dir}
	// Must not panic or fail; with no rows there is simply nothing to compare.
	if v := e.makeImageDuplicateChecker()(t.Context(), a, RuleConfig{}); v != nil {
		t.Errorf("expected no violation when the artist has no image rows, got %+v", v)
	}
}

// --------------------------------------------------------------------------
// #2341 -- the exact byte-hash tier.
// --------------------------------------------------------------------------

// TestImageDuplicateExact_DetectsByteIdenticalFanart is the headline case: the
// same file saved into two slots.
func TestImageDuplicateExact_DetectsByteIdenticalFanart(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-exact", "Exact Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	original := readBytes(t, filepath.Join(dir, "fanart.jpg"))
	// fanart2.jpg is byte-for-byte the same file as fanart.jpg.
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), original)

	insertTestImage(t, db, "art-exact", "fanart", 0)
	insertTestImage(t, db, "art-exact", "fanart", 1)

	a := &artist.Artist{ID: "art-exact", Name: "Exact Artist", Path: dir}
	v := e.makeImageDuplicateExactChecker()(t.Context(), a, RuleConfig{})
	if v == nil {
		t.Fatal("expected an exact-duplicate violation for two byte-identical fanart files, got nil")
	}
	if v.RuleID != RuleImageDuplicateExact {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleImageDuplicateExact)
	}
	if !v.Fixable {
		t.Error("Fixable = false; byte-identical duplicates are always safe to remove")
	}
	// The message must name the slot that would be REMOVED (the higher one),
	// never the one that is kept.
	if !strings.Contains(v.Message, "fanart slot 1") {
		t.Errorf("Message = %q; want it to name fanart slot 1 as removable", v.Message)
	}
	if strings.Contains(v.Message, "fanart slot 0") {
		t.Errorf("Message = %q; must not list slot 0, which is the copy that is kept", v.Message)
	}
}

// TestImageDuplicateExact_IgnoresVisuallyIdenticalButDifferentBytes is the
// no-false-positive guarantee, and the reason the perceptual tier still earns
// its keep. Two files that depict the same picture but differ byte-wise (here,
// re-encoded at a different JPEG quality) are NOT exact duplicates. The exact
// rule must stay silent -- while the perceptual rule, looking at the same two
// files, does fire.
func TestImageDuplicateExact_IgnoresVisuallyIdenticalButDifferentBytes(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-reenc", "Reencoded Artist")

	dir := t.TempDir()
	// Same gradient (variant 1) in both slots, but written by two separate
	// encode passes at different quality, so the bytes differ.
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 1)
	createGradientJPEGQuality(t, filepath.Join(dir, "fanart2.jpg"), 1, 60)

	if string(readBytes(t, filepath.Join(dir, "fanart.jpg"))) ==
		string(readBytes(t, filepath.Join(dir, "fanart2.jpg"))) {
		t.Fatal("fixture is wrong: the two files must NOT be byte-identical")
	}

	insertTestImage(t, db, "art-reenc", "fanart", 0)
	insertTestImage(t, db, "art-reenc", "fanart", 1)

	a := &artist.Artist{ID: "art-reenc", Name: "Reencoded Artist", Path: dir}

	if v := e.makeImageDuplicateExactChecker()(t.Context(), a, RuleConfig{}); v != nil {
		t.Errorf("exact rule fired on files that are not byte-identical: %q", v.Message)
	}

	// The perceptual rule is what catches this case, and it must still do so.
	if v := e.makeImageDuplicateChecker()(t.Context(), a, RuleConfig{}); v == nil {
		t.Error("perceptual rule missed a re-encoded duplicate; the two tiers are " +
			"complementary and this is precisely the case only the perceptual one sees")
	}
}

// TestImageDuplicateExact_ByteIdenticalStillSeenByPerceptualRule guards the
// detection hole that suppressing byte-identical pairs from the perceptual
// tier would open. The two rules have independent enable toggles, so a user
// running the perceptual rule with the exact rule switched off must still be
// able to see byte-identical duplicates.
func TestImageDuplicateExact_ByteIdenticalStillSeenByPerceptualRule(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-both", "Both Rules Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 3)
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	insertTestImage(t, db, "art-both", "fanart", 0)
	insertTestImage(t, db, "art-both", "fanart", 1)

	a := &artist.Artist{ID: "art-both", Name: "Both Rules Artist", Path: dir}
	if v := e.makeImageDuplicateChecker()(t.Context(), a, RuleConfig{}); v == nil {
		t.Fatal("perceptual rule went blind to a byte-identical duplicate; with the " +
			"exact rule disabled, nothing would detect it at all")
	}
}

// TestImageDuplicateExact_NoViolationForDistinctImages guards against the rule
// firing on an artist whose fanart is simply a set of different pictures.
func TestImageDuplicateExact_NoViolationForDistinctImages(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-distinct", "Distinct Artist")

	dir := t.TempDir()
	for i, n := range []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg"} {
		createGradientJPEG(t, filepath.Join(dir, n), i)
		insertTestImage(t, db, "art-distinct", "fanart", i)
	}

	a := &artist.Artist{ID: "art-distinct", Name: "Distinct Artist", Path: dir}
	if v := e.makeImageDuplicateExactChecker()(t.Context(), a, RuleConfig{}); v != nil {
		t.Errorf("exact rule fired on three distinct images: %q", v.Message)
	}
}

// TestImageDuplicateExact_UnhashedSlotsAreNotDuplicatesOfEachOther pins the
// "empty means unknown, not equal" rule. Two rows whose content hash could not
// be determined must never be grouped together as identical, which is what a
// naive GROUP BY content_hash would do.
func TestImageDuplicateExact_UnhashedSlotsAreNotDuplicatesOfEachOther(t *testing.T) {
	members := []imageDupMember{
		{imageType: "fanart", slotIndex: 0, contentHash: ""},
		{imageType: "fanart", slotIndex: 1, contentHash: ""},
		{imageType: "fanart", slotIndex: 2, contentHash: ""},
	}
	if got := exactFanartDuplicates(members); len(got) != 0 {
		t.Errorf("unhashed slots were grouped as duplicates: %v", got)
	}
}

// TestImageDuplicateExact_TransitiveGroupCollapsesToLowestSlot: byte equality
// IS transitive, unlike perceptual similarity, so three identical files
// collapse onto the lowest slot with no representative-walking needed.
func TestImageDuplicateExact_TransitiveGroupCollapsesToLowestSlot(t *testing.T) {
	members := []imageDupMember{
		{imageType: "fanart", slotIndex: 0, contentHash: "aaa"},
		{imageType: "fanart", slotIndex: 1, contentHash: "aaa"},
		{imageType: "fanart", slotIndex: 2, contentHash: "aaa"},
		{imageType: "fanart", slotIndex: 3, contentHash: "bbb"},
	}
	got := exactFanartDuplicates(members)
	if got[0] {
		t.Error("slot 0 marked for deletion; the lowest slot of a group must be kept")
	}
	if !got[1] || !got[2] {
		t.Errorf("slots 1 and 2 should both be removable, got %v", got)
	}
	if got[3] {
		t.Error("slot 3 has a distinct hash and must not be removed")
	}
}

// TestImageDuplicateExact_OnlyFanartParticipates: single-slot types cannot
// have a within-type duplicate, and the exact rule must not reach across types.
func TestImageDuplicateExact_OnlyFanartParticipates(t *testing.T) {
	members := []imageDupMember{
		{imageType: "thumb", slotIndex: 0, contentHash: "same"},
		{imageType: "fanart", slotIndex: 0, contentHash: "same"},
	}
	if got := exactFanartDuplicates(members); len(got) != 0 {
		t.Errorf("exact rule crossed image types: %v", got)
	}
}

// --------------------------------------------------------------------------
// #2341 -- the exact fixer.
// --------------------------------------------------------------------------

// TestImageDuplicateExactFixer_RemovesCopiesKeepsLowestAndRenumbers asserts the
// on-disk outcome, not the FixResult's boolean.
func TestImageDuplicateExactFixer_RemovesCopiesKeepsLowestAndRenumbers(t *testing.T) {
	_, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-fix", "Fix Artist")

	dir := t.TempDir()
	// slot 0 and slot 1 are byte-identical; slot 2 is a distinct image that
	// must survive and be renumbered down into the freed slot.
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 9)
	distinct := readBytes(t, filepath.Join(dir, "fanart3.jpg"))

	for i := 0; i < 3; i++ {
		insertTestImage(t, db, "art-fix", "fanart", i)
	}

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artist.NewService(db), testLogger())
	a := &artist.Artist{
		ID: "art-fix", Name: "Fix Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 3,
	}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed {
		t.Fatalf("Fixed = false, want true. Message: %s", res.Message)
	}
	if res.RuleID != RuleImageDuplicateExact {
		t.Errorf("RuleID = %q, want %q", res.RuleID, RuleImageDuplicateExact)
	}

	// The kept copy is still there.
	if _, err := os.Stat(filepath.Join(dir, "fanart.jpg")); err != nil {
		t.Errorf("fanart.jpg (the kept copy) is gone: %v", err)
	}
	// Exactly two fanart files remain, contiguously numbered.
	if _, err := os.Stat(filepath.Join(dir, "fanart3.jpg")); !os.IsNotExist(err) {
		t.Errorf("fanart3.jpg should have been renumbered away, stat err = %v", err)
	}
	// The surviving distinct image must be the one now sitting in slot 1 --
	// proving the DUPLICATE was deleted and the DISTINCT image preserved,
	// not the other way round.
	got := readBytes(t, filepath.Join(dir, "fanart2.jpg"))
	if string(got) != string(distinct) {
		t.Error("fanart2.jpg is not the distinct image; the fixer deleted the wrong file " +
			"and kept a duplicate")
	}
}

// TestImageDuplicateExactFixer_NeverDeletesMerelySimilarFiles is the most
// important guard in this file, because the exact fixer DELETES FILES.
//
// The rule ships Manual by default, but auto is a legitimate mode an
// operator can opt into (and a future contributor could flip the default
// back). This guard matters because the exact fixer can run unattended when
// an operator chooses that, not because it does so by default: the danger
// didn't go away with the default, it just got more subtle.
//
// The two fanart files here are perceptually identical (same picture) but not
// byte-identical (re-encoded at a different quality). The perceptual rule is
// entitled to flag them -- but it is manual, so a human decides. The exact
// fixer must delete NOTHING here. If it were ever wired to the perceptual
// deletion set, an operator running it on auto would get a file deleted on a
// similarity judgement, which is exactly the destructive false positive the
// two-tier split exists to prevent. Both files must survive.
func TestImageDuplicateExactFixer_NeverDeletesMerelySimilarFiles(t *testing.T) {
	_, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-similar", "Similar Artist")

	dir := t.TempDir()
	createGradientJPEGQuality(t, filepath.Join(dir, "fanart.jpg"), 4, 95)
	createGradientJPEGQuality(t, filepath.Join(dir, "fanart2.jpg"), 4, 55)
	insertTestImage(t, db, "art-similar", "fanart", 0)
	insertTestImage(t, db, "art-similar", "fanart", 1)

	if string(readBytes(t, filepath.Join(dir, "fanart.jpg"))) ==
		string(readBytes(t, filepath.Join(dir, "fanart2.jpg"))) {
		t.Fatal("fixture is wrong: the files must NOT be byte-identical")
	}

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artist.NewService(db), testLogger())
	a := &artist.Artist{
		ID: "art-similar", Name: "Similar Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 2,
	}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Errorf("the exact fixer deleted something on a merely-similar pair (Message: %s); "+
			"byte equality is the only thing it may act on, and it auto-runs", res.Message)
	}
	for _, n := range []string{"fanart.jpg", "fanart2.jpg"} {
		if _, statErr := os.Stat(filepath.Join(dir, n)); statErr != nil {
			t.Errorf("%s was auto-deleted on a similarity judgement: %v", n, statErr)
		}
	}
}

// TestImageDuplicateExactFixer_SkipsSharedFilesystem: destructive fixes never
// run on a library that may be shared with another writer.
func TestImageDuplicateExactFixer_SkipsSharedFilesystem(t *testing.T) {
	sharedCheck := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSConfirmed},
	}, testLogger())
	f := NewImageDuplicateFixer(nil, nil, sharedCheck, &fakeHashRecorder{}, testLogger())

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))

	a := &artist.Artist{ID: "art-shared", Name: "Shared Artist", Path: dir, LibraryID: "lib-test"}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true on a shared filesystem; destructive fixes must be skipped")
	}
	// Both files must still be on disk.
	for _, n := range []string{"fanart.jpg", "fanart2.jpg"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was deleted despite the shared-filesystem guard: %v", n, err)
		}
	}
}

// TestImageDuplicateFixer_CanFixBothRules: one fixer serves both tiers.
func TestImageDuplicateFixer_CanFixBothRules(t *testing.T) {
	f := NewImageDuplicateFixer(nil, nil, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if !f.CanFix(&Violation{RuleID: id}) {
			t.Errorf("CanFix(%q) = false, want true", id)
		}
	}
	if f.CanFix(&Violation{RuleID: RuleThumbExists}) {
		t.Error("CanFix(thumb_exists) = true; the fixer must not claim unrelated rules")
	}
}

// TestImageDuplicateExactFixer_SecondCycleKeepsDistinctArtwork is the guard for
// the stale-hash data-destruction bug, and it has to run TWO evaluation cycles
// to see it. Every other test in this file runs one, which is exactly why the
// suite was green while the fixer was destroying artwork.
//
// The mechanism: cycle 1 legitimately deletes the byte-identical copy in slot 1
// and renumbers the distinct image in slot 2 DOWN into slot 1. Slot 1's row,
// however, still carries the hash of the duplicate that used to live there --
// the file moved, the row did not. Cycle 2 then reads slot 0 and slot 1 as
// carrying the same content hash, concludes they are byte-identical, and deletes
// slot 1. Slot 1 is the distinct image. It was never a copy of anything.
//
// Persisting the hashes (the #2349 fix) is what exposes this: before, detection
// re-read every file on every evaluation and was accidentally self-correcting.
//
// The assertion is on the BYTES ON DISK after cycle 2, not on a Fixed flag or an
// error value -- the destructive version of this code reports success while
// destroying the file.
func TestImageDuplicateExactFixer_SecondCycleKeepsDistinctArtwork(t *testing.T) {
	_, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-stale", "Stale Artist")

	dir := t.TempDir()
	// slot 0 and slot 1: byte-identical. slot 2: a DISTINCT image.
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 9)
	kept := readBytes(t, filepath.Join(dir, "fanart.jpg"))
	distinct := readBytes(t, filepath.Join(dir, "fanart3.jpg"))
	if string(kept) == string(distinct) {
		t.Fatal("fixture is wrong: the distinct image must not equal the duplicated one")
	}

	for i := range 3 {
		insertTestImage(t, db, "art-stale", "fanart", i)
	}

	svc := artist.NewService(db)
	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), svc, testLogger())
	a := &artist.Artist{
		ID: "art-stale", Name: "Stale Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 3,
	}

	// Two cycles through the production path: Pipeline.FixViolation calls Fix
	// and then artistService.Update, and it is the SECOND pass over an artist
	// that has nothing left to fix which destroys the file.
	for cycle := 1; cycle <= 2; cycle++ {
		res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
		if err != nil {
			t.Fatalf("cycle %d: Fix: %v", cycle, err)
		}
		if err := svc.Update(t.Context(), a); err != nil {
			t.Fatalf("cycle %d: Update: %v", cycle, err)
		}
		if cycle == 2 && res.Fixed {
			t.Errorf("cycle 2 reported a fix (%q), but there was nothing left to de-duplicate; "+
				"it is acting on a hash that no longer describes the file in that slot", res.Message)
		}
	}

	// THE ASSERTION. Both files must be on disk, and slot 1 must still hold the
	// distinct image. If the stale hash won, fanart2.jpg is gone and only
	// fanart.jpg remains.
	survivors, err := image.DiscoverFanart(dir, "fanart.jpg")
	if err != nil {
		t.Fatalf("DiscoverFanart: %v", err)
	}
	if len(survivors) != 2 {
		t.Fatalf("DATA DESTRUCTION: %d fanart file(s) survive, want 2. The distinct image was "+
			"auto-deleted by the second exact-fixer cycle on a stale content_hash. Survivors: %v",
			len(survivors), survivors)
	}
	if got := readBytes(t, filepath.Join(dir, "fanart.jpg")); string(got) != string(kept) {
		t.Error("fanart.jpg is not the image that should have been kept")
	}
	if got := readBytes(t, filepath.Join(dir, "fanart2.jpg")); string(got) != string(distinct) {
		t.Error("DATA DESTRUCTION: fanart2.jpg is not the distinct image -- the distinct artwork " +
			"was deleted and a duplicate kept in its place")
	}
}

// TestImageDuplicateExactFixer_RenumberInvalidatesStoredHashes pins the
// mechanism the test above depends on, so that a regression is diagnosable
// rather than merely red: after a fix renumbers survivors, no fanart slot may
// still be carrying a hash computed from a file that has since moved.
//
// The fixer re-reads from disk before deleting (that is the guarantee), and it
// re-persists what it read. Either way, what must NOT survive is a row whose
// stored content hash describes a different file than the one in its slot.
func TestImageDuplicateExactFixer_RenumberInvalidatesStoredHashes(t *testing.T) {
	_, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-inv", "Inv Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 9)
	for i := range 3 {
		insertTestImage(t, db, "art-inv", "fanart", i)
	}

	svc := artist.NewService(db)
	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), svc, testLogger())
	a := &artist.Artist{
		ID: "art-inv", Name: "Inv Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 3,
	}
	if _, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if err := svc.Update(t.Context(), a); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Whatever slot 1 now stores, it must agree with the bytes actually in slot
	// 1's file. Compare against the truth on disk rather than against a
	// hard-coded expectation, so this holds whether the hash was cleared or
	// rewritten.
	var stored string
	if err := db.QueryRow(
		`SELECT content_hash FROM artist_images WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = 1`,
		"art-inv",
	).Scan(&stored); err != nil {
		t.Fatalf("reading slot 1 content_hash: %v", err)
	}
	onDisk, err := image.HashFile(filepath.Join(dir, "fanart2.jpg"), false)
	if err != nil {
		t.Fatalf("hashing the file now in slot 1: %v", err)
	}
	if stored != "" && stored != onDisk.Content {
		t.Errorf("slot 1 stores content_hash %q but its file hashes to %q -- the row describes a "+
			"file the slot no longer holds, which is what gets distinct artwork auto-deleted",
			stored, onDisk.Content)
	}
}

// TestImageDuplicateExactFixer_RefusesToDeleteOnAnUnverifiedHash pins the layer
// that the safety actually rests on: the fixer re-derives hashes from disk
// before deleting, so no stored hash -- however it got there -- can talk it into
// removing a file.
//
// This is the case call-site invalidation can NEVER cover. The stale hash here
// is not produced by any Stillwater code path: it is what a user gets by
// replacing fanart2.jpg over a network share. Nothing in this process observes
// that, and a rescan deliberately preserves the hash columns (see
// UpsertAll's ON CONFLICT), so the wrong hash simply persists. Invalidating on
// renumber does nothing for it.
//
// Simulated by writing slot 0's content hash onto slot 1's row while slot 1's
// file is a DISTINCT image. A fixer that trusts the DB reads them as
// byte-identical and deletes slot 1. A fixer that re-reads sees two different
// files and deletes nothing.
func TestImageDuplicateExactFixer_RefusesToDeleteOnAnUnverifiedHash(t *testing.T) {
	_, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-oob", "Out Of Band Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 9) // DISTINCT
	insertTestImage(t, db, "art-oob", "fanart", 0)
	insertTestImage(t, db, "art-oob", "fanart", 1)

	slot0, err := image.HashFile(filepath.Join(dir, "fanart.jpg"), true)
	if err != nil {
		t.Fatalf("hashing slot 0: %v", err)
	}
	// The lie: slot 1's row claims to hold a byte-identical copy of slot 0.
	// Its file does not. This is the state an out-of-band file swap leaves.
	for _, slot := range []int{0, 1} {
		if _, err := db.Exec(
			`UPDATE artist_images SET phash = ?, content_hash = ?
			 WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = ?`,
			image.HashHex(slot0.Perceptual), slot0.Content, "art-oob", slot,
		); err != nil {
			t.Fatalf("seeding stale hash for slot %d: %v", slot, err)
		}
	}

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artist.NewService(db), testLogger())
	a := &artist.Artist{
		ID: "art-oob", Name: "Out Of Band Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 2,
	}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Errorf("the fixer deleted a file on a hash it never verified against disk (%q)", res.Message)
	}

	// THE ASSERTION: both files still on disk. The two images are distinct; the
	// only thing that said otherwise was a row in a table.
	for _, n := range []string{"fanart.jpg", "fanart2.jpg"} {
		if _, statErr := os.Stat(filepath.Join(dir, n)); statErr != nil {
			t.Errorf("DATA DESTRUCTION: %s was deleted on an unverified stored hash: %v", n, statErr)
		}
	}
}

// TestImageDuplicateExactFixer_InvalidationFailureAbortsBeforeDestructiveRename
// is the F1 regression test (PR #2458 review, CodeRabbit CRITICAL). Before the
// fix, image.RenumberFanart invalidated hashes AFTER the on-disk renumber, so
// an invalidation-only failure -- a transient error with NOTHING to do with
// the filesystem -- could surface after the survivors were already sitting at
// their new, correct paths. The caller (deleteDuplicateFanartWithRollback)
// cannot distinguish that from a failed rename, so it rolls back by restoring
// the tombed duplicate to its ORIGINAL path -- a path a just-renumbered
// survivor can now occupy, silently overwriting distinct artwork with content
// that was supposed to be permanently gone.
//
// This sets up exactly that collision: slot 0 and slot 1 are byte-identical
// (slot 1 is tombed for deletion), slot 2 is a DISTINCT image that renumbers
// down into fanart2.jpg -- the EXACT path the tombed duplicate used to
// occupy. The hash recorder's InvalidateImageHashes call is forced to fail.
func TestImageDuplicateExactFixer_InvalidationFailureAbortsBeforeDestructiveRename(t *testing.T) {
	_, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-f1", "F1 Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 9)
	duplicate := readBytes(t, filepath.Join(dir, "fanart2.jpg"))
	distinct := readBytes(t, filepath.Join(dir, "fanart3.jpg"))

	for i := 0; i < 3; i++ {
		insertTestImage(t, db, "art-f1", "fanart", i)
	}

	sentinel := errors.New("db is down")
	recorder := &fakeHashRecorder{invalidateErr: sentinel}
	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), recorder, testLogger())
	a := &artist.Artist{
		ID: "art-f1", Name: "F1 Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 3,
	}

	_, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
	if err == nil {
		t.Fatal("Fix() = nil error, want an error (hash invalidation failed)")
	}

	// THE CRITICAL ASSERTIONS. With the bug: invalidation runs AFTER the
	// rename, so the rename succeeds first (fanart3.jpg's distinct bytes move
	// into fanart2.jpg), invalidation then fails, and the rollback restores
	// the tombed duplicate onto fanart2.jpg -- overwriting the distinct
	// image. With the fix: invalidation runs FIRST and fails, so the rename
	// never happens at all; every file must be exactly where it started.
	if _, statErr := os.Stat(filepath.Join(dir, "fanart3.jpg")); statErr != nil {
		t.Errorf("fanart3.jpg (the distinct survivor) is gone -- a destructive renumber proceeded "+
			"despite the invalidation failure: %v", statErr)
	} else if got := readBytes(t, filepath.Join(dir, "fanart3.jpg")); string(got) != string(distinct) {
		t.Error("fanart3.jpg's bytes changed even though the path still exists")
	}
	if got := readBytes(t, filepath.Join(dir, "fanart2.jpg")); string(got) != string(duplicate) {
		t.Errorf("fanart2.jpg no longer holds the original duplicate's bytes -- got the %s image; "+
			"the renumber-then-rollback sequence overwrote distinct artwork on the strength of a "+
			"cache-write failure",
			map[bool]string{true: "distinct", false: "unrecognized"}[string(got) == string(distinct)])
	}
	if _, statErr := os.Stat(filepath.Join(dir, "fanart.jpg")); statErr != nil {
		t.Errorf("fanart.jpg (the kept original) is gone: %v", statErr)
	}
}

// TestImageDuplicateExactFixer_StaleTmpSweepFailureStrandsNothing is the B1
// regression test (PR #2458 review round 2, adversarial-review finding,
// BLOCKING). Reproduces the reviewer's fault injection with ZERO code
// mutation: a non-empty directory squats the ".tmp" path a mid-list survivor
// needs, so the leftover-temp-file sweep's os.Remove fails with ENOTEMPTY --
// a realistic fault on a real filesystem, no fault-injection seam required.
//
// Before the fix, this sweep ran INSIDE the staging loop, immediately before
// each survivor's stage-rename, and its failure branch returned WITHOUT
// rolling back survivors already staged earlier in the same loop -- the one
// asymmetric exit in a function whose entire premise is two-phase crash
// safety (the adjacent os.Rename failure branch four lines below it DOES
// roll back). The stranded survivors sat at "fanart_renumber_N.ext.tmp",
// invisible to DiscoverFanart; the caller's restoreStaged() only knows about
// tombed duplicates, so it un-tombed the duplicate while the kept primary and
// distinct artwork stayed vanished -- and the NEXT renumber's stale-tmp sweep
// would then unlink those stranded originals PERMANENTLY.
//
// The fix hoists the whole sweep out of the staging loop into its own pass
// that runs before ANY survivor is staged, so a sweep failure now always
// happens before the first file moves: nothing to strand, nothing to roll
// back, matching the F1 invalidate-before-rename reorder's exact reasoning.
func TestImageDuplicateExactFixer_StaleTmpSweepFailureStrandsNothing(t *testing.T) {
	_, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-b1", "B1 Artist")

	dir := t.TempDir()
	// Mirrors the reviewer's reproduction: fanart.jpg=A (kept primary),
	// fanart2.jpg=A (duplicate, will be tombed), fanart3.jpg=B (distinct),
	// fanart4.jpg=C, fanart5.jpg=D.
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 3)
	createGradientJPEG(t, filepath.Join(dir, "fanart4.jpg"), 4)
	createGradientJPEG(t, filepath.Join(dir, "fanart5.jpg"), 5)

	original := map[string][]byte{
		"fanart.jpg":  readBytes(t, filepath.Join(dir, "fanart.jpg")),
		"fanart2.jpg": readBytes(t, filepath.Join(dir, "fanart2.jpg")),
		"fanart3.jpg": readBytes(t, filepath.Join(dir, "fanart3.jpg")),
		"fanart4.jpg": readBytes(t, filepath.Join(dir, "fanart4.jpg")),
		"fanart5.jpg": readBytes(t, filepath.Join(dir, "fanart5.jpg")),
	}

	for i := 0; i < 5; i++ {
		insertTestImage(t, db, "art-b1", "fanart", i)
	}

	// Survivors (slot 1 is the duplicate, tombed) are, in order:
	// fanart.jpg(0), fanart3.jpg(1), fanart4.jpg(2), fanart5.jpg(3).
	// renumberFanartFiles names survivor i's staging file
	// "fanart_renumber_{i}.jpg.tmp"; squat index 2's (fanart4.jpg's) path
	// with a non-empty directory so os.Remove fails with ENOTEMPTY -- no
	// source mutation, a realistic filesystem fault.
	squatPath := filepath.Join(dir, "fanart_renumber_2.jpg.tmp")
	if err := os.Mkdir(squatPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(squatPath, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artist.NewService(db), testLogger())
	a := &artist.Artist{
		ID: "art-b1", Name: "B1 Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 5,
	}

	_, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
	if err == nil {
		t.Fatal("Fix() = nil error, want an error (the stale-tmp sweep must fail on the squatted path)")
	}

	// THE ASSERTION: every original file must be at its ORIGINAL path with
	// its ORIGINAL bytes. Nothing may be stranded at a .tmp path, and no
	// distinct artwork may have vanished or been overwritten.
	for name, want := range original {
		got, statErr := os.ReadFile(filepath.Join(dir, name))
		if statErr != nil {
			t.Errorf("%s is gone: %v -- a survivor was stranded or lost by the failed renumber", name, statErr)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s bytes changed -- got overwritten by a rollback that clobbered it", name)
		}
	}

	// No stranded .tmp files anywhere in dir (the squatted directory itself
	// is expected and cleaned up by t.TempDir(); this checks for renamed-away
	// survivors, which is the actual data-loss shape).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") && e.Name() != "fanart_renumber_2.jpg.tmp" {
			t.Errorf("found an unexpected stranded temp file: %s -- a survivor was staged despite the "+
				"sweep failure", e.Name())
		}
	}
}

// TestImageDuplicateExactChecker_EmptySurvivorsInvalidationPreventsStaleMatch
// is the F2 regression test (PR #2458 review, CodeRabbit HIGH). Deleting the
// last/only fanart image for an artist calls image.RenumberFanart with an
// empty survivors slice; before the fix, that path returned before ever
// calling the hash invalidator, so the deleted slot's stale hash lingered in
// the DB. If a genuinely distinct image is later uploaded into the freed
// slot, and the ORIGINAL content is separately re-added elsewhere (e.g.
// re-fetched into another slot), the exact-duplicate checker's stored-hash
// fast path (fresh=false) can compare the new upload's STALE row against the
// re-added original's real hash and find them equal -- flagging a genuinely
// distinct, never-before-seen image as a duplicate of content that was
// supposed to be gone.
func TestImageDuplicateExactChecker_EmptySurvivorsInvalidationPreventsStaleMatch(t *testing.T) {
	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-f2", "F2 Artist")
	svc := artist.NewService(db)

	dir := t.TempDir()
	originalPath := filepath.Join(dir, "fanart.jpg")
	createGradientJPEG(t, originalPath, 0)
	original := readBytes(t, originalPath)

	// Slot 0 holds the artist's only fanart image, hashed and persisted.
	insertTestImage(t, db, "art-f2", "fanart", 0)
	oh, err := image.HashFile(originalPath, true)
	if err != nil {
		t.Fatalf("hashing original: %v", err)
	}
	if err := svc.UpdateImageHashes(t.Context(), "art-f2", "fanart", 0, image.HashHex(oh.Perceptual), oh.Content); err != nil {
		t.Fatalf("persisting slot 0 hash: %v", err)
	}

	// Delete the artist's only fanart image, then renumber with an empty
	// survivors slice -- exactly what a caller does after deleting the last
	// remaining fanart file (see internal/api/handlers_backdrop.go and
	// friends).
	if rmErr := os.Remove(originalPath); rmErr != nil {
		t.Fatal(rmErr)
	}
	if renumberErr := image.RenumberFanart(t.Context(), svc, "art-f2", dir, "fanart.jpg", nil, false); renumberErr != nil {
		t.Fatalf("RenumberFanart(empty survivors): %v", renumberErr)
	}

	// A genuinely DISTINCT image is uploaded into the now-freed slot 0.
	createGradientJPEG(t, originalPath, 9)
	distinct := readBytes(t, originalPath)
	if string(distinct) == string(original) {
		t.Fatal("fixture bug: distinct upload is byte-identical to the original")
	}

	// Separately, the ORIGINAL content is re-added at slot 1 (e.g. re-fetched
	// from a provider or restored from another platform).
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), original)
	insertTestImage(t, db, "art-f2", "fanart", 1)

	a := &artist.Artist{ID: "art-f2", Name: "F2 Artist", Path: dir}
	v := e.makeImageDuplicateExactChecker()(t.Context(), a, RuleConfig{})
	if v != nil {
		t.Errorf("got a false exact-duplicate violation: %s -- slot 0 now holds a genuinely distinct "+
			"image, but its pre-deletion hash was never invalidated when the last fanart was removed, "+
			"so the stale hash matched the re-added original now sitting at slot 1", v.Message)
	}
}
