package rule

import (
	"context"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestService_UpdateMakesPreviouslyEvaluatedArtistDirty is the contract for
// issue #698: any successful rule mutation must surface existing artists as
// dirty on the next ListDirtyIDs query. The implementation achieves this
// without a mark-all-dirty side-effect; rule.Update bumps rules.updated_at,
// and the artist.ListDirtyIDs JOIN picks up every artist whose
// rules_evaluated_at predates the bump.
func TestService_UpdateMakesPreviouslyEvaluatedArtistDirty(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	a := &artist.Artist{
		Name: "Pre-existing", SortName: "Pre-existing", Path: "/music/pre",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("Create artist: %v", err)
	}
	// Stamp rules_evaluated_at strictly after the seed's updated_at so the
	// artist starts clean.
	time.Sleep(time.Second)
	if err := artistSvc.MarkRulesEvaluated(ctx, a.ID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkRulesEvaluated: %v", err)
	}

	cleanIDs, err := artistSvc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("pre-update ListDirtyIDs: %v", err)
	}
	if len(cleanIDs) != 0 {
		t.Fatalf("artist should be clean before rule update, got %v", cleanIDs)
	}

	// Update an enabled rule with a real field mutation. The JOIN in
	// ListDirtyIDs filters on enabled = 1 AND updated_at > artists.
	// rules_evaluated_at, so the freshly bumped rules.updated_at must
	// resurface the artist. We mutate AutomationMode rather than leaving
	// r unchanged so the assertion is "a real rule mutation dirties
	// artists" rather than "a no-op save still bumps updated_at", which
	// would be a false green if Update ever became no-op-aware.
	r, err := ruleSvc.GetByID(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !r.Enabled {
		// Guard: the assertion below depends on the rule being enabled
		// both before and after the update (disable-then-update would
		// leave the JOIN's enabled=1 filter excluding the row).
		t.Skipf("RuleNFOExists default disabled; test assumes an enabled default rule")
	}
	if r.AutomationMode == AutomationModeAuto {
		r.AutomationMode = AutomationModeManual
	} else {
		r.AutomationMode = AutomationModeAuto
	}
	time.Sleep(time.Second)
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatalf("Update rule: %v", err)
	}

	dirtyIDs, err := artistSvc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("post-update ListDirtyIDs: %v", err)
	}
	if len(dirtyIDs) != 1 || dirtyIDs[0] != a.ID {
		t.Fatalf("expected artist %s in dirty list after rule update, got %v", a.ID, dirtyIDs)
	}
}

// TestService_DisablingRuleDoesNotMarkDirty verifies the JOIN's
// enabled = 1 filter: disabling a rule must not schedule a full-library
// re-evaluation, because the remaining enabled rules' outcomes are
// unchanged. The previous mark-all-dirty side-effect approach could not
// make this distinction.
func TestService_DisablingRuleDoesNotMarkDirty(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	a := &artist.Artist{
		Name: "Stable", SortName: "Stable", Path: "/music/stable",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("Create artist: %v", err)
	}
	time.Sleep(time.Second)
	if err := artistSvc.MarkRulesEvaluated(ctx, a.ID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkRulesEvaluated: %v", err)
	}

	// Disable an enabled rule. The JOIN filters disabled rules out, so
	// the bumped updated_at on the now-disabled row must not resurface
	// the artist.
	r, err := ruleSvc.GetByID(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !r.Enabled {
		t.Skipf("RuleNFOExists default disabled; cannot exercise disable path")
	}
	r.Enabled = false
	time.Sleep(time.Second)
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatalf("Update rule: %v", err)
	}

	dirtyIDs, err := artistSvc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("post-disable ListDirtyIDs: %v", err)
	}
	// Only enabled rules participate in the JOIN. If any other enabled
	// default rule has updated_at ahead of the artist's stamp the artist
	// will legitimately appear dirty, but the disabled rule alone must
	// not cause that.
	for _, id := range dirtyIDs {
		if id == a.ID {
			// Verify no other enabled rule has a newer updated_at; if so
			// this hit was from a different default and the assertion is
			// inconclusive rather than broken.
			t.Fatalf("artist %s became dirty after disabling (not enabling) a rule", a.ID)
		}
	}
}

// TestService_SeedDefaultsIdempotent verifies that repeated SeedDefaults
// calls do NOT bump rules.updated_at on existing rows. Without this
// guarantee every process restart would force a full re-evaluation on
// the next Run Rules pass. The cosmetic UPDATE in SeedDefaults intentionally
// omits updated_at from the SET list to preserve this property.
func TestService_SeedDefaultsIdempotent(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	a := &artist.Artist{
		Name: "Existing", SortName: "Existing", Path: "/music/existing",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// First seed: every default rule is new, so the artist should be
	// dirty on the next ListDirtyIDs call.
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("first SeedDefaults: %v", err)
	}
	firstDirty, err := artistSvc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs after first seed: %v", err)
	}
	if len(firstDirty) != 1 || firstDirty[0] != a.ID {
		t.Fatalf("artist should be dirty after first SeedDefaults, got %v", firstDirty)
	}

	// Stamp the artist as fully evaluated so it falls out of the dirty set.
	time.Sleep(time.Second)
	if err := artistSvc.MarkRulesEvaluated(ctx, a.ID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkRulesEvaluated: %v", err)
	}
	clean, err := artistSvc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs after evaluation: %v", err)
	}
	if len(clean) != 0 {
		t.Fatalf("artist should be clean after MarkRulesEvaluated, got %v", clean)
	}

	// Second seed: no rule is new, and the cosmetic refresh must not
	// bump updated_at. A no-op seed must leave the artist clean.
	time.Sleep(time.Second)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("second SeedDefaults: %v", err)
	}
	stillClean, err := artistSvc.ListDirtyIDs(ctx)
	if err != nil {
		t.Fatalf("ListDirtyIDs after second seed: %v", err)
	}
	if len(stillClean) != 0 {
		t.Fatalf("artist should remain clean after no-op SeedDefaults, got %v", stillClean)
	}
}
