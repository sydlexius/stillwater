package rule

import (
	"context"
	"errors"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// stubOriginOrchestrator is a test-only metadataOrchestrator that returns a
// fixed set of per-provider origin field results. Only FetchFieldFromProviders
// is exercised by the origin_missing fixer; the other methods are unused here
// and return zero values.
type stubOriginOrchestrator struct {
	fieldResults []provider.FieldProviderResult
	fieldErr     error
}

func (s *stubOriginOrchestrator) Search(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}

func (s *stubOriginOrchestrator) FetchMetadata(_ context.Context, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	return nil, nil
}

func (s *stubOriginOrchestrator) FetchFieldFromProviders(_ context.Context, _, _, field string, _ map[provider.ProviderName]string) ([]provider.FieldProviderResult, error) {
	if field != "origin" {
		return nil, errors.New("unexpected field: " + field)
	}
	return s.fieldResults, s.fieldErr
}

func TestMetadataFixer_CanFix_OriginMissing(t *testing.T) {
	f := &MetadataFixer{}
	if !f.CanFix(&Violation{RuleID: RuleOriginMissing}) {
		t.Error("MetadataFixer should handle origin_missing")
	}
}

// TestMetadataFixer_FixOrigin_AppliesFirstNonEmpty verifies auto-mode
// remediation: FetchFieldFromProviders returns results in priority order, and
// the fixer applies the first non-empty value, skipping providers that
// returned no data.
func TestMetadataFixer_FixOrigin_AppliesFirstNonEmpty(t *testing.T) {
	stub := &stubOriginOrchestrator{
		fieldResults: []provider.FieldProviderResult{
			{Provider: provider.NameWikipedia, HasData: false},
			{Provider: provider.NameAudioDB, Value: "Mandeville, Louisiana", HasData: true},
			{Provider: provider.NameWikidata, Value: "United States", HasData: true},
		},
	}
	f := &MetadataFixer{orchestrator: stub, logger: testLogger()}

	a := &artist.Artist{Name: "Test Artist", Origin: ""}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleOriginMissing})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !fr.Fixed {
		t.Fatalf("expected Fixed=true, got false (message: %q)", fr.Message)
	}
	if a.Origin != "Mandeville, Louisiana" {
		t.Errorf("a.Origin = %q, want %q", a.Origin, "Mandeville, Louisiana")
	}
}

// TestMetadataFixer_FixOrigin_NoData verifies the fixer reports Fixed=false
// (not an error) when no provider returns a usable origin value.
func TestMetadataFixer_FixOrigin_NoData(t *testing.T) {
	stub := &stubOriginOrchestrator{
		fieldResults: []provider.FieldProviderResult{
			{Provider: provider.NameWikipedia, HasData: false},
			{Provider: provider.NameAudioDB, Value: "   ", HasData: true},
		},
	}
	f := &MetadataFixer{orchestrator: stub, logger: testLogger()}

	a := &artist.Artist{Name: "Test Artist", Origin: ""}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleOriginMissing})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr.Fixed {
		t.Error("expected Fixed=false when no provider returned a non-empty origin")
	}
	if a.Origin != "" {
		t.Errorf("a.Origin = %q, want empty (no value should be applied)", a.Origin)
	}
}

func TestFirstNonEmptyFieldValue(t *testing.T) {
	tests := []struct {
		name       string
		results    []provider.FieldProviderResult
		wantValue  string
		wantSource string
	}{
		{
			name:      "all empty",
			results:   []provider.FieldProviderResult{{Provider: provider.NameWikipedia, HasData: false}},
			wantValue: "",
		},
		{
			name: "first with data wins",
			results: []provider.FieldProviderResult{
				{Provider: provider.NameWikipedia, Value: "London", HasData: true},
				{Provider: provider.NameMusicBrainz, Value: "United Kingdom", HasData: true},
			},
			wantValue:  "London",
			wantSource: "wikipedia",
		},
		{
			name: "skips has-data-but-blank",
			results: []provider.FieldProviderResult{
				{Provider: provider.NameWikipedia, Value: "  ", HasData: true},
				{Provider: provider.NameMusicBrainz, Value: "United Kingdom", HasData: true},
			},
			wantValue:  "United Kingdom",
			wantSource: "musicbrainz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, source := firstNonEmptyFieldValue(tt.results)
			if value != tt.wantValue {
				t.Errorf("value = %q, want %q", value, tt.wantValue)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
		})
	}
}

// originFixer is a test-only Fixer that handles origin_missing violations by
// setting the artist's Origin field in-memory and reporting Fixed=true. It
// records whether it was invoked so mode tests can prove the pipeline did or
// did not call the fixer. It deliberately does NOT implement
// CandidateDiscoverer, mirroring the real MetadataFixer, so manual mode does
// not invoke it.
type originFixer struct {
	applied  string
	fixCalls int
}

