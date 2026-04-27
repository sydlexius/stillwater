package rule

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// lookupViolationByRuleArtist returns the persisted (id, status) of the
// rule_violations row keyed by (rule_id, artist_id). The unique index makes
// this a stable lookup across re-evaluations because UpsertViolation reuses
// the existing row id on conflict. Returns ("", "", nil) when no row exists.
func lookupViolationByRuleArtist(ctx context.Context, db *sql.DB, ruleID, artistID string) (string, string, error) {
	var id, status string
	err := db.QueryRowContext(ctx,
		`SELECT id, status FROM rule_violations WHERE rule_id = ? AND artist_id = ?`,
		ruleID, artistID,
	).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return id, status, nil
}

// TestPipeline_ReEvalPassResolvesStaleViolation exercises issue #1105: when a
// rule that previously violated now passes (because the underlying condition
// was corrected out-of-band, e.g. user dropped artist.nfo into the directory),
// the existing open rule_violations row must transition to status='resolved'.
// Without the fix, persistPassResults would write the rule_results pass row
// but leave the violation row stuck at status='open' forever.
func TestPipeline_ReEvalPassResolvesStaleViolation(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleNFOExists)

	a := &artist.Artist{
		Name:      "Pass After Fix",
		SortName:  "Pass After Fix",
		Path:      t.TempDir(),
		NFOExists: true, // checkNFOExists -> nil (rule passes)
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Pre-seed an open violation row that pre-dates the (out-of-band) fix.
	stale := &RuleViolation{
		RuleID: RuleNFOExists, ArtistID: a.ID, ArtistName: a.Name,
		Severity: "error", Message: "stale; nfo now exists", Fixable: true,
		Status: ViolationStatusOpen,
	}
	if err := ruleSvc.UpsertViolation(ctx, stale); err != nil {
		t.Fatalf("seeding stale violation: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	if _, err := pipeline.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	// The pre-existing violation row must now be status='resolved' with
	// resolved_at populated. Without the #1105 fix it stays at 'open'.
	rv, err := ruleSvc.GetViolationByID(ctx, stale.ID)
	if err != nil {
		t.Fatalf("GetViolationByID: %v", err)
	}
	if rv.Status != ViolationStatusResolved {
		t.Errorf("violation status after re-eval = %q, want %q", rv.Status, ViolationStatusResolved)
	}
	if rv.ResolvedAt == nil {
		t.Errorf("resolved_at = nil, want populated")
	}
}

// TestPipeline_ReEvalDoesNotResolveDismissed verifies the #1105 fix respects
// the #1107 invariant: a rule that now passes must NOT clobber a
// previously-dismissed violation row back to resolved. Dismissed is terminal
// from the user's perspective and ResolveViolationIfActive only touches rows
// in status open or pending_choice.
func TestPipeline_ReEvalDoesNotResolveDismissed(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleNFOExists)

	a := &artist.Artist{
		Name: "Dismissed Survives", SortName: "Dismissed Survives",
		Path: t.TempDir(), NFOExists: true,
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	v := &RuleViolation{
		RuleID: RuleNFOExists, ArtistID: a.ID, ArtistName: a.Name,
		Severity: "error", Message: "old", Fixable: true,
		Status: ViolationStatusOpen,
	}
	if err := ruleSvc.UpsertViolation(ctx, v); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ruleSvc.DismissViolation(ctx, v.ID); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())
	if _, err := pipeline.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	rv, err := ruleSvc.GetViolationByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetViolationByID: %v", err)
	}
	if rv.Status != ViolationStatusDismissed {
		t.Errorf("dismissed violation status = %q, want %q (must survive re-eval)", rv.Status, ViolationStatusDismissed)
	}
}

