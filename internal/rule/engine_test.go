package rule

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

func testLogger() *slog.Logger {
	return slog.Default()
}

func TestEvaluate_FullyCompliant(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Disable image rules that need disk access for this test
	for _, id := range []string{RuleThumbSquare, RuleThumbMinRes} {
		r, _ := svc.GetByID(ctx, id)
		r.Enabled = false
		if err := svc.Update(ctx, r); err != nil {
			t.Fatalf("disabling rule %s: %v", id, err)
		}
	}

	engine := NewEngine(svc, db, nil, nil, testLogger())

	artistDir := filepath.Join(t.TempDir(), "Nirvana")
	if err := os.MkdirAll(artistDir, 0o755); err != nil {
		t.Fatalf("creating artist dir: %v", err)
	}

	a := &artist.Artist{
		ID:            "test-1",
		Name:          "Nirvana",
		MusicBrainzID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da",
		NFOExists:     true,
		ThumbExists:   true,
		FanartExists:  true,
		LogoExists:    true,
		Biography:     "Nirvana was an American rock band formed in Aberdeen, Washington, in 1987.",
		Path:          artistDir,
	}

	result, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(result.Violations) != 0 {
		t.Errorf("expected 0 violations, got %d: %v", len(result.Violations), result.Violations)
	}
	if result.HealthScore != 100.0 {
		t.Errorf("HealthScore = %.1f, want 100.0", result.HealthScore)
	}
	if result.RulesPassed != result.RulesTotal {
		t.Errorf("RulesPassed = %d, RulesTotal = %d, expected equal", result.RulesPassed, result.RulesTotal)
	}
}

func TestEvaluate_EmptyArtist(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Disable image rules that need disk access
	for _, id := range []string{RuleThumbSquare, RuleThumbMinRes} {
		r, _ := svc.GetByID(ctx, id)
		r.Enabled = false
		if err := svc.Update(ctx, r); err != nil {
			t.Fatalf("disabling rule %s: %v", id, err)
		}
	}

	engine := NewEngine(svc, db, nil, nil, testLogger())

	artistDir := filepath.Join(t.TempDir(), "Empty Artist")
	if err := os.MkdirAll(artistDir, 0o755); err != nil {
		t.Fatalf("creating artist dir: %v", err)
	}

	a := &artist.Artist{
		ID:   "test-2",
		Name: "Empty Artist",
		Path: artistDir,
	}

	result, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// An empty artist (no NFO, no images, no bio, no MBID) should fail most rules.
	// Assert relative properties so adding new rules does not break this test,
	// but verify specific known rules by checking the violations list.
	if result.RulesTotal == 0 {
		t.Fatal("RulesTotal = 0, expected enabled rules to be evaluated")
	}
	if result.RulesPassed >= result.RulesTotal {
		t.Errorf("RulesPassed = %d, RulesTotal = %d, expected violations for an empty artist", result.RulesPassed, result.RulesTotal)
	}
	if len(result.Violations) == 0 {
		t.Error("expected at least one violation for an empty artist")
	}
	if result.HealthScore >= 100.0 {
		t.Errorf("HealthScore = %.1f, expected < 100 for an empty artist", result.HealthScore)
	}
	// Verify specific known rules fire for an empty artist.
	violationRules := map[string]bool{}
	for _, v := range result.Violations {
		violationRules[v.RuleID] = true
	}
	for _, expected := range []string{RuleNFOExists, RuleThumbExists, RuleFanartExists, RuleBioExists} {
		if !violationRules[expected] {
			t.Errorf("expected violation for %s, not found", expected)
		}
	}
	// These rules should pass for an empty artist (nothing extraneous, dir name matches).
	for _, notExpected := range []string{RuleExtraneousImages, RuleDirectoryNameMismatch} {
		if violationRules[notExpected] {
			t.Errorf("unexpected violation for %s", notExpected)
		}
	}
}

func TestEvaluate_PartialCompliance(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Disable image rules that need disk access
	for _, id := range []string{RuleThumbSquare, RuleThumbMinRes} {
		r, _ := svc.GetByID(ctx, id)
		r.Enabled = false
		if err := svc.Update(ctx, r); err != nil {
			t.Fatalf("disabling rule %s: %v", id, err)
		}
	}

	engine := NewEngine(svc, db, nil, nil, testLogger())

	// Artist has NFO and MBID but nothing else
	artistDir := filepath.Join(t.TempDir(), "Partial")
	if err := os.MkdirAll(artistDir, 0o755); err != nil {
		t.Fatalf("creating artist dir: %v", err)
	}

	a := &artist.Artist{
		ID:            "test-3",
		Name:          "Partial",
		MusicBrainzID: "abc-123",
		NFOExists:     true,
		Path:          artistDir,
	}

	result, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// Partial artist has NFO + MBID but no images or bio. Assert relative
	// properties and verify specific known rules by checking violations.
	if result.RulesPassed == 0 {
		t.Error("RulesPassed = 0, expected nfo_exists and nfo_has_mbid to pass")
	}
	if result.RulesPassed >= result.RulesTotal {
		t.Errorf("RulesPassed = %d, RulesTotal = %d, expected some violations (no images, no bio)", result.RulesPassed, result.RulesTotal)
	}
	if result.HealthScore <= 0 || result.HealthScore >= 100.0 {
		t.Errorf("HealthScore = %.1f, expected between 0 and 100 for partial compliance", result.HealthScore)
	}
	// Verify specific rules: nfo_exists and nfo_has_mbid should NOT appear in violations.
	violationRules := map[string]bool{}
	for _, v := range result.Violations {
		violationRules[v.RuleID] = true
	}
	for _, shouldPass := range []string{RuleNFOExists, RuleNFOHasMBID, RuleExtraneousImages} {
		if violationRules[shouldPass] {
			t.Errorf("unexpected violation for %s (artist has NFO + MBID)", shouldPass)
		}
	}
	// Image rules should fire.
	for _, shouldFail := range []string{RuleThumbExists, RuleFanartExists, RuleBioExists} {
		if !violationRules[shouldFail] {
			t.Errorf("expected violation for %s, not found", shouldFail)
		}
	}
}

