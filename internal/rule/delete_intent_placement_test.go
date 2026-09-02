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

// capturingLogger returns a logger writing JSON records to the returned buffer.
func capturingLogger() (*strings.Builder, *slog.Logger) {
	buf := &strings.Builder{}
	return buf, slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
