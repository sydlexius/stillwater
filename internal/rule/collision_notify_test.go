package rule

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestCollisionNotifyEnabledFunc_Branches is the #2970 wiring test C1's
// hostile review demanded: it drives CollisionNotifyEnabledFunc (the
// extracted production predicate cmd/stillwater/main.go now calls) against a
// real *Service and a real database, covering the three branches the
// predicate must never regress.
//
// Mutation-proof: this kills both surviving mutants the review found in the
// former inline closure --
//   - "return enabled" -> "return true" (the Enabled toggle becomes inert):
//     killed by the disabled subtest, which fails if the predicate reports
//     true for a rule seeded/forced disabled.
//   - the error branch "return true" -> "return false" (fail-CLOSED): killed
//     by the unknown-rule subtest, which fails if the predicate reports
//     false when IsRuleEnabled errors.
func TestCollisionNotifyEnabledFunc_Branches(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	t.Run("enabled rule reports true", func(t *testing.T) {
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))

		// Precondition: nfo_exists really is enabled per the seeded defaults.
		enabled, err := svc.IsRuleEnabled(ctx, RuleNFOExists)
		if err != nil || !enabled {
			t.Fatalf("precondition failed: IsRuleEnabled(%s) = (%v, %v), want (true, nil)", RuleNFOExists, enabled, err)
		}

		predicate := CollisionNotifyEnabledFunc(svc, RuleNFOExists, logger)
		if got := predicate(ctx); !got {
			t.Errorf("predicate = false for an enabled rule, want true")
		}
		if logBuf.Len() != 0 {
			t.Errorf("predicate logged on the success path, want silence: %s", logBuf.String())
		}
	})

	t.Run("disabled rule reports false", func(t *testing.T) {
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))

		// Precondition: cross_artist_backdrop_collision really is seeded
		// disabled.
		enabled, err := svc.IsRuleEnabled(ctx, RuleCrossArtistBackdropCollision)
		if err != nil || enabled {
			t.Fatalf("precondition failed: IsRuleEnabled(%s) = (%v, %v), want (false, nil)", RuleCrossArtistBackdropCollision, enabled, err)
		}

		predicate := CollisionNotifyEnabledFunc(svc, RuleCrossArtistBackdropCollision, logger)
		if got := predicate(ctx); got {
			t.Errorf("predicate = true for a disabled rule, want false -- the Enabled toggle must gate the toast")
		}
		if logBuf.Len() != 0 {
			t.Errorf("predicate logged for a plain disabled rule (no error), want silence: %s", logBuf.String())
		}
	})

	t.Run("lookup error fails open and logs at Error", func(t *testing.T) {
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))

		// Precondition: this rule id really does not exist, so IsRuleEnabled
		// really does return an error (see TestIsRuleEnabled's own coverage
		// of that lookup).
		if _, err := svc.IsRuleEnabled(ctx, "nonexistent_rule_id"); err == nil {
			t.Fatal("precondition failed: IsRuleEnabled(nonexistent) returned no error")
		}

		predicate := CollisionNotifyEnabledFunc(svc, "nonexistent_rule_id", logger)
		if got := predicate(ctx); !got {
			t.Errorf("predicate = false on a lookup error, want true (fail open) -- a transient failure must never silently hide a real collision")
		}
		logged := logBuf.String()
		if !strings.Contains(logged, "level=ERROR") {
			t.Errorf("predicate did not log at Error on a genuine lookup failure, want a visible signal: %s", logged)
		}
		if !strings.Contains(logged, "nonexistent_rule_id") {
			t.Errorf("error log missing the rule id, want it identifiable: %s", logged)
		}
	})

	t.Run("our own cancellation fails open without an Error log", func(t *testing.T) {
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))

		cancelCtx, cancel := context.WithCancel(ctx)
		cancel() // canceled before the lookup even runs

		// Precondition: a canceled context really does make IsRuleEnabled
		// error (otherwise this subtest exercises nothing).
		if _, err := svc.IsRuleEnabled(cancelCtx, RuleNFOExists); err == nil {
			t.Skip("driver did not surface an error for a pre-canceled context; nothing to assert")
		}

		predicate := CollisionNotifyEnabledFunc(svc, RuleNFOExists, logger)
		if got := predicate(cancelCtx); !got {
			t.Errorf("predicate = false on our own cancellation, want true (fail open)")
		}
		if strings.Contains(logBuf.String(), "level=ERROR") {
			t.Errorf("predicate logged at Error for a plain shutdown cancellation, want silence: %s", logBuf.String())
		}
	})
}
