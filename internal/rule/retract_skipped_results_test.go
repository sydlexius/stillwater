package rule

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// Regression suite for the second half of #2509: RETRACTION.
//
// The capability gate stops the evaluator from writing a pass row for a rule it
// never ran, but it does nothing about the rows written BEFORE the gate existed
// (157 artists x 2 duplicate rules on the maintainer's library), nor about an
// artist that later loses the data a rule needs. persistPassResults only walks
// RulesConsidered, and a skipped rule is by construction not in that set, so a
// stale row is simply never revisited. Every reader that does not consult the
// gate -- the artist rule-result breakdown, the compliance grid, the per-rule
// pass-rate dashboards -- keeps reporting it as a genuine pass.
//
// The invariant pinned here: after ANY evaluation of an artist, a rule that was
// skipped for that artist has NO rule_results row.

// staleDuplicatePassRows is the exact pre-upgrade database state: a path-less
// artist carrying passed=1 rows for both duplicate rules, written by the old
// code when the checkers returned nil (= "no violation" = PASS) for any artist
// without a directory.
func staleDuplicatePassRows(t *testing.T, db *sql.DB, artistID string) {
	t.Helper()
	svc := NewService(db)
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if err := svc.UpsertRuleResultPass(t.Context(), artistID, id, time.Now().UTC()); err != nil {
			t.Fatalf("seeding stale pass row for %s: %v", id, err)
		}
	}
	// POSITIVE CONTROL. Without this the "the rows are gone" assertions below
	// pass vacuously on a database where they were never written at all.
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		passed, exists := ruleResultRow(t, db, artistID, id)
		if !exists {
			t.Fatalf("positive control FAILED: no stale row for %s was seeded, so this test "+
				"cannot prove anything about retracting it", id)
		}
		if !passed {
			t.Fatalf("positive control FAILED: seeded row for %s is not a PASS row", id)
		}
	}
}

// ruleResultRow reads one (artist, rule) row straight from the table, bypassing
// every service-level filter so the assertion is about what is actually stored.
func ruleResultRow(t *testing.T, db *sql.DB, artistID, ruleID string) (passed, exists bool) {
	t.Helper()
	var p int
	err := db.QueryRowContext(t.Context(),
		`SELECT passed FROM rule_results WHERE artist_id = ? AND rule_id = ?`,
		artistID, ruleID).Scan(&p)
	if err == sql.ErrNoRows {
		return false, false
	}
	if err != nil {
		t.Fatalf("reading rule_results row (%s, %s): %v", artistID, ruleID, err)
	}
	return p == 1, true
}

// TestRunForArtist_RetractsStaleDuplicatePassRows is the acceptance criterion.
//
// Mutant this kills: dropping the retractSkippedResults call from
// runForArtist/processArtistForRunAll, or making Service.DeleteRuleResult a
// no-op. Either restores the permanent phantom pass.
func TestRunForArtist_RetractsStaleDuplicatePassRows(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	// A path-less artist with TWO unhashed images: a pair exists (so a duplicate
	// is possible and the rules are not trivially satisfied) but nothing about it
	// can be compared, so BOTH duplicate rules are genuinely skipped. An artist
	// with ONE image would now PASS them, and this test would be vacuous.
	a := apiOnlyArtist(t, db, "API Stale Pass")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID)

	// PRECONDITION: the rules really are skipped for the state this run will see.
	// If they were evaluated instead, "the row is gone" would be testing something
	// else. It is asserted BEFORE the run, deliberately: the pipeline persists the
	// artist on its way out, and the in-memory artist a test hands it carries no
	// Images, so ArtistImageRepo.UpsertAll marks the seeded rows exists_flag = 0
	// afterwards. Re-deriving the skip set post-run would read a DIFFERENT artist
	// state (zero images -> both rules trivially pass) than the run itself acted on.
	res, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, skipped := skipReasonFor(res, id); !skipped {
			t.Fatalf("precondition: rule %s was NOT skipped for this artist; the test is not "+
				"exercising retraction at all", id)
		}
	}

	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, exists := ruleResultRow(t, db, a.ID, id); exists {
			t.Errorf("rule %s STILL has a rule_results row after an evaluation that skipped it. "+
				"A skipped rule must have its stale row RETRACTED, not merely left unwritten: "+
				"the artist detail page, the compliance grid and the pass-rate dashboards all "+
				"read this row and will keep reporting a pass for a rule that never ran.", id)
		}
	}

	// The rule must also not surface as a pass through the enabled-rule reader
	// that the artist-detail breakdown uses.
	enabled, err := ruleSvc.GetEnabledRuleResultsForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetEnabledRuleResultsForArtist: %v", err)
	}
	for i := range enabled {
		if enabled[i].RuleID == RuleImageDuplicate || enabled[i].RuleID == RuleImageDuplicateExact {
			t.Errorf("artist-detail reader still reports %s (passed=%v) for an artist the rule "+
				"never examined", enabled[i].RuleID, enabled[i].Passed)
		}
	}
}

// TestRunForArtist_RetractionDoesNotDeleteEvaluatedPasses is the "did you delete
// too much" guard. A rule that WAS evaluated and passed keeps its row.
//
// Mutant this kills: retracting on RulesConsidered (or on everything) instead of
// on RulesSkipped, which would wipe the health baseline for every artist and
// leave offlineHealthScore permanently refusing to score.
func TestRunForArtist_RetractionDoesNotDeleteEvaluatedPasses(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API Evaluated Pass")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID)

	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	eligible, err := engine.EligibleRuleIDs(ctx, a)
	if err != nil {
		t.Fatalf("EligibleRuleIDs: %v", err)
	}
	// POSITIVE CONTROL: at least one rule IS eligible, otherwise "every eligible
	// rule kept its row" is trivially true.
	if len(eligible) == 0 {
		t.Fatal("positive control FAILED: no rule is eligible for this artist at all")
	}
	for _, id := range eligible {
		if _, exists := ruleResultRow(t, db, a.ID, id); !exists {
			t.Errorf("eligible rule %s has NO rule_results row after a full run. Retraction "+
				"deleted a row it must not touch; offlineHealthScore will count this rule as "+
				"missing and refuse to score the artist.", id)
		}
	}
}

// TestRunForArtist_ArtistWithPathKeepsDuplicateRows pins the other side of the
// capability predicate: an artist WITH a local directory can always run the
// duplicate rules, so its rows are written and survive.
//
// Mutant this kills: a retraction keyed on the rule ID rather than on the
// per-artist skip set, which would clear duplicate rows for every artist.
func TestRunForArtist_ArtistWithPathKeepsDuplicateRows(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	artistSvc := artist.NewService(db)
	p := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	a := &artist.Artist{Name: "Local Artist", SortName: "Local Artist", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	res, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		// PRECONDITION: an artist WITH a path must NOT be skipped for these rules.
		if reason, skipped := skipReasonFor(res, id); skipped {
			t.Fatalf("precondition: rule %s was skipped for an artist WITH a path (reason: %q); "+
				"the capability gate is wrong, and this test cannot check retraction scope", id, reason)
		}
		passed, exists := ruleResultRow(t, db, a.ID, id)
		if !exists {
			t.Errorf("rule %s has no rule_results row for an artist with a local path. It was "+
				"evaluated, so its outcome must be recorded; retraction is over-reaching.", id)
			continue
		}
		if !passed {
			t.Errorf("rule %s = fail for an artist with an empty image directory; want pass", id)
		}
	}
}

