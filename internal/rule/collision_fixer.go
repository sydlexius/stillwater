package rule

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
)

// backdropCollisionRemediator is the narrow seam the collision fixer needs from
// the pipeline: back an artist's cross-artist fanart collisions out of the
// library. *Pipeline satisfies it via RemediatePHashMismatches. It is declared
// as an interface (rather than taking a *Pipeline directly) for two reasons:
// the pipeline is constructed AFTER the fixers slice in main.go, so the fixer is
// wired with a late SetRemediator setter; and tests can substitute a fake that
// asserts it was invoked for the right artist and not in dry-run mode.
type backdropCollisionRemediator interface {
	RemediatePHashMismatches(ctx context.Context, scope PHashMismatchScope, opts PHashRemediateOpts) (PHashRemediateResult, error)
}

// CrossArtistBackdropCollisionFixer is the auto-fix behind the durable #2540
// Action Queue entry. It is a thin wrapper: the destructive, reversible back-out
// logic lives entirely in the shipped #2564 remediation
// (Pipeline.RemediatePHashMismatches), which re-detects collisions from the live
// DB (a persisted finding is stale by the time an operator clicks Fix),
// quarantines before removal, and can be restored. This fixer only scopes that
// remediation to the affected artist and maps its result into a FixResult.
type CrossArtistBackdropCollisionFixer struct {
	remediator backdropCollisionRemediator
	logger     *slog.Logger
}

// NewCrossArtistBackdropCollisionFixer creates the fixer. The remediator is
// wired later via SetRemediator because the pipeline that implements it is built
// after the fixers slice; until then CanFix still reports true so the violation
// is recognized as fixable, and Fix fails loudly if invoked unwired.
func NewCrossArtistBackdropCollisionFixer(logger *slog.Logger) *CrossArtistBackdropCollisionFixer {
	return &CrossArtistBackdropCollisionFixer{
		logger: logger.With(slog.String("component", "cross-artist-backdrop-collision-fixer")),
	}
}

// SetRemediator wires the back-out remediator after pipeline construction. It is
// called once at startup, right after the pipeline is built.
func (f *CrossArtistBackdropCollisionFixer) SetRemediator(r backdropCollisionRemediator) {
	f.remediator = r
}

// CanFix claims only the cross-artist backdrop collision rule.
func (f *CrossArtistBackdropCollisionFixer) CanFix(v *Violation) bool {
	return v != nil && v.RuleID == RuleCrossArtistBackdropCollision
}

// Fix backs the polluting backdrop out of the affected artist by running the
// reversible #2564 remediation scoped to that artist. It re-detects from the
// live library (DryRun:false commits), so the fix acts on the CURRENT on-disk
// state rather than the possibly-stale persisted finding. Fixed is true only
// when at least one slot was actually removed; a zero-removal run (re-detection
// disagreed, or the collision was already cleaned) leaves the violation open.
func (f *CrossArtistBackdropCollisionFixer) Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	if f.remediator == nil {
		// A wiring defect, not a per-artist condition: fail loudly rather than
		// silently report "nothing to fix" on a destructive-recovery path.
		return nil, fmt.Errorf("cross-artist backdrop collision fixer: remediator not configured")
	}
	if a == nil {
		return nil, fmt.Errorf("cross-artist backdrop collision fixer: nil artist")
	}

	res, err := f.remediator.RemediatePHashMismatches(ctx,
		PHashMismatchScope{ArtistID: a.ID},
		PHashRemediateOpts{DryRun: false},
	)
	if err != nil {
		return nil, fmt.Errorf("backing out cross-artist backdrop collision for artist %s: %w", a.ID, err)
	}

	// The remediation succeeded, so this fix attempt MUST return a terminal
	// result -- Fixed or Dismissed. Removing nothing is a real, final answer
	// ("re-detection says this artist is clean"), not a failure to retry: the
	// remediation re-detects from the live library every time, so clicking Fix
	// again can only produce the same empty result. Returning neither would leave
	// the queue entry open forever with a Fix button that does nothing, which is
	// the report-success-while-doing-nothing shape this feature must not have.
	fixed := res.SlotsRemoved > 0
	msg := fmt.Sprintf(
		"No colliding backdrop remains for this artist; re-detection found nothing to back out (op %s)",
		res.OpID)
	if fixed {
		msg = fmt.Sprintf("Backed out %d colliding backdrop slot(s); restorable via op %s", res.SlotsRemoved, res.OpID)
	}
	f.logger.Info("cross-artist backdrop collision fix",
		slog.String("artist_id", a.ID),
		slog.Bool("fixed", fixed),
		slog.Bool("dismissed", !fixed),
		slog.Int("slots_removed", res.SlotsRemoved),
		slog.String("op_id", res.OpID))

	return &FixResult{
		RuleID:       RuleCrossArtistBackdropCollision,
		Fixed:        fixed,
		Dismissed:    !fixed,
		Message:      msg,
		SlotsRemoved: res.SlotsRemoved,
	}, nil
}
