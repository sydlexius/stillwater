package rule

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// The cross_artist_backdrop_collision rule is seeded DISABLED (it has no engine
// checker; its violations are raised event-driven at the write/push
// chokepoints). "Disabled" is an engine-evaluation property, but nothing about
// the durable half of the feature may depend on it -- a disabled rule whose
// violations were invisible or unfixable would mean the Action Queue entry is
// silently dead, which is exactly the report-success-while-doing-nothing
// failure this feature must not have.
//
// These tests pin all three properties against the REAL query/fix paths on a
// real SQLite DB, so a future change that adds an `AND rules.enabled = 1` to the
// violation queries, an enabled-check to the fix path, or that makes the engine
// consider disabled rules, fails here rather than in production:
//
//  1. a raised collision violation is returned by the Action Queue query
//     (including the code path that INNER JOINs the rules table),
//  2. it survives FixViolation all the way into the fixer, and
//  3. a full engine run does NOT auto-resolve it.

// seedCollisionArtist creates an artist and raises one collision violation for
// it, returning the artist and the persisted violation id.
func seedCollisionArtist(t *testing.T, db *sql.DB) (*artist.Artist, *Service, *artist.Service, string) {
	t.Helper()
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Precondition: the rule really is seeded disabled. Without this the tests
	// below would pass vacuously against an enabled rule and prove nothing about
	// the disabled case they exist to cover.
	r, err := ruleSvc.GetByID(ctx, RuleCrossArtistBackdropCollision)
	if err != nil {
		t.Fatalf("collision rule not seeded (it is the FK target for its violations): %v", err)
	}
	if r.Enabled {
		t.Fatalf("precondition failed: rule %q is seeded ENABLED; these tests cover the disabled-seed case",
			RuleCrossArtistBackdropCollision)
	}

	a := &artist.Artist{Name: "Collision Dest", SortName: "Collision Dest", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	const msg = "Backdrop matches Other Artist (94% similar, 1 artists) - possible cross-artist pollution"
	if err := ruleSvc.RaiseBackdropCollision(ctx, a.ID, a.Name, msg, "colliding-artist-id"); err != nil {
		t.Fatalf("RaiseBackdropCollision: %v", err)
	}

	// Recover the persisted id through the same query the dashboard uses.
	got, _, err := ruleSvc.ListViolationsFilteredPaged(ctx, ViolationListParams{Status: "active"})
	if err != nil {
		t.Fatalf("listing violations: %v", err)
	}
	for _, v := range got {
		if v.RuleID == RuleCrossArtistBackdropCollision {
			return a, ruleSvc, artistSvc, v.ID
		}
	}
	t.Fatal("raised collision violation not found via the active-violations query")
	return nil, nil, nil, ""
}

// TestCollisionViolation_RendersOnActionQueue proves property (1): the Action
// Queue query returns a violation whose rule is disabled -- including via the
// category-filtered path, which is the only one that INNER JOINs rules and so
// the only one that could drop the row.
func TestCollisionViolation_RendersOnActionQueue(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	a, ruleSvc, _, vid := seedCollisionArtist(t, db)

	// (a) The unfiltered Action Queue query (Status:"active", exactly what
	// handleDashboardActionQueue issues).
	violations, total, err := ruleSvc.ListViolationsFilteredPaged(ctx, ViolationListParams{
		Status: "active", Sort: "severity", Order: "desc",
	})
	if err != nil {
		t.Fatalf("action-queue query: %v", err)
	}
	if total < 1 {
		t.Fatalf("action-queue total = %d, want >= 1", total)
	}
	var found *RuleViolation
	for i := range violations {
		if violations[i].ID == vid {
			found = &violations[i]
		}
	}
	if found == nil {
		t.Fatal("collision violation NOT returned by the action-queue query: the durable half is invisible")
	}
	if !found.Fixable {
		t.Error("collision violation is not Fixable; the Action Queue would render no Fix action")
	}
	if found.Severity != "warning" {
		t.Errorf("severity = %q, want warning", found.Severity)
	}
	if found.ArtistID != a.ID {
		t.Errorf("artist_id = %q, want %q", found.ArtistID, a.ID)
	}

	// (b) The category-filtered path sets needJoin and emits
	// `JOIN rules r ON r.id = rv.rule_id`. A disabled rule still has a rules row,
	// so the INNER JOIN must be satisfied and the row must survive.
	byCategory, _, err := ruleSvc.ListViolationsFilteredPaged(ctx, ViolationListParams{
		Status:   "active",
		Category: TriFilter{Include: []string{string(RuleCategoryImage)}},
	})
	if err != nil {
		t.Fatalf("category-filtered query: %v", err)
	}
	inCategory := false
	for _, v := range byCategory {
		if v.ID == vid {
			inCategory = true
		}
	}
	if !inCategory {
		t.Error("collision violation dropped by the category-filtered query (the rules INNER JOIN path)")
	}

	// (c) The Fixable facet must also surface it, since that is how an operator
	// filters the queue down to actionable entries.
	byFixable, _, err := ruleSvc.ListViolationsFilteredPaged(ctx, ViolationListParams{
		Status:  "active",
		Fixable: TriFilter{Include: []string{"yes"}},
	})
	if err != nil {
		t.Fatalf("fixable-filtered query: %v", err)
	}
	inFixable := false
	for _, v := range byFixable {
		if v.ID == vid {
			inFixable = true
		}
	}
	if !inFixable {
		t.Error("collision violation missing from the Fixable=yes facet")
	}
}

// TestCollisionViolation_SurvivesFixViolationIntoFixer proves property (2): the
// whole {id}/fix path -- rule lookup from the cache, the attemptFix gates, fixer
// dispatch -- works for a violation whose rule is disabled, and that the
// DESTRUCTIVE back-out actually ran for the right artist.
func TestCollisionViolation_SurvivesFixViolationIntoFixer(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	a, ruleSvc, artistSvc, vid := seedCollisionArtist(t, db)

	rem := &fakeCollisionRemediator{result: PHashRemediateResult{OpID: "op-e2e", SlotsRemoved: 3}}
	fixer := NewCrossArtistBackdropCollisionFixer(testLogger())
	fixer.SetRemediator(rem)

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	fr, err := pipeline.FixViolation(ctx, vid)
	if err != nil {
		t.Fatalf("FixViolation: %v", err)
	}

	// The fix must have REACHED the fixer and run the real back-out for the
	// right artist -- not merely returned a nil error. A disabled-rule
	// early-return would leave calls == 0 while still returning cleanly.
	if rem.calls != 1 {
		t.Fatalf("remediator invoked %d times, want 1: the fix never reached the fixer", rem.calls)
	}
	if rem.gotID != a.ID {
		t.Errorf("remediated artist %q, want %q", rem.gotID, a.ID)
	}
	if rem.gotDry {
		t.Error("remediation ran dry-run; the operator's Fix must commit")
	}
	if !fr.Fixed || fr.SlotsRemoved != 3 {
		t.Errorf("FixResult = {Fixed:%v SlotsRemoved:%d}, want {true 3}; message: %s",
			fr.Fixed, fr.SlotsRemoved, fr.Message)
	}

	// And the row must actually transition, so the entry leaves the queue.
	got, err := ruleSvc.GetViolationByID(ctx, vid)
	if err != nil {
		t.Fatalf("GetViolationByID: %v", err)
	}
	if got.Status != ViolationStatusResolved {
		t.Errorf("violation status = %q, want %q", got.Status, ViolationStatusResolved)
	}
}

// TestCollisionViolation_SurvivesFullEngineRun proves property (3): a forced
// full evaluation of every artist does NOT auto-resolve the event-raised
// violation. This is the reason the rule is seeded disabled -- eligibleRules
// skips disabled rules, so the rule is never "considered", so persistPassResults
// never calls ResolveViolationIfActive for it. If someone enables the rule while
// its checker still returns nil for every artist, the rule would be considered,
// counted as a pass, and this violation would be swept away silently.
func TestCollisionViolation_SurvivesFullEngineRun(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	a, ruleSvc, artistSvc, vid := seedCollisionArtist(t, db)

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	res, err := pipeline.RunAllScoped(ctx, RunScopeAll)
	if err != nil {
		t.Fatalf("RunAllScoped: %v", err)
	}
	// Guard against a vacuous pass: if the run evaluated no artists (or
	// considered no rules) it never had the chance to resolve anything, and the
	// assertion below would hold for the wrong reason.
	if res.ArtistsProcessed < 1 {
		t.Fatalf("engine processed %d artists; the sweep never ran, so this proves nothing",
			res.ArtistsProcessed)
	}
	// Pin the MECHANISM, not just the outcome.
	//
	// persistPassResults writes a PASS row (passed=1) for every rule the engine
	// CONSIDERED that produced no violation, and calls ResolveViolationIfActive
	// for each. The collision rule already has a rule_results row -- but a FAIL
	// row (passed=0), written by UpsertViolation itself when RaiseBackdropCollision
	// persisted the violation (service.go), NOT by the engine.
	//
	// So the exact fingerprint of "the engine never considered this rule" is that
	// the row is still passed=0. If a future change enables the rule while its
	// checker still returns nil for every artist, the engine would consider it,
	// flip this to passed=1, and resolve the violation out from under the
	// operator -- and that flip is what this assertion catches.
	var otherPassRows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM rule_results WHERE artist_id = ? AND rule_id != ? AND passed = 1`,
		a.ID, RuleCrossArtistBackdropCollision).Scan(&otherPassRows); err != nil {
		t.Fatalf("counting engine pass rows for other rules: %v", err)
	}
	if otherPassRows == 0 {
		t.Fatal("engine wrote no pass rows for this artist: the sweep considered nothing, so this proves nothing")
	}
	var collisionPassed int
	if err := db.QueryRow(
		`SELECT passed FROM rule_results WHERE artist_id = ? AND rule_id = ?`,
		a.ID, RuleCrossArtistBackdropCollision).Scan(&collisionPassed); err != nil {
		t.Fatalf("reading the collision rule's rule_results row: %v", err)
	}
	if collisionPassed != 0 {
		t.Errorf("collision rule recorded passed=%d: the engine CONSIDERED a rule it must skip, "+
			"so a passing evaluation can resolve its event-raised violations", collisionPassed)
	}

	got, err := ruleSvc.GetViolationByID(ctx, vid)
	if err != nil {
		t.Fatalf("GetViolationByID after engine run: %v", err)
	}
	if got.Status != ViolationStatusOpen {
		t.Errorf("violation status = %q after a full engine run, want %q: the engine swept away an "+
			"event-raised violation it should never have considered", got.Status, ViolationStatusOpen)
	}
}
