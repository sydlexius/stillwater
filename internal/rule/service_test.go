package rule

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/database"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	src, err := os.ReadFile(templateDBPath)
	if err != nil {
		t.Fatalf("reading template db: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "test.db")
	if err := os.WriteFile(dst, src, 0o600); err != nil {
		t.Fatalf("writing test db: %v", err)
	}
	db, err := database.Open(dst)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
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

func TestCountActiveViolationsBySeverity(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Insert violations with different severities and statuses.
	violations := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "A1", Severity: "error", Message: "m1", Fixable: true, Status: ViolationStatusOpen},
		{RuleID: RuleThumbExists, ArtistID: "a1", ArtistName: "A1", Severity: "error", Message: "m2", Fixable: true, Status: ViolationStatusPendingChoice,
			Candidates: []ImageCandidate{{URL: "http://example.com/img.jpg", Width: 500, Height: 500, Source: "test", ImageType: "thumb"}}},
		{RuleID: RuleFanartExists, ArtistID: "a2", ArtistName: "A2", Severity: "warning", Message: "m3", Fixable: true, Status: ViolationStatusOpen},
		{RuleID: RuleNFOHasMBID, ArtistID: "a3", ArtistName: "A3", Severity: "info", Message: "m4", Fixable: true, Status: ViolationStatusOpen},
		// Resolved and dismissed should NOT be counted.
		{RuleID: RuleThumbSquare, ArtistID: "a4", ArtistName: "A4", Severity: "error", Message: "m5", Fixable: true, Status: ViolationStatusResolved},
		{RuleID: RuleExtraneousImages, ArtistID: "a5", ArtistName: "A5", Severity: "warning", Message: "m6", Fixable: true, Status: ViolationStatusDismissed},
	}
	for _, v := range violations {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	counts, err := svc.CountActiveViolationsBySeverity(ctx)
	if err != nil {
		t.Fatalf("CountActiveViolationsBySeverity: %v", err)
	}

	if counts["error"] != 2 {
		t.Errorf("error count = %d, want 2", counts["error"])
	}
	if counts["warning"] != 1 {
		t.Errorf("warning count = %d, want 1", counts["warning"])
	}
	if counts["info"] != 1 {
		t.Errorf("info count = %d, want 1", counts["info"])
	}

	// With no active violations (empty DB), all counts should be zero.
	db2 := setupTestDB(t)
	svc2 := NewService(db2)
	counts2, err := svc2.CountActiveViolationsBySeverity(ctx)
	if err != nil {
		t.Fatalf("CountActiveViolationsBySeverity (empty): %v", err)
	}
	for _, sev := range []string{"error", "warning", "info"} {
		if counts2[sev] != 0 {
			t.Errorf("%s count = %d, want 0 (empty DB)", sev, counts2[sev])
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

func TestListViolationsFiltered_DefaultSort(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// Insert violations with different severities
	violations := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "Charlie", Severity: "info", Message: "m1", Status: ViolationStatusOpen},
		{RuleID: RuleThumbExists, ArtistID: "a2", ArtistName: "Alice", Severity: "error", Message: "m2", Status: ViolationStatusOpen},
		{RuleID: RuleFanartExists, ArtistID: "a3", ArtistName: "Bob", Severity: "warning", Message: "m3", Status: ViolationStatusOpen},
	}
	for _, v := range violations {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	// Default sort should return errors first, then warning, then info
	result, err := svc.ListViolationsFiltered(ctx, ViolationListParams{Status: "active"})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 violations, got %d", len(result))
	}
	if result[0].Severity != "error" {
		t.Errorf("result[0].Severity = %q, want %q", result[0].Severity, "error")
	}
	if result[1].Severity != "warning" {
		t.Errorf("result[1].Severity = %q, want %q", result[1].Severity, "warning")
	}
	if result[2].Severity != "info" {
		t.Errorf("result[2].Severity = %q, want %q", result[2].Severity, "info")
	}
}

func TestListViolationsFiltered_ExplicitSeveritySort(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	violations := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "Charlie", Severity: "info", Message: "m1", Status: ViolationStatusOpen},
		{RuleID: RuleThumbExists, ArtistID: "a2", ArtistName: "Alice", Severity: "error", Message: "m2", Status: ViolationStatusOpen},
		{RuleID: RuleFanartExists, ArtistID: "a3", ArtistName: "Bob", Severity: "warning", Message: "m3", Status: ViolationStatusOpen},
	}
	for _, v := range violations {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	// Explicit sort=severity DESC should order error > warning > info
	result, err := svc.ListViolationsFiltered(ctx, ViolationListParams{Status: "active", Sort: "severity", Order: "desc"})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 violations, got %d", len(result))
	}
	if result[0].Severity != "error" {
		t.Errorf("DESC result[0].Severity = %q, want %q", result[0].Severity, "error")
	}
	if result[2].Severity != "info" {
		t.Errorf("DESC result[2].Severity = %q, want %q", result[2].Severity, "info")
	}

	// Explicit sort=severity ASC should order info > warning > error
	result, err = svc.ListViolationsFiltered(ctx, ViolationListParams{Status: "active", Sort: "severity", Order: "asc"})
	if err != nil {
		t.Fatalf("ListViolationsFiltered ASC: %v", err)
	}
	if result[0].Severity != "info" {
		t.Errorf("ASC result[0].Severity = %q, want %q", result[0].Severity, "info")
	}
	if result[2].Severity != "error" {
		t.Errorf("ASC result[2].Severity = %q, want %q", result[2].Severity, "error")
	}
}

