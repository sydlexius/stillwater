package rule

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
)

func TestNFOFixer_CanFix(t *testing.T) {
	f := &NFOFixer{}

	if !f.CanFix(&Violation{RuleID: RuleNFOExists}) {
		t.Error("NFOFixer should handle nfo_exists")
	}
	if f.CanFix(&Violation{RuleID: RuleThumbExists}) {
		t.Error("NFOFixer should not handle thumb_exists")
	}
}

func TestNFOFixer_Fix(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{
		Name:          "Test Artist",
		SortName:      "Test Artist",
		Path:          dir,
		MusicBrainzID: "abc-123",
	}

	f := &NFOFixer{}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleNFOExists})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !fr.Fixed {
		t.Errorf("Fixed = false, want true")
	}
	if fr.RuleID != RuleNFOExists {
		t.Errorf("RuleID = %q, want %q", fr.RuleID, RuleNFOExists)
	}

	// Verify the NFO was created
	nfoPath := filepath.Join(dir, "artist.nfo")
	data, err := os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("nfo file is empty")
	}

	// Verify the NFO content is valid
	parsed, err := nfo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing created nfo: %v", err)
	}
	if parsed.Name != "Test Artist" {
		t.Errorf("nfo Name = %q, want %q", parsed.Name, "Test Artist")
	}
	if parsed.MusicBrainzArtistID != "abc-123" {
		t.Errorf("nfo MBID = %q, want %q", parsed.MusicBrainzArtistID, "abc-123")
	}

	// Verify artist flag was updated
	if !a.NFOExists {
		t.Error("artist.NFOExists should be true after fix")
	}
}

func TestMetadataFixer_CanFix(t *testing.T) {
	f := &MetadataFixer{}

	if !f.CanFix(&Violation{RuleID: RuleNFOHasMBID}) {
		t.Error("MetadataFixer should handle nfo_has_mbid")
	}
	if !f.CanFix(&Violation{RuleID: RuleBioExists}) {
		t.Error("MetadataFixer should handle bio_exists")
	}
	if f.CanFix(&Violation{RuleID: RuleThumbExists}) {
		t.Error("MetadataFixer should not handle thumb_exists")
	}
}

func TestImageFixer_CanFix(t *testing.T) {
	f := &ImageFixer{}

	for _, ruleID := range []string{RuleThumbExists, RuleFanartExists, RuleLogoExists, RuleThumbSquare, RuleThumbMinRes} {
		if !f.CanFix(&Violation{RuleID: ruleID}) {
			t.Errorf("ImageFixer should handle %s", ruleID)
		}
	}

	if f.CanFix(&Violation{RuleID: RuleNFOExists}) {
		t.Error("ImageFixer should not handle nfo_exists")
	}
}

func TestImageFixer_Fix_NoMBID(t *testing.T) {
	f := &ImageFixer{}
	a := &artist.Artist{Name: "Test", MusicBrainzID: ""}

	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleThumbExists})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("should not fix when MBID is empty")
	}
}

func TestRuleToImageType(t *testing.T) {
	tests := []struct {
		ruleID string
		want   string
	}{
		{RuleThumbExists, "thumb"},
		{RuleThumbSquare, "thumb"},
		{RuleThumbMinRes, "thumb"},
		{RuleFanartExists, "fanart"},
		{RuleLogoExists, "logo"},
		{RuleNFOExists, ""},
		{RuleBioExists, ""},
	}

	for _, tt := range tests {
		if got := ruleToImageType(tt.ruleID); got != tt.want {
			t.Errorf("ruleToImageType(%q) = %q, want %q", tt.ruleID, got, tt.want)
		}
	}
}

func TestSetImageFlag(t *testing.T) {
	a := &artist.Artist{}

	setImageFlag(a, "thumb")
	if !a.ThumbExists {
		t.Error("ThumbExists should be true")
	}

	setImageFlag(a, "fanart")
	if !a.FanartExists {
		t.Error("FanartExists should be true")
	}

	setImageFlag(a, "logo")
	if !a.LogoExists {
		t.Error("LogoExists should be true")
	}

	setImageFlag(a, "banner")
	if !a.BannerExists {
		t.Error("BannerExists should be true")
	}
}

func TestImageFixer_CanFix_NewRules(t *testing.T) {
	f := &ImageFixer{}

	newRules := []string{
		RuleFanartMinRes, RuleFanartAspect, RuleLogoMinRes, RuleBannerExists, RuleBannerMinRes,
	}
	for _, ruleID := range newRules {
		if !f.CanFix(&Violation{RuleID: ruleID}) {
			t.Errorf("ImageFixer should handle %s", ruleID)
		}
	}
}

func TestRuleToImageType_NewRules(t *testing.T) {
	tests := []struct {
		ruleID string
		want   string
	}{
		{RuleFanartMinRes, "fanart"},
		{RuleFanartAspect, "fanart"},
		{RuleLogoMinRes, "logo"},
		{RuleBannerExists, "banner"},
		{RuleBannerMinRes, "banner"},
	}
	for _, tt := range tests {
		if got := ruleToImageType(tt.ruleID); got != tt.want {
			t.Errorf("ruleToImageType(%q) = %q, want %q", tt.ruleID, got, tt.want)
		}
	}
}

