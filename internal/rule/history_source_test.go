package rule

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// bioOverwritingFixer replaces the artist's biography in memory and reports
// Fixed, standing in for any real fixer that clobbers an operator's value. The
// damage shape matters: the blast-radius report only lists a row whose old_value
// is non-empty and differs from new_value, so a fixer that FILLED an empty field
// would produce a row the report ignores and prove nothing.
type bioOverwritingFixer struct {
	ruleID   string
	newBio   string
	fixCalls int
}

func (f *bioOverwritingFixer) CanFix(v *Violation) bool { return v.RuleID == f.ruleID }

func (f *bioOverwritingFixer) Fix(_ context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	f.fixCalls++
	a.Biography = f.newBio
	return &FixResult{RuleID: v.RuleID, Fixed: true, Message: "overwrote biography"}, nil
}

// attributionFixture wires a real SQLite artist service with history recording
// on and seeds one artist holding an operator-written biography.
func attributionFixture(t *testing.T, bio string) (*artist.Service, *artist.HistoryService, *artist.Artist, *Service, context.Context) {
	t.Helper()
	db := setupTestDB(t)
	ctx := context.Background()

	artistSvc := artist.NewService(db)
	historySvc := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(historySvc)

	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{
		Name:      "Attribution Subject",
		SortName:  "Attribution Subject",
		Path:      t.TempDir(),
		Biography: bio,
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	return artistSvc, historySvc, a, ruleSvc, ctx
}

// bioChangeRow returns the one metadata_changes row for the artist's biography,
// failing on any other count. Exactness is the point: a second row means two
// writers touched the field and the source under test could be either one's.
func bioChangeRow(t *testing.T, h *artist.HistoryService, artistID string) artist.MetadataChange {
	t.Helper()
	changes, _, err := h.List(context.Background(), artistID, 100, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	var bio []artist.MetadataChange
	for _, c := range changes {
		if c.Field == "biography" {
			bio = append(bio, c)
		}
	}
	if len(bio) != 1 {
		t.Fatalf("got %d biography history rows, want exactly 1: %+v", len(bio), bio)
	}
	return bio[0]
}

// TestRuleFix_StampsRuleSourceEndToEnd is the end-to-end guard for #3037's
// attribution half. A rule auto-fix that overwrites an operator's biography
// must leave a metadata_changes row naming the RULE, and the blast-radius
// report must bucket that row as automated rather than unknown. Before the fix
// the same run recorded source="manual", so the report showed the damage while
// attributing it to nobody and `source LIKE 'rule:%'` found nothing.
func TestRuleFix_StampsRuleSourceEndToEnd(t *testing.T) {
	artistSvc, historySvc, a, ruleSvc, ctx := attributionFixture(t, "the operator wrote this")

	rv := &RuleViolation{
		RuleID:     RuleBioExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "error",
		Message:    "biography needs work",
		Fixable:    true,
		Status:     ViolationStatusOpen,
	}
	if err := ruleSvc.UpsertViolation(ctx, rv); err != nil {
		t.Fatalf("upserting violation: %v", err)
	}

	fixer := &bioOverwritingFixer{ruleID: RuleBioExists, newBio: "a rule wrote this"}
	engine := NewEngine(ruleSvc, nil, nil, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	fr, err := pipeline.FixViolation(ctx, rv.ID)
	if err != nil {
		t.Fatalf("FixViolation: %v", err)
	}
	if !fr.Fixed {
		t.Fatalf("Fixed = false (%s); the write under test never happened", fr.Message)
	}
	// Precondition, not decoration: with zero Fix calls every assertion below
	// would be about a row no rule produced.
	if fixer.fixCalls == 0 {
		t.Fatal("fixer was never invoked; nothing exercised the attributed write path")
	}

	row := bioChangeRow(t, historySvc, a.ID)
	want := "rule:" + RuleBioExists
	if row.Source != want {
		t.Errorf("history source = %q, want %q; a rule-caused overwrite is unattributable", row.Source, want)
	}
	// The damage shape the blast-radius report keys on. Asserted so the
	// attribution assertion below cannot pass against a row the report would
	// never have listed in the first place.
	if row.OldValue != "the operator wrote this" || row.NewValue != "a rule wrote this" {
		t.Fatalf("history row is not the overwrite under test: old=%q new=%q", row.OldValue, row.NewValue)
	}

	// The consumer must agree. Asserting the classification, not just the
	// string, proves the stamp is useful rather than merely present.
	rows, err := historySvc.ListBlastRadius(ctx, artist.BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}
	var found *artist.BlastRadiusRow
	for i := range rows {
		if rows[i].ID == row.ID {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatalf("the rule-caused overwrite is missing from the blast-radius report entirely")
	}
	if found.Attribution != artist.BlastAttributionAutomated {
		t.Errorf("attribution = %q, want %q; the report can see the damage but cannot blame the rule",
			found.Attribution, artist.BlastAttributionAutomated)
	}
}

// TestNonRuleWrite_IsNotMisattributedToARule is the positive control. A write
// no rule made must keep its own source and must not acquire a "rule:" one:
// blaming a rule for an operator's edit is a confident wrong answer on a
// recovery surface, worse than the unknown bucket it replaced.
func TestNonRuleWrite_IsNotMisattributedToARule(t *testing.T) {
	artistSvc, historySvc, a, _, ctx := attributionFixture(t, "before")

	// A manual field edit through the same service the rule engine writes
	// through, with no rule anywhere in the call chain.
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "after"); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}

	row := bioChangeRow(t, historySvc, a.ID)
	if row.Source != "manual" {
		t.Errorf("history source = %q, want %q for an untagged operator edit", row.Source, "manual")
	}
	// The prefix artist.classifyBlastAttribution keys on, spelled out because
	// that helper is unexported.
	if strings.HasPrefix(row.Source, "rule:") {
		t.Errorf("an operator edit was attributed to a rule (source %q)", row.Source)
	}
}

// TestRunAccum_HistorySourceNamesTheRule covers the run paths' shared
// attribution decision, which FixViolation does not exercise: they persist the
// artist ONCE after every fixer has run, so the source is derived from what the
// whole pass did. The "" case is load-bearing -- a pass that fixed nothing still
// persists a recomputed health score, and tagging that write would attribute a
// rule fix to a pass that made none.
func TestRunAccum_HistorySourceNamesTheRule(t *testing.T) {
	cases := []struct {
		name  string
		rules []string
		want  string
	}{
		{name: "no fix", rules: nil, want: ""},
		{name: "one rule", rules: []string{RuleBioExists}, want: "rule:" + RuleBioExists},
		{
			name:  "same rule twice is still that rule",
			rules: []string{RuleBioExists, RuleBioExists},
			want:  "rule:" + RuleBioExists,
		},
		{
			name:  "two rules cannot be named by one write",
			rules: []string{RuleBioExists, RuleOriginMissing},
			want:  ruleHistorySourceMultiple,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := &runForArtistAccum{persistOK: true}
			for _, id := range tc.rules {
				acc.mergeOutcome(violationOutcome{
					fr:          &FixResult{RuleID: id, Fixed: true},
					fixed:       true,
					fixedRuleID: id,
				}, &RunResult{})
			}
			if got := acc.historySource(); got != tc.want {
				t.Errorf("historySource() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunPath_StampsRuleSource proves the RUN path carries the tag into a real
// history row. It drives RunForArtist, which persists through
// persistHealthAfterRun -- a different write site from FixViolation's.
func TestRunPath_StampsRuleSource(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	artistSvc := artist.NewService(db)
	historySvc := artist.NewHistoryService(db)
	artistSvc.SetHistoryService(historySvc)

	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleBioExists)
	if _, err := db.ExecContext(ctx,
		`UPDATE rules SET automation_mode = ? WHERE id = ?`, AutomationModeAuto, RuleBioExists); err != nil {
		t.Fatalf("setting automation_mode=auto: %v", err)
	}

	// Biography short enough that bio_exists violates, non-empty so overwriting
	// it is damage the report would list.
	a := &artist.Artist{
		Name:      "Run Path Subject",
		SortName:  "Run Path Subject",
		Path:      t.TempDir(),
		Biography: "short",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.MarkDirty(ctx, a.ID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}

	fixer := &bioOverwritingFixer{ruleID: RuleBioExists, newBio: "a rule wrote this longer biography"}
	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	if _, err := pipeline.RunForArtist(ctx, a); err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}
	if fixer.fixCalls == 0 {
		t.Fatal("fixer was never invoked; the run path never reached the attributed write")
	}

	row := bioChangeRow(t, historySvc, a.ID)
	want := "rule:" + RuleBioExists
	if row.Source != want {
		t.Errorf("run-path history source = %q, want %q", row.Source, want)
	}
}

// bioReturningScrapeAll is a provider.ScraperExecutor that returns one
// biography, so fetchMetadata reaches its artist write having changed a field
// artist.trackableFields actually covers.
type bioReturningScrapeAll struct{ bio string }

func (s bioReturningScrapeAll) ScrapeAll(_ context.Context, _, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	return &provider.FetchResult{Metadata: &provider.ArtistMetadata{Biography: s.bio}}, nil
}

// TestBulkFetchMetadata_StampsBulkSource guards the bulk fetch-metadata write
// (bulk_executor.go). With FixViolation and the run paths above, that is every
// site in this package known to produce a history row today. The sibling stamps
// (fetch-images, the MBID self-heal, the fanart collapse, the two phash passes)
// change no trackable field, so there is no row whose source a test could read.
func TestBulkFetchMetadata_StampsBulkSource(t *testing.T) {
	artistSvc, historySvc, a, _, ctx := attributionFixture(t, "")

	orch := provider.NewOrchestrator(nil, nil, testLogger(), nil)
	orch.SetExecutor(bioReturningScrapeAll{bio: "a bulk job wrote this"})
	e := &BulkExecutor{artistService: artistSvc, orchestrator: orch, logger: testLogger()}

	status, msg := e.fetchMetadata(ctx, a, BulkModeYOLO)
	if status != BulkItemFixed {
		t.Fatalf("status = %q (%s); the attributed write never happened", status, msg)
	}

	row := bioChangeRow(t, historySvc, a.ID)
	// Precondition: without it the source assertion could pass against a row
	// some other writer produced.
	if row.NewValue != "a bulk job wrote this" {
		t.Fatalf("history row is not the bulk write: old=%q new=%q", row.OldValue, row.NewValue)
	}
	if row.Source != ruleHistorySourceBulkFetchMetadata {
		t.Errorf("history source = %q, want %q; a bulk overwrite is unattributable",
			row.Source, ruleHistorySourceBulkFetchMetadata)
	}
}

// IsPseudoRuleSource must recognize exactly the sources this package stamps
// for writes no single catalogue rule owns -- and nothing else. The negative
// half matters as much as the positive: a real rule id misclassified as a
// pseudo-source would get its capability check skipped by the repair's
// caller, and an id-shaped stranger must fall through to the ordinary
// unknown-rule handling.
func TestIsPseudoRuleSource(t *testing.T) {
	for _, id := range []string{
		"multiple_rules",
		"bulk_fetch_metadata",
		"bulk_fetch_images",
		"bulk_fetch_images_mbid",
		"phash_mismatch_remediate",
		"phash_quarantine_restore",
	} {
		if !IsPseudoRuleSource(id) {
			t.Errorf("IsPseudoRuleSource(%q) = false, want true", id)
		}
	}
	for _, id := range []string{
		"metadata_quality",    // a real catalogue rule
		"",                    // empty
		"rule:multiple_rules", // the caller strips the prefix; the prefixed form is NOT the id
		"multiple_ruleZ",
	} {
		if IsPseudoRuleSource(id) {
			t.Errorf("IsPseudoRuleSource(%q) = true, want false", id)
		}
	}
}
