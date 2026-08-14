package rule

// collision_notify.go -- the #2970 predicate that gates the collision toast
// on a rule's Enabled toggle, extracted from cmd/stillwater/main.go so the
// patch-coverage gate can actually see it.
//
// cmd/stillwater/main.go is in codecov.yml's ignore list (application
// entrypoint wiring is not unit-testable without a full integration
// harness), so a closure built inline there is invisible to every coverage
// measurement -- the gate reports green over code nobody has run. This
// package is not ignored, so the same logic lives here as a named,
// independently testable function and main.go calls it instead of building
// its own closure. See collision_notify_test.go for the three branches this
// must never regress: enabled -> true, disabled -> false, lookup error ->
// true (fail open).

import (
	"context"
	"log/slog"

	"github.com/sydlexius/stillwater/internal/collision"
)

// CollisionNotifyEnabledFunc builds the collision.NotifyEnabledFunc that
// gates the ephemeral collision toast on ruleID's Enabled toggle (see
// IsRuleEnabled's doc comment for what this does and does not gate).
//
// FAIL-OPEN: a lookup error (a locked or momentarily unreachable database
// during a bulk import is the realistic case) returns true -- the toast
// still fires -- rather than risking a real collision going unreported. The
// error is logged so the operator has a signal something is wrong with the
// lookup itself, except for our own shutdown (context.Canceled /
// DeadlineExceeded), which is not a fault worth logging at Error (see
// isCanceled).
func CollisionNotifyEnabledFunc(svc *Service, ruleID string, logger *slog.Logger) collision.NotifyEnabledFunc {
	return func(ctx context.Context) bool {
		enabled, err := svc.IsRuleEnabled(ctx, ruleID)
		if err != nil {
			if !isCanceled(err) {
				logger.Error("checking rule enabled state for collision notify; failing open",
					slog.String("rule_id", ruleID),
					slog.String("error", err.Error()))
			}
			return true
		}
		return enabled
	}
}