// TestRunImageRulesForArtist_RetractsStaleDuplicatePassRows is the acceptance
// criterion for the CATEGORY-SCOPED path -- the one the bulk "Fetch Images"
// action actually calls (handlers_bulk_actions.go -> RunImageRulesForArtist).
//
// This is where the original bug survived the first fix. ScopeForCategory used to
// build its scope from the ELIGIBLE rules only, and EvaluateScoped keeps a skipped
// rule in RulesSkipped only when the rule is in scope, so a category run always
// reported RulesSkipped as empty and retracted nothing at all. An artist that was
// evaluated while it had a path, then lost it (a library re-imported API-only),
// kept a green "passed" badge for a rule that never examined it, forever.
//
// Mutant this kills: dropping the skipped rules back out of ScopeForCategory.
func TestRunImageRulesForArtist_RetractsStaleDuplicatePassRows(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API Category Stale Pass")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID)

	// PRECONDITION: this is genuinely a category-scoped run over rules that are
	// genuinely SKIPPED. Both halves matter: if the rules were evaluated, or if
	// the scope did not cover them, "the rows are gone" would be proving
	// something else.
	scope, err := engine.ScopeForCategory(ctx, a, "image")
	if err != nil {
		t.Fatalf("ScopeForCategory: %v", err)
	}
	pre, err := engine.EvaluateScoped(ctx, a, scope)
	if err != nil {
		t.Fatalf("EvaluateScoped: %v", err)
	}
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if !scope[id] {
			t.Fatalf("precondition: rule %s is not in the image-category scope, so a category "+
				"run cannot retract it", id)
		}
		if _, skipped := skipReasonFor(pre, id); !skipped {
			t.Fatalf("precondition: rule %s was NOT reported as skipped by the CATEGORY-SCOPED "+
				"evaluation; the retraction has nothing to act on and this test is vacuous", id)
		}
	}

	if _, err := p.RunImageRulesForArtist(ctx, a); err != nil {
		t.Fatalf("RunImageRulesForArtist: %v", err)
	}

	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, exists := ruleResultRow(t, db, a.ID, id); exists {
			t.Errorf("rule %s STILL has a rule_results row after the bulk fetch-images run "+
				"skipped it. The category-scoped retraction did not fire, so the artist keeps a "+
				"green Passed badge for a rule that never examined it (#2509).", id)
		}
	}
}

// TestRunImageRulesForArtist_LeavesSkippedRuleOutsideCategoryAlone is the "did
// you retract too much" guard on the category path. nfo_exists is skipped for a
// path-less artist too, but it is not in the image category, so a fetch-images
// run must not touch its row: that run never spoke to it.
//
// Mutant this kills: retracting from an UNSCOPED skip set (e.g. reverting the
// scope filter in EvaluateScoped, or retracting engine.Evaluate's RulesSkipped),
// which would let a category run clear verdicts it was never asked about.
func TestRunImageRulesForArtist_LeavesSkippedRuleOutsideCategoryAlone(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API Cross Category")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID)
	if err := ruleSvc.UpsertRuleResultPass(ctx, a.ID, RuleNFOExists, time.Now().UTC()); err != nil {
		t.Fatalf("seeding nfo_exists row: %v", err)
	}

	// PRECONDITION: nfo_exists really is skipped for this artist (it is
	// filesystem-dependent and the artist has no directory) and really is outside
	// the image category. Otherwise "its row survived" says nothing about scope.
	full, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, skipped := skipReasonFor(full, RuleNFOExists); !skipped {
		t.Fatalf("precondition: %s was not skipped for a path-less artist", RuleNFOExists)
	}
	scope, err := engine.ScopeForCategory(ctx, a, "image")
	if err != nil {
		t.Fatalf("ScopeForCategory: %v", err)
	}
	if scope[RuleNFOExists] {
		t.Fatalf("precondition: %s landed in the image-category scope; the category filter "+
			"in ScopeForCategory is broken", RuleNFOExists)
	}
	if _, exists := ruleResultRow(t, db, a.ID, RuleNFOExists); !exists {
		t.Fatalf("precondition: the nfo_exists row was not seeded")
	}

	if _, err := p.RunImageRulesForArtist(ctx, a); err != nil {
		t.Fatalf("RunImageRulesForArtist: %v", err)
	}

	if _, exists := ruleResultRow(t, db, a.ID, RuleNFOExists); !exists {
		t.Errorf("the fetch-images run retracted the %s row. A category-scoped run must only "+
			"withdraw verdicts for the rules it was asked about; this one belongs to another "+
			"category and its row is not this run's to touch.", RuleNFOExists)
	}
	// ... and it still retracted the ones it WAS asked about.
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, exists := ruleResultRow(t, db, a.ID, id); exists {
			t.Errorf("in-category skipped rule %s kept its stale row", id)
		}
	}
}

// TestEvaluateScoped_SkippedRulesInScopeDoNotInflateCounts pins the invariant
// that makes the ScopeForCategory fix safe: putting an INELIGIBLE rule ID in the
// scope must not evaluate it, count it, or score it. The scope entry exists only
// so the skipped-set filter can recognize the rule as in-category.
//
// Mutant this kills: "fixing" the scope by making EvaluateScoped iterate the
// scope instead of the eligible set, which would run a checker for an artist the
// capability gate just ruled out (and, for provider-backed rules, issue the
// unrequested outbound call #2476 exists to prevent).
func TestEvaluateScoped_SkippedRulesInScopeDoNotInflateCounts(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, _ := dupRuleEngine(t, db)

	a := apiOnlyArtist(t, db, "API Scoped Counts")
	insertBlindFanartPair(t, db, a.ID)

	scope, err := engine.ScopeForCategory(ctx, a, "image")
	if err != nil {
		t.Fatalf("ScopeForCategory: %v", err)
	}
	res, err := engine.EvaluateScoped(ctx, a, scope)
	if err != nil {
		t.Fatalf("EvaluateScoped: %v", err)
	}

	// POSITIVE CONTROL: the scope does contain the skipped duplicate rules, and
	// some other image rule IS eligible -- so "not counted" is a real assertion
	// and not the trivial consequence of an empty run.
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if !scope[id] {
			t.Fatalf("precondition: skipped rule %s is not in the scope", id)
		}
	}
	if res.RulesTotal == 0 {
		t.Fatal("precondition: the image category evaluated no rule at all for this artist")
	}

	if res.RulesTotal != len(res.RulesConsidered) {
		t.Errorf("RulesTotal = %d but RulesConsidered has %d entries; a scoped run must count "+
			"exactly the rules it evaluated", res.RulesTotal, len(res.RulesConsidered))
	}
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if slices.Contains(res.RulesConsidered, id) {
			t.Errorf("skipped rule %s appears in RulesConsidered. Being in the scope must not "+
				"make an INELIGIBLE rule evaluated, counted, or scored -- only recognized as "+
				"in-category by the skipped-set filter.", id)
		}
		if _, skipped := skipReasonFor(res, id); !skipped {
			t.Errorf("skipped rule %s is missing from RulesSkipped for a scope that contains it", id)
		}
	}
	if res.RulesPassed > res.RulesTotal {
		t.Errorf("RulesPassed = %d > RulesTotal = %d", res.RulesPassed, res.RulesTotal)
	}
}

