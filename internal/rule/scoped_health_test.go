package rule

import (
	"context"
	"slices"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// alwaysFixesFixer reports a successful fix for one rule without touching
// anything. FixViolation only rescores when a fix actually succeeded, so without
// a fixer that returns Fixed the rescore branch is never entered and any test
// aimed at it passes without executing the code it claims to cover.
type alwaysFixesFixer struct{ ruleID string }

func (f *alwaysFixesFixer) CanFix(v *Violation) bool { return v.RuleID == f.ruleID }

func (f *alwaysFixesFixer) Fix(_ context.Context, _ *artist.Artist, v *Violation) (*FixResult, error) {
	return &FixResult{RuleID: v.RuleID, Fixed: true, Message: "stub fix"}, nil
}

// Companion suite to scoped_evaluation_test.go, covering the parts of #2476 that
// scoping alone does not: the offline health recompute, the authoritative-run
// contract, and the two scoped call sites that are not RunRule.
//
// Each test here exists to kill a specific mutant that survived the first round
// of tests. If you weaken one, say which mutant now lives.

// TestRunImageRulesForArtist_MakesNoProviderCalls covers the CATEGORY-scoped
// path (the bulk "fetch images" action), which had the identical defect to the
// single-rule path: it evaluated every enabled rule and discarded the non-image
// violations afterwards, so asking for images queried MusicBrainz per artist.
//
// Mutant this kills: runForArtistFiltered ignoring `scope` and calling Evaluate.
func TestRunImageRulesForArtist_MakesNoProviderCalls(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	enableDiscographyRule(t, ruleSvc)
	engine, rg, md := engineWithProviderStubs(t, ruleSvc, db)
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	a := providerBackedArtist(t, "Category Scoped")
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// POSITIVE CONTROL: the whole-artist run DOES reach the provider. Without
	// this, the zero-calls assertion below could pass simply because nothing is
	// wired up.
	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("positive control: RunForArtist: %v", err)
	}
	if rg.calls == 0 {
		t.Fatal("positive control FAILED: a whole-artist run made no release-group " +
			"call, so the zero-calls assertion below would be vacuous")
	}

	rgBefore, mdBefore := rg.calls, md.calls

	if _, err := p.RunImageRulesForArtist(ctx, a); err != nil {
		t.Fatalf("RunImageRulesForArtist: %v", err)
	}

	if got := rg.calls - rgBefore; got != 0 {
		t.Errorf("RunImageRulesForArtist made %d MusicBrainz release-group call(s); want 0. "+
			"The image-category run must not evaluate the provider-backed rules (#2476).", got)
	}
	if got := md.calls - mdBefore; got != 0 {
		t.Errorf("RunImageRulesForArtist made %d metadata call(s); want 0.", got)
	}
}

// TestFixViolation_MakesNoProviderCalls covers the per-fix path: repairing one
// violation from the UI used to run a full unscoped Evaluate to rescore, so
// fixing a thumbnail issued a MusicBrainz query.
//
// Mutant this kills: FixViolation's rescore reverting to a full Evaluate.
func TestFixViolation_MakesNoProviderCalls(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	enableDiscographyRule(t, ruleSvc)
	engine, rg, md := engineWithProviderStubs(t, ruleSvc, db)

	// The baseline pipeline has NO fixers, so the violations it records survive as
	// open. Registering the fixer up front would let the baseline run auto-fix the
	// very violation this test needs to fix by hand, leaving nothing to act on.
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	a := providerBackedArtist(t, "Fix One")
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// A full run both establishes the baseline and produces violations to fix.
	// It also serves as the POSITIVE CONTROL that the provider is reachable.
	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("positive control: RunForArtist: %v", err)
	}
	if rg.calls == 0 {
		t.Fatal("positive control FAILED: the baseline run made no release-group call")
	}

	open, err := ruleSvc.ListViolations(ctx, "open")
	if err != nil {
		t.Fatalf("listing violations: %v", err)
	}
	var target string
	for i := range open {
		if open[i].RuleID == RuleThumbExists {
			target = open[i].ID
			break
		}
	}
	if target == "" {
		t.Fatalf("fixture produced no open %s violation, so the fixer would never "+
			"fire and the rescore branch would never run", RuleThumbExists)
	}

	rgBefore, mdBefore := rg.calls, md.calls

	// NOW register the fixer. FixViolation rescores ONLY when a fix succeeds, so
	// without a fixer that reports Fixed the rescore branch never executes and
	// every assertion below would hold regardless of what the rescore does.
	pFix := NewPipeline(engine, artistSvc, ruleSvc,
		[]Fixer{&alwaysFixesFixer{ruleID: RuleThumbExists}}, nil, testLogger())

	fr, err := pFix.FixViolation(ctx, target)
	if err != nil {
		t.Fatalf("FixViolation: %v", err)
	}
	if !fr.Fixed {
		t.Fatal("the stub fixer did not report Fixed, so FixViolation skipped the " +
			"rescore entirely and this test proves nothing")
	}

	if got := rg.calls - rgBefore; got != 0 {
		t.Errorf("FixViolation made %d MusicBrainz release-group call(s); want 0. "+
			"Fixing one violation must not re-run the provider-backed checkers (#2476).", got)
	}
	if got := md.calls - mdBefore; got != 0 {
		t.Errorf("FixViolation made %d metadata call(s); want 0.", got)
	}
}

