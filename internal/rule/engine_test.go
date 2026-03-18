package rule

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
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

	engine := NewEngine(svc, db, nil, testLogger())

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
		Biography:     "Nirvana was an American rock band.",
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

	engine := NewEngine(svc, db, nil, testLogger())

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
	// Only extraneous_images and directory_name_mismatch pass (nothing extraneous,
	// dir name matches). Assert relative properties rather than exact counts so
	// adding new rules does not break this test.
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

	engine := NewEngine(svc, db, nil, testLogger())

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

	// Partial artist has NFO + MBID, so nfo_exists, nfo_has_mbid, extraneous_images,
	// and directory_name_mismatch should pass. Assert relative properties so adding
	// new rules does not break this test.
	if result.RulesPassed == 0 {
		t.Error("RulesPassed = 0, expected nfo_exists and nfo_has_mbid to pass")
	}
	if result.RulesPassed >= result.RulesTotal {
		t.Errorf("RulesPassed = %d, RulesTotal = %d, expected some violations (no images, no bio)", result.RulesPassed, result.RulesTotal)
	}
	if result.HealthScore <= 0 || result.HealthScore >= 100.0 {
		t.Errorf("HealthScore = %.1f, expected between 0 and 100 for partial compliance", result.HealthScore)
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

	engine := NewEngine(svc, db, nil, testLogger())

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

	engine := NewEngine(svc, db, nil, testLogger())

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

	engine := NewEngine(svc, db, nil, slog.Default())
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
