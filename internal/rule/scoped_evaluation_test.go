package rule

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Regression tests for #2476: running one rule used to evaluate EVERY enabled
// rule, so asking for a purely local rule (byte-identical image de-dupe, which
// only hashes files on disk) also ran the provider-backed checkers and queried
// MusicBrainz once per artist.
//
// READ THIS BEFORE EDITING: every "no provider calls were made" assertion below
// is paired with a POSITIVE CONTROL that proves the provider stub is actually
// reachable from the engine and that the artist is eligible for the
// provider-backed rule. Without that control the assertion is worthless: the
// engine's provider fields default to nil, both provider-backed checkers
// hard-guard on nil and return early, and so `calls == 0` would hold even with
// the bug fully present. A test that cannot fail is not a test.

// engineWithProviderStubs wires BOTH provider-backed dependencies onto a real,
// DB-backed engine. Wiring is via the setters, which is the same path production
// uses, so a test cannot silently drift into the nil-provider no-op.
func engineWithProviderStubs(t *testing.T, ruleSvc *Service, db *sql.DB) (*Engine, *stubReleaseGroupFetcher, *stubMetadataProvider) {
	t.Helper()

	// The stub returns MORE release groups than the seeded NFO lists, so the
	// coverage comparison actually runs rather than short-circuiting.
	rg := &stubReleaseGroupFetcher{
		groups: []provider.ReleaseGroupInfo{
			{Title: "A", PrimaryType: "Album"},
			{Title: "B", PrimaryType: "Album"},
			{Title: "C", PrimaryType: "Album"},
			{Title: "D", PrimaryType: "Album"},
		},
	}
	md := &stubMetadataProvider{metadata: &provider.ArtistMetadata{}}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	engine.SetReleaseGroupFetcher(rg)
	engine.SetMetadataProvider(md)
	return engine, rg, md
}

// enableDiscographyRule turns on discography_populated, the provider-backed rule
// these tests use as their positive control. It ships DISABLED, and a disabled
// rule is not eligible, so without this the control could never fire and every
// "zero provider calls" assertion in this package would be vacuously true.
func enableDiscographyRule(t *testing.T, ruleSvc *Service) {
	t.Helper()
	r, err := ruleSvc.GetByID(context.Background(), RuleDiscographyPopulated)
	if err != nil {
		t.Fatalf("loading rule %s: %v", RuleDiscographyPopulated, err)
	}
	r.Enabled = true
	if err := ruleSvc.Update(context.Background(), r); err != nil {
		t.Fatalf("enabling rule %s: %v", RuleDiscographyPopulated, err)
	}
}

// providerBackedArtist seeds an artist that genuinely REACHES the discography
// checker's provider call. Both preconditions are load-bearing:
//
//   - a MusicBrainz ID, or the checker returns before fetching anything;
//   - an artist.nfo with at least one album, or the checker short-circuits on
//     "empty discography" (Signal 1) and flags a violation WITHOUT a fetch.
//
// Get either wrong and the provider stub is never called, which is exactly how a
// "no provider calls were made" test ends up proving nothing.
func providerBackedArtist(t *testing.T, name string) *artist.Artist {
	t.Helper()
	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name: name,
		Albums: []nfo.DiscographyAlbum{
			{Title: "A", MusicBrainzReleaseGroupID: "rg-a"},
		},
	})
	return &artist.Artist{
		Name:          name,
		Path:          dir,
		MusicBrainzID: "11111111-2222-3333-4444-555555555555",
	}
}

// TestEvaluateScoped_LocalRuleMakesNoProviderCalls is the core #2476 regression.
//
// The positive control runs FIRST and must fail the test if the stub is
// unreachable. Only then does the "local rule makes no calls" assertion mean
// anything.
func TestEvaluateScoped_LocalRuleMakesNoProviderCalls(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	enableDiscographyRule(t, ruleSvc)
	engine, rg, _ := engineWithProviderStubs(t, ruleSvc, db)

	a := providerBackedArtist(t, "Scoped Eval")

	// POSITIVE CONTROL: evaluating the provider-backed rule MUST reach the stub.
	// This proves the wiring, the MBID, and the rule's enablement are all real.
	if _, err := engine.EvaluateScoped(ctx, a, map[string]bool{RuleDiscographyPopulated: true}); err != nil {
		t.Fatalf("positive control: evaluating %s: %v", RuleDiscographyPopulated, err)
	}
	if rg.calls == 0 {
		t.Fatal("positive control FAILED: evaluating discography_populated made no " +
			"release-group call. The stub is not reachable from the engine, so the " +
			"zero-calls assertion below would pass vacuously. Fix the harness, not the assertion.")
	}

	// Now the actual regression: a local rule must not touch the network.
	before := rg.calls
	if _, err := engine.EvaluateScoped(ctx, a, map[string]bool{RuleImageDuplicateExact: true}); err != nil {
		t.Fatalf("evaluating %s: %v", RuleImageDuplicateExact, err)
	}
	if got := rg.calls - before; got != 0 {
		t.Errorf("evaluating the byte-identical image de-dupe rule made %d release-group "+
			"call(s); want 0. Evaluation is not scoped to the requested rule (#2476).", got)
	}
}

