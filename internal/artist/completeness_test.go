package artist

import (
	"testing"
)

// TestBuildCompletenessReport_Empty verifies the report structure when there are no artists.
func TestBuildCompletenessReport_Empty(t *testing.T) {
	report := buildCompletenessReport(nil, nil)

	if report.TotalArtists != 0 {
		t.Errorf("TotalArtists = %d, want 0", report.TotalArtists)
	}
	if report.OverallScore != 0 {
		t.Errorf("OverallScore = %.1f, want 0", report.OverallScore)
	}
	if len(report.FieldCoverage) == 0 {
		t.Error("FieldCoverage should be non-empty even with no artists (empty placeholder entries)")
	}
	if report.LowestCompleteness == nil {
		t.Error("LowestCompleteness should not be nil (should be empty slice)")
	}
	if len(report.LowestCompleteness) != 0 {
		t.Errorf("LowestCompleteness = %d entries, want 0", len(report.LowestCompleteness))
	}
}

// TestBuildCompletenessReport_FullyCovered verifies that 100% coverage is computed
// when all artists have all fields populated.
func TestBuildCompletenessReport_FullyCovered(t *testing.T) {
	rows := []CompletenessRow{
		{
			ID:        "1",
			Name:      "Artist A",
			Type:      "group",
			Biography: "Some bio",
			Genres:    `["Rock"]`,
			Styles:    `["Alternative"]`,
			NFOExists: true,
			HasMBID:   true,
			HasThumb:  true,
			HasFanart: true,
			HasLogo:   true,
			HasBanner: true,
			Formed:    "1990",
			Disbanded: "1995",
		},
	}

	report := buildCompletenessReport(rows, nil)

	if report.TotalArtists != 1 {
		t.Errorf("TotalArtists = %d, want 1", report.TotalArtists)
	}

	for _, fc := range report.FieldCoverage {
		if fc.Percentage != 100.0 {
			t.Errorf("field %q: Percentage = %.1f, want 100.0", fc.Field, fc.Percentage)
		}
		if fc.Count != fc.Total {
			t.Errorf("field %q: Count (%d) != Total (%d)", fc.Field, fc.Count, fc.Total)
		}
	}

	if report.OverallScore != 100.0 {
		t.Errorf("OverallScore = %.1f, want 100.0", report.OverallScore)
	}
}

// TestBuildCompletenessReport_NoCoverage verifies 0% coverage when all fields are empty.
func TestBuildCompletenessReport_NoCoverage(t *testing.T) {
	rows := []CompletenessRow{
		{
			ID:   "1",
			Name: "Bare Artist",
			Type: "group",
		},
	}

	report := buildCompletenessReport(rows, nil)

	if report.OverallScore != 0 {
		t.Errorf("OverallScore = %.1f, want 0", report.OverallScore)
	}

	for _, fc := range report.FieldCoverage {
		if fc.Count != 0 {
			t.Errorf("field %q: Count = %d, want 0", fc.Field, fc.Count)
		}
	}
}

// TestBuildCompletenessReport_TypeAware verifies that type-specific fields are
// only counted for applicable artist types.
func TestBuildCompletenessReport_TypeAware(t *testing.T) {
	rows := []CompletenessRow{
		// Group artist: "Formed date" applies, "Born date" does not.
		{ID: "1", Name: "The Band", Type: "group", Formed: "1985"},
		// Person artist: "Born date" applies, "Formed date" does not.
		{ID: "2", Name: "Solo Singer", Type: "person", Born: "1970"},
	}

	report := buildCompletenessReport(rows, nil)

	// Find the "Formed date" and "Born date" entries.
	var formedFC, bornFC *FieldCoverage
	for i := range report.FieldCoverage {
		switch report.FieldCoverage[i].Field {
		case "Formed date":
			formedFC = &report.FieldCoverage[i]
		case "Born date":
			bornFC = &report.FieldCoverage[i]
		}
	}

	if formedFC == nil {
		t.Fatal("Formed date field missing from coverage report")
	}
	if bornFC == nil {
		t.Fatal("Born date field missing from coverage report")
	}

	// Only the group artist is counted for Formed date.
	if formedFC.Total != 1 {
		t.Errorf("Formed date Total = %d, want 1 (only group)", formedFC.Total)
	}
	if formedFC.Count != 1 {
		t.Errorf("Formed date Count = %d, want 1", formedFC.Count)
	}
	if formedFC.Percentage != 100.0 {
		t.Errorf("Formed date Percentage = %.1f, want 100.0", formedFC.Percentage)
	}

	// Only the person artist is counted for Born date.
	if bornFC.Total != 1 {
		t.Errorf("Born date Total = %d, want 1 (only person)", bornFC.Total)
	}
	if bornFC.Count != 1 {
		t.Errorf("Born date Count = %d, want 1", bornFC.Count)
	}
}

