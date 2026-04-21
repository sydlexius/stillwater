package rule

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
)

// disableAllRulesExcept marks every rule disabled except those in keep so
// the pipeline only evaluates a controlled subset. This keeps the test focused
// on the rule_results persistence boundary, not on whether the mock artist
// satisfies the complete built-in rule set.
func disableAllRulesExcept(t *testing.T, db *sql.DB, keep ...string) {
	t.Helper()
	keepSet := make(map[string]bool, len(keep))
	for _, id := range keep {
		keepSet[id] = true
	}
	rows, err := db.QueryContext(context.Background(), `SELECT id FROM rules`)
	if err != nil {
		t.Fatalf("listing rules: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var toDisable []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning rule id: %v", err)
		}
		if !keepSet[id] {
			toDisable = append(toDisable, id)
		}
	}
	for _, id := range toDisable {
		if _, err := db.ExecContext(context.Background(),
			`UPDATE rules SET enabled = 0 WHERE id = ?`, id); err != nil {
			t.Fatalf("disabling rule %s: %v", id, err)
		}
	}
}

func TestPipeline_WritesPassResults(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	// Keep only two rules that the minimal-NFO artist satisfies.
	disableAllRulesExcept(t, db, RuleNFOExists, RuleNFOHasMBID)

	a := &artist.Artist{
		Name:          "Pass All Rules",
		SortName:      "Pass All Rules",
		Path:          t.TempDir(),
		NFOExists:     true, // checkNFOExists reads this flag
		MusicBrainzID: "abc-123",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	if _, err := pipeline.RunAllScoped(ctx, RunScopeAll); err != nil {
		t.Fatalf("RunAllScoped: %v", err)
	}

	// Both enabled rules should have a passed=1 row for this artist.
	results, err := ruleSvc.GetRuleResultsForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetRuleResultsForArtist: %v", err)
	}
	byRule := make(map[string]RuleResult, len(results))
	for _, r := range results {
		byRule[r.RuleID] = r
	}
	for _, rid := range []string{RuleNFOExists, RuleNFOHasMBID} {
		got, ok := byRule[rid]
		if !ok {
			t.Errorf("missing rule_result for %s", rid)
			continue
		}
		if !got.Passed {
			t.Errorf("%s passed = false, want true", rid)
		}
		if got.LastPassedAt == nil {
			t.Errorf("%s last_passed_at = nil, want set", rid)
		}
	}
}