// TestEvaluateScoped_OnlyConsidersRequestedRules pins the scope itself, so a
// regression that quietly widens it is caught even if no provider is wired.
func TestEvaluateScoped_OnlyConsidersRequestedRules(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	engine, _, _ := engineWithProviderStubs(t, ruleSvc, db)

	a := &artist.Artist{Name: "Scope", Path: t.TempDir(), MusicBrainzID: "mbid"}

	res, err := engine.EvaluateScoped(ctx, a, map[string]bool{RuleImageDuplicateExact: true})
	if err != nil {
		t.Fatalf("EvaluateScoped: %v", err)
	}
	if len(res.RulesConsidered) != 1 || res.RulesConsidered[0] != RuleImageDuplicateExact {
		t.Errorf("RulesConsidered = %v; want exactly [%s]", res.RulesConsidered, RuleImageDuplicateExact)
	}
	if !res.Scoped {
		t.Error("Scoped = false; a scoped result must mark itself so its HealthScore is not persisted")
	}
	if res.HealthScore != 0 {
		t.Errorf("HealthScore = %v; a scoped result must not carry a subset score (#2476)", res.HealthScore)
	}

	// An UNSCOPED evaluation still considers everything and still scores.
	full, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(full.RulesConsidered) <= 1 {
		t.Fatalf("unscoped Evaluate considered %d rule(s); want the full set", len(full.RulesConsidered))
	}
	if full.Scoped {
		t.Error("unscoped Evaluate returned Scoped = true")
	}
}

// TestEvaluateScoped_EmptyScopeEvaluatesNothing guards the nil-versus-empty trap.
//
// A category matching no eligible rules produces an empty, non-nil scope. If
// emptiness were treated as "unscoped", the engine would run EVERY rule and
// silently reintroduce #2476 through the back door. Empty means empty.
func TestEvaluateScoped_EmptyScopeEvaluatesNothing(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	engine, rg, md := engineWithProviderStubs(t, ruleSvc, db)

	a := &artist.Artist{Name: "Empty Scope", Path: t.TempDir(), MusicBrainzID: "mbid"}

	res, err := engine.EvaluateScoped(ctx, a, map[string]bool{})
	if err != nil {
		t.Fatalf("EvaluateScoped: %v", err)
	}
	if len(res.RulesConsidered) != 0 {
		t.Errorf("RulesConsidered = %v; an empty non-nil scope must evaluate NOTHING, "+
			"not fall back to every rule", res.RulesConsidered)
	}
	if rg.calls != 0 || md.calls != 0 {
		t.Errorf("empty scope made provider calls (release-groups=%d metadata=%d); want 0/0",
			rg.calls, md.calls)
	}
}

// TestScopeForCategory_UnknownCategoryIsEmptyNotNil is the companion to the test
// above: the resolver must not hand back nil (which means "everything") for a
// category that simply matched nothing.
func TestScopeForCategory_UnknownCategoryIsEmptyNotNil(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	engine, _, _ := engineWithProviderStubs(t, ruleSvc, db)
	a := &artist.Artist{Name: "Cat", Path: t.TempDir()}

	scope, err := engine.ScopeForCategory(ctx, a, "no-such-category")
	if err != nil {
		t.Fatalf("ScopeForCategory: %v", err)
	}
	if scope == nil {
		t.Fatal("ScopeForCategory returned nil for an unmatched category; nil means " +
			"\"evaluate everything\", which would run every rule instead of none")
	}
	if len(scope) != 0 {
		t.Errorf("scope = %v; want empty", scope)
	}

	// The empty category is the whole-artist run and MUST be nil.
	all, err := engine.ScopeForCategory(ctx, a, "")
	if err != nil {
		t.Fatalf("ScopeForCategory(\"\"): %v", err)
	}
	if all != nil {
		t.Errorf("ScopeForCategory(\"\") = %v; want nil (no scoping)", all)
	}

	// A real category matches at least one rule.
	img, err := engine.ScopeForCategory(ctx, a, "image")
	if err != nil {
		t.Fatalf("ScopeForCategory(image): %v", err)
	}
	if len(img) == 0 {
		t.Error("ScopeForCategory(image) matched no rules; the category filter is broken")
	}
}

// TestRunRule_LocalImageRuleMakesNoProviderCalls is the end-to-end version, at
// the pipeline level, on the exact path the operator's button uses.
//
// It carries the same positive control, for the same reason.
func TestRunRule_LocalImageRuleMakesNoProviderCalls(t *testing.T) {
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

	a := providerBackedArtist(t, "Pipeline Scoped")
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// POSITIVE CONTROL: running the provider-backed rule through the pipeline
	// must reach the stubs. If this does not fire, the assertion below proves
	// nothing.
	if _, err := p.RunRule(ctx, RuleDiscographyPopulated); err != nil {
		t.Fatalf("positive control: RunRule(%s): %v", RuleDiscographyPopulated, err)
	}
	if rg.calls == 0 {
		t.Fatal("positive control FAILED: RunRule(discography_populated) made no " +
			"release-group call, so the zero-calls assertion below is vacuous")
	}

	rgBefore, mdBefore := rg.calls, md.calls

	// The regression: a local, filesystem-only rule must complete with zero
	// outbound provider traffic.
	if _, err := p.RunRule(ctx, RuleImageDuplicateExact); err != nil {
		t.Fatalf("RunRule(%s): %v", RuleImageDuplicateExact, err)
	}

	if got := rg.calls - rgBefore; got != 0 {
		t.Errorf("RunRule(%s) made %d MusicBrainz release-group call(s); want 0. "+
			"A local image rule must not query an external provider (#2476).",
			RuleImageDuplicateExact, got)
	}
	if got := md.calls - mdBefore; got != 0 {
		t.Errorf("RunRule(%s) made %d metadata call(s); want 0.",
			RuleImageDuplicateExact, got)
	}
}
