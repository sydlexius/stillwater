package rule

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestProcessArtistForRunRule_EvaluateError covers the early-return branch in
// processArtistForRunRule where the engine's Evaluate fails. It is the
// single-rule counterpart to TestProcessArtistForRunAll_EvaluateError and reuses
// the same injection: the integration tests never surface an engine error, so
// this drives the unit directly. With a cold rule cache, closing the DB forces
// the engine's cachedRules -> List to error, which Evaluate propagates. The
// method must bail with (contrib{}, false) and zero violations recorded so the
// artist stays dirty for the next pass (#983) rather than being silently marked
// evaluated on a dropped evaluation.
func TestProcessArtistForRunRule_EvaluateError(t *testing.T) {
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
	// targetRule is never dereferenced on the Evaluate-error path (the method
	// returns before the automation-mode dispatch), but pass a non-nil rule so
	// the test exercises the real call shape rather than a nil argument.
	contrib, persistOK := p.processArtistForRunRule(context.Background(), a, RuleNFOExists, &Rule{ID: RuleNFOExists})
	if persistOK {
		t.Error("persistOK = true; want false when Evaluate errors (artist must stay dirty for retry)")
	}
	if contrib.violationsFound != 0 {
		t.Errorf("violationsFound = %d; want 0 on the Evaluate-error early return", contrib.violationsFound)
	}
}

// TestRunRuleScoped_EvaluateError_Accounting drives the PUBLIC RunRuleScoped
// through the same Evaluate-error branch and asserts the observable run-result
// accounting the direct unit test cannot see. The pipeline's own
// ruleService/artistService stay on an open DB so the up-front GetByID(rule) and
// the scope=all artist walk both succeed, while the engine is backed by a
// SEPARATE, already-closed DB: its cold-cache cachedRules -> List errors and
// Evaluate propagates the failure for the artist.
//
// The failed evaluation must land in the denominator (ArtistsTotal) but NOT be
// counted as processed -- mergeContribution leaves ArtistsProcessed at 0 on a
// false persistOK so the "evaluating X of Y" summary never over-reports and the
// artist stays dirty for the next pass. RunRuleScoped swallows the per-artist
// error (warn-logged) and still returns a nil error to the caller.
func TestRunRuleScoped_EvaluateError_Accounting(t *testing.T) {
	ctx := context.Background()

	// Main DB: holds the target rule and one eligible artist. Stays open so the
	// pipeline's GetByID(rule), CountEligibleArtists, and scope=all List all
	// succeed -- the run reaches the per-artist evaluation.
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	a := &artist.Artist{Name: "Eval Err Public", SortName: "Eval Err Public", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Engine DB: a distinct database closed before any evaluation, so the
	// engine's cold rule cache forces cachedRules -> List to error. Closing the
	// pipeline's own DB instead would fail GetByID first and never reach the
	// per-artist path this test targets.
	engineDB := setupTestDB(t)
	if err := engineDB.Close(); err != nil {
		t.Fatalf("closing engine db: %v", err)
	}
	engine := NewEngine(NewService(engineDB), engineDB, nil, nil, testLogger())
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	result, err := p.RunRuleScoped(ctx, RuleNFOExists, RunScopeAll)
	if err != nil {
		t.Fatalf("RunRuleScoped returned error; a per-artist Evaluate failure must be swallowed and warn-logged: %v", err)
	}
	if result == nil {
		t.Fatal("RunRuleScoped returned a nil result")
	}
	if result.ArtistsTotal != 1 {
		t.Errorf("ArtistsTotal = %d; want 1 (the eligible artist is the denominator even when its evaluation fails)", result.ArtistsTotal)
	}
	if result.ArtistsProcessed != 0 {
		t.Errorf("ArtistsProcessed = %d; want 0 (a failed Evaluate must not be counted as processed)", result.ArtistsProcessed)
	}
	if result.ViolationsFound != 0 {
		t.Errorf("ViolationsFound = %d; want 0 (the method bails before the violation loop)", result.ViolationsFound)
	}
}

// newSplitDBPipelineForRunRule builds a Pipeline whose evaluate/health writes
// land on a healthy DB while its rule-result/violation writes land on a closed
// DB. The engine and the artist service share the healthy DB (so Evaluate and
// updateHealthScore succeed and postEval is non-nil), while the pipeline's
// ruleService is backed by a separately-migrated DB that is then closed, so any
// UpsertViolation / UpsertRuleResultPass it performs fails. This isolates the
// two post-fix persistence-failure branches of processArtistForRunRule, neither
// of which is reachable from an integration run (updateHealthScore and the
// resolved-row/pass-row writes all share one DB there, so a failure at one
// point fails the earlier writes too).
func newSplitDBPipelineForRunRule(t *testing.T, fixers []Fixer) (*Pipeline, *artist.Service) {
	t.Helper()

	goodDB := setupTestDB(t)
	goodRuleSvc := NewService(goodDB)
	if err := goodRuleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding default rules: %v", err)
	}
	artistSvc := artist.NewService(goodDB)
	engine := NewEngine(goodRuleSvc, goodDB, nil, nil, testLogger())

	// A separate migrated DB, closed immediately so every write through the
	// pipeline's ruleService fails. The engine keeps using goodRuleSvc, so
	// evaluation and the health-score writeback stay healthy.
	badDB := setupTestDB(t)
	badRuleSvc := NewService(badDB)
	if err := badDB.Close(); err != nil {
		t.Fatalf("closing bad db: %v", err)
	}

	p := NewPipeline(engine, artistSvc, badRuleSvc, fixers, nil, testLogger())
	return p, artistSvc
}