// TestRunForArtistFiltered_CategoryOfOnlySkippedRulesEvaluatesNothingAndRetracts
// is the corner the #2508 nil-versus-empty rule was written for, now that the
// scope carries skipped rules: a category whose every rule is skipped for this
// artist must evaluate NOTHING (never fall back to "run everything") and must
// still withdraw those rules' stale rows.
func TestRunForArtistFiltered_CategoryOfOnlySkippedRulesEvaluatesNothingAndRetracts(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	// Reduce the nfo category to exactly one rule -- nfo_exists, which is
	// filesystem-dependent and therefore always skipped for a path-less artist.
	all, err := ruleSvc.List(ctx)
	if err != nil {
		t.Fatalf("listing rules: %v", err)
	}
	for i := range all {
		r := &all[i]
		if string(r.Category) != string(RuleCategoryNFO) || r.ID == RuleNFOExists || !r.Enabled {
			continue
		}
		r.Enabled = false
		if err := ruleSvc.Update(ctx, r); err != nil {
			t.Fatalf("disabling %s: %v", r.ID, err)
		}
	}
	engine.InvalidateRuleCache()

	a := apiOnlyArtist(t, db, "API NFO Only Skipped")
	if err := ruleSvc.UpsertRuleResultPass(ctx, a.ID, RuleNFOExists, time.Now().UTC()); err != nil {
		t.Fatalf("seeding stale nfo_exists row: %v", err)
	}

	scope, err := engine.ScopeForCategory(ctx, a, string(RuleCategoryNFO))
	if err != nil {
		t.Fatalf("ScopeForCategory: %v", err)
	}
	// PRECONDITIONS: the scope is exactly the one skipped rule, and its stale row
	// is really there.
	if len(scope) != 1 || !scope[RuleNFOExists] {
		t.Fatalf("precondition: nfo scope = %v; want exactly {%s}", scope, RuleNFOExists)
	}
	if _, exists := ruleResultRow(t, db, a.ID, RuleNFOExists); !exists {
		t.Fatalf("precondition: the stale nfo_exists row was not seeded")
	}
	pre, err := engine.EvaluateScoped(ctx, a, scope)
	if err != nil {
		t.Fatalf("EvaluateScoped: %v", err)
	}
	if len(pre.RulesConsidered) != 0 || pre.RulesTotal != 0 {
		t.Fatalf("a category of only-skipped rules evaluated %d rule(s) (%v); it must evaluate "+
			"NOTHING, not fall back to the full set", pre.RulesTotal, pre.RulesConsidered)
	}
	if _, skipped := skipReasonFor(pre, RuleNFOExists); !skipped {
		t.Fatalf("precondition: %s is not in RulesSkipped for its own scope", RuleNFOExists)
	}

	if _, err := p.runForArtistFiltered(ctx, a, string(RuleCategoryNFO)); err != nil {
		t.Fatalf("runForArtistFiltered: %v", err)
	}

	if _, exists := ruleResultRow(t, db, a.ID, RuleNFOExists); exists {
		t.Errorf("the stale %s row survived a run over the category that owns it. A rule that "+
			"is skipped for this artist must leave no verdict behind.", RuleNFOExists)
	}
	// The duplicate rules are in a different category and were never seeded, but
	// the image-category rules must not have been evaluated at all: a run over a
	// scope of only-skipped rules writes no pass rows anywhere.
	for _, id := range []string{RuleImageDuplicate, RuleThumbExists} {
		if _, exists := ruleResultRow(t, db, a.ID, id); exists {
			t.Errorf("running the nfo category wrote a rule_results row for %s. An empty "+
				"evaluation set must not fall back to evaluating every rule.", id)
		}
	}
}

// TestRetraction_ResolvesOpenViolationForSkippedRule covers the OTHER table.
//
// rule_results was only half the stale verdict. An artist that had an OPEN
// image_duplicate violation and then lost capability (path cleared, hashes gone)
// keeps that violation forever: persistPassResults only resolves violations for
// rules in RulesConsidered, and a skipped rule is never in that set. The finding
// keeps counting against compliance and showing in "needs attention" with no
// evaluation left that could ever clear it -- and, worse, startup's
// backfillRuleResultsFromViolations re-creates a rule_results row from any open
// violation, resurrecting the row the retraction just deleted.
//
// Mutant this kills: reverting RetractRuleVerdict to a bare rule_results DELETE.
func TestRetraction_ResolvesOpenViolationForSkippedRule(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API Open Violation")
	insertBlindFanartPair(t, db, a.ID)

	// The verdict an earlier, capable evaluation left behind: an open finding for
	// the perceptual duplicate rule, and a dismissed one for the exact rule (the
	// operator's terminal decision, which retraction must NOT overwrite).
	seedViolation(t, ruleSvc, a, RuleImageDuplicate, ViolationStatusOpen)
	seedViolation(t, ruleSvc, a, RuleImageDuplicateExact, ViolationStatusDismissed)

	// PRECONDITIONS: the rows are really there with the statuses this test
	// reasons about, and both rules really are skipped now.
	if got := violationStatus(t, db, a.ID, RuleImageDuplicate); got != ViolationStatusOpen {
		t.Fatalf("precondition: seeded %s violation has status %q, want %q",
			RuleImageDuplicate, got, ViolationStatusOpen)
	}
	if got := violationStatus(t, db, a.ID, RuleImageDuplicateExact); got != ViolationStatusDismissed {
		t.Fatalf("precondition: seeded %s violation has status %q, want %q",
			RuleImageDuplicateExact, got, ViolationStatusDismissed)
	}
	res, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, skipped := skipReasonFor(res, id); !skipped {
			t.Fatalf("precondition: rule %s was not skipped, so this test proves nothing about "+
				"a skipped rule's violation", id)
		}
	}

	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	if got := violationStatus(t, db, a.ID, RuleImageDuplicate); got != ViolationStatusResolved {
		t.Errorf("open violation for skipped rule %s has status %q after an evaluation that "+
			"skipped it; want %q. Nothing else will ever re-check this rule for this artist, so "+
			"the finding would count against compliance forever.",
			RuleImageDuplicate, got, ViolationStatusResolved)
	}
	if got := violationStatus(t, db, a.ID, RuleImageDuplicateExact); got != ViolationStatusDismissed {
		t.Errorf("retraction changed a DISMISSED violation for %s to %q. Dismissal is the "+
			"operator's terminal decision (#1107) and is not a verdict this code may withdraw.",
			RuleImageDuplicateExact, got)
	}
}