// TestBuildCompletenessReport_OrchestraType verifies that orchestra artists are
// treated like groups: "Formed date" applies but "Born date" does not.
func TestBuildCompletenessReport_OrchestraType(t *testing.T) {
	rows := []CompletenessRow{
		// Orchestra: "Formed date" applies, "Born date" does not.
		{ID: "1", Name: "Berlin Phil", Type: "orchestra", Formed: "1882"},
	}

	report := buildCompletenessReport(rows, nil)

	var formedFC, bornFC *FieldCoverage
	for i := range report.FieldCoverage {
		switch report.FieldCoverage[i].Field {
		case "Formed date":
			formedFC = &report.FieldCoverage[i]
		case "Born date":
			bornFC = &report.FieldCoverage[i]
		}
	}

	if formedFC == nil {
		t.Fatal("Formed date field missing from coverage report for orchestra")
	}
	if formedFC.Total != 1 {
		t.Errorf("Formed date Total = %d, want 1 (orchestra counts)", formedFC.Total)
	}
	if formedFC.Count != 1 {
		t.Errorf("Formed date Count = %d, want 1", formedFC.Count)
	}

	// "Born date" is not applicable to orchestras; it should be omitted entirely
	// since no artist has total > 0 for it.
	if bornFC != nil {
		t.Errorf("Born date should be absent from coverage report when only orchestras present (Total=%d)", bornFC.Total)
	}
}

// TestBuildCompletenessReport_EmptyType verifies that artists with an empty type
// string are treated as groups for formed/born applicability: "Formed date"
// applies but "Born date" does not.
func TestBuildCompletenessReport_EmptyType(t *testing.T) {
	rows := []CompletenessRow{
		// Unknown type: empty string. The allFieldDefs rule treats "" like "group"
		// for "Formed date" and excludes it from "Born date".
		{ID: "1", Name: "Unknown Artist", Type: "", Formed: "2000"},
	}

	report := buildCompletenessReport(rows, nil)

	var formedFC, bornFC *FieldCoverage
	for i := range report.FieldCoverage {
		switch report.FieldCoverage[i].Field {
		case "Formed date":
			formedFC = &report.FieldCoverage[i]
		case "Born date":
			bornFC = &report.FieldCoverage[i]
		}
	}

	if formedFC == nil {
		t.Fatal("Formed date field missing from coverage report for empty-type artist")
	}
	if formedFC.Total != 1 {
		t.Errorf("Formed date Total = %d, want 1 (empty type treated as applicable)", formedFC.Total)
	}
	if formedFC.Count != 1 {
		t.Errorf("Formed date Count = %d, want 1", formedFC.Count)
	}

	// "Born date" is not applicable to empty-type artists.
	if bornFC != nil {
		t.Errorf("Born date should be absent when only empty-type artists are present (Total=%d)", bornFC.Total)
	}
}

// TestBuildCompletenessReport_ChoirType verifies that choir artists are treated
// like groups: "Formed date" and "Disbanded date" apply, "Born date" and
// "Died date" do not.
func TestBuildCompletenessReport_ChoirType(t *testing.T) {
	rows := []CompletenessRow{
		{ID: "1", Name: "City Choir", Type: "choir", Formed: "1920"},
	}

	report := buildCompletenessReport(rows, nil)

	var formedFC, bornFC, disbandedFC, diedFC *FieldCoverage
	for i := range report.FieldCoverage {
		switch report.FieldCoverage[i].Field {
		case "Formed date":
			formedFC = &report.FieldCoverage[i]
		case "Born date":
			bornFC = &report.FieldCoverage[i]
		case "Disbanded date":
			disbandedFC = &report.FieldCoverage[i]
		case "Died date":
			diedFC = &report.FieldCoverage[i]
		}
	}

	if formedFC == nil {
		t.Fatal("Formed date field missing from coverage report for choir")
	}
	if formedFC.Total != 1 {
		t.Errorf("Formed date Total = %d, want 1 (choir counts)", formedFC.Total)
	}
	if formedFC.Count != 1 {
		t.Errorf("Formed date Count = %d, want 1", formedFC.Count)
	}

	if disbandedFC == nil {
		t.Fatal("Disbanded date field missing from coverage report for choir")
	}
	if disbandedFC.Total != 1 {
		t.Errorf("Disbanded date Total = %d, want 1 (choir counts)", disbandedFC.Total)
	}
	if disbandedFC.Count != 0 {
		t.Errorf("Disbanded date Count = %d, want 0 (not populated)", disbandedFC.Count)
	}

	if bornFC != nil {
		t.Errorf("Born date should be absent from coverage report when only choirs present (Total=%d)", bornFC.Total)
	}
	if diedFC != nil {
		t.Errorf("Died date should be absent from coverage report when only choirs present (Total=%d)", diedFC.Total)
	}
}

