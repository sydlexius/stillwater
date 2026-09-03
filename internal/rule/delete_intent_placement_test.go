package rule

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/image/deleteintenttest"
)

// #3015 PLACEMENT. The tests in delete_intent_fixer_test.go assert that a
// marker EXISTS once a fixer returns. That is a strictly weaker claim than the
// one the design makes, and the gap is the whole bug: code that marks AFTER its
// unlink satisfies it just as well as code that marks before, while leaving the
// window img.MarkDeleteIntent's doc comment describes -- the file gone, the
// intent not yet visible, an in-flight push free to restore it.
//
// Measured: relocating both rule-fixer marks to after their unlinks leaves the
// whole internal/rule suite green. The tests here are the ones that go red.
//
// They sample the marker store from INSIDE the unlink, through the unlinkHook
// seam (unlink_seam.go), using the shared probe in
// internal/image/deleteintenttest so internal/api's five call sites make the
// same assertion in the same words.

// installUnlinkHook points unlinkHook at fn for the duration of the test and
// restores the nil production value afterwards.
//
// unlinkHook is a plain package variable with no mutex -- the same shape as
// internal/image's quarantine hooks -- so a test that calls this must not run
// in parallel with another that does. None of them call t.Parallel.
func installUnlinkHook(t *testing.T, fn func(path string, remove func() error) error) {
	t.Helper()
	if unlinkHook != nil {
		t.Fatal("unlinkHook is already installed: a previous test did not restore it, so this test would " +
			"be observing that test's probe instead of its own")
	}
	unlinkHook = fn
	t.Cleanup(func() { unlinkHook = nil })
}

// TestExtraneousImagesFixer_MarksBeforeTheUnlink is the placement half for the
// extraneous fixer: the marker must already answer true for every canonical
// type at the instant the first extraneous file is unlinked.
func TestExtraneousImagesFixer_MarksBeforeTheUnlink(t *testing.T) {
	dir := t.TempDir()
	// A canonical file that must survive, so a fixer that deleted everything
	// could not pass, and one extraneous file for it to remove.
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "backdrop_old.png"), 1)

	probe := deleteintenttest.NewUnlinkProbe(t, dir, img.AllSlots...)
	installUnlinkHook(t, probe.Around)

	f := NewExtraneousImagesFixer(nil, nonSharedFSCheck(), testLogger())
	res, err := f.Fix(t.Context(), &artist.Artist{
		ID: "art-placement-ext", Name: "Placement Ext", Path: dir, LibraryID: "lib-test",
	}, &Violation{RuleID: RuleExtraneousImages})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed {
		t.Fatalf("precondition failed: the fixer removed nothing (%q), so it never reached an unlink and "+
			"the placement assertion below has no instant to measure", res.Message)
	}

	probe.AssertMarkedBeforeUnlink("ExtraneousImagesFixer.Fix")
}

// TestDeleteDuplicateFanart_MarksBeforeTheTombUnlink is the placement half for
// the duplicate fixer.
//
// This path stages by RENAMING each doomed file to a tomb and only unlinks the
// tombs after RenumberFanart commits, so the mark sits at that commit point
// rather than at the top -- a marker written before staging would outlive a
// rollback that put the files back. The property that still has to hold, and
// the one asserted here, is that the mark precedes the PERMANENT unlink: from
// the tomb unlink onward the bytes are unrecoverable, and that is the instant a
// concurrent push could find a file gone for good.
func TestDeleteDuplicateFanart_MarksBeforeTheTombUnlink(t *testing.T) {
	a, dir := dupArtistDir(t)

	probe := deleteintenttest.NewUnlinkProbe(t, dir, "fanart")
	installUnlinkHook(t, probe.Around)

	f := newDupFixerFor(t, &fakeHashRecorder{})
	removed, err := f.deleteDuplicateFanartWithRollback(t.Context(), a, "fanart.jpg", false, map[int]bool{1: true})
	if err != nil {
		t.Fatalf("deleteDuplicateFanartWithRollback: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("precondition failed: the fixer removed %d file(s), want exactly 1; with no deletion "+
			"there is no commit point and no unlink to sample", len(removed))
	}

	probe.AssertMarkedBeforeUnlink("deleteDuplicateFanartWithRollback (tomb unlink)")
}