// TestRunAllScoped_RetractsStaleDuplicatePassRows covers the BULK sweep --
// RunAllScoped -> processArtistForRunAll -- which is the path the "Run Rules"
// button drives and the one migration 024's dirty-marking self-heal depends on:
// the migration NULLs rules_evaluated_at precisely so the next incremental pass
// re-walks these artists through here.
//
// It had NO test. A reviewer deleted the entire retraction block from
// processArtistForRunAll, ran the whole package, and it stayed green. The
// migration's whole story rested on unexercised code.
//
// Mutant this kills: deleting the retractSkippedResults call from
// processArtistForRunAll (verified: this test fails, the rows survive).
func TestRunAllScoped_RetractsStaleDuplicatePassRows(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API RunAll Stale Pass")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID)

	// PRECONDITION: both rules really are SKIPPED for this artist by the UNSCOPED
	// evaluation processArtistForRunAll performs. If they were merely passing, the
	// "rows are gone" assertion below would be testing the pass path instead.
	pre, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, skipped := skipReasonFor(pre, id); !skipped {
			t.Fatalf("precondition: rule %s was NOT skipped for this artist, so the bulk run "+
				"would evaluate it and this test would prove nothing about retraction", id)
		}
	}

	res, err := p.RunAllScoped(ctx, RunScopeAll)
	if err != nil {
		t.Fatalf("RunAllScoped: %v", err)
	}
	// POSITIVE CONTROL: the sweep actually visited this artist. A scope that
	// walked nobody would leave the rows in place for the wrong reason.
	if res.ArtistsProcessed == 0 {
		t.Fatalf("positive control FAILED: RunAllScoped processed 0 artists")
	}

	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, exists := ruleResultRow(t, db, a.ID, id); exists {
			t.Errorf("the bulk Run Rules sweep left the stale rule_results row for skipped rule "+
				"%s in place. This is the path migration 024's dirty-marking self-heal relies on: "+
				"the migration re-dirties these artists so this sweep can retract their rows, and "+
				"if it does not, every re-imported API-only artist keeps a phantom pass.", id)
		}
	}
}

// TestRunRuleScoped_RetractsStaleDuplicatePassRowsWithinItsScope covers the
// single-rule library sweep (RunRuleScoped -> processArtistForRunRule), the other
// retraction call site that no test exercised.
//
// It pins BOTH halves of that call site: the retraction fires, and it fires only
// within the run's scope. A single-rule run must not withdraw a verdict for a rule
// it was never asked to evaluate.
//
// Mutant this kills: deleting the retractSkippedResults call from
// processArtistForRunRule -- the requested rule's stale row then survives.
//
// It does NOT kill "pass nil instead of passFilter" there, and cannot: EvaluateScoped
// already narrows RulesSkipped to the run's scope, and processArtistForRunRule scopes
// to the single requested rule, so RulesSkipped can never name another rule and the
// filter has nothing left to exclude. The filter is defense in depth, kept to mirror
// persistPassResults, and it is inert by construction rather than by luck. The scope
// assertion below still pins the two-layer mutant: remove BOTH the filter and
// EvaluateScoped's scope narrowing and the other rule's row does disappear.
func TestRunRuleScoped_RetractsStaleDuplicatePassRowsWithinItsScope(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API RunRule Stale Pass")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID)

	// PRECONDITION: the requested rule is genuinely SKIPPED by the single-rule
	// scoped evaluation this run performs, and it is genuinely reported as such.
	only := map[string]bool{RuleImageDuplicate: true}
	pre, err := engine.EvaluateScoped(ctx, a, only)
	if err != nil {
		t.Fatalf("EvaluateScoped: %v", err)
	}
	if _, skipped := skipReasonFor(pre, RuleImageDuplicate); !skipped {
		t.Fatalf("precondition: %s was not reported as skipped by the single-rule scoped "+
			"evaluation; there is nothing for the retraction to act on", RuleImageDuplicate)
	}
	if len(pre.RulesConsidered) != 0 {
		t.Fatalf("precondition: the scoped run evaluated %v; it must evaluate nothing here",
			pre.RulesConsidered)
	}

	res, err := p.RunRuleScoped(ctx, RuleImageDuplicate, RunScopeAll)
	if err != nil {
		t.Fatalf("RunRuleScoped: %v", err)
	}
	if res.ArtistsProcessed == 0 {
		t.Fatalf("positive control FAILED: RunRuleScoped processed 0 artists")
	}

	if _, exists := ruleResultRow(t, db, a.ID, RuleImageDuplicate); exists {
		t.Errorf("the single-rule sweep for %s left its stale rule_results row in place for an "+
			"artist it skipped; the rule keeps reporting a pass it never earned", RuleImageDuplicate)
	}
	// Scope guard: the run was asked about ONE rule. The other duplicate rule was
	// skipped too, but this run never spoke to it, so its row is not this run's to
	// withdraw.
	if _, exists := ruleResultRow(t, db, a.ID, RuleImageDuplicateExact); !exists {
		t.Errorf("the single-rule sweep for %s also retracted the %s row. A scoped run must only "+
			"withdraw verdicts for the rule it was asked about.",
			RuleImageDuplicate, RuleImageDuplicateExact)
	}
}

// TestRetraction_PreservesPendingChoiceViolation is the human-decision guard.
//
// pending_choice means an image violation is parked awaiting the OPERATOR's
// decision on which candidate image to keep. A capability loss is transient -- a
// rescan that cleared the stored hashes, an artist re-imported API-only -- and
// silently resolving that violation would destroy a queued human decision to tidy
// up a stale row. Automation must never overrule a human decision, so retraction
// resolves OPEN violations only, exactly as it already leaves DISMISSED alone.
//
// Mutant this kills: reverting RetractRuleVerdict to ResolveViolationIfActive (or
// to status IN (open, pending_choice)), which resolves the parked decision.
func TestRetraction_PreservesPendingChoiceViolation(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API Pending Choice")
	insertBlindFanartPair(t, db, a.ID)

	seedViolation(t, ruleSvc, a, RuleImageDuplicate, ViolationStatusPendingChoice)
	seedViolation(t, ruleSvc, a, RuleImageDuplicateExact, ViolationStatusDismissed)
	staleDuplicatePassRows(t, db, a.ID)

	// PRECONDITIONS: the statuses this test reasons about are really stored, and
	// both rules are really skipped now (so retraction really does run over them).
	if got := violationStatus(t, db, a.ID, RuleImageDuplicate); got != ViolationStatusPendingChoice {
		t.Fatalf("precondition: seeded %s violation has status %q, want %q",
			RuleImageDuplicate, got, ViolationStatusPendingChoice)
	}
	if got := violationStatus(t, db, a.ID, RuleImageDuplicateExact); got != ViolationStatusDismissed {
		t.Fatalf("precondition: seeded %s violation has status %q, want %q",
			RuleImageDuplicateExact, got, ViolationStatusDismissed)
	}
	pre, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, skipped := skipReasonFor(pre, id); !skipped {
			t.Fatalf("precondition: rule %s was not skipped, so retraction never runs for it and "+
				"this test proves nothing", id)
		}
	}

	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	// POSITIVE CONTROL: retraction genuinely ran -- the stale rule_results rows are
	// gone. Without this, "the violation survived" would also hold if retraction
	// never fired at all.
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, exists := ruleResultRow(t, db, a.ID, id); exists {
			t.Fatalf("positive control FAILED: retraction did not run (the stale %s row is still "+
				"there), so this test cannot say anything about what it preserves", id)
		}
	}

	if got := violationStatus(t, db, a.ID, RuleImageDuplicate); got != ViolationStatusPendingChoice {
		t.Errorf("retraction changed a PENDING_CHOICE violation for %s to %q. That violation is "+
			"parked awaiting the operator's decision on which image to keep; resolving it on a "+
			"transient capability loss destroys a queued human decision, and automation must "+
			"never overrule a human decision.", RuleImageDuplicate, got)
	}
	if got := violationStatus(t, db, a.ID, RuleImageDuplicateExact); got != ViolationStatusDismissed {
		t.Errorf("retraction changed a DISMISSED violation for %s to %q", RuleImageDuplicateExact, got)
	}
}

