package rule

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/database"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSeedDefaults(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	rules, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(rules) != 9 {
		t.Fatalf("expected 9 rules, got %d", len(rules))
	}

	// Verify known rule IDs exist
	ids := make(map[string]bool)
	for _, r := range rules {
		ids[r.ID] = true
	}

	expected := []string{
		RuleNFOExists, RuleNFOHasMBID,
		RuleThumbExists, RuleThumbSquare, RuleThumbMinRes,
		RuleFanartExists, RuleLogoExists,
		RuleBioExists, RuleFallbackUsed,
	}
	for _, id := range expected {
		if !ids[id] {
			t.Errorf("missing rule %q", id)
		}
	}
}

func TestSeedDefaults_Idempotent(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("first SeedDefaults: %v", err)
	}
	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("second SeedDefaults: %v", err)
	}

	rules, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rules) != 9 {
		t.Errorf("expected 9 rules after double seed, got %d", len(rules))
	}
}

func TestGetByID(t *testing.T) {
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
	if r.Name != "NFO file exists" {
		t.Errorf("Name = %q, want %q", r.Name, "NFO file exists")
	}
	if r.Category != "nfo" {
		t.Errorf("Category = %q, want %q", r.Category, "nfo")
	}
	if !r.Enabled {
		t.Error("expected Enabled to be true")
	}
}

func TestGetByID_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	_, err := svc.GetByID(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent rule")
	}
}

func TestUpdate(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	r, err := svc.GetByID(ctx, RuleThumbMinRes)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	// Disable and change config
	r.Enabled = false
	r.Config.MinWidth = 1000
	r.Config.MinHeight = 1000

	if err := svc.Update(ctx, r); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := svc.GetByID(ctx, RuleThumbMinRes)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.Enabled {
		t.Error("expected Enabled to be false after update")
	}
	if got.Config.MinWidth != 1000 {
		t.Errorf("MinWidth = %d, want 1000", got.Config.MinWidth)
	}
	if got.Config.MinHeight != 1000 {
		t.Errorf("MinHeight = %d, want 1000", got.Config.MinHeight)
	}
}

func TestRecordHealthSnapshot(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.RecordHealthSnapshot(ctx, 100, 75, 75.0); err != nil {
		t.Fatalf("RecordHealthSnapshot: %v", err)
	}

	// Verify the row was inserted
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM health_history").Scan(&count); err != nil {
		t.Fatalf("counting health_history: %v", err)
	}
	if count != 1 {
		t.Errorf("health_history count = %d, want 1", count)
	}
}

func TestGetHealthHistory(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Insert multiple snapshots
	if err := svc.RecordHealthSnapshot(ctx, 100, 50, 50.0); err != nil {
		t.Fatalf("recording snapshot 1: %v", err)
	}
	if err := svc.RecordHealthSnapshot(ctx, 100, 75, 75.0); err != nil {
		t.Fatalf("recording snapshot 2: %v", err)
	}

	history, err := svc.GetHealthHistory(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetHealthHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(history))
	}

	// Verify ascending order by recorded_at
	if !history[0].RecordedAt.Before(history[1].RecordedAt) && history[0].RecordedAt != history[1].RecordedAt {
		t.Error("expected snapshots ordered by recorded_at ASC")
	}
}

func TestGetHealthHistory_Empty(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	history, err := svc.GetHealthHistory(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetHealthHistory: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected 0 snapshots, got %d", len(history))
	}
}

func TestGetLatestHealthSnapshot(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.RecordHealthSnapshot(ctx, 100, 50, 50.0); err != nil {
		t.Fatalf("recording snapshot 1: %v", err)
	}
	if err := svc.RecordHealthSnapshot(ctx, 100, 80, 80.0); err != nil {
		t.Fatalf("recording snapshot 2: %v", err)
	}

	latest, err := svc.GetLatestHealthSnapshot(ctx)
	if err != nil {
		t.Fatalf("GetLatestHealthSnapshot: %v", err)
	}
	if latest == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if latest.Score != 80.0 {
		t.Errorf("expected score 80.0, got %v", latest.Score)
	}
}

func TestGetLatestHealthSnapshot_Empty(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	latest, err := svc.GetLatestHealthSnapshot(ctx)
	if err != nil {
		t.Fatalf("GetLatestHealthSnapshot: %v", err)
	}
	if latest != nil {
		t.Errorf("expected nil for empty table, got %v", latest)
	}
}

func TestList_OrderedByCategoryAndName(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	rules, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Verify ordering: image rules first, then metadata, then nfo
	// (alphabetical by category, then by name within category)
	prevCategory := ""
	prevName := ""
	for _, r := range rules {
		if r.Category < prevCategory {
			t.Errorf("rules not ordered by category: %q came after %q", r.Category, prevCategory)
		}
		if r.Category == prevCategory && r.Name < prevName {
			t.Errorf("rules not ordered by name within category: %q came after %q", r.Name, prevName)
		}
		prevCategory = r.Category
		prevName = r.Name
	}
}