// TestDeleteDuplicateFanart_RollbackUnlinksNothingAndMarksNothing is the
// negative half of the same class, and it pins the rollback criterion at the
// unlink rather than only at the marker store.
//
// RenumberFanart is forced to fail at InvalidateImageHashes, its first
// statement, which runs before anything moves on disk -- so the function takes
// its restoreStaged path. Two things must then be true together: nothing was
// permanently unlinked (the tombs were renamed back, not removed), and no
// marker was recorded. Asserting only the second would pass for a run that
// unlinked the files and simply forgot to mark them, which is a worse outcome
// than the one under test.
func TestDeleteDuplicateFanart_RollbackUnlinksNothingAndMarksNothing(t *testing.T) {
	a, dir := dupArtistDir(t)

	probe := deleteintenttest.NewUnlinkProbe(t, dir, "fanart")
	installUnlinkHook(t, probe.Around)

	sentinel := errors.New("db is down")
	f := newDupFixerFor(t, &fakeHashRecorder{invalidateErr: sentinel})

	_, err := f.deleteDuplicateFanartWithRollback(t.Context(), a, "fanart.jpg", false, map[int]bool{1: true})
	if err == nil {
		t.Fatal("precondition failed: the fixer returned no error, so the rollback path never ran and " +
			"this test asserts nothing about it")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("precondition failed: the fixer failed with %v, not the injected invalidation error; the "+
			"run took some other exit and may not be the pre-commit rollback path", err)
	}

	if got := probe.UnlinkCount(); got != 0 {
		t.Errorf("the rollback path permanently unlinked %d file(s) in %s; a staged duplicate delete that "+
			"rolls back must restore every tomb by rename, never remove one -- nothing puts those bytes "+
			"back (#3015)", got, dir)
	}
	probe.AssertNeverMarked("deleteDuplicateFanartWithRollback's rollback path")
}

// TestExtraneousImagesFixer_RemovalFailureIsReportedNotSilent is the #3015
// amplification case, and it is the one
// TestExtraneousImagesFixer_CleanDirectoryMarksNothing claims to cover in prose
// and does not: that test exercises a CLEAN directory, where the loop never
// reaches the mark at all.
//
// Here the directory holds an extraneous file that CANNOT be removed. The mark
// has to precede the unlink, so it happens; the removal then fails; and the
// fixer returns Fixed: false having suppressed image repair for this artist on
// all four canonical types for the full retention. In auto mode the next
// evaluation walks the same file and re-marks, so the suppression does not
// lapse on its own.
//
// The fix is not to move the mark -- marking after the unlink is the one
// placement this mechanism forbids -- but to stop the state being SILENT. This
// asserts both halves of that: an ERROR-level log naming the consequence, and a
// FixResult message that says so rather than the bare "no extraneous files to
// delete", which was actively false here (there was one, it just could not be
// deleted).
func TestExtraneousImagesFixer_RemovalFailureIsReportedNotSilent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not deny unlink, so the removal would succeed " +
			"and this test would measure nothing")
	}
	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	extraneous := filepath.Join(dir, "backdrop_old.png")
	createGradientJPEG(t, extraneous, 1)

	// A directory that denies unlink. Restored on cleanup so t.TempDir can
	// remove the tree.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("making the artist directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	buf, logger := capturingLogger()
	f := NewExtraneousImagesFixer(nil, nonSharedFSCheck(), logger)
	res, err := f.Fix(t.Context(), &artist.Artist{
		ID: "art-ro", Name: "Read Only", Path: dir, LibraryID: "lib-test",
	}, &Violation{RuleID: RuleExtraneousImages})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}

	// PRECONDITIONS. Without these the assertions below could pass on a run
	// that simply deleted the file (making the whole scenario moot) or on one
	// that never saw it.
	if res.Fixed {
		t.Fatalf("precondition failed: the fixer reported a successful deletion (%q) in a read-only "+
			"directory, so the removal-failure path was not exercised", res.Message)
	}
	if _, statErr := os.Stat(extraneous); statErr != nil {
		t.Fatalf("precondition failed: the extraneous file is gone (%v), so the removal did NOT fail and "+
			"there is no marked-but-deleted-nothing state to report", statErr)
	}
	// The state actually arose: markers are live despite nothing being deleted.
	// This is the defect being REPORTED, not one being fixed -- the mark cannot
	// move without reopening #2712.
	for _, slot := range img.AllSlots {
		if !img.DeleteIntentAfter(dir, slot, time.Time{}) {
			t.Fatalf("precondition failed: no %s marker was written, so the amplification this test is "+
				"about did not occur and the reporting assertions below prove nothing", slot)
		}
	}

	logged := buf.String()
	if !strings.Contains(logged, `"level":"ERROR"`) ||
		!strings.Contains(logged, "marked delete intent but removed nothing") {
		t.Errorf("the fixer marked four delete markers and deleted nothing without logging it at ERROR; "+
			"the operator sees only Fixed:false and cannot tell that image repair is now suppressed for "+
			"this artist. Captured log:\n%s", logged)
	}
	if !strings.Contains(res.Message, "could not delete") {
		t.Errorf("FixResult.Message = %q; it must say the deletions failed. The old message "+
			"(%q) was false here -- there WAS an extraneous file, it just could not be removed",
			res.Message, "no extraneous files to delete")
	}
}

