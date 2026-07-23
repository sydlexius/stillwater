package rule

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Issue #2738: Engine.EvaluateScoped computes every violation up front, so
// checker order was never the problem. The bug lives in the FIX DISPATCH
// loop (processArtistForRunAll / dispatchViolations), which iterated
// eval.Violations in the inherited "category, name" order and invoked each
// violation's Fix() in that order. A producer's Fix mutates the shared
// *artist.Artist in place (nfo_has_mbid sets MusicBrainzID;
// provider_id_missing's backfill sets DiscogsID/DeezerID/SpotifyID); a
// consumer dispatched earlier in the same loop reads the stale,
// pre-mutation value. This file tests the StateProducer-tier reorder that
// fixes it.

// stubSearchOrchestrator is a test-only metadataOrchestrator whose Search
// always returns one fixed result carrying mbid. Only Search is exercised by
// MetadataFixer.fixMBID; the other two methods are unused in these tests and
// return zero values.
type stubSearchOrchestrator struct {
	mbid string
}

func (s *stubSearchOrchestrator) Search(_ context.Context, name string) ([]provider.ArtistSearchResult, error) {
	return []provider.ArtistSearchResult{{Name: name, MusicBrainzID: s.mbid, Score: 100}}, nil
}

func (s *stubSearchOrchestrator) FetchMetadata(_ context.Context, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	return nil, nil
}

func (s *stubSearchOrchestrator) FetchFieldFromProviders(_ context.Context, _, _, _ string, _ map[provider.ProviderName]string) ([]provider.FieldProviderResult, error) {
	return nil, nil
}

// consumerFixer is a test-only Fixer standing in for a rule whose Fix depends
// on state a producer fixer writes in the same pass (DiscogsID, written by
// ProviderIDBackfillFixer.Fix via setProviderIDForName). It is registered
// under ruleID -- a real, seeded rule row is required so the row survives
// the pipeline's getCachedRule/GetByID lookup and the rule_violations /
// rule_results machinery, but the fixer's behavior has nothing to do with
// whatever fixer normally handles that rule ID (none of the real fixers for
// that rule are registered in these tests, so there is no collision).
//
// fixCalls and sawEmptyAtFixTime record what Fix observed so the test can
// prove dispatch order actually reached this fixer (guards against a vacuous
// pass) and what it saw at the moment it ran.
type consumerFixer struct {
	ruleID            string
	fixCalls          int
	sawEmptyAtFixTime bool
}

func (f *consumerFixer) CanFix(v *Violation) bool { return v.RuleID == f.ruleID }

func (f *consumerFixer) Fix(_ context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	f.fixCalls++
	if a.DiscogsID == "" {
		f.sawEmptyAtFixTime = true
		return &FixResult{
			RuleID:  v.RuleID,
			Fixed:   false,
			Message: "consumer ran before producer: discogs id not yet set",
		}, nil
	}
	return &FixResult{
		RuleID:  v.RuleID,
		Fixed:   true,
		Message: "consumer observed producer output: " + a.DiscogsID,
	}, nil
}

// consumerChecker returns a Checker that flags the artist whenever DiscogsID
// is empty, mirroring consumerFixer's own Fix condition so the pre-fix
// Evaluate raises exactly the violation the fixer is built to resolve.
func consumerChecker(ruleID string) Checker {
	return func(_ context.Context, a *artist.Artist, cfg RuleConfig) *Violation {
		if a.DiscogsID != "" {
			return nil
		}
		return &Violation{
			RuleID:   ruleID,
			RuleName: "test consumer (requires discogs id)",
			Category: string(RuleCategoryMetadata),
			Severity: effectiveSeverity(cfg),
			Message:  "discogs id required",
			Fixable:  true,
		}
	}
}

// enableRuleAuto enables ruleID and forces its automation_mode to auto,
// mirroring the setup fixer_runall_test.go uses for provider_id_missing --
// several rules relevant to this file (provider_id_missing,
// discography_populated) seed disabled/manual by default.
func enableRuleAuto(t *testing.T, ctx context.Context, ruleSvc *Service, ruleID string) {
	t.Helper()
	r, err := ruleSvc.GetByID(ctx, ruleID)
	if err != nil {
		t.Fatalf("loading rule %s: %v", ruleID, err)
	}
	r.Enabled = true
	r.AutomationMode = AutomationModeAuto
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatalf("enabling rule %s: %v", ruleID, err)
	}
}