func TestListViolationsFiltered_SortByArtistName(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	violations := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "Charlie", Severity: "error", Message: "m1", Status: ViolationStatusOpen},
		{RuleID: RuleThumbExists, ArtistID: "a2", ArtistName: "Alice", Severity: "warning", Message: "m2", Status: ViolationStatusOpen},
		{RuleID: RuleFanartExists, ArtistID: "a3", ArtistName: "Bob", Severity: "info", Message: "m3", Status: ViolationStatusOpen},
	}
	for _, v := range violations {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	result, err := svc.ListViolationsFiltered(ctx, ViolationListParams{
		Status: "active",
		Sort:   "artist_name",
		Order:  "asc",
	})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 violations, got %d", len(result))
	}
	if result[0].ArtistName != "Alice" {
		t.Errorf("first artist = %q, want Alice", result[0].ArtistName)
	}
	if result[1].ArtistName != "Bob" {
		t.Errorf("second artist = %q, want Bob", result[1].ArtistName)
	}
	if result[2].ArtistName != "Charlie" {
		t.Errorf("third artist = %q, want Charlie", result[2].ArtistName)
	}
}

func TestListViolationsFiltered_FilterBySeverity(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	violations := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "A1", Severity: "error", Message: "m1", Status: ViolationStatusOpen},
		{RuleID: RuleThumbExists, ArtistID: "a2", ArtistName: "A2", Severity: "warning", Message: "m2", Status: ViolationStatusOpen},
		{RuleID: RuleFanartExists, ArtistID: "a3", ArtistName: "A3", Severity: "error", Message: "m3", Status: ViolationStatusOpen},
	}
	for _, v := range violations {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	result, err := svc.ListViolationsFiltered(ctx, ViolationListParams{
		Status:   "active",
		Severity: "error",
	})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 error violations, got %d", len(result))
	}
	for _, v := range result {
		if v.Severity != "error" {
			t.Errorf("severity = %q, want error", v.Severity)
		}
	}
}

func TestListViolationsFiltered_FilterByCategory(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	violations := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "A1", Severity: "error", Message: "m1", Status: ViolationStatusOpen},
		{RuleID: RuleThumbExists, ArtistID: "a2", ArtistName: "A2", Severity: "warning", Message: "m2", Status: ViolationStatusOpen},
		{RuleID: RuleBioExists, ArtistID: "a3", ArtistName: "A3", Severity: "info", Message: "m3", Status: ViolationStatusOpen},
	}
	for _, v := range violations {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	// Filter by "nfo" category (only nfo_exists should match)
	result, err := svc.ListViolationsFiltered(ctx, ViolationListParams{
		Status:   "active",
		Category: "nfo",
	})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 nfo violation, got %d", len(result))
	}
	if result[0].RuleID != RuleNFOExists {
		t.Errorf("rule_id = %q, want %q", result[0].RuleID, RuleNFOExists)
	}
}