// TestRunForArtist_NothingToCompareWritesAPassRow closes the loop on the
// capability change at the PERSISTENCE layer: a path-less artist with one image is
// now eligible, so a full run must record a genuine passed=1 row for both
// duplicate rules -- the checker really does find no pairs and return no
// violation, rather than erroring or being quietly dropped.
//
// Mutant this kills: making the checker raise a violation (or the gate skip) for
// an artist with fewer than two candidate images. Either leaves no pass row here.
func TestRunForArtist_NothingToCompareWritesAPassRow(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API Single Image")
	insertAPIImage(t, db, a.ID, "fanart", 0, hashUnknown, "")

	// PRECONDITION: no row exists yet, so "there is a pass row" cannot be inherited
	// from a seed.
	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, exists := ruleResultRow(t, db, a.ID, id); exists {
			t.Fatalf("precondition: a rule_results row for %s already exists before the run", id)
		}
	}

	if _, err := p.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		passed, exists := ruleResultRow(t, db, a.ID, id)
		if !exists {
			t.Errorf("rule %s recorded NO outcome for a path-less artist with one image. It has "+
				"no pair to compare, so it cannot hold a duplicate: the rule is trivially "+
				"satisfied and must record an honest PASS, not vanish from the artist's "+
				"breakdown.", id)
			continue
		}
		if !passed {
			t.Errorf("rule %s = FAIL for an artist with a single image; a duplicate needs a pair", id)
		}
	}
}

// seedViolation writes one rule_violations row for (artist, rule) with the given
// status, exactly as a capable evaluation would have left it.
func seedViolation(t *testing.T, svc *Service, a *artist.Artist, ruleID, status string) {
	t.Helper()
	rv := &RuleViolation{
		RuleID:     ruleID,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "warning",
		Message:    "seeded by test",
		Fixable:    true,
		Status:     status,
	}
	if err := svc.UpsertViolation(t.Context(), rv); err != nil {
		t.Fatalf("seeding %s violation for %s: %v", status, ruleID, err)
	}
}

// violationStatus reads the stored status straight from the table, bypassing
// every service-level filter.
func violationStatus(t *testing.T, db *sql.DB, artistID, ruleID string) string {
	t.Helper()
	var status string
	err := db.QueryRowContext(t.Context(),
		`SELECT status FROM rule_violations WHERE artist_id = ? AND rule_id = ?`,
		artistID, ruleID).Scan(&status)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("reading rule_violations row (%s, %s): %v", artistID, ruleID, err)
	}
	return status
}

// TestHealthSubscriber_RetractsStaleDuplicatePassRows covers the second
// persistence path. The subscriber runs on plain artist updates, entirely
// outside the pipeline, and already carries a compensating write for the
// pass -> fail transition; pass -> skipped needs the same treatment.
//
// Mutant this kills: dropping the RulesSkipped loop from evaluateArtist.
func TestHealthSubscriber_RetractsStaleDuplicatePassRows(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, _ := dupRuleEngine(t, db)
	artistSvc := artist.NewService(db)

	a := apiOnlyArtist(t, db, "API Sub Stale Pass")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID)

	NewHealthSubscriber(engine, artistSvc, testLogger()).evaluateArtist(ctx, a.ID)

	for _, id := range []string{RuleImageDuplicate, RuleImageDuplicateExact} {
		if _, exists := ruleResultRow(t, db, a.ID, id); exists {
			t.Errorf("health subscriber left the stale rule_results row for skipped rule %s in "+
				"place; it must retract it, as it already does for the pass -> fail transition", id)
		}
	}
}

// armRuleResultsDeleteTrap makes any DELETE of the given rule's rule_results row
// fail, and ONLY that rule's. This is honest failure injection: a real SQLite
// trigger aborts a real write on a real connection, so the production code sees
// exactly the driver error it would see from a disk fault or a lock timeout. No
// mock is involved, and nothing about the read path is altered.
//
// Trapping ONE rule is what gives the tests below their teeth. Both duplicate
// rules are skipped for a blind-fanart artist, so a retraction loop that ABORTS on
// the first failure and one that WARNS AND CONTINUES are distinguishable: only the
// second one goes on to retract the untrapped rule.
//
// Install it AFTER seeding, or the seed writes trip it.
func armRuleResultsDeleteTrap(t *testing.T, db *sql.DB, ruleID string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), fmt.Sprintf(`
		CREATE TRIGGER trap_delete_rule_results
		BEFORE DELETE ON rule_results
		WHEN OLD.rule_id = %s
		BEGIN
			SELECT RAISE(ABORT, 'injected: rule_results DELETE failed');
		END;`, quoteSQLLiteral(ruleID)))
	if err != nil {
		t.Fatalf("arming the rule_results DELETE trap: %v", err)
	}
}

// armViolationUpdateTrap makes any UPDATE of the given rule's rule_violations rows
// fail. Same rationale as armRuleResultsDeleteTrap.
func armViolationUpdateTrap(t *testing.T, db *sql.DB, ruleID string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), fmt.Sprintf(`
		CREATE TRIGGER trap_update_rule_violations
		BEFORE UPDATE ON rule_violations
		WHEN OLD.rule_id = %s
		BEGIN
			SELECT RAISE(ABORT, 'injected: rule_violations UPDATE failed');
		END;`, quoteSQLLiteral(ruleID)))
	if err != nil {
		t.Fatalf("arming the rule_violations UPDATE trap: %v", err)
	}
}

// quoteSQLLiteral renders a Go string as a SQL string literal. Trigger bodies
// cannot take bound parameters, so the rule ID has to be inlined; the values are
// this package's own rule-ID constants, and doubling any quote keeps that true
// even if one ever grows an apostrophe.
func quoteSQLLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// skippedDuplicateRulesInOrder returns the two duplicate rule IDs in the order the
// engine reports them in RulesSkipped, which is the order the retraction loops walk.
//
// The order is load-bearing and it is NOT alphabetical, so it must not be assumed.
// A test that proves "the loop CONTINUES past a failure" has to trap the rule the
// loop reaches FIRST: trap the second one instead and an implementation that aborts
// on the first error still retracts the first rule before dying, the assertion holds,
// and the test passes while proving nothing. (That mistake was made here first and
// caught by mutation-testing the abort variant, which survived.)
func skippedDuplicateRulesInOrder(t *testing.T, res *EvaluationResult) (first, second string) {
	t.Helper()
	var order []string
	for _, s := range res.RulesSkipped {
		if s.RuleID == RuleImageDuplicate || s.RuleID == RuleImageDuplicateExact {
			order = append(order, s.RuleID)
		}
	}
	if len(order) != 2 {
		t.Fatalf("precondition: want BOTH duplicate rules skipped (the loop needs a rule to "+
			"continue TO after the trapped one fails), got %v from RulesSkipped %+v",
			order, res.RulesSkipped)
	}
	return order[0], order[1]
}

// rulesEvaluatedAt reads the artist's rules-evaluated watermark straight from the
// table. NULL (reported as not set) means the artist is still DIRTY and the next
// incremental pass will pick it up again -- the safe failure mode a failed
// persistence step is required to leave behind.
func rulesEvaluatedAt(t *testing.T, db *sql.DB, artistID string) bool {
	t.Helper()
	var at sql.NullString
	err := db.QueryRowContext(t.Context(),
		`SELECT rules_evaluated_at FROM artists WHERE id = ?`, artistID).Scan(&at)
	if err != nil {
		t.Fatalf("reading rules_evaluated_at for %s: %v", artistID, err)
	}
	return at.Valid
}

