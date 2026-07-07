package rule

import "testing"

// #2306: the nfo_exists rule must ship enabled and in auto mode so a missing
// artist.nfo is generated without any manual toggle.
func TestDefaultRules_NFOExists_EnabledAuto(t *testing.T) {
	var found bool
	for _, r := range defaultRules {
		if r.ID != RuleNFOExists {
			continue
		}
		found = true
		if !r.Enabled {
			t.Error("nfo_exists must ship Enabled=true")
		}
		if r.AutomationMode != AutomationModeAuto {
			t.Errorf("nfo_exists AutomationMode=%q, want %q", r.AutomationMode, AutomationModeAuto)
		}
	}
	if !found {
		t.Fatal("nfo_exists not found in defaultRules")
	}
}