// TestBuildCompletenessReport_SoloType verifies that solo artists get "Born date"
// and "Died date" counted, not "Formed date" or "Disbanded date".
func TestBuildCompletenessReport_SoloType(t *testing.T) {
	rows := []CompletenessRow{
		{ID: "1", Name: "Solo Singer", Type: "solo", Born: "1975", Died: "2020"},
	}

	report := buildCompletenessReport(rows, nil)

	var formedFC, bornFC, disbandedFC, diedFC *FieldCoverage
	for i := range report.FieldCoverage {
		switch report.FieldCoverage[i].Field {
		case "Formed date":
			formedFC = &report.FieldCoverage[i]
		case "Born date":
			bornFC = &report.FieldCoverage[i]
		case "Disbanded date":
			disbandedFC = &report.FieldCoverage[i]
		case "Died date":
			diedFC = &report.FieldCoverage[i]
		}
	}

	if bornFC == nil {
		t.Fatal("Born date field missing from coverage report for solo type")
	}
	if bornFC.Total != 1 {
		t.Errorf("Born date Total = %d, want 1 (solo counts)", bornFC.Total)
	}
	if bornFC.Count != 1 {
		t.Errorf("Born date Count = %d, want 1", bornFC.Count)
	}

	if diedFC == nil {
		t.Fatal("Died date field missing from coverage report for solo type")
	}
	if diedFC.Total != 1 {
		t.Errorf("Died date Total = %d, want 1 (solo counts)", diedFC.Total)
	}
	if diedFC.Count != 1 {
		t.Errorf("Died date Count = %d, want 1", diedFC.Count)
	}

	if formedFC != nil {
		t.Errorf("Formed date should be absent when only solo artists present (Total=%d)", formedFC.Total)
	}
	if disbandedFC != nil {
		t.Errorf("Disbanded date should be absent when only solo artists present (Total=%d)", disbandedFC.Total)
	}
}

// TestBuildCompletenessReport_CharacterType verifies that character artists get
// "Born date" and "Died date" counted, not "Formed date" or "Disbanded date".
func TestBuildCompletenessReport_CharacterType(t *testing.T) {
	rows := []CompletenessRow{
		{ID: "1", Name: "Fictional Character", Type: "character", Born: "1800"},
	}

	report := buildCompletenessReport(rows, nil)

	var bornFC, diedFC, formedFC *FieldCoverage
	for i := range report.FieldCoverage {
		switch report.FieldCoverage[i].Field {
		case "Born date":
			bornFC = &report.FieldCoverage[i]
		case "Died date":
			diedFC = &report.FieldCoverage[i]
		case "Formed date":
			formedFC = &report.FieldCoverage[i]
		}
	}

	if bornFC == nil {
		t.Fatal("Born date field missing from coverage report for character type")
	}
	if bornFC.Total != 1 {
		t.Errorf("Born date Total = %d, want 1 (character counts)", bornFC.Total)
	}
	if bornFC.Count != 1 {
		t.Errorf("Born date Count = %d, want 1", bornFC.Count)
	}

	if diedFC == nil {
		t.Fatal("Died date field missing from coverage report for character type")
	}
	if diedFC.Total != 1 {
		t.Errorf("Died date Total = %d, want 1 (character counts)", diedFC.Total)
	}

	if formedFC != nil {
		t.Errorf("Formed date should be absent when only character artists present (Total=%d)", formedFC.Total)
	}
}