// TestRetractRuleVerdict_ReportsWhetherAnythingWasWithdrawn pins the EXISTS-probe
// contract: the returned bool says whether there was a verdict to withdraw, and it
// is the probe's own output, so it cannot be right unless the probe ran.
//
// Retraction runs on EVERY evaluation of a skipped rule, so on all but the first
// pass there is nothing left to withdraw. That steady state is the common one on a
// real library (many API-only artists, re-walked every incremental pass), which is
// why the probe comes first and the two writes are issued only when they have
// something to write -- an unconditional DELETE + UPDATE would open two write
// transactions per skipped rule per pass against SQLite's single writer, matching
// zero rows every time.
//
// The terminal operator decisions are also nothing-to-withdraw: a DISMISSED or a
// PENDING_CHOICE violation is not a verdict this code made, so it is neither
// counted by the probe nor touched by the writes, and the call reports false.
//
// Mutant this kills: dropping the EXISTS probe and returning true unconditionally,
// or widening the probe's status filter beyond `open`.
func TestRetractRuleVerdict_ReportsWhetherAnythingWasWithdrawn(t *testing.T) {
	tests := []struct {
		name string
		// seed leaves the (artist, rule) state the probe will read.
		seed func(t *testing.T, db *sql.DB, svc *Service, a *artist.Artist)
		// wantRetracted is what RetractRuleVerdict must report.
		wantRetracted bool
		// wantViolationStatus is the status the violation row must still carry
		// afterwards ("" = no violation row at all).
		wantViolationStatus string
		// wantResultRow is whether a rule_results row must still exist.
		wantResultRow bool
	}{
		{
			name:                "nothing stored at all",
			seed:                func(*testing.T, *sql.DB, *Service, *artist.Artist) {},
			wantRetracted:       false,
			wantViolationStatus: "",
			wantResultRow:       false,
		},
		{
			name: "stale pass row only",
			seed: func(t *testing.T, db *sql.DB, _ *Service, a *artist.Artist) {
				staleDuplicatePassRows(t, db, a.ID)
			},
			wantRetracted:       true,
			wantViolationStatus: "",
			wantResultRow:       false,
		},
		{
			name: "open violation only",
			seed: func(t *testing.T, _ *sql.DB, svc *Service, a *artist.Artist) {
				seedViolation(t, svc, a, RuleImageDuplicate, ViolationStatusOpen)
			},
			wantRetracted: true,
			// The open finding is withdrawn, and UpsertViolation's companion
			// rule_results fail row goes with it.
			wantViolationStatus: ViolationStatusResolved,
			wantResultRow:       false,
		},
		{
			name: "dismissed violation only",
			seed: func(t *testing.T, db *sql.DB, svc *Service, a *artist.Artist) {
				seedViolation(t, svc, a, RuleImageDuplicate, ViolationStatusDismissed)
				// Strip the companion rule_results row so the ONLY thing on the
				// table is the operator's dismissal. Otherwise the probe would
				// fire on the result row and this case would never reach the
				// short-circuit it exists to test.
				clearRuleResultRow(t, db, a.ID, RuleImageDuplicate)
			},
			// A dismissal is the operator's terminal decision (#1107), not a
			// verdict this code made. There is nothing here to withdraw.
			wantRetracted:       false,
			wantViolationStatus: ViolationStatusDismissed,
			wantResultRow:       false,
		},
		{
			name: "pending_choice violation only",
			seed: func(t *testing.T, db *sql.DB, svc *Service, a *artist.Artist) {
				seedViolation(t, svc, a, RuleImageDuplicate, ViolationStatusPendingChoice)
				clearRuleResultRow(t, db, a.ID, RuleImageDuplicate)
			},
			// A parked human decision. Same reasoning as dismissed.
			wantRetracted:       false,
			wantViolationStatus: ViolationStatusPendingChoice,
			wantResultRow:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			ctx := t.Context()
			_, svc := dupRuleEngine(t, db)
			a := apiOnlyArtist(t, db, "API Probe "+tc.name)
			tc.seed(t, db, svc, a)

			retracted, err := svc.RetractRuleVerdict(ctx, a.ID, RuleImageDuplicate)
			if err != nil {
				t.Fatalf("RetractRuleVerdict: %v", err)
			}

			if retracted != tc.wantRetracted {
				t.Errorf("RetractRuleVerdict reported retracted=%v; want %v. The bool is the "+
					"EXISTS probe's own answer: reporting a withdrawal that did not happen (or "+
					"missing one that did) means the probe is not deciding whether to write.",
					retracted, tc.wantRetracted)
			}
			if got := violationStatus(t, db, a.ID, RuleImageDuplicate); got != tc.wantViolationStatus {
				t.Errorf("violation status = %q; want %q", got, tc.wantViolationStatus)
			}
			if _, exists := ruleResultRow(t, db, a.ID, RuleImageDuplicate); exists != tc.wantResultRow {
				t.Errorf("rule_results row exists = %v; want %v", exists, tc.wantResultRow)
			}
		})
	}
}

// clearRuleResultRow removes the (artist, rule) rule_results row and asserts it is
// gone, so a test that needs "no stored result" starts from a state it has proved.
func clearRuleResultRow(t *testing.T, db *sql.DB, artistID, ruleID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(),
		`DELETE FROM rule_results WHERE artist_id = ? AND rule_id = ?`, artistID, ruleID); err != nil {
		t.Fatalf("clearing the rule_results row: %v", err)
	}
	if _, exists := ruleResultRow(t, db, artistID, ruleID); exists {
		t.Fatalf("precondition: the rule_results row for %s survived its deletion", ruleID)
	}
}

