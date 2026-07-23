package rule

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestProcessArtistForRunAll_EvaluateError covers the early-return branch in
// processArtistForRunAll where the engine's Evaluate fails. The integration
// tests never inject an engine error, so this drives the unit directly: with a
// cold rule cache, closing the DB forces cachedRules -> List to error, which
// Evaluate propagates. The method must bail with (contrib{}, false) and zero
// violations recorded.
func TestProcessArtistForRunAll_EvaluateError(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	// Close before any Evaluate so the engine rule cache stays cold; the next
	// cachedRules call hits the closed DB and errors.
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	a := &artist.Artist{Name: "Eval Err", Path: t.TempDir()}
	contrib, persistOK := p.processArtistForRunAll(context.Background(), a)
	if persistOK {
		t.Error("persistOK = true; want false when Evaluate errors")
	}
	if contrib.violationsFound != 0 {
		t.Errorf("violationsFound = %d; want 0 on the Evaluate-error early return", contrib.violationsFound)
	}
}

// TestProcessArtistForRunAll_RuleLookupError covers the getCachedRule-error
// branch inside the per-violation loop. Evaluate is warmed first (engine rule
// cache populated, violations confirmed) so the post-close Evaluate still
// succeeds from cache; closing the DB afterward makes the pipeline-level
// getCachedRule -> GetByID error, driving acc.persistOK false via the continue
// branch. The trailing persist steps (health, resolved rows, pass results) also
// fail against the closed DB, reinforcing persistOK == false.
func TestProcessArtistForRunAll_RuleLookupError(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	// Seed the default rules so Evaluate has enabled rules to check; a no-NFO
	// artist reliably violates RuleNFOExists, a field-based rule that still
	// fires after the DB is closed (the rule set comes from the warm cache).
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding default rules: %v", err)
	}
	a := &artist.Artist{Name: "Lookup Err", SortName: "Lookup Err", NFOExists: false, Path: t.TempDir()}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Warm the engine rule cache and confirm the artist violates at least one
	// rule, so the per-violation loop runs after the DB is closed.
	eval, err := engine.Evaluate(context.Background(), a)
	if err != nil {
		t.Fatalf("warm-up Evaluate: %v", err)
	}
	if len(eval.Violations) == 0 {
		t.Fatal("expected the bare artist to violate at least one rule")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	contrib, persistOK := p.processArtistForRunAll(context.Background(), a)
	if persistOK {
		t.Error("persistOK = true; want false when getCachedRule errors")
	}
	if contrib.violationsFound == 0 {
		t.Error("violationsFound = 0; want the warmed-cache Evaluate to still surface violations")
	}
}

// TestMergeIntoContrib exercises every branch of the mergeIntoContrib helper,
// which is the per-violation merge path used by processArtistForRunAll. The
// imageFix and persistFailed branches cannot be reached by the integration
// tests in fixer_parallel_test.go (they require a successful image fix or a
// DB error in the upsert path respectively), so this unit test covers them
// directly.
func TestMergeIntoContrib(t *testing.T) {
	t.Run("fr_nil_persistFailed", func(t *testing.T) {
		acc := &runForArtistAccum{persistOK: true}
		var contrib artistContribution
		acc.mergeIntoContrib(violationOutcome{persistFailed: true}, &contrib)
		if acc.persistOK {
			t.Error("persistOK should be false after persistFailed outcome")
		}
		if contrib.fixesAttempted != 0 || contrib.fixesSucceeded != 0 {
			t.Error("no fixer counters should increment for a nil-fr outcome")
		}
	})

	t.Run("fixed_imageFix", func(t *testing.T) {
		acc := &runForArtistAccum{persistOK: true}
		var contrib artistContribution
		fr := &FixResult{RuleID: "img_rule", Fixed: true, ImageType: "thumb"}
		rv := &RuleViolation{RuleID: "img_rule"}
		acc.mergeIntoContrib(violationOutcome{
			fr: fr, fixed: true, imageFix: true, imageType: "thumb",
			resolvedRow: rv,
		}, &contrib)
		if !acc.artistDirty {
			t.Error("artistDirty should be set on a successful fix")
		}
		if len(acc.fixedImageTypes) != 1 || acc.fixedImageTypes[0] != "thumb" {
			t.Errorf("fixedImageTypes = %v, want [thumb]", acc.fixedImageTypes)
		}
		if acc.metadataFixed {
			t.Error("metadataFixed should not be set for an image fix")
		}
		if contrib.fixesSucceeded != 1 {
			t.Errorf("fixesSucceeded = %d, want 1", contrib.fixesSucceeded)
		}
		if len(acc.resolvedRows) != 1 || acc.resolvedRows[0] != rv {
			t.Error("resolvedRow should be stashed in acc.resolvedRows")
		}
	})

	t.Run("fixed_metadata", func(t *testing.T) {
		acc := &runForArtistAccum{persistOK: true}
		var contrib artistContribution
		fr := &FixResult{RuleID: "bio_rule", Fixed: true}
		acc.mergeIntoContrib(violationOutcome{fr: fr, fixed: true}, &contrib)
		if !acc.metadataFixed {
			t.Error("metadataFixed should be set for a non-image fix")
		}
		if len(acc.fixedImageTypes) != 0 {
			t.Error("fixedImageTypes should be empty for a metadata fix")
		}
	})
}