func TestListViolationsFiltered_FilterByRuleID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	violations := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "A1", Severity: "error", Message: "m1", Status: ViolationStatusOpen},
		{RuleID: RuleThumbExists, ArtistID: "a2", ArtistName: "A2", Severity: "warning", Message: "m2", Status: ViolationStatusOpen},
	}
	for _, v := range violations {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	result, err := svc.ListViolationsFiltered(ctx, ViolationListParams{
		Status: "active",
		RuleID: RuleThumbExists,
	})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(result))
	}
	if result[0].RuleID != RuleThumbExists {
		t.Errorf("rule_id = %q, want %q", result[0].RuleID, RuleThumbExists)
	}
}

func TestListViolationsFiltered_ActiveStatus(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	violations := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "A1", Severity: "error", Message: "m1", Status: ViolationStatusOpen},
		{RuleID: RuleThumbExists, ArtistID: "a2", ArtistName: "A2", Severity: "warning", Message: "m2", Status: ViolationStatusDismissed},
		{RuleID: RuleFanartExists, ArtistID: "a3", ArtistName: "A3", Severity: "info", Message: "m3", Status: ViolationStatusPendingChoice,
			Candidates: []ImageCandidate{{URL: "http://example.com/img.jpg", Width: 500, Height: 500, Source: "test", ImageType: "thumb"}}},
	}
	for _, v := range violations {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	result, err := svc.ListViolationsFiltered(ctx, ViolationListParams{Status: "active"})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 active violations, got %d", len(result))
	}
	for _, v := range result {
		if v.Status == ViolationStatusDismissed {
			t.Error("dismissed violation should not appear in active results")
		}
	}
}

func TestListViolationsFiltered_CombinedFilters(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	violations := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: "a1", ArtistName: "A1", Severity: "error", Message: "m1", Status: ViolationStatusOpen},
		{RuleID: RuleNFOHasMBID, ArtistID: "a2", ArtistName: "A2", Severity: "warning", Message: "m2", Status: ViolationStatusOpen},
		{RuleID: RuleThumbExists, ArtistID: "a3", ArtistName: "A3", Severity: "error", Message: "m3", Status: ViolationStatusOpen},
	}
	for _, v := range violations {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	// Filter by category=nfo AND severity=error
	result, err := svc.ListViolationsFiltered(ctx, ViolationListParams{
		Status:   "active",
		Category: "nfo",
		Severity: "error",
	})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 violation (nfo + error), got %d", len(result))
	}
	if result[0].RuleID != RuleNFOExists {
		t.Errorf("rule_id = %q, want %q", result[0].RuleID, RuleNFOExists)
	}
}

func TestListViolationsFiltered_InvalidSort(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	// An invalid sort column should fall back to default (no SQL injection)
	_, err := svc.ListViolationsFiltered(ctx, ViolationListParams{
		Sort: "DROP TABLE rule_violations; --",
	})
	if err != nil {
		t.Fatalf("ListViolationsFiltered with invalid sort should not error: %v", err)
	}
}

func TestGroupViolations_ByArtist(t *testing.T) {
	violations := []RuleViolation{
		{ArtistID: "a1", ArtistName: "Alice", RuleID: RuleNFOExists, Severity: "error"},
		{ArtistID: "a2", ArtistName: "Bob", RuleID: RuleThumbExists, Severity: "warning"},
		{ArtistID: "a1", ArtistName: "Alice", RuleID: RuleFanartExists, Severity: "info"},
	}

	groups := GroupViolations(violations, "artist")
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// First group should be Alice (a1) with 2 violations
	if groups[0].Key != "a1" {
		t.Errorf("first group key = %q, want a1", groups[0].Key)
	}
	if groups[0].Label != "Alice" {
		t.Errorf("first group label = %q, want Alice", groups[0].Label)
	}
	if groups[0].Count != 2 {
		t.Errorf("first group count = %d, want 2", groups[0].Count)
	}
	if groups[1].Key != "a2" {
		t.Errorf("second group key = %q, want a2", groups[1].Key)
	}
	if groups[1].Count != 1 {
		t.Errorf("second group count = %d, want 1", groups[1].Count)
	}
}