func TestEvaluate_DisabledRulesSkipped(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Disable all rules
	rules, _ := svc.List(ctx)
	for i := range rules {
		rules[i].Enabled = false
		if err := svc.Update(ctx, &rules[i]); err != nil {
			t.Fatalf("disabling rule %s: %v", rules[i].ID, err)
		}
	}

	engine := NewEngine(svc, db, nil, nil, testLogger())

	a := &artist.Artist{
		ID:   "test-4",
		Name: "No Rules",
		Path: t.TempDir(),
	}

	result, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if result.RulesTotal != 0 {
		t.Errorf("RulesTotal = %d, want 0", result.RulesTotal)
	}
	if result.HealthScore != 100.0 {
		t.Errorf("HealthScore = %.1f, want 100.0 (no rules = fully compliant)", result.HealthScore)
	}
}

func TestEvaluateAll(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Keep only one rule enabled for simplicity
	rules, _ := svc.List(ctx)
	for i := range rules {
		rules[i].Enabled = rules[i].ID == RuleNFOExists
		if err := svc.Update(ctx, &rules[i]); err != nil {
			t.Fatalf("updating rule %s: %v", rules[i].ID, err)
		}
	}

	engine := NewEngine(svc, db, nil, nil, testLogger())

	artists := []artist.Artist{
		{ID: "a1", Name: "Has NFO", NFOExists: true, Path: t.TempDir()},
		{ID: "a2", Name: "No NFO", NFOExists: false, Path: t.TempDir()},
	}

	results, err := engine.EvaluateAll(ctx, artists)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].HealthScore != 100.0 {
		t.Errorf("artist 1 score = %.1f, want 100.0", results[0].HealthScore)
	}
	if results[1].HealthScore != 0.0 {
		t.Errorf("artist 2 score = %.1f, want 0.0", results[1].HealthScore)
	}
}

func TestCalculateHealthScore(t *testing.T) {
	tests := []struct {
		passed int
		total  int
		want   float64
	}{
		{0, 0, 100.0},
		{0, 8, 0.0},
		{8, 8, 100.0},
		{4, 8, 50.0},
		{2, 6, 33.3},
		{1, 3, 33.3},
		{5, 6, 83.3},
	}

	for _, tt := range tests {
		got := calculateHealthScore(tt.passed, tt.total)
		if got != tt.want {
			t.Errorf("calculateHealthScore(%d, %d) = %.1f, want %.1f", tt.passed, tt.total, got, tt.want)
		}
	}
}

func TestEngine_WithRealDB(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	if err := svc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	engine := NewEngine(svc, db, nil, nil, slog.Default())
	if engine == nil {
		t.Fatal("expected non-nil engine")
	}

	// Verify all default rules have a corresponding checker registered.
	rules, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("listing rules: %v", err)
	}
	if len(engine.checkers) != len(rules) {
		t.Errorf("checkers (%d) != rules (%d): every seeded rule should have a checker", len(engine.checkers), len(rules))
	}
}