// TestBuildCompletenessReport_DisbandedDateGroup verifies that Disbanded date
// is tracked for group artists.
func TestBuildCompletenessReport_DisbandedDateGroup(t *testing.T) {
	rows := []CompletenessRow{
		{ID: "1", Name: "Disbanded Band", Type: "group", Formed: "1980", Disbanded: "1995"},
		{ID: "2", Name: "Active Band", Type: "group", Formed: "2000"},
	}

	report := buildCompletenessReport(rows, nil)

	var disbandedFC *FieldCoverage
	for i := range report.FieldCoverage {
		if report.FieldCoverage[i].Field == "Disbanded date" {
			disbandedFC = &report.FieldCoverage[i]
			break
		}
	}

	if disbandedFC == nil {
		t.Fatal("Disbanded date field missing from coverage report")
	}
	if disbandedFC.Total != 2 {
		t.Errorf("Disbanded date Total = %d, want 2", disbandedFC.Total)
	}
	if disbandedFC.Count != 1 {
		t.Errorf("Disbanded date Count = %d, want 1 (only disbanded band has it)", disbandedFC.Count)
	}
	if disbandedFC.Percentage != 50.0 {
		t.Errorf("Disbanded date Percentage = %.1f, want 50.0", disbandedFC.Percentage)
	}
}

// TestBuildCompletenessReport_DiedDatePerson verifies that Died date
// is tracked for person artists.
func TestBuildCompletenessReport_DiedDatePerson(t *testing.T) {
	rows := []CompletenessRow{
		{ID: "1", Name: "Deceased Artist", Type: "person", Born: "1900", Died: "1980"},
		{ID: "2", Name: "Living Artist", Type: "person", Born: "1970"},
	}

	report := buildCompletenessReport(rows, nil)

	var diedFC *FieldCoverage
	for i := range report.FieldCoverage {
		if report.FieldCoverage[i].Field == "Died date" {
			diedFC = &report.FieldCoverage[i]
			break
		}
	}

	if diedFC == nil {
		t.Fatal("Died date field missing from coverage report")
	}
	if diedFC.Total != 2 {
		t.Errorf("Died date Total = %d, want 2", diedFC.Total)
	}
	if diedFC.Count != 1 {
		t.Errorf("Died date Count = %d, want 1 (only deceased artist has it)", diedFC.Count)
	}
	if diedFC.Percentage != 50.0 {
		t.Errorf("Died date Percentage = %.1f, want 50.0", diedFC.Percentage)
	}
}

// TestBuildCompletenessReport_LibraryCoverage verifies that per-library
// breakdown is populated when more than one library is present.
func TestBuildCompletenessReport_LibraryCoverage(t *testing.T) {
	rows := []CompletenessRow{
		{ID: "1", Name: "Artist A", Type: "group", LibraryID: "lib-a", Biography: "Bio"},
		{ID: "2", Name: "Artist B", Type: "group", LibraryID: "lib-a"},
		{ID: "3", Name: "Artist C", Type: "group", LibraryID: "lib-b", Biography: "Bio"},
	}
	libNames := map[string]string{
		"lib-a": "Library Alpha",
		"lib-b": "Library Beta",
	}

	report := buildCompletenessReport(rows, nil)
	report.LibraryCoverage = buildLibraryCoverage(rows, libNames)

	if len(report.LibraryCoverage) != 2 {
		t.Fatalf("LibraryCoverage length = %d, want 2", len(report.LibraryCoverage))
	}

	libA := report.LibraryCoverage[0]
	if libA.LibraryID != "lib-a" {
		t.Errorf("LibraryCoverage[0].LibraryID = %q, want %q", libA.LibraryID, "lib-a")
	}
	if libA.LibraryName != "Library Alpha" {
		t.Errorf("LibraryCoverage[0].LibraryName = %q, want %q", libA.LibraryName, "Library Alpha")
	}
	if libA.TotalArtists != 2 {
		t.Errorf("LibraryCoverage[0].TotalArtists = %d, want 2", libA.TotalArtists)
	}
	if libA.Score < 0 || libA.Score > 100 {
		t.Errorf("LibraryCoverage[0].Score = %.1f, want between 0 and 100", libA.Score)
	}
	if len(libA.Fields) == 0 {
		t.Error("LibraryCoverage[0].Fields is empty, want at least one field entry")
	}

	libB := report.LibraryCoverage[1]
	if libB.LibraryID != "lib-b" {
		t.Errorf("LibraryCoverage[1].LibraryID = %q, want %q", libB.LibraryID, "lib-b")
	}
	if libB.TotalArtists != 1 {
		t.Errorf("LibraryCoverage[1].TotalArtists = %d, want 1", libB.TotalArtists)
	}
}

