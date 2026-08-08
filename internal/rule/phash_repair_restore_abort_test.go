//go:build unix

package rule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/image"
)

// #2976 review, and the sharpest finding in it: fixing the INNER loops created
// the identical defect one level UP.
//
// restoreOneQuarantined now returns a cancellation or a stalled-read cap
// refusal instead of swallowing it. But RestorePHashQuarantine's outer loop
// caught every error, appended it to result.Failures, and continued -- so an
// abort meant to stop the operation became one per-entry failure among many.
// Against an unresponsive mount the loop would grind through the entire
// manifest, produce a failure line per entry, and return a NIL top-level error:
// a "completed" restore whose failures all share one cause nothing named.
//
// That is the same shape as the bug this PR fixes (a whole-set condition
// classified as a per-item one), reached by fixing the inner half and leaving
// the outer half unexamined.

// wedgeRestoreFifo plants a FIFO with no writer at path, so a read of it
// blocks until the reader's context gives up.
//
// Per-FILE rather than process-wide, which is the whole point: every global
// condition (a dead ctx, a saturated cap) fails at one of
// RestorePHashQuarantine's entry-point reads and never reaches the per-entry
// loop. Wedging just this file leaves the artist load and the manifest read
// healthy, so the loop is entered normally and only the entry's own byte read
// stalls.
//
// A writer is opened at cleanup so the abandoned read drains and the
// process-wide stalled-read counter self-heals rather than leaking into later
// tests in this package.
func wedgeRestoreFifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	t.Cleanup(func() {
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
}

// TestRestorePHashQuarantine_AbortsInsteadOfRecordingPerEntryFailures is the
// regression: a loop-wide condition must stop the operation and surface its
// cause, not accumulate as per-entry noise.
func TestRestorePHashQuarantine_AbortsInsteadOfRecordingPerEntryFailures(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if res.SlotsRemoved != 1 {
		t.Fatalf("setup: expected the polluted slot to be removed, got %+v", res)
	}

	// PRECONDITION: with a live ctx this same restore SUCCEEDS. Without it,
	// an assertion that a canceled ctx returns an error could pass for a
	// reason having nothing to do with the abort path -- a missing manifest,
	// an unseeded artist, any setup slip.
	m, mErr := image.ReadRepairManifest(context.Background(), dirA, res.OpID)
	if mErr != nil || m == nil || len(m.Entries) != 1 {
		t.Fatalf("setup: expected 1 manifest entry, got %+v (err %v)", m, mErr)
	}

	// WEDGE THE ONE ENTRY'S STORED FILE, and nothing else.
	//
	// TWO EARLIER ATTEMPTS BOTH PASSED WITH THE FIX DELETED, each for the same
	// reason in a different place, and both are worth naming because the shape
	// keeps recurring:
	//
	//   - a pre-canceled ctx died at artistService.GetByID, the function's
	//     first call;
	//   - a globally saturated read cap died at img.ReadRepairManifest, also
	//     before the loop.
	//
	// Every process-wide condition fails at one of RestorePHashQuarantine's
	// ENTRY-POINT reads, so no global approach can reach the per-entry loop --
	// the same first-line short-circuit that made the original #2933 cap test
	// vacuous, hit twice more in one review round.
	//
	// A FIFO on the entry's stored file is per-FILE: the manifest read stays
	// healthy, the loop is entered normally, and only img.RepairEntryBytes
	// wedges. With a deadline on the ctx, that read returns a cancellation
	// from INSIDE the loop -- exactly the state the outer handler must abort
	// on rather than record as one entry's failure.
	stored := filepath.Join(dirA, image.RepairDirName, res.OpID, m.Entries[0].StoredName)
	if rmErr := os.Remove(stored); rmErr != nil {
		t.Fatalf("clearing stored bytes at %s: %v", stored, rmErr)
	}
	wedgeRestoreFifo(t, stored)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	rres, err := p.RestorePHashQuarantine(ctx, "art-a", res.OpID)
	if err == nil {
		t.Fatalf("a loop-wide abort must return a top-level error, got nil with result %+v.\n"+
			"Recording the cause as a per-entry failure and continuing means the operation "+
			"reports 'completed' while every remaining entry hits the same condition.", rres)
	}
	// The FIFO wedge produces a CANCELLATION (the ctx deadline fires while the
	// read is blocked), which is the other half of what ReadFailureDistrustsLoop
	// aborts on. Assert that exact cause: any-non-nil would also pass on the
	// ordinary per-entry read failure this test must be distinguishable from.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded; the abort must name its cause", err)
	}
	// The cause belongs in the returned error, NOT buried in a per-entry list.
	// A caller that only checks err would otherwise see the abort while a
	// caller that only reads Failures would see an ordinary bad file.
	if len(rres.Failures) > 0 {
		t.Errorf("Failures = %v, want empty -- a loop-wide abort is not a per-entry failure", rres.Failures)
	}
}

// TestRestorePHashQuarantine_OrdinaryFailureStillRecordsAndContinues is the
// anti-overreach guard. If the abort branch swallowed EVERY error, the test
// above would still pass while a single unreadable file killed the whole
// restore -- destroying the per-entry resilience that loop exists for.
//
// Drives a genuinely per-entry failure: a manifest naming a stored file that is
// not on disk. That is "this one entry is broken", not "the mount is gone".
func TestRestorePHashQuarantine_OrdinaryFailureStillRecordsAndContinues(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}

	// Remove the quarantined bytes so the entry cannot be restored, leaving
	// the manifest entry itself intact.
	m, err := image.ReadRepairManifest(context.Background(), dirA, res.OpID)
	if err != nil || m == nil || len(m.Entries) != 1 {
		t.Fatalf("setup: expected 1 manifest entry, got %+v (err %v)", m, err)
	}
	stored := filepath.Join(dirA, image.RepairDirName, res.OpID, m.Entries[0].StoredName)
	if rmErr := os.Remove(stored); rmErr != nil {
		t.Fatalf("removing quarantined bytes at %s: %v", stored, rmErr)
	}

	rres, restoreErr := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
	if restoreErr != nil {
		t.Fatalf("an ordinary per-entry failure must NOT abort the operation, got %v", restoreErr)
	}
	if len(rres.Failures) == 0 {
		t.Error("expected the unreadable entry to be recorded in Failures")
	}
}
