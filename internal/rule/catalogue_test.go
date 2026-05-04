package rule

import "testing"

// TestCatalogue_AllDefaultRulesPresent locks the documentation catalogue to
// the rule registry: every rule returned by DefaultRules() must have an
// explicit entry in rulesCatalogue. Without this, a newly registered rule
// silently falls through to the zero-value RuleCatalogueEntry, and the
// generated reference page would document it as detection-only with no
// human review.
//
// To intentionally mark a rule as detection-only, add an entry with
// FixBehavior: "" -- the explicit entry is what this test enforces.
func TestCatalogue_AllDefaultRulesPresent(t *testing.T) {
	for _, r := range DefaultRules() {
		if _, ok := rulesCatalogue[r.ID]; !ok {
			t.Errorf("rule %q has no entry in rulesCatalogue (add one in internal/rule/catalogue.go)", r.ID)
		}
	}
}

// TestCatalogue_NoOrphanedEntries flags catalogue entries whose rule ID is
// no longer registered. Stale entries are documentation-only debt but make
// the at-a-glance numbers wrong if anything ever counts the catalogue map.
func TestCatalogue_NoOrphanedEntries(t *testing.T) {
	known := make(map[string]struct{}, len(DefaultRules()))
	for _, r := range DefaultRules() {
		known[r.ID] = struct{}{}
	}
	for id := range rulesCatalogue {
		if _, ok := known[id]; !ok {
			t.Errorf("rulesCatalogue has entry for %q which is not in DefaultRules()", id)
		}
	}
}