// TestBuildCompletenessReport_LibraryCoverage_SingleLibrary verifies that
// LibraryCoverage is nil when all artists belong to the same library.
func TestBuildCompletenessReport_LibraryCoverage_SingleLibrary(t *testing.T) {
	rows := []CompletenessRow{
		{ID: "1", Name: "Artist A", Type: "group", LibraryID: "lib-a"},
		{ID: "2", Name: "Artist B", Type: "group", LibraryID: "lib-a"},
	}

	coverage := buildLibraryCoverage(rows, nil)
	if coverage != nil {
		t.Errorf("LibraryCoverage should be nil for single-library data, got %d entries", len(coverage))
	}
}

// TestBuildCompletenessReport_LibraryCoverage_UnknownLibraryID verifies that a
// library ID with no name entry in the map falls back to the ID itself.
func TestBuildCompletenessReport_LibraryCoverage_UnknownLibraryID(t *testing.T) {
	rows := []CompletenessRow{
		{ID: "1", Name: "Artist A", Type: "group", LibraryID: "lib-x"},
		{ID: "2", Name: "Artist B", Type: "group", LibraryID: "lib-y"},
	}

	coverage := buildLibraryCoverage(rows, nil)
	if len(coverage) != 2 {
		t.Fatalf("LibraryCoverage length = %d, want 2", len(coverage))
	}
	if coverage[0].LibraryName != "lib-x" {
		t.Errorf("LibraryName = %q, want %q (fallback to ID)", coverage[0].LibraryName, "lib-x")
	}
}

// TestBuildCompletenessReport_PartialCoverage verifies correct percentage calculation
// with a mix of populated and missing fields.
func TestBuildCompletenessReport_PartialCoverage(t *testing.T) {
	rows := []CompletenessRow{
		{ID: "1", Name: "Artist A", Type: "group", Biography: "Bio A", NFOExists: true},
		{ID: "2", Name: "Artist B", Type: "group"}, // no bio, no NFO
	}

	report := buildCompletenessReport(rows, nil)

	// Find biography and NFO fields.
	var bioFC, nfoFC *FieldCoverage
	for i := range report.FieldCoverage {
		switch report.FieldCoverage[i].Field {
		case "Biography":
			bioFC = &report.FieldCoverage[i]
		case "Metadata file (NFO)":
			nfoFC = &report.FieldCoverage[i]
		}
	}

	if bioFC == nil {
		t.Fatal("Biography field missing from coverage report")
	}
	if nfoFC == nil {
		t.Fatal("Metadata file (NFO) field missing from coverage report")
	}

	if bioFC.Count != 1 {
		t.Errorf("Biography Count = %d, want 1", bioFC.Count)
	}
	if bioFC.Total != 2 {
		t.Errorf("Biography Total = %d, want 2", bioFC.Total)
	}
	if bioFC.Percentage != 50.0 {
		t.Errorf("Biography Percentage = %.1f, want 50.0", bioFC.Percentage)
	}

	if nfoFC.Count != 1 {
		t.Errorf("NFO Count = %d, want 1", nfoFC.Count)
	}
	if nfoFC.Percentage != 50.0 {
		t.Errorf("NFO Percentage = %.1f, want 50.0", nfoFC.Percentage)
	}
}

// TestBuildCompletenessReport_SortedAscending verifies that FieldCoverage is
// sorted by percentage ascending (lowest coverage first).
func TestBuildCompletenessReport_SortedAscending(t *testing.T) {
	// Two artists: one with biography, one without. No other fields populated.
	// Biography should have 50% coverage; all others 0%.
	rows := []CompletenessRow{
		{ID: "1", Name: "Artist A", Type: "group", Biography: "Has bio"},
		{ID: "2", Name: "Artist B", Type: "group"},
	}

	report := buildCompletenessReport(rows, nil)

	for i := 1; i < len(report.FieldCoverage); i++ {
		if report.FieldCoverage[i].Percentage < report.FieldCoverage[i-1].Percentage {
			t.Errorf("FieldCoverage not sorted ascending at index %d: %.1f < %.1f",
				i, report.FieldCoverage[i].Percentage, report.FieldCoverage[i-1].Percentage)
		}
	}
}