// TestPipeline_FixFlipsFailToPassInSameRun exercises the propagation fix for
// CR comment 3114386788: a rule that starts failing and is auto-fixed mid-run
// must end the pass with passed=1 in rule_results for the same evaluation. The
// prior implementation froze the skip-set from the pre-fix Violations slice,
// so a repaired rule never got a pass row written until the next Run Rules
// pass re-evaluated the artist. Drive this via NFOFixer (creates artist.nfo
// and flips a.NFOExists to true), then assert rule_results shows Passed=true
// after a single RunAllScoped invocation.
func TestPipeline_FixFlipsFailToPassInSameRun(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	// Keep only the nfo_exists rule enabled so the test focuses on the
	// fail-to-pass transition without noise from other checkers.
	disableAllRulesExcept(t, db, RuleNFOExists)
	// Seeded defaults use automation_mode=manual, which the pipeline
	// treats as "discover candidates only; never apply". NFOFixer does
	// not implement CandidateDiscoverer, so in manual mode the fix is
	// skipped and the rule stays failing. Force auto so the fixer runs.
	if _, err := db.ExecContext(ctx,
		`UPDATE rules SET automation_mode = ? WHERE id = ?`,
		AutomationModeAuto, RuleNFOExists); err != nil {
		t.Fatalf("setting automation_mode=auto: %v", err)
	}

	a := &artist.Artist{
		Name:      "Fix Flips Pass",
		SortName:  "Fix Flips Pass",
		Path:      t.TempDir(), // dir exists, artist.nfo does not -> initial fail
		NFOExists: false,
		LibraryID: "lib-test", // nonSharedFSCheck returns SharedFSNone unconditionally
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	// Real NFOFixer: writes artist.nfo and sets a.NFOExists=true so the
	// post-fix engine.Evaluate call inside updateHealthScore observes the
	// repair. The fixer needs a SharedFSCheck that reports non-shared so
	// the write is allowed.
	nfoFixer := &NFOFixer{fsCheck: nonSharedFSCheck()}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{nfoFixer}, nil, testLogger())

	runResult, err := pipeline.RunAllScoped(ctx, RunScopeAll)
	if err != nil {
		t.Fatalf("RunAllScoped: %v", err)
	}
	if runResult.FixesSucceeded != 1 {
		t.Fatalf("FixesSucceeded = %d, want 1 (NFOFixer should have repaired the violation)", runResult.FixesSucceeded)
	}

	// rule_results for this (artist, rule) pair must show passed=1 even
	// though the pre-fix Violations slice contained this rule. That is
	// the whole point of deriving the pass skip-set from the post-fix
	// evaluation.
	results, err := ruleSvc.GetRuleResultsForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetRuleResultsForArtist: %v", err)
	}
	var found bool
	for _, r := range results {
		if r.RuleID != RuleNFOExists {
			continue
		}
		found = true
		if !r.Passed {
			t.Errorf("rule_results.passed = false for rule that was fixed this run, want true")
		}
		if r.LastPassedAt == nil {
			t.Errorf("rule_results.last_passed_at = nil after in-run fix, want set")
		}
	}
	if !found {
		t.Fatalf("no rule_results row for %s after fix-in-run (want one with passed=1)", RuleNFOExists)
	}
}

func TestPipeline_WritesFailResultLinkedToViolation(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	// Keep only nfo_exists enabled; the artist has an empty path so it fails.
	disableAllRulesExcept(t, db, RuleNFOExists)

	a := &artist.Artist{
		Name:     "Fail NFO",
		SortName: "Fail NFO",
		Path:     t.TempDir(), // dir exists but no NFO
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	// No fixer registered: the pipeline will persist the violation as open.
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

	if _, err := pipeline.RunAllScoped(ctx, RunScopeAll); err != nil {
		t.Fatalf("RunAllScoped: %v", err)
	}

	// The fail rule_results row must link to the corresponding violation.
	var ruleResultViolationID sql.NullString
	var passedInt int
	if err := db.QueryRowContext(ctx, `
		SELECT passed, violation_id FROM rule_results
		WHERE artist_id = ? AND rule_id = ?`,
		a.ID, RuleNFOExists).Scan(&passedInt, &ruleResultViolationID); err != nil {
		t.Fatalf("querying rule_result: %v", err)
	}
	if passedInt != 0 {
		t.Errorf("passed = %d, want 0 for failing rule", passedInt)
	}
	if !ruleResultViolationID.Valid || ruleResultViolationID.String == "" {
		t.Fatalf("violation_id is NULL on fail row")
	}

	var violationID string
	if err := db.QueryRowContext(ctx, `
		SELECT id FROM rule_violations
		WHERE artist_id = ? AND rule_id = ? AND status = ?`,
		a.ID, RuleNFOExists, ViolationStatusOpen).Scan(&violationID); err != nil {
		t.Fatalf("querying rule_violations: %v", err)
	}
	if ruleResultViolationID.String != violationID {
		t.Errorf("rule_results.violation_id = %q, want %q",
			ruleResultViolationID.String, violationID)
	}
}

func TestHealthSubscriber_WritesResultsOnArtistUpdated(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleNFOExists, RuleNFOHasMBID)

	a := &artist.Artist{
		Name:          "Health Sub Test",
		SortName:      "Health Sub Test",
		Path:          t.TempDir(),
		NFOExists:     true,
		MusicBrainzID: "abc-123",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	sub := NewHealthSubscriber(engine, artistSvc, testLogger())

	// Invoke evaluateArtist directly (bypasses the debounce ticker) so the
	// assertions run synchronously without needing the goroutine.
	sub.evaluateArtist(ctx, a.ID)

	results, err := ruleSvc.GetRuleResultsForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetRuleResultsForArtist: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("rule_results rows = %d, want 2 (one per enabled rule)", len(results))
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("rule %s passed = false, want true", r.RuleID)
		}
	}
}