// TestProcessArtistForRunRule_FinalizeResolvedRowsError covers the #983-ordering
// branch where updateHealthScore persists cleanly (persistOKHealth == true) but
// the deferred resolved-row upsert then fails. A successful auto-fix stashes a
// resolvedRow; finalizeResolvedRows upserts it through the closed rule DB,
// returns false, and folds into persistOK.
//
// The mock fix mutates an unrelated field, so the artist STILL violates the
// target rule post-fix. That keeps persistPassResults a no-op (the single
// considered rule this pass filters to is still violated, so nothing is
// written), which means the resulting persistOK == false is attributable SOLELY
// to the finalizeResolvedRows failure -- were that branch not taken, persistOK
// would remain true (health write succeeded, pass-row write was skipped).
func TestProcessArtistForRunRule_FinalizeResolvedRowsError(t *testing.T) {
	// Reports a successful fix without actually satisfying NFOExists (only
	// touches Biography), so the post-fix Evaluate still flags the rule.
	fixer := &mockArtistMutatingFixer{
		canFixRuleID: RuleNFOExists,
		mutate: func(a *artist.Artist) {
			a.Biography = "set-by-runrule-finalize-test"
		},
	}
	p, artistSvc := newSplitDBPipelineForRunRule(t, []Fixer{fixer})

	ctx := context.Background()
	a := &artist.Artist{
		Name:      "Finalize Fail",
		SortName:  "Finalize Fail",
		NFOExists: false, // violates RuleNFOExists so the auto-fix dispatches
		Path:      t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist on healthy DB: %v", err)
	}

	// Auto mode so the violation dispatches through processAutoFixViolation and
	// the successful fix stashes a resolvedRow for finalizeResolvedRows.
	targetRule := &Rule{ID: RuleNFOExists, AutomationMode: AutomationModeAuto}
	contrib, persistOK := p.processArtistForRunRule(ctx, a, RuleNFOExists, targetRule)

	if contrib.violationsFound == 0 {
		t.Fatal("violationsFound = 0; expected the NFO-less artist to violate RuleNFOExists so a resolvedRow is produced")
	}
	if persistOK {
		t.Error("persistOK = true; want false when finalizeResolvedRows fails on the closed rule DB")
	}
}

// TestProcessArtistForRunRule_PersistPassResultsError covers the branch where the
// post-fix pass-row write fails. The artist PASSES the target rule (no violation,
// no fix, empty resolvedRows) so finalizeResolvedRows is a no-op that returns
// true -- the finalize branch is deliberately NOT the cause here. updateHealthScore
// still succeeds (healthy DB) so postEval is non-nil, then persistPassResults
// attempts to write the pass row for the target rule through the closed rule DB
// and returns false. The resulting persistOK == false is therefore attributable
// SOLELY to the persistPassResults failure.
func TestProcessArtistForRunRule_PersistPassResultsError(t *testing.T) {
	p, artistSvc := newSplitDBPipelineForRunRule(t, nil)

	ctx := context.Background()
	a := &artist.Artist{
		Name:      "Pass Fail",
		SortName:  "Pass Fail",
		NFOExists: true, // passes RuleNFOExists -> a pass row is written for it
		Path:      t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist on healthy DB: %v", err)
	}

	targetRule := &Rule{ID: RuleNFOExists, AutomationMode: AutomationModeAuto}
	contrib, persistOK := p.processArtistForRunRule(ctx, a, RuleNFOExists, targetRule)

	if contrib.violationsFound != 0 {
		t.Errorf("violationsFound = %d; want 0 since the artist passes RuleNFOExists (no dispatch, empty resolvedRows)", contrib.violationsFound)
	}
	if persistOK {
		t.Error("persistOK = true; want false when persistPassResults fails on the closed rule DB")
	}
}
