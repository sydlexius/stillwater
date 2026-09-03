package rule

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// #3015: the two rule-fixer delete paths must record operator delete intent,
// and neither may suppress its own subsequent push.
//
// These fixers are a DIFFERENT ACTOR CLASS from the API delete handlers covered
// by internal/api/delete_intent_wiring_test.go. They are operator-TRIGGERED but
// system-INITIATED: the operator asks the rule engine to fix something and the
// engine picks the files. That difference is what makes the placement of the
// marker a design question rather than a missing call, and each of the three
// properties below is one half of that design pinned by a test:
//
//  1. THE MARK HAPPENS at all, on both paths, so a concurrent push declines to
//     resurrect what the fixer removed.
//  2. THE ROLLBACK PATH NEVER MARKS. deleteDuplicateFanartWithRollback restores
//     tombed files when RenumberFanart fails; a marker written before staging
//     would still be live for DeleteIntentRetention afterwards, asserting the
//     operator wanted a file gone that this code deliberately put back.
//  3. NEITHER FIXER SELF-SUPPRESSES. Two independent properties carry this, and
//     both are asserted rather than argued: the ORDERING (a push stamped after
//     the mark is not covered by it) and the ROUTING (both fixers leave
//     FixResult.ImageType empty, so the pipeline sends their push to
//     PublishMetadata, which never reaches reassertLocalImage).
//
// Every test uses its own t.TempDir, so the process-global marker store keyed by
// cleaned directory cannot leak between them. assertNoFixerIntentYet pins that
// as a PRECONDITION: without it a leaked marker makes a positive assertion pass
// no matter what the fixer did, and -- worse for the rollback test -- a leaked
// marker would make its negative assertion FAIL for the right reason by
// accident, which is the same false-verification trap in reverse.

// assertNoFixerIntentYet fails unless dir carries no delete marker of any age
// for imageType.
func assertNoFixerIntentYet(t *testing.T, dir, imageType string) {
	t.Helper()
	if img.DeleteIntentAfter(dir, imageType, time.Time{}) {
		t.Fatalf("precondition failed: %s already carries a %s delete marker before the fixer runs, so "+
			"every assertion below would be reading a marker this test did not write", dir, imageType)
	}
}

// assertFixerIntentSince fails unless a delete marker for (dir, imageType) was
// recorded at or after since -- exactly the test reassertLocalImage applies.
func assertFixerIntentSince(t *testing.T, dir, imageType string, since time.Time, callSite string) {
	t.Helper()
	if !img.DeleteIntentAfter(dir, imageType, since) {
		t.Fatalf("no %s delete marker recorded for %s by %s: a push already in flight would read ENOENT, "+
			"find no operator intent, and restore the artwork the fixer just removed (#2712/#3015)",
			imageType, dir, callSite)
	}
}

// newDupFixerFor builds an ImageDuplicateFixer over the given recorder with no
// database, no platform service, and a non-shared filesystem. The tests below
// call deleteDuplicateFanartWithRollback directly rather than through Fix so the
// deletion set is chosen by the test instead of by duplicate detection -- the
// marker's placement inside that function is what is under test, and routing it
// through hash discovery would make a failure ambiguous between "the fixer did
// not mark" and "the fixer found nothing to delete".
func newDupFixerFor(t *testing.T, recorder imageHashRecorder) *ImageDuplicateFixer {
	t.Helper()
	return NewImageDuplicateFixer(nil, nil, nonSharedFSCheck(), recorder, testLogger())
}

// dupArtistDir writes three distinct fanart files and returns the artist rooted
// at their directory.
func dupArtistDir(t *testing.T) (*artist.Artist, string) {
	t.Helper()
	dir := t.TempDir()
	for i, name := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"} {
		createGradientJPEG(t, filepath.Join(dir, name), i)
	}
	return &artist.Artist{ID: "art-3015", Name: "Delete Intent Fixer", Path: dir, LibraryID: "lib-test"}, dir
}