func (f *originFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleOriginMissing
}

func (f *originFixer) Fix(_ context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	f.fixCalls++
	a.Origin = f.applied
	return &FixResult{RuleID: v.RuleID, Fixed: true, Message: "applied by test fixer"}, nil
}

// TestPipeline_OriginMissing_DisabledMode verifies a disabled origin_missing
// rule is never evaluated: no violation row is written and the fixer is not
// called.
func TestPipeline_OriginMissing_DisabledMode(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	// origin_missing ships disabled by default; disable everything so the
	// pipeline evaluates nothing.
	disableAllRulesExcept(t, db)

	a := &artist.Artist{Name: "No Origin", SortName: "No Origin", Origin: ""}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	fixer := &originFixer{applied: "should-not-apply"}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	if _, err := pipeline.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	if fixer.fixCalls != 0 {
		t.Errorf("fixer invoked %d times for a disabled rule, want 0", fixer.fixCalls)
	}
	_, status, err := lookupViolationByRuleArtist(ctx, db, RuleOriginMissing, a.ID)
	if err != nil {
		t.Fatalf("lookupViolationByRuleArtist: %v", err)
	}
	if status != "" {
		t.Errorf("violation row written for disabled rule (status=%q), want none", status)
	}

	reloaded, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if reloaded.Origin != "" {
		t.Errorf("Origin = %q, want empty (disabled rule must not mutate)", reloaded.Origin)
	}
}

// TestPipeline_OriginMissing_ManualMode verifies manual mode surfaces an open
// violation for the user to act on and never auto-applies a value. The real
// MetadataFixer does not implement CandidateDiscoverer, so the pipeline records
// the violation without invoking the fixer.
func TestPipeline_OriginMissing_ManualMode(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleOriginMissing)
	if _, err := db.ExecContext(ctx,
		`UPDATE rules SET enabled = 1, automation_mode = ? WHERE id = ?`,
		AutomationModeManual, RuleOriginMissing); err != nil {
		t.Fatalf("enabling origin_missing in manual mode: %v", err)
	}

	a := &artist.Artist{Name: "Manual Origin", SortName: "Manual Origin", Origin: ""}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	fixer := &originFixer{applied: "should-not-apply-in-manual"}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	if _, err := pipeline.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}

	// Manual mode must surface the violation as open, not auto-apply.
	_, status, err := lookupViolationByRuleArtist(ctx, db, RuleOriginMissing, a.ID)
	if err != nil {
		t.Fatalf("lookupViolationByRuleArtist: %v", err)
	}
	if status != ViolationStatusOpen {
		t.Errorf("violation status = %q, want %q (manual mode surfaces an open violation)", status, ViolationStatusOpen)
	}
	if fixer.fixCalls != 0 {
		t.Errorf("fixer invoked %d times in manual mode, want 0 (no side-effect fixer in manual mode)", fixer.fixCalls)
	}

	reloaded, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if reloaded.Origin != "" {
		t.Errorf("Origin = %q, want empty (manual mode must not auto-apply)", reloaded.Origin)
	}
}

// TestPipeline_OriginMissing_AutoMode verifies auto mode applies the fixer's
// value and resolves the violation.
func TestPipeline_OriginMissing_AutoMode(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleOriginMissing)
	if _, err := db.ExecContext(ctx,
		`UPDATE rules SET enabled = 1, automation_mode = ? WHERE id = ?`,
		AutomationModeAuto, RuleOriginMissing); err != nil {
		t.Fatalf("enabling origin_missing in auto mode: %v", err)
	}

	a := &artist.Artist{Name: "Auto Origin", SortName: "Auto Origin", Origin: ""}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	fixer := &originFixer{applied: "Seattle, Washington"}
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	runResult, err := pipeline.RunForArtist(ctx, a)
	if err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}
	if runResult.FixesSucceeded != 1 {
		t.Fatalf("FixesSucceeded = %d, want 1", runResult.FixesSucceeded)
	}
	if fixer.fixCalls != 1 {
		t.Errorf("fixer invoked %d times in auto mode, want 1", fixer.fixCalls)
	}

	reloaded, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if reloaded.Origin != "Seattle, Washington" {
		t.Errorf("Origin = %q, want %q (auto mode applies the fix)", reloaded.Origin, "Seattle, Washington")
	}

	_, status, err := lookupViolationByRuleArtist(ctx, db, RuleOriginMissing, a.ID)
	if err != nil {
		t.Fatalf("lookupViolationByRuleArtist: %v", err)
	}
	if status != ViolationStatusResolved {
		t.Errorf("violation status = %q, want %q (auto-fix resolves the violation)", status, ViolationStatusResolved)
	}
}