// TestPersistHealthAfterRun_ScopedResultNeverPersistsSubsetScore is the guard on
// the single most dangerous mutant: taking a SCOPED result's HealthScore at face
// value. A scoped result carries HealthScore == 0 by construction, so persisting
// it would zero the health of every artist touched by any single-rule run.
//
// Mutant this kills: `score, ok = postEval.HealthScore, true` for a scoped eval.
func TestPersistHealthAfterRun_ScopedResultNeverPersistsSubsetScore(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	engine, _, _ := engineWithProviderStubs(t, ruleSvc, db)
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	a := providerBackedArtist(t, "Subset Score")
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Establish a full baseline so an honest offline score is derivable.
	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("baseline RunForArtist: %v", err)
	}
	baseline := a.HealthScore
	if baseline == 0 {
		t.Fatal("baseline health is 0; pick a fixture that scores above zero, or " +
			"this test cannot distinguish a persisted subset score from a real one")
	}

	// A scoped evaluation: HealthScore is zero and Scoped is set.
	scoped, err := engine.EvaluateScoped(ctx, a, map[string]bool{RuleImageDuplicateExact: true})
	if err != nil {
		t.Fatalf("EvaluateScoped: %v", err)
	}
	if scoped.HealthScore != 0 || !scoped.Scoped {
		t.Fatalf("precondition: want a scoped result with HealthScore 0; got %v / Scoped=%v",
			scoped.HealthScore, scoped.Scoped)
	}

	if ok := p.persistHealthAfterRun(ctx, a, scoped, false); !ok {
		t.Fatal("persistHealthAfterRun reported the run non-authoritative; the " +
			"evaluation succeeded, so it should be authoritative")
	}

	if a.HealthScore == 0 {
		t.Fatal("the scoped result's subset score (0) was persisted as the artist's " +
			"health. Health must be recomputed offline, never taken from a scoped eval (#2476).")
	}
	if a.HealthScore != baseline {
		t.Errorf("health = %v; want the baseline %v (nothing changed on disk, so the "+
			"offline recompute must reproduce the full evaluation's score)",
			a.HealthScore, baseline)
	}
}

// TestOfflineHealthScore_RefusesWhenAnEligibleRuleHasNoResult pins the
// DENOMINATOR, and with it the refuse-rather-than-guess contract.
//
// The distinguishing scenario is a rule ENABLED since the last full pass: it is
// eligible, but the artist has no rule_results row for it, so the artist's true
// score is unknown. The only honest answer is to refuse and leave the stored
// score alone.
//
// Note it must be an ENABLED-since rule, not a DISABLED-since one: disabling a
// rule also deletes its rows (cleanupDisabledRuleState), so "persisted rows" and
// "eligible rules" stay in agreement and the two denominators are
// indistinguishable. Enabling is the case where they diverge.
//
// Mutant this kills: deriving the denominator from the persisted rows, which
// would score the artist over the rules it happens to have rows for and report a
// confident number for an artist whose state is genuinely unknown.
func TestOfflineHealthScore_RefusesWhenAnEligibleRuleHasNoResult(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	engine, _, _ := engineWithProviderStubs(t, ruleSvc, db)
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	a := providerBackedArtist(t, "Denominator")
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Baseline with discography_populated still DISABLED, so it gets no row.
	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("baseline RunForArtist: %v", err)
	}
	if _, ok := p.offlineHealthScore(ctx, a, nil); !ok {
		t.Fatal("precondition: a complete baseline must be scoreable")
	}

	// Now enable a rule that has never been evaluated for this artist.
	enableDiscographyRule(t, ruleSvc)

	// Cold engine: the rule list is cached for ruleCacheTTL, and this test is
	// about the denominator, not the cache window.
	engine2, _, _ := engineWithProviderStubs(t, ruleSvc, db)
	p2 := NewPipeline(engine2, artistSvc, ruleSvc, nil, nil, testLogger())

	eligible, err := engine2.EligibleRuleIDs(ctx, a)
	if err != nil {
		t.Fatalf("EligibleRuleIDs: %v", err)
	}
	if !slices.Contains(eligible, RuleDiscographyPopulated) {
		t.Fatalf("precondition: %s must now be eligible; eligible=%v",
			RuleDiscographyPopulated, eligible)
	}
	rows, err := ruleSvc.GetRuleResultsForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("reading rule results: %v", err)
	}
	for i := range rows {
		if rows[i].RuleID == RuleDiscographyPopulated {
			t.Fatal("precondition: the newly enabled rule must have NO persisted row")
		}
	}

	if score, ok := p2.offlineHealthScore(ctx, a, nil); ok {
		t.Errorf("offlineHealthScore returned %v for an artist with an eligible rule "+
			"that has never been evaluated; it must REFUSE. The denominator is coming "+
			"from the persisted rows rather than the currently eligible rules, so the "+
			"unevaluated rule is invisible and the score is a confident guess (#2476).",
			score)
	}
}

// TestPersistHealthAfterRun_FailedEvaluationIsNotAuthoritative guards the
// contract the old updateHealthScore encoded by returning a nil result.
//
// If a failed post-fix evaluation reported the run as authoritative, the caller
// would stamp rules_evaluated_at, dropping the artist out of the dirty set with
// stale rule_results so no incremental pass would ever revisit it.
func TestPersistHealthAfterRun_FailedEvaluationIsNotAuthoritative(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	a := &artist.Artist{Name: "Eval Failed", Path: t.TempDir()}

	// A nil postEval is exactly what the callers pass when EvaluateScoped errored.
	if ok := p.persistHealthAfterRun(ctx, a, nil, false); ok {
		t.Error("persistHealthAfterRun reported a FAILED evaluation as authoritative. " +
			"The caller will stamp rules_evaluated_at and the artist will leave the " +
			"dirty set with stale rule_results, never to be re-evaluated.")
	}
	if a.HealthEvaluatedAt != nil {
		t.Error("HealthEvaluatedAt was stamped despite the evaluation failing")
	}
}
