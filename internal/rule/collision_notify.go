package rule

// collision_notify.go -- the #2970 predicate that gates an event-driven
// rule's ephemeral notification on its Enabled toggle, extracted from
// cmd/stillwater/main.go so the patch-coverage gate can actually see it.
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
//
// Originally named CollisionNotifyEnabledFunc and built only for
// cross_artist_backdrop_collision's toast. #2970's second round reused it
// for mbid_resolves's sweep-summary notification too, so it is renamed
// RuleEnabledNotifyFunc and returns a plain func(context.Context) bool
// rather than the collision-package-specific alias: both
// collision.NotifyEnabledFunc and mbidcheck.NotifyEnabledFunc share that
// same underlying signature, and an unnamed function type is assignable to
// either named one without a conversion at the call site.

import (
	"context"
	"log/slog"
)

// RuleEnabledNotifyFunc builds the predicate that gates an event-driven
// rule's ephemeral notification (a toast, a sweep-completion summary, ...) on
// ruleID's Enabled toggle (see IsRuleEnabled's doc comment for what this does
// and does not gate). Assignable directly to collision.NotifyEnabledFunc,
// mbidcheck.NotifyEnabledFunc, or any other named func(context.Context) bool
// seam an event-driven rule's wiring introduces.
//
// FAIL-OPEN: a lookup error (a locked or momentarily unreachable database
// during a bulk import is the realistic case) returns true -- the
// notification still fires -- rather than risking a real finding going
// unreported. The error is logged so the operator has a signal something is
// wrong with the lookup itself, except for our own shutdown
// (context.Canceled / DeadlineExceeded), which is not a fault worth logging
// at Error (see isCanceled).
func RuleEnabledNotifyFunc(svc *Service, ruleID string, logger *slog.Logger) func(context.Context) bool {
	// A nil logger falls back rather than panicking, matching
	// collision.NewNotifier and mbidcheck.NewSweep. The dereference below sits
	// on the error path inside the gate predicate, so an unguarded nil would
	// panic exactly when the lookup is already failing -- taking out the
	// gating at the one moment it is being asked to fail open.
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context) bool {
		enabled, err := svc.IsRuleEnabled(ctx, ruleID)
		if err != nil {
			if !isCanceled(err) {
				logger.Error("checking rule enabled state for event-driven notify; failing open",
					slog.String("rule_id", ruleID),
					slog.String("error", err.Error()))
			}
			return true
		}
		return enabled
	}
}