// TestPipeline_FixerInvalidatesFSCache exercises issue #1108: after a fixer
// mutates the filesystem, the engine's FSCache must be invalidated so the
// next Evaluate call re-reads the directory listing instead of serving the
// pre-mutation snapshot. We exercise this by populating the dir cache, then
// driving the NFOFixer through the pipeline (which writes artist.nfo), and
// asserting the cached dir entry is gone.
func TestPipeline_FixerInvalidatesFSCache(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleNFOExists)
	if _, err := db.ExecContext(ctx,
		`UPDATE rules SET automation_mode = ? WHERE id = ?`,
		AutomationModeAuto, RuleNFOExists); err != nil {
		t.Fatalf("setting automation_mode=auto: %v", err)
	}

	dir := t.TempDir()
	a := &artist.Artist{
		Name: "Cache Invalidation", SortName: "Cache Invalidation",
		Path: dir, NFOExists: false, LibraryID: "lib-test",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	cache := NewFSCache(60*time.Second, 100, testLogger())
	engine.SetFSCache(cache)

	// Prime the cache by reading the (currently empty) directory.
	if _, err := cache.ReadDir(dir); err != nil {
		t.Fatalf("priming cache ReadDir: %v", err)
	}
	if cache.Len() == 0 {
		t.Fatalf("cache empty after prime; expected at least one entry")
	}

	nfoFixer := &NFOFixer{fsCheck: nonSharedFSCheck()}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{nfoFixer}, nil, testLogger())

	runResult, err := pipeline.RunForArtist(ctx, a)
	if err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}
	if runResult.FixesSucceeded != 1 {
		t.Fatalf("FixesSucceeded = %d, want 1", runResult.FixesSucceeded)
	}

	// After the fix, a fresh ReadDir(dir) must observe artist.nfo. If the
	// cache was not invalidated the call would return the stale empty
	// listing and miss the new file.
	entries, err := cache.ReadDir(dir)
	if err != nil {
		t.Fatalf("post-fix ReadDir: %v", err)
	}
	var sawNFO bool
	for _, e := range entries {
		if e.Name == "artist.nfo" {
			sawNFO = true
			break
		}
	}
	if !sawNFO {
		t.Errorf("post-fix ReadDir missed artist.nfo; cache was not invalidated")
	}

	// Sanity: artist.nfo really does exist on disk -- the test is meaningful
	// only when the underlying filesystem mutation actually happened.
	if _, err := os.Stat(filepath.Join(dir, "artist.nfo")); err != nil {
		t.Fatalf("artist.nfo missing on disk: %v", err)
	}
}

