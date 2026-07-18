package rule

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// fakeCollisionRemediator records how RemediatePHashMismatches was invoked and
// returns a canned result/error, so the fixer's destructive-path contract can be
// asserted on the inputs (right artist, NOT dry-run) rather than a nil error.
type fakeCollisionRemediator struct {
	calls  int
	gotID  string
	gotDry bool
	gotTol float64
	result PHashRemediateResult
	err    error
}

func (f *fakeCollisionRemediator) RemediatePHashMismatches(_ context.Context, scope PHashMismatchScope, opts PHashRemediateOpts) (PHashRemediateResult, error) {
	f.calls++
	f.gotID = scope.ArtistID
	f.gotTol = scope.Tolerance
	f.gotDry = opts.DryRun
	return f.result, f.err
}

func TestCollisionFixer_CanFix(t *testing.T) {
	f := NewCrossArtistBackdropCollisionFixer(slog.Default())
	if !f.CanFix(&Violation{RuleID: RuleCrossArtistBackdropCollision}) {
		t.Error("CanFix should be true for the collision rule id")
	}
	for _, other := range []string{RuleImageDuplicate, RuleImageDuplicateExact, RuleFanartExists, ""} {
		if f.CanFix(&Violation{RuleID: other}) {
			t.Errorf("CanFix should be false for rule id %q", other)
		}
	}
	if f.CanFix(nil) {
		t.Error("CanFix(nil) should be false")
	}
}

func TestCollisionFixer_Fix_BacksOutForRightArtist_NotDryRun(t *testing.T) {
	rem := &fakeCollisionRemediator{
		result: PHashRemediateResult{OpID: "op-123", SlotsRemoved: 2},
	}
	f := NewCrossArtistBackdropCollisionFixer(slog.Default())
	f.SetRemediator(rem)

	a := &artist.Artist{ID: "artist-42", Name: "Dest"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleCrossArtistBackdropCollision})
	if err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}

	// Assert the DESTRUCTIVE call actually happened, for the RIGHT artist, and
	// was NOT a dry run (a dry run would report success while removing nothing).
	if rem.calls != 1 {
		t.Fatalf("remediator called %d times, want 1", rem.calls)
	}
	if rem.gotID != "artist-42" {
		t.Errorf("remediated artist %q, want artist-42", rem.gotID)
	}
	if rem.gotDry {
		t.Error("remediation ran in dry-run mode; must commit (DryRun:false)")
	}
	// Tolerance 0 selects the detector's own vetted default. Passing a bogus
	// non-zero tolerance into a path that DELETES files is the failure mode this
	// guards: a too-low value would widen the deletion set.
	if rem.gotTol != 0 {
		t.Errorf("scope.Tolerance = %v, want 0 (defer to the detector default)", rem.gotTol)
	}

	// Assert the result mapping: SlotsRemoved>0 -> Fixed, count carried through.
	if !res.Fixed {
		t.Error("Fixed should be true when slots were removed")
	}
	if res.SlotsRemoved != 2 {
		t.Errorf("SlotsRemoved = %d, want 2", res.SlotsRemoved)
	}
	if res.RuleID != RuleCrossArtistBackdropCollision {
		t.Errorf("RuleID = %q, want the collision rule", res.RuleID)
	}
}

func TestCollisionFixer_Fix_ZeroRemoved_IsTerminalDismissed(t *testing.T) {
	rem := &fakeCollisionRemediator{result: PHashRemediateResult{OpID: "op-0", SlotsRemoved: 0}}
	f := NewCrossArtistBackdropCollisionFixer(slog.Default())
	f.SetRemediator(rem)

	res, err := f.Fix(context.Background(), &artist.Artist{ID: "a1"}, &Violation{RuleID: RuleCrossArtistBackdropCollision})
	if err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed must be false when nothing was removed: no artwork was backed out")
	}
	if res.SlotsRemoved != 0 {
		t.Errorf("SlotsRemoved = %d, want 0", res.SlotsRemoved)
	}
	// The load-bearing assertion. A successful run that removed nothing is a
	// FINAL answer -- the remediation re-detects from the live library, so
	// clicking Fix again can only repeat it. Neither-Fixed-nor-Dismissed leaves
	// the queue entry open forever behind a Fix button that does nothing.
	if !res.Dismissed {
		t.Error("zero-removal result is neither Fixed nor Dismissed: the violation would stick " +
			"in the Action Queue permanently with a no-op Fix button")
	}
	if res.Message == "" {
		t.Error("terminal result needs a message explaining why nothing was backed out")
	}
}

func TestCollisionFixer_Fix_UnwiredRemediator_FailsLoudly(t *testing.T) {
	f := NewCrossArtistBackdropCollisionFixer(slog.Default())
	// No SetRemediator: a wiring defect must error, not silently no-op on a
	// destructive-recovery path.
	_, err := f.Fix(context.Background(), &artist.Artist{ID: "a"}, &Violation{RuleID: RuleCrossArtistBackdropCollision})
	if err == nil {
		t.Fatal("Fix with an unwired remediator must return an error")
	}
}

func TestCollisionFixer_Fix_PropagatesRemediatorError(t *testing.T) {
	rem := &fakeCollisionRemediator{err: errors.New("quarantine failed")}
	f := NewCrossArtistBackdropCollisionFixer(slog.Default())
	f.SetRemediator(rem)

	_, err := f.Fix(context.Background(), &artist.Artist{ID: "a"}, &Violation{RuleID: RuleCrossArtistBackdropCollision})
	if err == nil {
		t.Fatal("Fix must propagate the remediator error")
	}
}