// TestBuildCompletenessReport_OverallScore verifies the overall score is the mean
// field-coverage across all applicable pairs.
func TestBuildCompletenessReport_OverallScore(t *testing.T) {
	// Single artist, group type. Universal fields + Formed and Disbanded date
	// are applicable. Born date, Died date are not applicable for groups.
	// The exact count is not asserted here; the test verifies the formula.
	rows := []CompletenessRow{
		{
			ID:        "1",
			Name:      "Artist A",
			Type:      "group",
			Biography: "Bio",
			Genres:    `["Rock"]`,
			Formed:    "1990",
		},
	}

	report := buildCompletenessReport(rows, nil)

	// Count populated / total across all applicable fields.
	var totalCount, totalApplicable int
	for _, fc := range report.FieldCoverage {
		totalCount += fc.Count
		totalApplicable += fc.Total
	}

	if totalApplicable == 0 {
		t.Fatal("No applicable field/artist pairs found")
	}

	expected := roundOneDecimal(float64(totalCount) / float64(totalApplicable) * 100.0)
	if report.OverallScore != expected {
		t.Errorf("OverallScore = %.1f, want %.1f", report.OverallScore, expected)
	}
}

// TestBuildCompletenessReport_LowestCompleteness verifies that the lowest
// completeness list is passed through unchanged.
func TestBuildCompletenessReport_LowestCompleteness(t *testing.T) {
	lowest := []LowestCompletenessArtist{
		{ID: "3", Name: "Worst Artist", HealthScore: 10},
		{ID: "4", Name: "Bad Artist", HealthScore: 25},
	}

	rows := []CompletenessRow{
		{ID: "1", Name: "Good Artist", Type: "group"},
	}

	report := buildCompletenessReport(rows, lowest)

	if len(report.LowestCompleteness) != 2 {
		t.Errorf("LowestCompleteness length = %d, want 2", len(report.LowestCompleteness))
	}
	if report.LowestCompleteness[0].ID != "3" {
		t.Errorf("LowestCompleteness[0].ID = %q, want %q", report.LowestCompleteness[0].ID, "3")
	}
}

// TestRoundOneDecimal verifies rounding behavior.
func TestRoundOneDecimal(t *testing.T) {
	tests := []struct {
		input float64
		want  float64
	}{
		{66.666, 66.7},
		{33.333, 33.3},
		{100.0, 100.0},
		{0.0, 0.0},
		{50.05, 50.1},
		{50.04, 50.0},
	}
	for _, tc := range tests {
		got := roundOneDecimal(tc.input)
		if got != tc.want {
			t.Errorf("roundOneDecimal(%.3f) = %.1f, want %.1f", tc.input, got, tc.want)
		}
	}
}

// TestSortFieldCoverageAsc verifies the sort is stable and ascending.
func TestSortFieldCoverageAsc(t *testing.T) {
	coverage := []FieldCoverage{
		{Field: "C", Percentage: 75.0},
		{Field: "A", Percentage: 25.0},
		{Field: "B", Percentage: 50.0},
		{Field: "D", Percentage: 25.0},
	}
	sortFieldCoverageAsc(coverage)

	// First two should be the 25% fields, ordered by name.
	if coverage[0].Field != "A" {
		t.Errorf("coverage[0].Field = %q, want %q", coverage[0].Field, "A")
	}
	if coverage[1].Field != "D" {
		t.Errorf("coverage[1].Field = %q, want %q", coverage[1].Field, "D")
	}
	if coverage[2].Percentage != 50.0 {
		t.Errorf("coverage[2].Percentage = %.1f, want 50.0", coverage[2].Percentage)
	}
	if coverage[3].Percentage != 75.0 {
		t.Errorf("coverage[3].Percentage = %.1f, want 75.0", coverage[3].Percentage)
	}
}

// TestBuildCompletenessReport_GenresEmpty verifies that the empty JSON array "[]"
// and the empty string are both treated as missing genres.
func TestBuildCompletenessReport_GenresEmpty(t *testing.T) {
	rows := []CompletenessRow{
		{ID: "1", Name: "A", Type: "group", Genres: "[]"},
		{ID: "2", Name: "B", Type: "group", Genres: ""},
		{ID: "3", Name: "C", Type: "group", Genres: `["Rock"]`},
	}

	report := buildCompletenessReport(rows, nil)

	var genresFC *FieldCoverage
	for i := range report.FieldCoverage {
		if report.FieldCoverage[i].Field == "Genres" {
			genresFC = &report.FieldCoverage[i]
			break
		}
	}

	if genresFC == nil {
		t.Fatal("Genres field missing from coverage report")
	}
	if genresFC.Count != 1 {
		t.Errorf("Genres Count = %d, want 1 (only artist C has genres)", genresFC.Count)
	}
	if genresFC.Total != 3 {
		t.Errorf("Genres Total = %d, want 3", genresFC.Total)
	}
}