// TestDeleteDuplicateFanart_CommitMarksDeleteIntent is the positive half of the
// duplicate path: a committed deletion records intent.
func TestDeleteDuplicateFanart_CommitMarksDeleteIntent(t *testing.T) {
	a, dir := dupArtistDir(t)
	assertNoFixerIntentYet(t, dir, "fanart")

	f := newDupFixerFor(t, &fakeHashRecorder{})

	// The bytes that must NOT survive anywhere in the directory. Asserting only
	// on filenames would miss a renumber that shuffled the deleted content onto
	// a surviving path.
	doomed := readBytes(t, filepath.Join(dir, "fanart1.jpg"))

	before := time.Now()
	removed, err := f.deleteDuplicateFanartWithRollback(t.Context(), a, "fanart.jpg", false, map[int]bool{1: true})
	if err != nil {
		t.Fatalf("deleteDuplicateFanartWithRollback: %v", err)
	}

	// PRECONDITION on the fixture, not just the outcome: a run that removed
	// nothing would trivially satisfy "no tomb left behind" below and would tell
	// us nothing about the marker.
	if len(removed) != 1 {
		t.Fatalf("precondition failed: the fixer removed %d file(s), want exactly 1; with no deletion "+
			"there is no commit point and nothing for this test to assert", len(removed))
	}
	// Non-kodi numbering names index 1 "fanart2.jpg" (FanartFilename adds one),
	// so the two survivors compact onto fanart.jpg and fanart2.jpg.
	for _, name := range []string{"fanart.jpg", "fanart2.jpg"} {
		if _, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil {
			t.Errorf("survivor %s is missing after a committed duplicate delete: %v", name, statErr)
			continue
		}
		if got := readBytes(t, filepath.Join(dir, name)); string(got) == string(doomed) {
			t.Errorf("%s holds the bytes of the file the fixer reported deleting; the deletion this "+
				"marker vouches for did not actually happen", name)
		}
	}
	if entries, readErr := os.ReadDir(dir); readErr == nil {
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".tmp" {
				t.Errorf("a staged tomb (%s) survived the commit, so the tomb-unlink loop the marker "+
					"guards did not run to completion", e.Name())
			}
		}
	}

	assertFixerIntentSince(t, dir, "fanart", before, "deleteDuplicateFanartWithRollback's commit point")
}

// TestDeleteDuplicateFanart_RollbackWritesNoMarker is the #3015 rollback
// criterion, and it is the reason the mark sits at the commit point rather than
// before the stage loop.
//
// RenumberFanart is forced to fail at its FIRST step -- InvalidateImageHashes,
// which runs before anything moves on disk -- so the function takes its
// restoreStaged path with the tombs still present. Two properties must hold
// together: no marker on record (the marker must not outlive a rollback), and
// every staged file back at its original path (the rollback actually ran).
// Asserting only the first would pass for a fixer that never staged anything.
func TestDeleteDuplicateFanart_RollbackWritesNoMarker(t *testing.T) {
	a, dir := dupArtistDir(t)
	assertNoFixerIntentYet(t, dir, "fanart")

	// Content identity, captured before the run: "the file exists again" is a
	// weaker claim than "the operator's bytes are back at that path".
	want := map[string][]byte{}
	for _, name := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"} {
		want[name] = readBytes(t, filepath.Join(dir, name))
	}

	sentinel := errors.New("db is down")
	f := newDupFixerFor(t, &fakeHashRecorder{invalidateErr: sentinel})

	before := time.Now()
	_, err := f.deleteDuplicateFanartWithRollback(t.Context(), a, "fanart.jpg", false, map[int]bool{1: true})
	if err == nil {
		t.Fatal("precondition failed: the fixer returned no error, so the rollback path never ran and " +
			"this test asserts nothing about it")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("precondition failed: the fixer failed with %v, not the injected invalidation error; the "+
			"run took some other exit and may not be the pre-commit rollback path", err)
	}

	// THE #3015 ROLLBACK ASSERTION. Asked with a zero `since`, so it is true for
	// a marker of ANY age: this is not "no marker newer than the run", it is "no
	// marker at all". A marker here would stay live for DeleteIntentRetention,
	// telling the next push that the operator wanted a file gone that the line
	// below proves is back on disk.
	if img.DeleteIntentAfter(dir, "fanart", time.Time{}) {
		t.Error("the rollback path recorded delete intent: the restored files are now covered by a marker " +
			"claiming the operator deleted them, so a push finding one of them missing for a GENUINE peer " +
			"reason within the retention window will decline to repair it (#3015)")
	}
	// Belt and braces on the same claim from the other direction: nothing was
	// written at or after the run began either.
	if img.DeleteIntentAfter(dir, "fanart", before) {
		t.Error("a delete marker was recorded during a run that rolled back")
	}

	// The rollback completed: every staged file is back at its original path
	// with its original bytes.
	for name, bytesWanted := range want {
		p := filepath.Join(dir, name)
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("%s was not restored by the rollback: %v -- with the file gone, the marker assertion "+
				"above passes for the wrong reason", name, statErr)
			continue
		}
		if got := readBytes(t, p); string(got) != string(bytesWanted) {
			t.Errorf("%s was restored with different bytes than it started with", name)
		}
	}
	if entries, readErr := os.ReadDir(dir); readErr == nil {
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".tmp" {
				t.Errorf("a staged tomb (%s) is still on disk, so restoreStaged did not put every "+
					"staged file back", e.Name())
			}
		}
	}
}

