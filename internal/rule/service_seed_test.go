package rule

import "testing"

// TestSeededToleranceReadByChecker enforces that any rule seeding a non-zero
// Tolerance has a corresponding entry in the rulesThatReadTolerance allow-list.
// This is a hand-maintained constraint that catches when a new rule is added
// with a Tolerance seed but without a checker that reads it (dead code).
// The constraint does not detect the opposite case (a checker that stops reading
// Tolerance); that is caught when the rule's test coverage fails.
func TestSeededToleranceReadByChecker(t *testing.T) {
	// Hand-maintained allow-list: rules whose checkers read cfg.Tolerance.
	// See the checker functions in checkers.go for the actual tolerance reads.
	rulesThatReadTolerance := map[string]bool{
		RuleThumbSquare:      true, // makeThumbSquareChecker reads cfg.Tolerance
		RuleFanartAspect:     true, // makeFanartAspectChecker reads cfg.Tolerance
		RuleArtistIDMismatch: true, // checkArtistIDMismatch reads cfg.Tolerance
		RuleImageDuplicate:   true, // makeImageDuplicateChecker reads cfg.Tolerance
	}

	// Check that all seeded rules with non-zero Tolerance are in the allow-list.
	for _, rule := range defaultRules {
		if rule.Config.Tolerance == 0 {
			// Skip rules that don't seed a Tolerance value.
			continue
		}

		if !rulesThatReadTolerance[rule.ID] {
			t.Errorf("rule %q seeds Tolerance %.2f but is not in rulesThatReadTolerance allow-list (no checker reads cfg.Tolerance)",
				rule.ID, rule.Config.Tolerance)
		}
	}
}