// TestService_DisablingRuleSoftResolvesViolations is the #1143 contract test
// distinct from TestService_DisablingRuleDoesNotMarkDirty: when a rule is
// disabled, every active violation for that rule transitions to resolved with
// resolved_at populated. The test seeds an artist + a rule + an open
// violation, then disables the rule and asserts the row is now resolved.
func TestService_DisablingRuleSoftResolvesViolations(t *testing.T) {
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
	if !r.Enabled {
		// Force-enable so the test is independent of seed defaults.
		r.Enabled = true
		if err := svc.Update(ctx, r); err != nil {
			t.Fatalf("force-enable: %v", err)
		}
	}

	v := &RuleViolation{
		RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "A1",
		Severity: "error", Message: "missing nfo", Fixable: true,
		Status: ViolationStatusOpen,
	}
	if err := svc.UpsertViolation(ctx, v); err != nil {
		t.Fatalf("UpsertViolation: %v", err)
	}

	// Refresh the rule and disable it.
	r, err = svc.GetByID(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("GetByID after seed: %v", err)
	}
	r.Enabled = false
	if err := svc.Update(ctx, r); err != nil {
		t.Fatalf("disable Update: %v", err)
	}

	got, err := svc.GetViolationByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetViolationByID: %v", err)
	}
	if got.Status != ViolationStatusResolved {
		t.Errorf("status after disable = %q, want %q (soft-resolve per #1143)", got.Status, ViolationStatusResolved)
	}
	if got.ResolvedAt == nil {
		t.Errorf("resolved_at = nil, want populated")
	}

	// rule_results must also be purged on the manual-disable path so the
	// per-rule dashboard stops surfacing stale pass/fail counts. Without this
	// assertion the manual disable's rule_results invariant could regress
	// silently while only the auto-disable test catches it.
	var resultsCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rule_results WHERE rule_id = ?`, RuleNFOExists,
	).Scan(&resultsCount); err != nil {
		t.Fatalf("count rule_results: %v", err)
	}
	if resultsCount != 0 {
		t.Errorf("rule_results rows for %s = %d, want 0 after manual disable", RuleNFOExists, resultsCount)
	}
}

// TestPipeline_BioViolationResolvedAfterBiographyPopulated exercises issue
// #1027: the post-refresh hook in handlers_refresh.go runs RunForArtist after
// a successful provider fetch populates Biography. The bio_exists rule should
// then evaluate as a pass for that artist, and the previously-open
// rule_violations row must transition to status='resolved'. This test bypasses
// the HTTP layer and simulates the same sequence directly: seed an open
// bio_missing violation against an artist with no biography, persist a
// biography update through the artist service, then run the pipeline and
// assert the row is resolved with resolved_at populated.
//
// Without the W2.B (#1105) resolve-on-pass fix this test would still fail,
// because the pipeline only resolves rows it produced inline (via the
// resolvedRows path) and not stale rows whose underlying condition was
// corrected out-of-band. The test also guards against future regressions in
// the post-refresh wiring: any change that prevents the pipeline from seeing
// the persisted Biography (e.g. the post-refresh handler reading a stale
// in-memory copy instead of GetByID-ing fresh state) would surface here as a
// re-evaluation that still reports bio_missing and never resolves the row.
func TestPipeline_BioViolationResolvedAfterBiographyPopulated(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleBioExists)

	// Step 1: insert an artist with empty biography. checkBioExists fires.
	a := &artist.Artist{
		Name:      "Bio Refresh",
		SortName:  "Bio Refresh",
		Path:      t.TempDir(),
		Biography: "",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	// Step 2: first eval -- bio_missing violation row must exist as 'open'.
	if _, err := pipeline.RunForArtist(ctx, a); err != nil {
		t.Fatalf("first RunForArtist: %v", err)
	}
	violationID, openStatus, err := lookupViolationByRuleArtist(ctx, db, RuleBioExists, a.ID)
	if err != nil {
		t.Fatalf("looking up bio violation after first eval: %v", err)
	}
	if violationID == "" {
		t.Fatalf("expected bio_missing violation row after first eval, got none")
	}
	if openStatus != ViolationStatusOpen {
		t.Fatalf("violation status after first eval = %q, want %q", openStatus, ViolationStatusOpen)
	}

	// Step 3: simulate provider refresh populating the biography. The
	// production path is internal/api/handlers_refresh.go executeRefreshCtx,
	// which calls artist.Service.Update with Biography filled in. We
	// reproduce the persisted side-effect directly.
	a.Biography = "Bio Refresh is a rock band formed for testing in 2026."
	if err := artistSvc.Update(ctx, a); err != nil {
		t.Fatalf("Update with populated biography: %v", err)
	}

	// Step 4: re-run the pipeline against the freshly-loaded artist, the
	// same way runRulesAfterRefresh does (GetByID then RunForArtist). The
	// bio_exists rule now passes, so persistPassResults must call
	// ResolveViolationIfActive and transition the row to resolved.
	fresh, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID after Update: %v", err)
	}
	if fresh.Biography == "" {
		t.Fatalf("biography did not persist; reloaded artist has empty Biography")
	}
	if _, err := pipeline.RunForArtist(ctx, fresh); err != nil {
		t.Fatalf("post-refresh RunForArtist: %v", err)
	}

	rv, err := ruleSvc.GetViolationByID(ctx, violationID)
	if err != nil {
		t.Fatalf("GetViolationByID: %v", err)
	}
	if rv.Status != ViolationStatusResolved {
		t.Errorf("bio violation status after post-refresh eval = %q, want %q",
			rv.Status, ViolationStatusResolved)
	}
	if rv.ResolvedAt == nil {
		t.Errorf("resolved_at = nil, want populated after post-refresh eval")
	}
}

// TestService_DisableFilesystemRulesSoftResolvesViolations asserts that the
// auto-disable path (DisableFilesystemRules, triggered when the last local
// library is removed) runs the same cleanup as a manual disable Update: open
// violations transition to resolved and rule_results rows are purged. Without
// this, an auto-disabled fs rule keeps surfacing stale "needs attention"
// counts and stale pass/fail rows on the dashboard.
func TestService_DisableFilesystemRulesSoftResolvesViolations(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// RuleNFOExists is the lone filesystem-dependent rule; force-enable it so
	// the test is independent of seed defaults.
	r, err := svc.GetByID(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !r.Enabled {
		r.Enabled = true
		if err := svc.Update(ctx, r); err != nil {
			t.Fatalf("force-enable: %v", err)
		}
	}

	// Seed an artist (FK target) before upserting a violation. UpsertViolation
	// atomically writes a sibling rule_results row, so we don't need to insert
	// rule_results manually -- after the auto-disable, both rows must be gone
	// (rule_results) or resolved (rule_violations).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`,
		"a1", "A1",
	); err != nil {
		t.Fatalf("seed artist: %v", err)
	}

	v := &RuleViolation{
		RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "A1",
		Severity: "error", Message: "missing nfo", Fixable: true,
		Status: ViolationStatusOpen,
	}
	if err := svc.UpsertViolation(ctx, v); err != nil {
		t.Fatalf("UpsertViolation: %v", err)
	}

	count, err := svc.DisableFilesystemRules(ctx)
	if err != nil {
		t.Fatalf("DisableFilesystemRules: %v", err)
	}
	if count < 1 {
		t.Fatalf("DisableFilesystemRules count = %d, want >= 1", count)
	}

	got, err := svc.GetViolationByID(ctx, v.ID)
	if err != nil {
		t.Fatalf("GetViolationByID: %v", err)
	}
	if got.Status != ViolationStatusResolved {
		t.Errorf("status after auto-disable = %q, want %q", got.Status, ViolationStatusResolved)
	}
	if got.ResolvedAt == nil {
		t.Errorf("resolved_at = nil, want populated after auto-disable")
	}

	var resultsCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rule_results WHERE rule_id = ?`, RuleNFOExists,
	).Scan(&resultsCount); err != nil {
		t.Fatalf("count rule_results: %v", err)
	}
	if resultsCount != 0 {
		t.Errorf("rule_results rows for %s = %d, want 0 (cleanup should have purged)", RuleNFOExists, resultsCount)
	}
}