// TestDeleteDuplicateFanart_ZeroStagedMarksNothing is the duplicate path's
// counterpart to TestExtraneousImagesFixer_CleanDirectoryMarksNothing.
//
// toDelete and this call's own DiscoverFanart enumeration can disagree: the
// caller (Fix) re-detects duplicates against a fresh hash pass, while
// deleteDuplicateFanartWithRollback re-discovers the directory independently,
// and the directory can shrink between the two (an operator delete, a peer, a
// prior fixer in the same run). Naming an index the discovery does not yield
// reproduces that disagreement directly: the stage loop tombs nothing,
// RenumberFanart still succeeds (there is nothing to renumber away), and
// without the len(staged) guard the function would mark a delete that never
// happened.
func TestDeleteDuplicateFanart_ZeroStagedMarksNothing(t *testing.T) {
	a, dir := dupArtistDir(t)
	assertNoFixerIntentYet(t, dir, "fanart")

	// Content identity, captured before the run. RenumberFanart renumbers
	// every SURVIVOR positionally regardless of whether anything was staged
	// for deletion, so a zero-staged run can still rename files on disk (e.g.
	// fanart1.jpg -> fanart2.jpg to close a gap that never opened) -- the
	// precondition below must check that no BYTES were lost, not that the
	// original filenames are unchanged.
	want := map[string][]byte{}
	for _, name := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"} {
		want[name] = readBytes(t, filepath.Join(dir, name))
	}

	f := newDupFixerFor(t, &fakeHashRecorder{})

	// dupArtistDir writes exactly 3 files (indices 0-2), so index 7 is not a
	// key DiscoverFanart's result can produce -- toDelete[7] is never
	// consulted by the stage loop, which ranges over the discovered paths.
	removed, err := f.deleteDuplicateFanartWithRollback(t.Context(), a, "fanart.jpg", false, map[int]bool{7: true})
	if err != nil {
		t.Fatalf("deleteDuplicateFanartWithRollback: %v", err)
	}

	// PRECONDITION: the fixture must actually reproduce the zero-staged shape,
	// not merely fail to see it. Without this, a fixer that staged and removed
	// something would still make the "no marker" assertion below trivially
	// pass for the wrong reason.
	if len(removed) != 0 {
		t.Fatalf("precondition failed: the fixer removed %d file(s), want 0; toDelete named an index the "+
			"discovery evidently does yield, so this run does not exercise the zero-staged shape", len(removed))
	}

	// Every byte sequence that existed before the run must still exist
	// somewhere in the directory afterward, and no staged tomb may remain --
	// together these prove nothing was actually unlinked, independent of
	// which filename each survivor landed on.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("precondition failed: reading %s: %v", dir, readErr)
	}
	if len(entries) != len(want) {
		t.Fatalf("precondition failed: directory holds %d entries after the run, want %d -- a file was lost "+
			"even though the fixer reported removing nothing", len(entries), len(want))
	}
	got := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("a staged tomb (%s) survived a run that reported removing nothing", e.Name())
			continue
		}
		got[e.Name()] = readBytes(t, filepath.Join(dir, e.Name()))
	}
	for wantName, wantBytes := range want {
		found := false
		for _, gotBytes := range got {
			if string(gotBytes) == string(wantBytes) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("precondition failed: the bytes originally at %s are gone from %s -- something was "+
				"unlinked despite the fixer reporting zero removals", wantName, dir)
		}
	}

	if img.DeleteIntentAfter(dir, "fanart", time.Time{}) {
		t.Error("a fanart delete marker was recorded for a run that staged and removed nothing: every push " +
			"for this artist in the next " + img.DeleteIntentRetention.String() + " will decline to repair a " +
			"genuine peer clobber of fanart it never actually caused (#3015 hostile round 1, MAJOR-1)")
	}
}