// TestRetractRuleVerdict_WriteFailureSurfacesAndDoesNotHalfRetract covers the three
// failure branches, and each asserts what the code DOES on failure, not merely that
// it failed.
//
// The shared contract: report the error, report retracted=false, and leave the
// database exactly as it was found. Retraction touches TWO tables, and a failure
// partway through must not leave the artist withdrawn in one and intact in the
// other -- the fixer folds the error into persistOK, the artist stays dirty, and
// the NEXT pass retries the whole retraction from a state it can still recognize.
// A half-retraction is unrecoverable: the second pass sees the row already gone,
// short-circuits on the probe, and the surviving open violation is stranded forever.
//
// Mutants this kills: swallowing any of the three errors (returning nil), reporting
// retracted=true alongside an error, or continuing to the violation UPDATE after
// the rule_results DELETE has failed.
func TestRetractRuleVerdict_WriteFailureSurfacesAndDoesNotHalfRetract(t *testing.T) {
	tests := []struct {
		name string
		// inject breaks the database AFTER the state is seeded.
		inject func(t *testing.T, db *sql.DB)
		// seedResultRow controls whether a rule_results row survives into the call.
		seedResultRow bool
		// wantErrContains identifies WHICH step failed, so an operator reading the
		// log can tell a probe failure from a write failure.
		wantErrContains string
	}{
		{
			name: "the EXISTS probe itself fails",
			// No table, no probe. The engine cannot tell whether there is a
			// verdict to withdraw, so it must not guess.
			inject:          func(t *testing.T, db *sql.DB) { dropTable(t, db, "rule_results") },
			seedResultRow:   true,
			wantErrContains: "checking stored verdict for skipped rule",
		},
		{
			name:            "the rule_results DELETE fails",
			inject:          func(t *testing.T, db *sql.DB) { armRuleResultsDeleteTrap(t, db, RuleImageDuplicate) },
			seedResultRow:   true,
			wantErrContains: "deleting rule_result for skipped rule",
		},
		{
			name: "the open-violation UPDATE fails",
			// No result row, so the DELETE is skipped by the probe and the UPDATE
			// is the FIRST write the call makes: this isolates the third branch.
			inject:          func(t *testing.T, db *sql.DB) { armViolationUpdateTrap(t, db, RuleImageDuplicate) },
			seedResultRow:   false,
			wantErrContains: "resolving open violation for skipped rule",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			ctx := t.Context()
			_, svc := dupRuleEngine(t, db)
			a := apiOnlyArtist(t, db, "API Retract Failure")

			// Seed the stale verdict: an OPEN violation (which UpsertViolation
			// records with its companion rule_results fail row), optionally
			// stripped back down to violation-only.
			seedViolation(t, svc, a, RuleImageDuplicate, ViolationStatusOpen)
			if !tc.seedResultRow {
				clearRuleResultRow(t, db, a.ID, RuleImageDuplicate)
			}

			// PRECONDITIONS. Both halves of the state this call must NOT damage
			// are asserted present before the injection, so every "it is still
			// there" assertion below is a real one.
			if _, exists := ruleResultRow(t, db, a.ID, RuleImageDuplicate); exists != tc.seedResultRow {
				t.Fatalf("precondition: rule_results row exists = %v; want %v", exists, tc.seedResultRow)
			}
			if got := violationStatus(t, db, a.ID, RuleImageDuplicate); got != ViolationStatusOpen {
				t.Fatalf("precondition: violation status = %q; want %q (there must be an OPEN "+
					"finding for the retraction to have work to do)", got, ViolationStatusOpen)
			}

			tc.inject(t, db)

			retracted, err := svc.RetractRuleVerdict(ctx, a.ID, RuleImageDuplicate)
			if err == nil {
				t.Fatalf("RetractRuleVerdict SWALLOWED a write failure (retracted=%v). The fixer "+
					"folds this error into persistOK to keep the artist dirty; swallowing it "+
					"marks the artist clean with the stale verdict still on the table.", retracted)
			}
			if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error = %v; want it to contain %q so the failing step is identifiable "+
					"in the log", err, tc.wantErrContains)
			}
			if retracted {
				t.Errorf("RetractRuleVerdict reported retracted=true alongside an error; nothing " +
					"was withdrawn")
			}

			// The database is as it was found. The violation in particular is
			// still OPEN: on the DELETE-failure case that proves the call did NOT
			// go on to resolve it, which would be exactly the half-retraction
			// that strands a finding for good.
			if got := violationStatus(t, db, a.ID, RuleImageDuplicate); got != ViolationStatusOpen {
				t.Errorf("violation status = %q after a FAILED retraction; want it left %q. A "+
					"retraction that resolves the violation but does not delete the result row "+
					"(or vice versa) cannot be repaired by a retry: the next pass sees a "+
					"half-withdrawn verdict and short-circuits on the probe.",
					got, ViolationStatusOpen)
			}
		})
	}
}

// TestRetractSkippedResults_FilterConfinesRetractionToItsScope is the scope guard on
// the pipeline's retraction loop.
//
// filter mirrors the consideredFilter that persistPassResults gets, so a single-rule
// run withdraws only within its own scope. Both duplicate rules are skipped for this
// artist, but a run that was asked about ONE of them has no mandate over the other's
// verdict: it never evaluated that rule and cannot say whether the artist still
// passes it.
//
// Mutant this kills: dropping the `if filter != nil && !filter(s.RuleID) { continue }`
// guard, which makes every scoped run silently withdraw every skipped rule's verdict
// library-wide.
func TestRetractSkippedResults_FilterConfinesRetractionToItsScope(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API Filter Scope")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID) // asserts both rows are really there

	// PRECONDITION: both rules really are skipped, so both are candidates for
	// retraction and the filter is the only thing that can spare one of them.
	res, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	skipped := res.RulesSkipped
	if len(skipped) < 2 {
		t.Fatalf("precondition: want both duplicate rules skipped, got %d skipped: %+v",
			len(skipped), skipped)
	}

	inScope := func(ruleID string) bool { return ruleID == RuleImageDuplicate }
	if ok := p.retractSkippedResults(ctx, a, skipped, inScope); !ok {
		t.Fatalf("retractSkippedResults reported failure on a healthy database")
	}

	if _, exists := ruleResultRow(t, db, a.ID, RuleImageDuplicate); exists {
		t.Errorf("the IN-SCOPE rule %s kept its stale row; the filter is rejecting the very "+
			"rule the run was asked about", RuleImageDuplicate)
	}
	passed, exists := ruleResultRow(t, db, a.ID, RuleImageDuplicateExact)
	if !exists {
		t.Errorf("the OUT-OF-SCOPE rule %s had its verdict withdrawn by a run scoped to %s. A "+
			"scoped run never evaluated that rule and has no standing to retract it.",
			RuleImageDuplicateExact, RuleImageDuplicate)
	} else if !passed {
		t.Errorf("the OUT-OF-SCOPE rule %s had its stored row rewritten (passed=false)",
			RuleImageDuplicateExact)
	}
}

// TestRetractSkippedResults_FailureFoldsIntoPersistOKAndLoopContinues covers the
// error branch of the pipeline's retraction loop, and asserts BOTH halves of what
// it is supposed to do on failure.
//
//  1. Report false. The caller folds that into persistOK, the artist is not marked
//     rules-evaluated, and the next pass retries -- exactly like a failed pass write.
//     Swallowing it marks the artist clean with the stale verdict still on the table,
//     and nothing ever revisits it.
//  2. Keep going. A failure retracting rule A must not abandon rule B: the loop
//     warns and continues. Only ONE rule's DELETE is trapped here, so an
//     abort-on-first-error implementation leaves the untrapped rule's stale row
//     behind and this test catches it.
//
// Mutants this kills: `return false` in place of `ok = false; continue`; and
// discarding the error (`_, _ = p.ruleService.RetractRuleVerdict(...)`).
func TestRetractSkippedResults_FailureFoldsIntoPersistOKAndLoopContinues(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, ruleSvc := dupRuleEngine(t, db)
	p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

	a := apiOnlyArtist(t, db, "API Retract Loop Failure")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID)

	res, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// The trap goes on the rule the loop reaches FIRST, so that everything after it
	// is at risk. See skippedDuplicateRulesInOrder.
	trapped, later := skippedDuplicateRulesInOrder(t, res)

	armRuleResultsDeleteTrap(t, db, trapped)

	if ok := p.retractSkippedResults(ctx, a, res.RulesSkipped, nil); ok {
		t.Errorf("retractSkippedResults reported SUCCESS after a retraction failed. The caller " +
			"folds this into persistOK; reporting success stamps rules_evaluated_at and the " +
			"artist falls out of the dirty set with its stale verdict intact.")
	}

	if _, exists := ruleResultRow(t, db, a.ID, trapped); !exists {
		t.Errorf("the trapped rule's row is gone; the injected failure did not actually stop " +
			"the DELETE and this test proves nothing")
	}
	if _, exists := ruleResultRow(t, db, a.ID, later); exists {
		t.Errorf("the loop ABANDONED %s after %s (which it reaches FIRST) failed to retract. "+
			"One rule's database error must not strand every later rule's stale verdict; the "+
			"loop warns and continues.", later, trapped)
	}
}

