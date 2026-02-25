package rule

import (
	"context"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
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

	a := &artist.Artist{
		ID:            "test-1",
		Name:          "Nirvana",
		MusicBrainzID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da",
		NFOExists:     true,
		ThumbExists:   true,
		FanartExists:  true,
		LogoExists:    true,
		Biography:     "Nirvana was an American rock band.",
		Path:          t.TempDir(),
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

	a := &artist.Artist{
		ID:   "test-2",
		Name: "Empty Artist",
		Path: t.TempDir(),
	}

	result, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// Should fail 6 of 7 remaining enabled rules (extraneous_images passes on empty dir)
	if result.RulesTotal != 7 {
		t.Errorf("RulesTotal = %d, want 7", result.RulesTotal)
	}
	if result.RulesPassed != 1 {
		t.Errorf("RulesPassed = %d, want 1", result.RulesPassed)
	}
	expectedScore := 14.3 // 1/7 * 100, rounded to 1 decimal
	if result.HealthScore != expectedScore {
		t.Errorf("HealthScore = %.1f, want %.1f", result.HealthScore, expectedScore)
	}
	if len(result.Violations) != 6 {
		t.Errorf("Violations = %d, want 6", len(result.Violations))
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
	a := &artist.Artist{
		ID:            "test-3",
		Name:          "Partial",
		MusicBrainzID: "abc-123",
		NFOExists:     true,
		Path:          t.TempDir(),
	}

	result, err := engine.Evaluate(ctx, a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// Passes: nfo_exists, nfo_has_mbid, extraneous_images (3 of 7)
	if result.RulesPassed != 3 {
		t.Errorf("RulesPassed = %d, want 3", result.RulesPassed)
	}
	if result.RulesTotal != 7 {
		t.Errorf("RulesTotal = %d, want 7", result.RulesTotal)
	}

	expectedScore := 42.9 // 3/7 * 100, rounded to 1 decimal
	if result.HealthScore != expectedScore {
		t.Errorf("HealthScore = %.1f, want %.1f", result.HealthScore, expectedScore)
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
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := NewService(db)
	if err := svc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	engine := NewEngine(svc, db, nil, slog.Default())
	if engine == nil {
		t.Fatal("expected non-nil engine")
	}

	// Verify all checkers are registered (8 core + 5 image quality + 1 extraneous = 14)
	if len(engine.checkers) != 14 {
		t.Errorf("expected 14 checkers, got %d", len(engine.checkers))
	}
}