func TestBackfill_SeedsFirstFailedAtFromViolationCreatedAt(t *testing.T) {
	// Open a fresh, raw DB: the shared template already has Migrate applied
	// with the new backfill step, so to exercise the backfill itself we
	// need to simulate the "v1 instance with pre-existing violations but
	// no rule_results yet" state.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "backfill.db")
	raw, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening raw db: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	// Apply the full schema via Migrate so rule_results exists, then
	// wipe its rows (simulating a DB whose 001 ran before #699 shipped).
	if err := database.Migrate(raw); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(),
		`DELETE FROM rule_results`); err != nil {
		t.Fatalf("clearing rule_results: %v", err)
	}

	// Seed an artist, a rule, and an open violation with an old created_at.
	seededCreatedAt := "2025-01-15T08:30:00Z"
	if _, err := raw.ExecContext(context.Background(), `
		INSERT INTO artists (id, name, path) VALUES ('a1', 'A', '')`); err != nil {
		t.Fatalf("inserting artist: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(), `
		INSERT INTO rules (id, name, description, category, enabled, automation_mode, config)
		VALUES ('r1', 'R1', 'desc', 'nfo', 1, 'auto', '{}')`); err != nil {
		t.Fatalf("inserting rule: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(), `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status, created_at, updated_at)
		VALUES ('v1', 'r1', 'a1', 'A', 'warning', 'bad', 0, 'open', ?, ?)`,
		seededCreatedAt, seededCreatedAt); err != nil {
		t.Fatalf("inserting violation: %v", err)
	}

	// Re-run Migrate: the backfill must fill in the missing rule_results
	// row and preserve the original created_at as first_failed_at.
	if err := database.Migrate(raw); err != nil {
		t.Fatalf("re-running migrations: %v", err)
	}

	var firstFailedAt sql.NullString
	if err := raw.QueryRowContext(context.Background(), `
		SELECT first_failed_at FROM rule_results
		WHERE artist_id = 'a1' AND rule_id = 'r1'`).Scan(&firstFailedAt); err != nil {
		t.Fatalf("querying rule_results: %v", err)
	}
	if !firstFailedAt.Valid {
		t.Fatalf("first_failed_at is NULL; backfill did not seed it")
	}
	if firstFailedAt.String != seededCreatedAt {
		t.Errorf("first_failed_at = %q, want %q (must mirror violation.created_at)",
			firstFailedAt.String, seededCreatedAt)
	}

	// Re-running Migrate a third time must be idempotent (no error, and
	// the existing row is not clobbered by another INSERT).
	preUpdate := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := raw.ExecContext(context.Background(), `
		UPDATE rule_results SET last_passed_at = ?
		WHERE artist_id = 'a1' AND rule_id = 'r1'`, preUpdate); err != nil {
		t.Fatalf("touching rule_results: %v", err)
	}
	if err := database.Migrate(raw); err != nil {
		t.Fatalf("third Migrate: %v", err)
	}
	var lastPassedAt sql.NullString
	if err := raw.QueryRowContext(context.Background(), `
		SELECT last_passed_at FROM rule_results
		WHERE artist_id = 'a1' AND rule_id = 'r1'`).Scan(&lastPassedAt); err != nil {
		t.Fatalf("querying rule_results post-re-Migrate: %v", err)
	}
	if !lastPassedAt.Valid || lastPassedAt.String != preUpdate {
		t.Errorf("last_passed_at = %+v, want preserved %q (INSERT OR IGNORE should not clobber)",
			lastPassedAt, preUpdate)
	}
}
