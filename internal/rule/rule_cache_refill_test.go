package rule

import (
	"context"
	"testing"
)

// TestGetCachedRule_ClearDuringFillIsNotOverwritten (#3138): a fill that read
// the rule BEFORE an update committed must not store it AFTER the update's
// ClearRuleCache. Otherwise the revoked config (prune_platform_copies, or an
// old automation mode) stays cached, with nothing left to clear it, until the
// next rule write or a restart. The update and clear run inside the window
// between the fill's DB read and its store.
func TestGetCachedRule_ClearDuringFillIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	svc := NewService(db)
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	r0, err := svc.GetByID(ctx, RuleImageDuplicate)
	if err != nil {
		t.Fatal(err)
	}
	r0.Config.PrunePlatformCopies = true
	if err := svc.Update(ctx, r0); err != nil {
		t.Fatal(err)
	}
	p := NewPipeline(nil, nil, svc, nil, nil, testLogger())
	fired := false
	afterRuleCacheFetch = func() {
		afterRuleCacheFetch = nil
		fired = true
		r1, err := svc.GetByID(ctx, RuleImageDuplicate)
		if err != nil {
			t.Fatal(err)
		}
		r1.Config.PrunePlatformCopies = false
		if err := svc.Update(ctx, r1); err != nil {
			t.Fatal(err)
		}
		p.ClearRuleCache()
	}
	t.Cleanup(func() { afterRuleCacheFetch = nil })

	first, err := p.getCachedRule(ctx, RuleImageDuplicate)
	if err != nil {
		t.Fatal(err)
	}
	if !fired || !first.Config.PrunePlatformCopies {
		t.Fatalf("precondition: the racing fill must have read the pre-update rule (fired=%v, value=%v)", fired, first.Config.PrunePlatformCopies)
	}
	got, err := p.getCachedRule(ctx, RuleImageDuplicate)
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.PrunePlatformCopies {
		t.Fatal("stale refill: the update turned prune_platform_copies off, but the pipeline cache still holds true after the update's clear")
	}
}