func TestPipeline_PendingChoiceViolation(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{
		Name:     "Candidate Test",
		SortName: "Candidate Test",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	candidates := []ImageCandidate{
		{URL: "http://example.com/img1.jpg", Width: 1920, Height: 1080, Source: "prov", ImageType: "fanart"},
		{URL: "http://example.com/img2.jpg", Width: 3840, Height: 2160, Source: "prov", ImageType: "fanart"},
	}
	// Fixer returns multiple candidates (pending_choice)
	fixer := &mockFixer{
		canFix: true,
		result: &FixResult{
			RuleID:     RuleNFOExists,
			Fixed:      false,
			Message:    "multiple candidates",
			Candidates: candidates,
		},
	}

	engine := NewEngine(ruleSvc, db, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunAll(ctx)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if result.FixesSucceeded != 0 {
		t.Errorf("FixesSucceeded = %d, want 0 (pending_choice)", result.FixesSucceeded)
	}

	// Verify violation was persisted as pending_choice with candidates
	violations, err := ruleSvc.ListViolations(ctx, ViolationStatusPendingChoice)
	if err != nil {
		t.Fatalf("ListViolations: %v", err)
	}
	found := false
	for _, v := range violations {
		if v.ArtistID == a.ID {
			found = true
			if v.Status != ViolationStatusPendingChoice {
				t.Errorf("status = %q, want %q", v.Status, ViolationStatusPendingChoice)
			}
			if len(v.Candidates) != 2 {
				t.Errorf("Candidates len = %d, want 2", len(v.Candidates))
			}
		}
	}
	if !found {
		t.Error("expected pending_choice violation for artist, none found")
	}
}

// mockFixer is a test helper that records calls.
type mockFixer struct {
	canFix bool
	result *FixResult
	err    error
	calls  int
}

func (m *mockFixer) CanFix(_ *Violation) bool {
	return m.canFix
}

func (m *mockFixer) Fix(_ context.Context, _ *artist.Artist, v *Violation) (*FixResult, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	if m.result != nil {
		return m.result, nil
	}
	return &FixResult{RuleID: v.RuleID, Fixed: true, Message: "mock fixed"}, nil
}

func TestPipeline_RunAll(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Create a test artist with some violations
	a := &artist.Artist{
		Name:     "Pipeline Test",
		SortName: "Pipeline Test",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, testLogger())
	fixer := &mockFixer{canFix: true}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunAll(ctx)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	if result.ArtistsProcessed != 1 {
		t.Errorf("ArtistsProcessed = %d, want 1", result.ArtistsProcessed)
	}
	if result.ViolationsFound == 0 {
		t.Error("expected at least one violation for empty artist")
	}
	if fixer.calls == 0 {
		t.Error("expected fixer to be called at least once")
	}
}

func TestPipeline_RunRule(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{
		Name:     "Rule Test",
		SortName: "Rule Test",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, testLogger())
	fixer := &mockFixer{canFix: true}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunRule(ctx, RuleNFOExists)
	if err != nil {
		t.Fatalf("RunRule: %v", err)
	}

	if result.ArtistsProcessed != 1 {
		t.Errorf("ArtistsProcessed = %d, want 1", result.ArtistsProcessed)
	}
	if result.ViolationsFound != 1 {
		t.Errorf("ViolationsFound = %d, want 1 (nfo_exists)", result.ViolationsFound)
	}
	if result.FixesAttempted != 1 {
		t.Errorf("FixesAttempted = %d, want 1", result.FixesAttempted)
	}
}

func TestPipeline_SkipsExcludedArtists(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{
		Name:            "Various Artists",
		SortName:        "Various Artists",
		Path:            t.TempDir(),
		IsExcluded:      true,
		ExclusionReason: "default exclusion list",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, testLogger())
	fixer := &mockFixer{canFix: true}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunAll(ctx)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	if result.ArtistsProcessed != 0 {
		t.Errorf("ArtistsProcessed = %d, want 0 (excluded)", result.ArtistsProcessed)
	}
}

func TestPipeline_NoFixerAvailable(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{
		Name:     "No Fixer",
		SortName: "No Fixer",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, testLogger())
	// Register a fixer that can't fix anything
	fixer := &mockFixer{canFix: false}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, testLogger())

	result, err := pipeline.RunAll(ctx)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	for _, fr := range result.Results {
		if fr.Fixed {
			t.Errorf("no fix should succeed when fixer.CanFix is false")
		}
		if fr.Message != "no fixer available" {
			t.Errorf("Message = %q, want 'no fixer available'", fr.Message)
		}
	}
}

func TestWriteArtistNFO(t *testing.T) {
	dir := t.TempDir()
	a := &artist.Artist{
		Name:          "WriteTest",
		SortName:      "WriteTest",
		Path:          dir,
		MusicBrainzID: "test-mbid",
	}

	writeArtistNFO(a, nil)

	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("nfo file is empty")
	}

	parsed, err := nfo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing nfo: %v", err)
	}
	if parsed.MusicBrainzArtistID != "test-mbid" {
		t.Errorf("MBID = %q, want 'test-mbid'", parsed.MusicBrainzArtistID)
	}
}