// TestEvaluateAll_RuleListCachedAcrossArtists verifies that EvaluateAll
// populates the in-memory rule list cache on the first artist evaluation and
// reuses it for subsequent artists without hitting the database again.
// The test asserts that service.List is called at most once for a batch of
// multiple artists, confirming the N+1 DB query pattern is eliminated.
func TestEvaluateAll_RuleListCachedAcrossArtists(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Disable image rules that need disk access.
	for _, id := range []string{RuleThumbSquare, RuleThumbMinRes} {
		r, _ := svc.GetByID(ctx, id)
		r.Enabled = false
		if err := svc.Update(ctx, r); err != nil {
			t.Fatalf("disabling rule %s: %v", id, err)
		}
	}

	engine := NewEngine(svc, db, nil, nil, testLogger())

	// Confirm the cache is empty before any evaluation.
	engine.ruleCacheMu.RLock()
	if engine.ruleList != nil {
		t.Error("expected nil ruleList before first evaluation")
	}
	engine.ruleCacheMu.RUnlock()

	artists := []artist.Artist{
		{ID: "c1", Name: "Artist One", Path: t.TempDir()},
		{ID: "c2", Name: "Artist Two", Path: t.TempDir()},
		{ID: "c3", Name: "Artist Three", Path: t.TempDir()},
	}

	results, err := engine.EvaluateAll(ctx, artists)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if len(results) != len(artists) {
		t.Fatalf("expected %d results, got %d", len(artists), len(results))
	}

	// After EvaluateAll, the cache must be populated.
	engine.ruleCacheMu.RLock()
	cachedList := engine.ruleList
	cachedAt := engine.ruleFetchedAt
	engine.ruleCacheMu.RUnlock()

	if len(cachedList) == 0 {
		t.Error("expected ruleList to be populated after EvaluateAll")
	}
	if cachedAt.IsZero() {
		t.Error("expected ruleFetchedAt to be non-zero after EvaluateAll")
	}

	// A second EvaluateAll within the TTL window must reuse the same cache
	// entry (fetchedAt must not advance, confirming no DB round-trip occurred).
	results2, err := engine.EvaluateAll(ctx, artists)
	if err != nil {
		t.Fatalf("second EvaluateAll: %v", err)
	}
	if len(results2) != len(artists) {
		t.Fatalf("second EvaluateAll: expected %d results, got %d", len(artists), len(results2))
	}

	engine.ruleCacheMu.RLock()
	secondCachedAt := engine.ruleFetchedAt
	engine.ruleCacheMu.RUnlock()

	if secondCachedAt != cachedAt {
		t.Error("ruleFetchedAt changed on second EvaluateAll within TTL; expected cache hit (no DB round-trip)")
	}
}

// TestEngine_InvalidateRuleCache verifies that InvalidateRuleCache clears the
// cached rule list and that the next evaluation re-fetches from the database.
func TestEngine_InvalidateRuleCache(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	engine := NewEngine(svc, db, nil, nil, testLogger())

	a := &artist.Artist{ID: "inv-1", Name: "Invalidate Test", Path: t.TempDir()}

	// Populate the cache via a first evaluation.
	if _, err := engine.Evaluate(ctx, a); err != nil {
		t.Fatalf("first Evaluate: %v", err)
	}

	engine.ruleCacheMu.RLock()
	beforeInvalidate := engine.ruleFetchedAt
	engine.ruleCacheMu.RUnlock()

	if beforeInvalidate.IsZero() {
		t.Fatal("expected cache to be populated after first Evaluate")
	}

	// Invalidate the cache.
	engine.InvalidateRuleCache()

	engine.ruleCacheMu.RLock()
	afterInvalidate := engine.ruleList
	afterTime := engine.ruleFetchedAt
	engine.ruleCacheMu.RUnlock()

	if afterInvalidate != nil {
		t.Error("expected ruleList to be nil after InvalidateRuleCache")
	}
	if !afterTime.IsZero() {
		t.Error("expected ruleFetchedAt to be zero after InvalidateRuleCache")
	}

	// A subsequent evaluation must re-populate the cache.
	if _, err := engine.Evaluate(ctx, a); err != nil {
		t.Fatalf("second Evaluate after invalidation: %v", err)
	}

	engine.ruleCacheMu.RLock()
	afterReeval := engine.ruleList
	engine.ruleCacheMu.RUnlock()

	if len(afterReeval) == 0 {
		t.Error("expected ruleList to be repopulated after evaluation following invalidation")
	}
}

// TestCachedRules_ConcurrentAccess verifies that concurrent Evaluate calls
// from multiple goroutines do not cause data races or panics. All goroutines
// must receive a valid (non-empty) result.
func TestCachedRules_ConcurrentAccess(t *testing.T) {
	const numGoroutines = 20

	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Disable image rules that need disk access.
	for _, id := range []string{RuleThumbSquare, RuleThumbMinRes} {
		r, _ := svc.GetByID(ctx, id)
		r.Enabled = false
		if err := svc.Update(ctx, r); err != nil {
			t.Fatalf("disabling rule %s: %v", id, err)
		}
	}

	engine := NewEngine(svc, db, nil, nil, testLogger())

	artistDir := filepath.Join(t.TempDir(), "ConcurrentArtist")
	if err := os.MkdirAll(artistDir, 0o755); err != nil {
		t.Fatalf("creating artist dir: %v", err)
	}

	a := &artist.Artist{
		ID:   "concurrent-1",
		Name: "ConcurrentArtist",
		Path: artistDir,
	}

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	errs := make([]error, numGoroutines)
	results := make([]*EvaluationResult, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			r, err := engine.Evaluate(ctx, a)
			errs[i] = err
			results[i] = r
		}()
	}

	wg.Wait()

	for i := 0; i < numGoroutines; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: Evaluate returned error: %v", i, errs[i])
			continue
		}
		if results[i] == nil {
			t.Errorf("goroutine %d: Evaluate returned nil result", i)
			continue
		}
		if results[i].RulesTotal == 0 {
			t.Errorf("goroutine %d: RulesTotal = 0, expected enabled rules to be evaluated", i)
		}
	}
}
