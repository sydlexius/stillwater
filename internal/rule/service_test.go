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

	if len(rules) != len(defaultRules) {
		t.Fatalf("expected %d rules, got %d", len(defaultRules), len(rules))
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
		RuleBioExists,
		RuleFanartMinRes, RuleFanartAspect,
		RuleLogoMinRes, RuleBannerExists, RuleBannerMinRes,
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
	if len(rules) != len(defaultRules) {
		t.Errorf("expected %d rules after double seed, got %d", len(defaultRules), len(rules))
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

func TestGetViolationByID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	rv := &RuleViolation{
		RuleID:     RuleNFOExists,
		ArtistID:   "artist-1",
		ArtistName: "Test Artist",
		Severity:   "error",
		Message:    "missing nfo",
		Fixable:    true,
		Status:     ViolationStatusOpen,
	}
	if err := svc.UpsertViolation(ctx, rv); err != nil {
		t.Fatalf("UpsertViolation: %v", err)
	}

	got, err := svc.GetViolationByID(ctx, rv.ID)
	if err != nil {
		t.Fatalf("GetViolationByID: %v", err)
	}
	if got.RuleID != rv.RuleID {
		t.Errorf("RuleID = %q, want %q", got.RuleID, rv.RuleID)
	}
	if got.ArtistName != rv.ArtistName {
		t.Errorf("ArtistName = %q, want %q", got.ArtistName, rv.ArtistName)
	}
}

func TestGetViolationByID_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	_, err := svc.GetViolationByID(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent violation")
	}
}

func TestListViolations_ActiveStatus(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	open := &RuleViolation{
		RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "A1",
		Severity: "error", Message: "open", Fixable: true, Status: ViolationStatusOpen,
	}
	pending := &RuleViolation{
		RuleID: RuleThumbExists, ArtistID: "a2", ArtistName: "A2",
		Severity: "warning", Message: "pending", Fixable: true,
		Status:     ViolationStatusPendingChoice,
		Candidates: []ImageCandidate{{URL: "http://example.com/img.jpg", Width: 500, Height: 500, Source: "test", ImageType: "thumb"}},
	}
	dismissed := &RuleViolation{
		RuleID: RuleFanartExists, ArtistID: "a3", ArtistName: "A3",
		Severity: "info", Message: "dismissed", Fixable: true, Status: ViolationStatusDismissed,
	}

	for _, v := range []*RuleViolation{open, pending, dismissed} {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	active, err := svc.ListViolations(ctx, "active")
	if err != nil {
		t.Fatalf("ListViolations(active): %v", err)
	}
	if len(active) != 2 {
		t.Errorf("active violations = %d, want 2 (open + pending_choice)", len(active))
	}
	for _, v := range active {
		if v.Status == ViolationStatusDismissed {
			t.Errorf("dismissed violation should not appear in active results")
		}
	}
}

func TestUpsertViolation_WithCandidates(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	candidates := []ImageCandidate{
		{URL: "http://example.com/fanart1.jpg", Width: 1920, Height: 1080, Source: "fanarttv", ImageType: "fanart"},
		{URL: "http://example.com/fanart2.jpg", Width: 3840, Height: 2160, Source: "fanarttv", ImageType: "fanart"},
	}
	rv := &RuleViolation{
		RuleID:     RuleFanartMinRes,
		ArtistID:   "artist-x",
		ArtistName: "Candidate Artist",
		Severity:   "warning",
		Message:    "low res fanart",
		Fixable:    true,
		Status:     ViolationStatusPendingChoice,
		Candidates: candidates,
	}
	if err := svc.UpsertViolation(ctx, rv); err != nil {
		t.Fatalf("UpsertViolation: %v", err)
	}

	got, err := svc.GetViolationByID(ctx, rv.ID)
	if err != nil {
		t.Fatalf("GetViolationByID: %v", err)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("Candidates len = %d, want 2", len(got.Candidates))
	}
	if got.Candidates[0].Width != 1920 {
		t.Errorf("Candidates[0].Width = %d, want 1920", got.Candidates[0].Width)
	}
	if got.Candidates[1].Source != "fanarttv" {
		t.Errorf("Candidates[1].Source = %q, want fanarttv", got.Candidates[1].Source)
	}
}

func TestMarshalUnmarshalCandidates(t *testing.T) {
	cs := []ImageCandidate{
		{URL: "http://example.com/img.jpg", Width: 800, Height: 600, Source: "test", ImageType: "thumb"},
	}
	s := marshalCandidates(cs)
	if s == "" || s == "[]" {
		t.Fatal("marshalCandidates returned empty for non-empty slice")
	}
	got := unmarshalCandidates(s)
	if len(got) != 1 {
		t.Fatalf("unmarshalCandidates returned %d items, want 1", len(got))
	}
	if got[0].URL != cs[0].URL {
		t.Errorf("URL = %q, want %q", got[0].URL, cs[0].URL)
	}
	if got[0].Width != 800 {
		t.Errorf("Width = %d, want 800", got[0].Width)
	}

	// Empty cases
	if got2 := unmarshalCandidates(""); len(got2) != 0 {
		t.Errorf("unmarshalCandidates(\"\") = %v, want empty", got2)
	}
	if got3 := unmarshalCandidates("[]"); len(got3) != 0 {
		t.Errorf("unmarshalCandidates(\"[]\") = %v, want empty", got3)
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