func TestGroupViolations_BySeverity(t *testing.T) {
	violations := []RuleViolation{
		{ArtistID: "a1", ArtistName: "A1", RuleID: RuleNFOExists, Severity: "error"},
		{ArtistID: "a2", ArtistName: "A2", RuleID: RuleThumbExists, Severity: "warning"},
		{ArtistID: "a3", ArtistName: "A3", RuleID: RuleFanartExists, Severity: "error"},
	}

	groups := GroupViolations(violations, "severity")
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0].Key != "error" {
		t.Errorf("first group key = %q, want error", groups[0].Key)
	}
	if groups[0].Count != 2 {
		t.Errorf("error group count = %d, want 2", groups[0].Count)
	}
	if groups[1].Key != "warning" {
		t.Errorf("second group key = %q, want warning", groups[1].Key)
	}
}

func TestGroupViolations_ByRule(t *testing.T) {
	violations := []RuleViolation{
		{ArtistID: "a1", ArtistName: "A1", RuleID: RuleNFOExists, Severity: "error"},
		{ArtistID: "a2", ArtistName: "A2", RuleID: RuleNFOExists, Severity: "error"},
		{ArtistID: "a3", ArtistName: "A3", RuleID: RuleThumbExists, Severity: "warning"},
	}

	groups := GroupViolations(violations, "rule")
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0].Key != RuleNFOExists {
		t.Errorf("first group key = %q, want %q", groups[0].Key, RuleNFOExists)
	}
	if groups[0].Count != 2 {
		t.Errorf("first group count = %d, want 2", groups[0].Count)
	}
}

func TestGroupViolations_ByCategory(t *testing.T) {
	violations := []RuleViolation{
		{ArtistID: "a1", ArtistName: "A1", RuleID: RuleNFOExists, Severity: "error"},
		{ArtistID: "a2", ArtistName: "A2", RuleID: RuleThumbExists, Severity: "warning"},
		{ArtistID: "a3", ArtistName: "A3", RuleID: RuleBioExists, Severity: "info"},
		{ArtistID: "a4", ArtistName: "A4", RuleID: RuleFanartExists, Severity: "warning"},
	}

	groups := GroupViolations(violations, "category")
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups (nfo, image, metadata), got %d", len(groups))
	}

	// Build a map for easier assertion
	byKey := make(map[string]ViolationGroup)
	for _, g := range groups {
		byKey[g.Key] = g
	}
	if byKey["nfo"].Count != 1 {
		t.Errorf("nfo group count = %d, want 1", byKey["nfo"].Count)
	}
	if byKey["image"].Count != 2 {
		t.Errorf("image group count = %d, want 2", byKey["image"].Count)
	}
	if byKey["metadata"].Count != 1 {
		t.Errorf("metadata group count = %d, want 1", byKey["metadata"].Count)
	}
}

func TestGroupViolations_Empty(t *testing.T) {
	groups := GroupViolations(nil, "artist")
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for nil violations, got %d", len(groups))
	}
}

func TestGroupViolations_NoGroupBy(t *testing.T) {
	violations := []RuleViolation{
		{ArtistID: "a1", ArtistName: "A1", RuleID: RuleNFOExists, Severity: "error"},
		{ArtistID: "a2", ArtistName: "A2", RuleID: RuleThumbExists, Severity: "warning"},
	}

	groups := GroupViolations(violations, "")
	if len(groups) != 1 {
		t.Fatalf("expected 1 group for empty groupBy, got %d", len(groups))
	}
	if groups[0].Count != 2 {
		t.Errorf("group count = %d, want 2", groups[0].Count)
	}
	if groups[0].Key != "all" {
		t.Errorf("group key = %q, want all", groups[0].Key)
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
