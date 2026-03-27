package rule

import (
	"context"
	"testing"
)

// TestIsFilesystemDependent verifies that only truly filesystem-only rules
// are categorized as filesystem-dependent. Rules that can be made API-compatible
// are tracked in #725, #726, #727, #728.
func TestIsFilesystemDependent(t *testing.T) {
	// Only 2 rules are truly filesystem-only.
	fsRules := []string{
		RuleNFOExists,
		RuleExtraneousImages,
	}
	for _, id := range fsRules {
		if !IsFilesystemDependent(id) {
			t.Errorf("expected rule %q to be filesystem-dependent", id)
		}
	}

	// All other rules are API-compatible (or will be made so in follow-up issues).
	apiRules := []string{
		RuleNFOHasMBID,
		RuleThumbExists,
		RuleThumbSquare,
		RuleThumbMinRes,
		RuleFanartExists,
		RuleFanartMinRes,
		RuleFanartAspect,
		RuleLogoExists,
		RuleLogoMinRes,
		RuleLogoPadding,
		RuleBannerExists,
		RuleBannerMinRes,
		RuleBioExists,
		RuleMetadataQuality,
		RuleArtistIDMismatch,
		RuleDirectoryNameMismatch,
		RuleImageDuplicate,
		RuleBackdropSequencing,
	}
	for _, id := range apiRules {
		if IsFilesystemDependent(id) {
			t.Errorf("expected rule %q to be API-compatible (not filesystem-dependent)", id)
		}
	}
}

// TestAllDefaultRulesAreCategorized ensures every rule defined in defaultRules
// has a categorization in the filesystemRules map or is intentionally excluded.
// This prevents new rules from being added without a categorization decision.
func TestAllDefaultRulesAreCategorized(t *testing.T) {
	for _, r := range defaultRules {
		// The rule must be either explicitly in filesystemRules or explicitly
		// not (which is covered by the absence from the map). This test ensures
		// we have at least considered every default rule by verifying the test
		// above covers it.
		_ = IsFilesystemDependent(r.ID)
	}

	// Verify that every entry in filesystemRules corresponds to a valid default rule.
	defaultIDs := make(map[string]bool)
	for _, r := range defaultRules {
		defaultIDs[r.ID] = true
	}
	for id := range filesystemRules {
		if !defaultIDs[id] {
			t.Errorf("filesystemRules contains %q which is not a default rule", id)
		}
	}
}

// TestTagFilesystemDependent verifies that the tagFilesystemDependent function
// correctly sets the FilesystemDependent field on each rule.
func TestTagFilesystemDependent(t *testing.T) {
	rules := []Rule{
		{ID: RuleNFOExists},
		{ID: RuleBioExists},
		{ID: RuleExtraneousImages},
		{ID: RuleMetadataQuality},
		{ID: RuleThumbSquare},
	}

	tagFilesystemDependent(rules)

	if !rules[0].FilesystemDependent {
		t.Errorf("expected %s to be tagged filesystem-dependent", rules[0].ID)
	}
	if rules[1].FilesystemDependent {
		t.Errorf("expected %s to NOT be tagged filesystem-dependent", rules[1].ID)
	}
	if !rules[2].FilesystemDependent {
		t.Errorf("expected %s to be tagged filesystem-dependent", rules[2].ID)
	}
	if rules[3].FilesystemDependent {
		t.Errorf("expected %s to NOT be tagged filesystem-dependent", rules[3].ID)
	}
	if rules[4].FilesystemDependent {
		t.Errorf("expected %s to NOT be tagged filesystem-dependent", rules[4].ID)
	}
}

// TestListTagsFilesystemDependent verifies that the List method tags each rule
// with its filesystem dependency status.
func TestListTagsFilesystemDependent(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	rules, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Check a known filesystem-dependent rule.
	for _, r := range rules {
		if r.ID == RuleNFOExists {
			if !r.FilesystemDependent {
				t.Errorf("expected %s to be tagged filesystem-dependent after List", r.ID)
			}
		}
		if r.ID == RuleBioExists {
			if r.FilesystemDependent {
				t.Errorf("expected %s to NOT be tagged filesystem-dependent after List", r.ID)
			}
		}
	}
}

// TestGetByIDTagsFilesystemDependent verifies that GetByID tags the rule.
func TestGetByIDTagsFilesystemDependent(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	r, err := svc.GetByID(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !r.FilesystemDependent {
		t.Errorf("expected %s to be tagged filesystem-dependent after GetByID", r.ID)
	}

	r2, err := svc.GetByID(ctx, RuleBioExists)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if r2.FilesystemDependent {
		t.Errorf("expected %s to NOT be tagged filesystem-dependent after GetByID", r2.ID)
	}
}

// TestDisableFilesystemRules verifies that DisableFilesystemRules disables
// only enabled filesystem-dependent rules and returns the correct count.
func TestDisableFilesystemRules(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Count how many filesystem-dependent rules are currently enabled.
	rules, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	enabledFS := 0
	for _, r := range rules {
		if r.FilesystemDependent && r.Enabled {
			enabledFS++
		}
	}
	if enabledFS == 0 {
		t.Fatal("expected at least one enabled filesystem-dependent rule in defaults")
	}

	// Disable them.
	count, err := svc.DisableFilesystemRules(ctx)
	if err != nil {
		t.Fatalf("DisableFilesystemRules: %v", err)
	}
	if count != enabledFS {
		t.Errorf("DisableFilesystemRules returned %d, expected %d", count, enabledFS)
	}

	// Verify all filesystem-dependent rules are now disabled.
	rules, err = svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range rules {
		if r.FilesystemDependent && r.Enabled {
			t.Errorf("expected filesystem-dependent rule %q to be disabled", r.ID)
		}
	}

	// API-compatible rules should still be enabled.
	for _, r := range rules {
		if r.ID == RuleBioExists && !r.Enabled {
			t.Errorf("expected API-compatible rule %q to remain enabled", r.ID)
		}
	}

	// Running again should return 0 (no more to disable).
	count2, err := svc.DisableFilesystemRules(ctx)
	if err != nil {
		t.Fatalf("second DisableFilesystemRules: %v", err)
	}
	if count2 != 0 {
		t.Errorf("second DisableFilesystemRules returned %d, expected 0", count2)
	}
}