// TestOrderForDispatch_ProviderIDBeforeConsumer is the primary #2738
// regression. The artist already has an MBID; provider_id_missing (the
// producer, tier -1) and a consumer rule that requires DiscogsID (tier 0,
// the implicit default) are both enabled in auto mode.
//
// Both rules are category "metadata", so under the OLD "category, name"
// dispatch order the consumer rule ID (registered here under
// RuleDiscographyPopulated, "discography_populated") sorts alphabetically
// BEFORE "provider_id_missing" and is dispatched first -- exactly the
// reported bug. This test MUST fail without the reorder: without it,
// consumerFixer.Fix runs while a.DiscogsID is still empty, records
// sawEmptyAtFixTime, and persists an Open rule_violations row with the
// "ran before producer" message. What would still pass if the fix were
// wrong: rule_results.Passed, because writeFilteredPassResults derives it
// from a POST-fix re-Evaluate that runs after every fixer in the loop has
// already mutated the artist, so it comes out true regardless of dispatch
// order -- this is why the test asserts against the rule_violations table
// (populated by the dispatch loop itself, mid-pass) and not rule_results.
func TestOrderForDispatch_ProviderIDBeforeConsumer(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding default rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleProviderIDMissing, RuleDiscographyPopulated)
	enableRuleAuto(t, ctx, ruleSvc, RuleProviderIDMissing)
	enableRuleAuto(t, ctx, ruleSvc, RuleDiscographyPopulated)

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	engine.SetProviderAvailability(&stubProviderAvailability{available: allThreeAvailable()})
	// Override the real discography checker with the test consumer: the real
	// one has nothing to do with provider IDs (it reads NFO album counts), so
	// it cannot exercise the producer/consumer chain this test targets.
	engine.checkers[RuleDiscographyPopulated] = consumerChecker(RuleDiscographyPopulated)

	fetcher := &stubMetadataProvider{metadata: mbURLMetadata()}
	providerFixer := NewProviderIDBackfillFixer(fetcher, artistSvc, testLogger())
	consumer := &consumerFixer{ruleID: RuleDiscographyPopulated}
	p := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{providerFixer, consumer}, nil, testLogger())

	a := &artist.Artist{
		Name:          "Dispatch Order Artist",
		SortName:      "Dispatch Order Artist",
		MusicBrainzID: "mbid-abc",
		Path:          t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	if _, err := p.RunAllScoped(ctx, RunScopeAll); err != nil {
		t.Fatalf("RunAllScoped: %v", err)
	}

	if consumer.fixCalls == 0 {
		t.Fatal("consumerFixer.Fix was never invoked; the test did not exercise the dispatch-order invariant")
	}
	if consumer.sawEmptyAtFixTime {
		t.Error("consumer Fix ran with an empty DiscogsID: provider_id_missing (tier -1) must dispatch before the tier-0 consumer")
	}

	violations, err := ruleSvc.ListViolationsFiltered(ctx, ViolationListParams{ArtistID: a.ID})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	for _, v := range violations {
		if v.RuleID != RuleDiscographyPopulated {
			continue
		}
		if v.Status == ViolationStatusOpen {
			t.Errorf("consumer rule violation left Status=Open (message %q): the fix should have succeeded in this pass", v.Message)
		}
	}
}

// TestOrderForDispatch_ChainNFOThenProviderIDThenConsumer is the two-level
// chain case: an artist starts with NO MusicBrainz ID at all, so nfo_has_mbid
// (tier -2), provider_id_missing (tier -1), and a DiscogsID consumer (tier 0)
// all fire in the SAME pass. This guards against an implementation that only
// reorders provider_id_missing ahead of its consumer (2-tier fix) while
// leaving nfo_has_mbid at the default tier 0 -- under "category, name"
// ordering nfo_has_mbid (category "nfo") already sorts before the
// metadata-category rules, so a 2-tier-only fix would pass
// TestOrderForDispatch_ProviderIDBeforeConsumer while still failing here:
// provider_id_missing's own Fix requires a.MusicBrainzID != "", so without
// nfo_has_mbid at tier -2 the backfill fixer would no-op on this artist.
func TestOrderForDispatch_ChainNFOThenProviderIDThenConsumer(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding default rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleNFOHasMBID, RuleProviderIDMissing, RuleDiscographyPopulated)
	// RuleNFOHasMBID already seeds enabled+auto (service.go); only the other
	// two need the explicit enable.
	enableRuleAuto(t, ctx, ruleSvc, RuleProviderIDMissing)
	enableRuleAuto(t, ctx, ruleSvc, RuleDiscographyPopulated)

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	engine.SetProviderAvailability(&stubProviderAvailability{available: allThreeAvailable()})
	engine.checkers[RuleDiscographyPopulated] = consumerChecker(RuleDiscographyPopulated)

	searchOrch := &stubSearchOrchestrator{mbid: "mbid-fresh"}
	metadataFixer := NewMetadataFixer(nil, testLogger())
	metadataFixer.orchestrator = searchOrch
	fetcher := &stubMetadataProvider{metadata: mbURLMetadata()}
	providerFixer := NewProviderIDBackfillFixer(fetcher, artistSvc, testLogger())
	consumer := &consumerFixer{ruleID: RuleDiscographyPopulated}
	p := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{metadataFixer, providerFixer, consumer}, nil, testLogger())

	a := &artist.Artist{
		Name:     "Chain Artist",
		SortName: "Chain Artist",
		Path:     t.TempDir(),
		// MusicBrainzID intentionally empty: nfo_has_mbid must fire and be
		// fixed in this same pass before provider_id_missing can do anything.
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	if _, err := p.RunAllScoped(ctx, RunScopeAll); err != nil {
		t.Fatalf("RunAllScoped: %v", err)
	}

	if consumer.fixCalls == 0 {
		t.Fatal("consumerFixer.Fix was never invoked; the test did not exercise the chain")
	}
	if consumer.sawEmptyAtFixTime {
		t.Error("consumer Fix ran with an empty DiscogsID: the nfo_has_mbid -> provider_id_missing -> consumer chain did not complete in dispatch order")
	}

	reloaded, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.MusicBrainzID != "mbid-fresh" {
		t.Errorf("artist.MusicBrainzID = %q, want %q (nfo_has_mbid fix must persist)", reloaded.MusicBrainzID, "mbid-fresh")
	}
	if reloaded.DiscogsID == "" {
		t.Error("artist.DiscogsID is empty; provider_id_missing must have backfilled it in the same pass")
	}

	violations, err := ruleSvc.ListViolationsFiltered(ctx, ViolationListParams{ArtistID: a.ID})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	for _, v := range violations {
		if v.RuleID != RuleDiscographyPopulated {
			continue
		}
		if v.Status == ViolationStatusOpen {
			t.Errorf("consumer rule violation left Status=Open (message %q): the whole chain should resolve in this one pass", v.Message)
		}
	}
}

