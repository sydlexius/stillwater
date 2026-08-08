package image

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
)

// #2933. Three loops in three packages consulted only ctx.Err() after a bounded
// read failed, so the process-wide abandoned-read cap took the SKIP branch.
// Both causes mean the read did not happen for a reason that applies to every
// later candidate: the cap saturates precisely BECAUSE reads are already wedged
// against an unresponsive mount.
//
// WHY THIS TESTS THE PREDICATE RATHER THAN DRIVING THE CALLERS.
// A first attempt saturated the real cap and called the rule package's
// quarantinedImagePresence, and it PASSED BEFORE THE FIX -- vacuously.
// Diagnosis: DiscoverFanart, the first line of that function, reads the
// directory through the SAME capped primitive, so a saturated cap fails there
// and returns before the candidate loop ever runs. Global saturation cannot
// reach the branch at all.
//
// The branch is reachable only through a CONCURRENCY WINDOW: the loop's opening
// read succeeds below the cap, then another operation pushes it over before the
// per-candidate reads. Reproducing that window would race the counter on every
// run -- a flaky test asserting a real property badly. Testing the predicate
// directly asserts the same property deterministically, and each caller's use
// of it is a single line.

// TestReadFailureDistrustsLoop_CapAbortsLikeCancellation is the #2933
// regression: the cap sentinel must abort a loop exactly as a cancellation
// does, while genuinely per-candidate failures stay skippable.
func TestReadFailureDistrustsLoop_CapAbortsLikeCancellation(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		// wantAbort is true when the failure distrusts the whole loop.
		wantAbort bool
	}{
		// The #2933 fix proper.
		{
			name:      "stalled-read cap distrusts the loop",
			ctx:       context.Background(),
			err:       ErrTooManyStalledReads,
			wantAbort: true,
		},
		{
			name:      "a wrapped cap error is still recognized",
			ctx:       context.Background(),
			err:       fmt.Errorf("reading candidate: %w", ErrTooManyStalledReads),
			wantAbort: true,
		},
		// The cause that already worked; asserted so the fix cannot regress it.
		{
			name:      "a canceled ctx distrusts the loop",
			ctx:       canceled,
			err:       errors.New("read failed for some other reason"),
			wantAbort: true,
		},
		// The ordinary skip cases every call site explicitly relies on. Each
		// says something about ONE path and nothing about the rest of the set.
		{
			name:      "a vanished file is per-candidate",
			ctx:       context.Background(),
			err:       fs.ErrNotExist,
			wantAbort: false,
		},
		{
			name:      "an over-size file is per-candidate",
			ctx:       context.Background(),
			err:       ErrImageTooLarge,
			wantAbort: false,
		},
		{
			name:      "a permissions error is per-candidate",
			ctx:       context.Background(),
			err:       os.ErrPermission,
			wantAbort: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ReadFailureDistrustsLoop(tc.ctx, tc.err)
			if tc.wantAbort && got == nil {
				t.Fatalf("ReadFailureDistrustsLoop(%v) = nil, want a non-nil abort error", tc.err)
			}
			if !tc.wantAbort && got != nil {
				t.Fatalf("ReadFailureDistrustsLoop(%v) = %v, want nil (this failure is per-candidate and must keep skipping)", tc.err, got)
			}
		})
	}
}

// TestReadFailureDistrustsLoop_CapErrorSurvivesToTheCaller pins that the
// returned error still matches the sentinel, so a caller (or an operator
// reading a log) can tell a stalled-mount refusal from a cancellation.
// Returning a freshly-minted error would satisfy every "is it non-nil"
// assertion above while destroying that distinction.
func TestReadFailureDistrustsLoop_CapErrorSurvivesToTheCaller(t *testing.T) {
	t.Parallel()
	got := ReadFailureDistrustsLoop(context.Background(), ErrTooManyStalledReads)
	if !errors.Is(got, ErrTooManyStalledReads) {
		t.Errorf("returned error %v does not wrap ErrTooManyStalledReads", got)
	}
}

// TestReadFailureDistrustsLoop_CancellationWinsOverAPerCandidateError covers
// the ordering inside the predicate: a canceled ctx aborts even when the read
// reported an ordinarily-skippable error on its way out. A read abandoned
// mid-flight can surface any error, so keying only on the error value would let
// a cancellation be swallowed as a skip.
func TestReadFailureDistrustsLoop_CancellationWinsOverAPerCandidateError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := ReadFailureDistrustsLoop(ctx, fs.ErrNotExist)
	if got == nil {
		t.Fatal("a canceled ctx must abort the loop even when the read reported a skippable error")
	}
	// The EXACT error, not merely non-nil (#2976 review). Accepting any
	// non-nil value would pass if the predicate returned the fs.ErrNotExist
	// it was handed -- which is the per-candidate error, the opposite of the
	// property under test. The abort must name the CAUSE.
	if !errors.Is(got, context.Canceled) {
		t.Errorf("got %v, want an error wrapping context.Canceled", got)
	}
}

// TestReadFailureDistrustsLoop_NilErrorNeverAborts guards the entry condition.
// Every call site consults this only INSIDE an `if readErr != nil` branch, but
// a predicate that aborted on a nil error would break the moment one of them is
// refactored to ask unconditionally -- and a successful read must never be
// mistaken for a stalled mount.
func TestReadFailureDistrustsLoop_NilErrorNeverAborts(t *testing.T) {
	t.Parallel()
	if got := ReadFailureDistrustsLoop(context.Background(), nil); got != nil {
		t.Errorf("ReadFailureDistrustsLoop(bg, nil) = %v, want nil", got)
	}

	// THE CASE THIS TEST WAS NAMED FOR BUT DID NOT COVER (#2976 review).
	// With only the live-ctx case above, "never aborts" was asserted against
	// the one input that could not fail it: the predicate consulted ctx BEFORE
	// the error, so a nil readErr under a CANCELED ctx returned ctx.Err() --
	// reporting a successful read as "the mount is unresponsive". Unreachable
	// today (every caller asks inside an `if readErr != nil` branch), which is
	// precisely why nothing caught it.
	//
	// A read that returned bytes is evidence the mount answered, whatever the
	// context says.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := ReadFailureDistrustsLoop(canceled, nil); got != nil {
		t.Errorf("ReadFailureDistrustsLoop(canceledCtx, nil) = %v, want nil -- "+
			"a successful read must never be classified as a distrusted loop", got)
	}
}