// TestExtraneousImagesFixer_MixedRemovalReportsPartialFailure is the #3015
// fix-round-2 case: a partial failure, not a total one. Two extraneous files
// are present; one unlink succeeds and one is forced to fail via unlinkHook
// (matched by path, so the successful one is untouched). Before this fix, the
// loop's per-file Warn plus a bare "continue" left len(deleted) > 0, so the
// fixer fell straight past the len(deleted)==0 branch and reported plain
// Fixed:true naming only the file that WAS removed -- the same silent-
// amplification shape the zero-deleted branch above already closes, one notch
// along: the operator is told the cleanup completed when a file is still
// there and every canonical type is still marked to suppress its repair.
//
// This must fail RED without the fix: reverting it restores the old
// fall-through, where res.Message names only the deleted file and no ERROR is
// logged, so both assertions below fail.
func TestExtraneousImagesFixer_MixedRemovalReportsPartialFailure(t *testing.T) {
	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	survives := filepath.Join(dir, "backdrop_old.png")
	createGradientJPEG(t, survives, 1)
	removable := filepath.Join(dir, "backdrop_older.png")
	createGradientJPEG(t, removable, 2)

	installUnlinkHook(t, func(path string, remove func() error) error {
		if path == survives {
			return errors.New("injected: permission denied")
		}
		return remove()
	})

	buf, logger := capturingLogger()
	f := NewExtraneousImagesFixer(nil, nonSharedFSCheck(), logger)
	res, err := f.Fix(t.Context(), &artist.Artist{
		ID: "art-mixed", Name: "Mixed Removal", Path: dir, LibraryID: "lib-test",
	}, &Violation{RuleID: RuleExtraneousImages})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}

	// PRECONDITIONS: both outcomes actually happened, and independently of each
	// other, or the assertions below prove nothing about a MIXED result.
	if _, statErr := os.Stat(removable); statErr == nil {
		t.Fatalf("precondition failed: %s still exists, so the forced-success unlink did not run", removable)
	}
	if _, statErr := os.Stat(survives); statErr != nil {
		t.Fatalf("precondition failed: %s is gone (%v), so the injected failure did not take effect and "+
			"this is a total-failure run, not a mixed one", survives, statErr)
	}
	for _, slot := range img.AllSlots {
		if !img.DeleteIntentAfter(dir, slot, time.Time{}) {
			t.Fatalf("precondition failed: no %s marker was written, so this run never reached an unlink "+
				"attempt and the amplification under test did not occur", slot)
		}
	}

	logged := buf.String()
	if !strings.Contains(logged, `"level":"ERROR"`) {
		t.Errorf("a partial removal failure was not logged at ERROR; the operator sees Fixed:true and "+
			"cannot tell that a file survives and image repair is now suppressed for this artist. "+
			"Captured log:\n%s", logged)
	}
	if !strings.Contains(res.Message, "could not be removed") {
		t.Errorf("FixResult.Message = %q; it must say that some deletions failed, not just name the file "+
			"that succeeded -- the old message reported the mixed outcome as a plain success", res.Message)
	}
}