// TestExtraneousImagesFixer_MarksDeleteIntentForEveryCanonicalType covers the
// second fixer path. The marker key carries an image TYPE and no filename, and
// the files this fixer removes are by construction outside the canonical name
// set, so it marks the whole canonical set -- asserted here for EVERY entry of
// img.AllSlots rather than for "fanart" alone, because a fix that marked only
// one type would pass a single-type assertion while leaving the other three
// resurrectable.
func TestExtraneousImagesFixer_MarksDeleteIntentForEveryCanonicalType(t *testing.T) {
	dir := t.TempDir()
	for _, slot := range img.AllSlots {
		assertNoFixerIntentYet(t, dir, slot)
	}
	// One canonical file that must SURVIVE, and one extraneous file that must go.
	// Without the survivor a fixer that deleted the whole directory would pass.
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "backdrop_old.png"), 1)

	f := NewExtraneousImagesFixer(nil, nonSharedFSCheck(), testLogger())
	a := &artist.Artist{ID: "art-extraneous", Name: "Extraneous", Path: dir, LibraryID: "lib-test"}

	before := time.Now()
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleExtraneousImages})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed {
		t.Fatalf("precondition failed: the fixer removed nothing (%q), so it never reached the marking "+
			"branch and this test asserts nothing", res.Message)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "backdrop_old.png")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("precondition failed: the extraneous file is still on disk (stat err = %v), so no "+
			"deletion occurred", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "fanart.jpg")); statErr != nil {
		t.Errorf("the canonical fanart was deleted as extraneous: %v", statErr)
	}

	for _, slot := range img.AllSlots {
		assertFixerIntentSince(t, dir, slot, before, "ExtraneousImagesFixer.Fix")
	}
}

// TestExtraneousImagesFixer_CleanDirectoryMarksNothing is the other half of the
// lazy guard. A fixer that marked at function entry would suppress repairs for
// an artist whose directory turned out to need no work at all -- and because
// this fixer runs across the whole library in auto mode, that is every clean
// artist, on every evaluation.
func TestExtraneousImagesFixer_CleanDirectoryMarksNothing(t *testing.T) {
	dir := t.TempDir()
	for _, slot := range img.AllSlots {
		assertNoFixerIntentYet(t, dir, slot)
	}
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)

	f := NewExtraneousImagesFixer(nil, nonSharedFSCheck(), testLogger())
	a := &artist.Artist{ID: "art-clean", Name: "Clean", Path: dir, LibraryID: "lib-test"}

	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleExtraneousImages})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Fatalf("precondition failed: the fixer reported a deletion in a clean directory (%q), so the "+
			"no-deletion case is not what was exercised", res.Message)
	}

	for _, slot := range img.AllSlots {
		if img.DeleteIntentAfter(dir, slot, time.Time{}) {
			t.Errorf("a %s delete marker was written for a directory the fixer removed nothing from: "+
				"every push for this artist in the next %s will decline to repair a genuine peer clobber",
				slot, img.DeleteIntentRetention)
		}
	}
}

// --------------------------------------------------------------------------
// #3015 AC: "a fixer's own subsequent push is proven NOT to self-suppress, by a
// test rather than by argument". Two independent properties carry that, and
// each is asserted separately below so a change that breaks either one is
// caught on its own terms. The publisher-side half -- that a push stamped after
// a marker still repairs a genuine peer clobber -- lives in
// internal/publish/delete_intent_prologue_test.go, because only that package
// can drive a real Publisher through a real repair.
// --------------------------------------------------------------------------