// TestPipeline_RetractionFailureLeavesArtistDirty carries the persistOK contract up
// to the two pipeline entry points that call retractSkippedResults, and asserts the
// OBSERVABLE consequence rather than an internal flag.
//
// persistOK=false means the artist is not counted as processed and (on the RunAll
// path, which owns the watermark) rules_evaluated_at is NOT stamped -- so the artist
// stays in the dirty set and the next incremental pass retries the retraction. That
// retry is the entire reason the failure is propagated at all.
//
// Each entry point is run twice, trap armed and trap disarmed. The disarmed run is
// the positive control: it proves the artist IS processed and IS marked when nothing
// fails, so "processed=0" in the armed run is caused by the injected failure and not
// by the artist being unreachable to the run in the first place.
//
// Mutant this kills: dropping `acc.persistOK = false` at either call site, i.e.
// evaluating retractSkippedResults for its side effect and ignoring its answer.
func TestPipeline_RetractionFailureLeavesArtistDirty(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, p *Pipeline) *RunResult
		// marksWatermark: only the all-rules pass owns rules_evaluated_at. A
		// single-rule run deliberately leaves it alone (running rule A must not
		// mark the artist clean for rule B), so the watermark says nothing there.
		marksWatermark bool
	}{
		{
			name: "RunAllScoped",
			run: func(t *testing.T, p *Pipeline) *RunResult {
				res, err := p.RunAllScoped(t.Context(), RunScopeAll)
				if err != nil {
					t.Fatalf("RunAllScoped: %v", err)
				}
				return res
			},
			marksWatermark: true,
		},
		{
			name: "RunRuleScoped",
			run: func(t *testing.T, p *Pipeline) *RunResult {
				res, err := p.RunRuleScoped(t.Context(), RuleImageDuplicate, RunScopeAll)
				if err != nil {
					t.Fatalf("RunRuleScoped: %v", err)
				}
				return res
			},
			marksWatermark: false,
		},
	}

	for _, tc := range tests {
		for _, armed := range []bool{false, true} {
			label := "retraction succeeds"
			if armed {
				label = "retraction fails"
			}
			t.Run(tc.name+"/"+label, func(t *testing.T) {
				db := setupTestDB(t)
				engine, ruleSvc := dupRuleEngine(t, db)
				p := NewPipeline(engine, artist.NewService(db), ruleSvc, nil, nil, testLogger())

				a := apiOnlyArtist(t, db, "API Pipeline Retract")
				insertBlindFanartPair(t, db, a.ID)
				staleDuplicatePassRows(t, db, a.ID)

				// PRECONDITION: the artist starts DIRTY. Otherwise "the watermark
				// is still unset" would hold for a run that never touched it.
				if set := rulesEvaluatedAt(t, db, a.ID); set {
					t.Fatalf("precondition: rules_evaluated_at is already set")
				}
				// PRECONDITION: the rule this run retracts really is skipped.
				pre, err := engine.Evaluate(t.Context(), a)
				if err != nil {
					t.Fatalf("Evaluate: %v", err)
				}
				if _, skipped := skipReasonFor(pre, RuleImageDuplicate); !skipped {
					t.Fatalf("precondition: %s must be skipped for this artist", RuleImageDuplicate)
				}

				if armed {
					armRuleResultsDeleteTrap(t, db, RuleImageDuplicate)
				}

				res := tc.run(t, p)

				if armed {
					if res.ArtistsProcessed != 0 {
						t.Errorf("ArtistsProcessed = %d after the artist's retraction FAILED; "+
							"want 0. A failed persistence step must not count as a completed "+
							"evaluation.", res.ArtistsProcessed)
					}
					if set := rulesEvaluatedAt(t, db, a.ID); set {
						t.Errorf("rules_evaluated_at was STAMPED after the artist's retraction " +
							"failed. The artist has fallen out of the dirty set carrying a " +
							"stale pass row for a rule that never examined it, and no later " +
							"pass will ever revisit it.")
					}
					if _, exists := ruleResultRow(t, db, a.ID, RuleImageDuplicate); !exists {
						t.Errorf("the trapped DELETE went through; the injected failure did not " +
							"fire and this case proves nothing")
					}
					return
				}

				// Positive control.
				if res.ArtistsProcessed != 1 {
					t.Fatalf("positive control FAILED: ArtistsProcessed = %d on a healthy "+
						"database; want 1. The armed case's assertion that it drops to 0 is "+
						"only meaningful if the run reaches this artist at all.",
						res.ArtistsProcessed)
				}
				if _, exists := ruleResultRow(t, db, a.ID, RuleImageDuplicate); exists {
					t.Fatalf("positive control FAILED: the stale row for %s survived a healthy "+
						"run, so this run does not retract and the armed case is not testing "+
						"a retraction failure", RuleImageDuplicate)
				}
				if set := rulesEvaluatedAt(t, db, a.ID); set != tc.marksWatermark {
					t.Errorf("rules_evaluated_at set = %v after a healthy %s; want %v",
						set, tc.name, tc.marksWatermark)
				}
			})
		}
	}
}

// TestHealthSubscriber_RetractionFailureIsLoggedAndDoesNotStrandLaterRules covers the
// error branch of the subscriber's retraction loop.
//
// The subscriber runs outside the pipeline and has no persistOK to fold a failure
// into: an artist-updated event is fire-and-forget, and there is no retry hook to
// keep the artist dirty for. So the contract here is narrower and it is deliberate:
// WARN and CONTINUE. The failure is visible in the log, the remaining skipped rules
// are still retracted, and the next event (or the next pipeline pass, which DOES
// keep the artist dirty) retries the one that failed.
//
// Only ONE of the two skipped rules is trapped, so an implementation that aborts the
// loop on the first error is distinguishable from one that continues.
//
// Mutant this kills: `return` (or a bare propagate) in place of the warn-and-continue
// in the RulesSkipped loop, which would strand every skipped rule after the first
// failure.
func TestHealthSubscriber_RetractionFailureIsLoggedAndDoesNotStrandLaterRules(t *testing.T) {
	db := setupTestDB(t)
	ctx := t.Context()
	engine, _ := dupRuleEngine(t, db)
	artistSvc := artist.NewService(db)

	a := apiOnlyArtist(t, db, "API Sub Retract Failure")
	insertBlindFanartPair(t, db, a.ID)
	staleDuplicatePassRows(t, db, a.ID)

	// PRECONDITION: both duplicate rules are skipped for this artist, so the
	// subscriber's retraction loop has two iterations and "it continued past the
	// failure" is an observable claim. The trap goes on whichever the loop reaches
	// FIRST -- see skippedDuplicateRulesInOrder.
	pre, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	trapped, later := skippedDuplicateRulesInOrder(t, pre)

	armRuleResultsDeleteTrap(t, db, trapped)

	NewHealthSubscriber(engine, artistSvc, testLogger()).evaluateArtist(ctx, a.ID)

	if _, exists := ruleResultRow(t, db, a.ID, trapped); !exists {
		t.Fatalf("the trapped DELETE went through; the injected failure did not fire and this " +
			"test proves nothing")
	}
	if _, exists := ruleResultRow(t, db, a.ID, later); exists {
		t.Errorf("the subscriber ABANDONED %s after %s (which it reaches FIRST) failed to "+
			"retract, leaving a rule that never examined this artist recorded as a PASS. One "+
			"rule's database error must not strand every later rule's stale verdict: the loop "+
			"warns and continues.", later, trapped)
	}
}