// TestDeleteDuplicateFanart_SweepsOrphanedTombFromPriorFailedUnlink is F1b
// (#3015 fix-round-2): a tomb stranded by a fixerRemove failure AFTER
// RenumberFanart already compacted the survivors is invisible to the stage
// loop's own "clear any leftover tomb" step on every later run, because that
// step only reconstructs a tombPath from a path the CURRENT DiscoverFanart
// call returns -- and the stranded tomb's pre-suffix name no longer exists
// (renumbering moved the survivors down past it). Left unswept it is inert
// (DiscoverFanart's extension allowlist excludes ".tmp") but permanently
// orphaned.
//
// This drives that exact sequence: unlinkHook forces the FIRST run's
// post-commit tomb-unlink to fail, stranding the tomb; a SECOND, independent
// call must then remove it via the directory-wide stray-tomb sweep, without
// any run's DiscoverFanart ever enumerating the stranded name again.
func TestDeleteDuplicateFanart_SweepsOrphanedTombFromPriorFailedUnlink(t *testing.T) {
	a, dir := dupArtistDir(t)

	installUnlinkHook(t, func(path string, remove func() error) error {
		return errors.New("injected: transient network-share failure")
	})

	f := newDupFixerFor(t, &fakeHashRecorder{})
	removed, err := f.deleteDuplicateFanartWithRollback(t.Context(), a, "fanart.jpg", false, map[int]bool{1: true})
	if err != nil {
		t.Fatalf("deleteDuplicateFanartWithRollback (first run): %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("precondition failed: first run removed %d file(s), want exactly 1", len(removed))
	}

	strandedTomb := filepath.Join(dir, "fanart1.jpg"+dupTombSuffix)
	if _, statErr := os.Stat(strandedTomb); statErr != nil {
		t.Fatalf("precondition failed: %s does not exist after the forced unlink failure (%v), so the "+
			"stranding this test is about did not occur", strandedTomb, statErr)
	}
	// Confirm the ordinary discovery path really cannot see it -- the property
	// that makes the per-index clear unable to reach it on a later run.
	rediscovered, discErr := img.DiscoverFanart(t.Context(), dir, "fanart.jpg")
	if discErr != nil {
		t.Fatalf("DiscoverFanart: %v", discErr)
	}
	for _, p := range rediscovered {
		if p == strandedTomb {
			t.Fatalf("precondition failed: DiscoverFanart returned the stranded tomb path %s; if discovery "+
				"can see it, the per-index clear could reach it too and this test would not isolate the "+
				"sweep", strandedTomb)
		}
	}

	// Uninstall the failing hook: the second run's own tomb-unlink must
	// succeed, isolating the sweep as the thing that removes the FIRST run's
	// leftover.
	unlinkHook = nil

	// A second, independent call -- no new duplicates staged (empty toDelete)
	// -- to isolate the sweep from any stage-loop activity of its own.
	removed2, err := f.deleteDuplicateFanartWithRollback(t.Context(), a, "fanart.jpg", false, map[int]bool{})
	if err != nil {
		t.Fatalf("deleteDuplicateFanartWithRollback (second run): %v", err)
	}
	if len(removed2) != 0 {
		t.Fatalf("second run staged %d file(s) with an empty toDelete set; it should have found nothing new "+
			"to remove, so any removal it reports would confound the sweep assertion below", len(removed2))
	}

	if _, statErr := os.Stat(strandedTomb); !os.IsNotExist(statErr) {
		t.Errorf("the stranded tomb %s still exists after a second, independent fixer run; the directory-"+
			"wide sweep should have removed it regardless of whether its pre-suffix name is still "+
			"discoverable (stat error: %v)", strandedTomb, statErr)
	}
}

// capturingLogger returns a logger writing JSON records to the returned buffer.
func capturingLogger() (*strings.Builder, *slog.Logger) {
	buf := &strings.Builder{}
	return buf, slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