// TestOrderForDispatch_CategoryScopedRunAlsoReorders proves dispatchViolations
// (the category-scoped dispatch path a category-filtered run like
// RunImageRulesForArtist uses) shares orderForDispatch, not just
// processArtistForRunAll's run-all path. Without wiring the shared helper
// into dispatchViolations too, a category-scoped run would keep the old
// "category, name" order even after processArtistForRunAll was fixed.
func TestOrderForDispatch_CategoryScopedRunAlsoReorders(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding default rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleProviderIDMissing, RuleDiscographyPopulated)
	enableRuleAuto(t, ctx, ruleSvc, RuleProviderIDMissing)
	enableRuleAuto(t, ctx, ruleSvc, RuleDiscographyPopulated)

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	engine.SetProviderAvailability(&stubProviderAvailability{available: allThreeAvailable()})
	engine.checkers[RuleDiscographyPopulated] = consumerChecker(RuleDiscographyPopulated)

	fetcher := &stubMetadataProvider{metadata: mbURLMetadata()}
	providerFixer := NewProviderIDBackfillFixer(fetcher, artistSvc, testLogger())
	consumer := &consumerFixer{ruleID: RuleDiscographyPopulated}
	p := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{providerFixer, consumer}, nil, testLogger())

	a := &artist.Artist{
		Name:          "Category Scoped Artist",
		SortName:      "Category Scoped Artist",
		MusicBrainzID: "mbid-abc",
		Path:          t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// runForArtistFiltered is unexported but this test lives in package rule;
	// "metadata" exercises dispatchViolations with a non-empty categoryFilter,
	// which both provider_id_missing and the consumer belong to.
	if _, err := p.runForArtistFiltered(ctx, a, string(RuleCategoryMetadata)); err != nil {
		t.Fatalf("runForArtistFiltered: %v", err)
	}

	if consumer.fixCalls == 0 {
		t.Fatal("consumerFixer.Fix was never invoked; the test did not exercise dispatchViolations")
	}
	if consumer.sawEmptyAtFixTime {
		t.Error("consumer Fix ran with an empty DiscogsID via the category-scoped dispatchViolations path")
	}
}

// TestOrderForDispatch_PreservesTierZeroRelativeOrder guards against an
// implementation that reorders every rule (e.g. sorting by RuleID) instead of
// leaving tier-0 rules (the vast majority -- anything with no
// producer/consumer relationship) in their original relative order.
// sort.SliceStable is what orderForDispatch is documented to require; this
// test would fail under sort.Slice or any comparator that does not treat
// equal-tier elements as already in the desired order.
func TestOrderForDispatch_PreservesTierZeroRelativeOrder(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	p := &Pipeline{engine: engine, fixers: nil, logger: testLogger()}

	// Neither rule ID has a registered fixer (p.fixers is nil), so
	// dispatchPriority returns tier 0 for both via the "no fixer found"
	// branch -- exactly the common case orderForDispatch must leave alone.
	violations := []Violation{
		{RuleID: "zzz_last_alphabetically"},
		{RuleID: "aaa_first_alphabetically"},
		{RuleID: "mmm_middle"},
	}

	ordered := orderForDispatch(p, violations)

	wantOrder := []string{"zzz_last_alphabetically", "aaa_first_alphabetically", "mmm_middle"}
	if len(ordered) != len(wantOrder) {
		t.Fatalf("orderForDispatch returned %d violations, want %d", len(ordered), len(wantOrder))
	}
	for i, want := range wantOrder {
		if ordered[i].RuleID != want {
			t.Errorf("ordered[%d].RuleID = %q, want %q (tier-0 relative order must be preserved)", i, ordered[i].RuleID, want)
		}
	}

	// The input slice itself must be untouched -- orderForDispatch returns a
	// copy, per its doc comment, so display/API ordering derived from the
	// original (EvaluationResult.Violations, RulesConsidered) is unaffected.
	if violations[0].RuleID != "zzz_last_alphabetically" {
		t.Error("orderForDispatch must not mutate its input slice")
	}
}