// TestProcessArtistForRunAll_ProviderIDBackfill_PersistsResultRow is the
// issue #2699 regression: a same-pass auto-fix of provider_id_missing must
// leave the artist with a rule_results row for that rule (pass or fail),
// exactly like every other auto-fixable rule does.
//
// Before the fix, ProviderIDBackfillFixer.Fix wrote the backfilled Discogs/
// Deezer/Spotify ID only to the database (via UpdateProviderField) and never
// updated the in-memory *artist.Artist the pipeline goes on to re-evaluate in
// the same pass (Pipeline.updateHealthScore's postEval := engine.Evaluate(ctx,
// a)). That stale re-read kept reporting the artist as violating
// provider_id_missing, so persistPassResults (which only writes a pass row for
// rules postEval does NOT consider violated) skipped it -- and because the
// violation had already been dispatched-and-resolved, nothing else wrote a row
// either. The artist ended up with a fresh rules_evaluated_at (the pass
// otherwise succeeded) but literally no rule_results row for the rule,
// which is exactly the "no complete evaluation baseline" freeze
// offlineHealthScore logs (fixer.go:2237, first_missing_rule=provider_id_missing).
func TestProcessArtistForRunAll_ProviderIDBackfill_PersistsResultRow(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding default rules: %v", err)
	}

	// provider_id_missing seeds disabled (service.go); enable it in auto mode
	// so the pipeline both evaluates and auto-fixes it, matching the reported
	// prod configuration.
	r, err := ruleSvc.GetByID(ctx, RuleProviderIDMissing)
	if err != nil {
		t.Fatalf("loading provider_id_missing rule: %v", err)
	}
	r.Enabled = true
	r.AutomationMode = AutomationModeAuto
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatalf("enabling provider_id_missing: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	// Discogs and Deezer are "available" (configured), matching the prod rule
	// row's dynamic-default resolution with no RequiredProviderIDs override.
	engine.SetProviderAvailability(&stubProviderAvailability{available: allThreeAvailable()})

	fetcher := &stubMetadataProvider{metadata: mbURLMetadata()}
	fixer := NewProviderIDBackfillFixer(fetcher, artistSvc, testLogger())
	p := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	// An artist with an MBID and no provider IDs: the checker flags it, and
	// the backfill fixer can derive all three from the MusicBrainz relations
	// mbURLMetadata() carries, so the fix succeeds in this same pass.
	a := &artist.Artist{Name: "Backfill Artist", SortName: "Backfill Artist", MusicBrainzID: "mbid-abc", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	if _, ok := p.processArtistForRunAll(ctx, a); !ok {
		t.Fatal("processArtistForRunAll reported a persist failure")
	}

	rows, err := ruleSvc.GetRuleResultsForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetRuleResultsForArtist: %v", err)
	}
	var found *RuleResult
	for i := range rows {
		if rows[i].RuleID == RuleProviderIDMissing {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no rule_results row for %s after a same-pass auto-fix; rows: %+v", RuleProviderIDMissing, rows)
	}
	if !found.Passed {
		t.Errorf("rule_results row for %s has passed=false after the backfill fixed every in-scope ID", RuleProviderIDMissing)
	}
}