// TestDeleteIntentFixers_OwnPushRoutesToMetadata is the ROUTING half.
//
// publishAfterFix and publishAccumulated both branch on FixResult.ImageType:
// non-empty routes to SyncImageToPlatforms (which stamps a pushScope and can
// reach reassertLocalImage), empty routes to PublishMetadata (which never calls
// reassertLocalImage at all and so never reads a delete marker). Both fixers
// leave it empty, so their own push cannot consult the marker they just wrote
// no matter what the clock did.
//
// This is asserted against a FixResult produced by a REAL end-to-end Fix, not a
// hand-built struct: a struct literal would only restate the belief this test
// exists to check. The duplicate fixer runs against a real database with two
// byte-identical fanart files, so the marker write and the routing fact come
// out of the same run.
func TestDeleteIntentFixers_OwnPushRoutesToMetadata(t *testing.T) {
	_, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-route", "Route Artist")

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	writeBytes(t, filepath.Join(dir, "fanart2.jpg"), readBytes(t, filepath.Join(dir, "fanart.jpg")))
	for i := 0; i < 2; i++ {
		insertTestImage(t, db, "art-route", "fanart", i)
	}
	assertNoFixerIntentYet(t, dir, "fanart")

	dupFixer := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artist.NewService(db), testLogger())
	a := &artist.Artist{
		ID: "art-route", Name: "Route Artist", Path: dir, LibraryID: "lib-test",
		FanartExists: true, FanartCount: 2,
	}

	before := time.Now()
	dupRes, err := dupFixer.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicateExact})
	if err != nil {
		t.Fatalf("ImageDuplicateFixer.Fix: %v", err)
	}
	if !dupRes.Fixed {
		t.Fatalf("precondition failed: the duplicate fixer removed nothing (%q), so no marker was written "+
			"and the routing question does not arise for this run", dupRes.Message)
	}
	// The marker really was written by this run, so the ImageType assertion
	// below is about a run that actually recorded intent.
	assertFixerIntentSince(t, dir, "fanart", before, "ImageDuplicateFixer.Fix (end to end)")

	if dupRes.ImageType != "" {
		t.Errorf("ImageDuplicateFixer.Fix returned ImageType %q; a non-empty ImageType routes the "+
			"fixer's own push through SyncImageToPlatforms, which reaches reassertLocalImage and reads "+
			"the marker this same run just wrote -- the fixer would suppress its own repair (#3015)",
			dupRes.ImageType)
	}

	// The extraneous fixer, same question, its own run.
	extDir := t.TempDir()
	createGradientJPEG(t, filepath.Join(extDir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(extDir, "backdrop_old.png"), 1)
	extFixer := NewExtraneousImagesFixer(nil, nonSharedFSCheck(), testLogger())
	extRes, err := extFixer.Fix(t.Context(), &artist.Artist{
		ID: "art-route-ext", Name: "Route Ext", Path: extDir, LibraryID: "lib-test",
	}, &Violation{RuleID: RuleExtraneousImages})
	if err != nil {
		t.Fatalf("ExtraneousImagesFixer.Fix: %v", err)
	}
	if !extRes.Fixed {
		t.Fatalf("precondition failed: the extraneous fixer removed nothing (%q)", extRes.Message)
	}
	if extRes.ImageType != "" {
		t.Errorf("ExtraneousImagesFixer.Fix returned ImageType %q; see above -- a non-empty ImageType "+
			"routes its own push into the repair that reads its own marker", extRes.ImageType)
	}
}

// TestDeleteIntentFixers_MarkDoesNotCoverALaterPush is the ORDERING half.
//
// It is the property the API handlers rely on exclusively, and the one the
// fixers would rely on if their routing ever changed: a push stamping snapAt
// AFTER the mark is not covered by that mark, because DeleteIntentAfter's test
// is "recorded at or after since" and the mark is strictly before.
//
// The snapAt here is taken from a REAL fixer run's completion rather than from
// a bare time.Now() next to a bare MarkDeleteIntent, so what is measured is the
// actual gap the production sequence produces -- if a fixer ever marked at a
// point that landed after its caller's push began, this reads it.
func TestDeleteIntentFixers_MarkDoesNotCoverALaterPush(t *testing.T) {
	a, dir := dupArtistDir(t)
	assertNoFixerIntentYet(t, dir, "fanart")

	f := newDupFixerFor(t, &fakeHashRecorder{})

	before := time.Now()
	if _, err := f.deleteDuplicateFanartWithRollback(t.Context(), a, "fanart.jpg", false, map[int]bool{1: true}); err != nil {
		t.Fatalf("deleteDuplicateFanartWithRollback: %v", err)
	}
	// PRECONDITION: the marker is live right now. Without this the assertion
	// below passes for a fixer that never marked at all, which is the exact
	// vacuous-pass shape this whole file guards against.
	assertFixerIntentSince(t, dir, "fanart", before, "deleteDuplicateFanartWithRollback")

	// The fixer's own push, stamped at ITS function entry, which is necessarily
	// after the fixer returned. The clock has a finite resolution, so a sleep
	// short enough not to slow the suite but long enough to exceed it keeps
	// "strictly after" from degenerating into "equal", which DeleteIntentAfter
	// deliberately treats as concurrent.
	time.Sleep(2 * time.Millisecond)
	snapAt := time.Now()

	if img.DeleteIntentAfter(dir, "fanart", snapAt) {
		t.Fatal("the fixer's marker covers a push stamped AFTER it: the fixer would decline to repair a " +
			"genuine peer clobber caused by its own subsequent push, silently losing the #2698 protection " +
			"this mechanism exists to preserve (#3015)")
	}

	// The other direction, so this is a statement about ORDER and not about the
	// marker having quietly expired or never existed: the same marker still
	// answers true for a baseline taken before the fixer ran.
	if !img.DeleteIntentAfter(dir, "fanart", before) {
		t.Fatal("the marker is not live for a pre-run baseline either, so the stand-down above proves " +
			"nothing about ordering -- there is simply no marker to consult")
	}
}
