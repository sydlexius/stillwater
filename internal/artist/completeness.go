package artist

import (
	"context"
	"fmt"
	"math"
)

// FieldCoverage holds the completeness statistics for a single metadata field.
type FieldCoverage struct {
	// Field is the human-readable field name shown in the UI (e.g. "Biography").
	Field string `json:"field"`
	// Count is the number of artists that have this field populated.
	Count int `json:"count"`
	// Total is the number of artists for which this field is applicable.
	Total int `json:"total"`
	// Percentage is Count/Total*100 rounded to one decimal place.
	// Zero when Total is zero.
	Percentage float64 `json:"percentage"`
}

// LibraryCompleteness summarizes field coverage for a single library.
type LibraryCompleteness struct {
	LibraryID    string          `json:"library_id"`
	LibraryName  string          `json:"library_name"`
	TotalArtists int             `json:"total_artists"`
	Score        float64         `json:"score"`
	Fields       []FieldCoverage `json:"fields"`
}

// MetadataCompletenessReport is the full response for the completeness endpoint.
type MetadataCompletenessReport struct {
	// OverallScore is the mean field-coverage percentage across all applicable
	// field/artist pairs for non-excluded artists.
	OverallScore float64 `json:"overall_score"`
	// TotalArtists is the count of non-excluded artists included in the report.
	TotalArtists int `json:"total_artists"`
	// FieldCoverage is the per-field breakdown sorted by percentage ascending
	// (lowest coverage first so the worst gaps appear at the top of the chart).
	FieldCoverage []FieldCoverage `json:"field_coverage"`
	// LibraryCoverage is the per-library summary, omitted when only one
	// (or no) library exists.
	LibraryCoverage []LibraryCompleteness `json:"library_coverage,omitempty"`
	// LowestCompleteness lists the bottom-N artists by health score so users
	// can quickly identify which artists need the most attention.
	LowestCompleteness []LowestCompletenessArtist `json:"lowest_completeness"`
}

// fieldDef describes a single field to evaluate during completeness calculation.
type fieldDef struct {
	// name is the label shown in the UI.
	name string
	// applicable returns true when the field is meaningful for a given artist type.
	applicable func(artistType string) bool
	// present returns true when the field has a non-empty value on the given row.
	present func(row CompletenessRow) bool
}

// allFieldDefs is the ordered list of metadata fields assessed for completeness.
// Artist type-aware fields (born/died vs formed/disbanded) are represented as
// separate entries with appropriate applicable guards.
var allFieldDefs = []fieldDef{
	{
		name:       "Metadata file (NFO)",
		applicable: func(_ string) bool { return true },
		present:    func(r CompletenessRow) bool { return r.NFOExists },
	},
	{
		name:       "MusicBrainz ID",
		applicable: func(_ string) bool { return true },
		present:    func(r CompletenessRow) bool { return r.HasMBID },
	},
	{
		name:       "Biography",
		applicable: func(_ string) bool { return true },
		present:    func(r CompletenessRow) bool { return r.Biography != "" },
	},
	{
		name:       "Genres",
		applicable: func(_ string) bool { return true },
		present:    func(r CompletenessRow) bool { return r.Genres != "" && r.Genres != "[]" && r.Genres != "null" },
	},
	{
		name:       "Styles",
		applicable: func(_ string) bool { return true },
		present:    func(r CompletenessRow) bool { return r.Styles != "" && r.Styles != "[]" && r.Styles != "null" },
	},
	{
		name:       "Thumb image",
		applicable: func(_ string) bool { return true },
		present:    func(r CompletenessRow) bool { return r.HasThumb },
	},
	{
		name:       "Fanart image",
		applicable: func(_ string) bool { return true },
		present:    func(r CompletenessRow) bool { return r.HasFanart },
	},
	{
		name:       "Logo image",
		applicable: func(_ string) bool { return true },
		present:    func(r CompletenessRow) bool { return r.HasLogo },
	},
	{
		name:       "Banner image",
		applicable: func(_ string) bool { return true },
		present:    func(r CompletenessRow) bool { return r.HasBanner },
	},
	{
		// Formed date: applicable for groups, orchestras, and choirs, not solo persons.
		name: "Formed date",
		applicable: func(t string) bool {
			return t == "group" || t == "orchestra" || t == "choir" || t == ""
		},
		present: func(r CompletenessRow) bool { return r.Formed != "" },
	},
	{
		// Born date: applicable for individual persons (person, solo, character).
		name: "Born date",
		applicable: func(t string) bool {
			return t == "person" || t == "solo" || t == "character"
		},
		present: func(r CompletenessRow) bool { return r.Born != "" },
	},
	{
		// Disbanded date: applicable for groups, orchestras, and choirs.
		name: "Disbanded date",
		applicable: func(t string) bool {
			return t == "group" || t == "orchestra" || t == "choir" || t == ""
		},
		present: func(r CompletenessRow) bool { return r.Disbanded != "" },
	},
	{
		// Died date: applicable for individual persons (person, solo, character).
		name: "Died date",
		applicable: func(t string) bool {
			return t == "person" || t == "solo" || t == "character"
		},
		present: func(r CompletenessRow) bool { return r.Died != "" },
	},
}

// GetMetadataCompleteness computes the aggregate metadata completeness report.
// When libraryID is non-empty the report is scoped to that library. The report
// excludes artists marked is_excluded = 1 (scanner exclusion list entries).
//
// libNames maps library IDs to human-readable library names. It is used to
// populate LibraryCoverage entries. If a library ID has no entry in the map
// the library ID itself is used as the name. Pass nil or an empty map when
// library names are unavailable.
func (s *Service) GetMetadataCompleteness(ctx context.Context, libraryID string, lowestLimit int, libNames map[string]string) (*MetadataCompletenessReport, error) {
	if s.completeness == nil {
		return nil, fmt.Errorf("completeness repository not configured")
	}
	rows, err := s.completeness.GetCompletenessRows(ctx, libraryID)
	if err != nil {
		return nil, err
	}

	lowest, err := s.completeness.GetLowestCompleteness(ctx, libraryID, lowestLimit)
	if err != nil {
		return nil, err
	}

	report := buildCompletenessReport(rows, lowest)

	// Build per-library coverage when the request is not already scoped to a
	// single library and more than one library is present in the result set.
	if libraryID == "" {
		report.LibraryCoverage = buildLibraryCoverage(rows, libNames)
	}

	return report, nil
}

// buildCompletenessReport constructs the MetadataCompletenessReport from raw rows.
// It is a pure function with no I/O, which makes it straightforward to unit-test.
func buildCompletenessReport(rows []CompletenessRow, lowest []LowestCompletenessArtist) *MetadataCompletenessReport {
	report := &MetadataCompletenessReport{
		TotalArtists:       len(rows),
		LowestCompleteness: lowest,
	}
	if lowest == nil {
		report.LowestCompleteness = []LowestCompletenessArtist{}
	}

	if len(rows) == 0 {
		report.FieldCoverage = emptyFieldCoverage()
		return report
	}

	// Accumulate counts per field.
	counts := make([]int, len(allFieldDefs))
	totals := make([]int, len(allFieldDefs))

	for _, row := range rows {
		for i, fd := range allFieldDefs {
			if !fd.applicable(row.Type) {
				continue
			}
			totals[i]++
			if fd.present(row) {
				counts[i]++
			}
		}
	}

	// Build FieldCoverage slice (sorted lowest percentage first).
	coverage := make([]FieldCoverage, 0, len(allFieldDefs))
	for i, fd := range allFieldDefs {
		if totals[i] == 0 {
			// Field not applicable to any artist in this data set -- skip it.
			continue
		}
		pct := float64(counts[i]) / float64(totals[i]) * 100.0
		pct = roundOneDecimal(pct)
		coverage = append(coverage, FieldCoverage{
			Field:      fd.name,
			Count:      counts[i],
			Total:      totals[i],
			Percentage: pct,
		})
	}

	sortFieldCoverageAsc(coverage)
	report.FieldCoverage = coverage

	// Overall score is the mean across all applicable field/artist pairs.
	var totalCount, totalApplicable int
	for i := range coverage {
		totalCount += coverage[i].Count
		totalApplicable += coverage[i].Total
	}
	if totalApplicable > 0 {
		overall := float64(totalCount) / float64(totalApplicable) * 100.0
		report.OverallScore = roundOneDecimal(overall)
	}

	return report
}

// buildLibraryCoverage groups rows by library and produces a
// LibraryCompleteness entry for each library that contains at least one artist.
// When only one distinct library ID is present the slice is returned as-is;
// the caller is responsible for deciding whether to omit it.
func buildLibraryCoverage(rows []CompletenessRow, libNames map[string]string) []LibraryCompleteness {
	// Collect rows per library, preserving insertion order for determinism.
	type libEntry struct {
		rows []CompletenessRow
	}
	order := make([]string, 0)
	byLib := make(map[string]*libEntry)

	for _, row := range rows {
		lid := row.LibraryID
		if _, exists := byLib[lid]; !exists {
			order = append(order, lid)
			byLib[lid] = &libEntry{}
		}
		byLib[lid].rows = append(byLib[lid].rows, row)
	}

	// Only emit library_coverage when more than one library is present.
	if len(order) <= 1 {
		return nil
	}

	result := make([]LibraryCompleteness, 0, len(order))
	for _, lid := range order {
		entry := byLib[lid]
		sub := buildCompletenessReport(entry.rows, nil)

		name := lid
		if n, ok := libNames[lid]; ok && n != "" {
			name = n
		}

		result = append(result, LibraryCompleteness{
			LibraryID:    lid,
			LibraryName:  name,
			TotalArtists: sub.TotalArtists,
			Score:        sub.OverallScore,
			Fields:       sub.FieldCoverage,
		})
	}
	return result
}

// emptyFieldCoverage returns a zero-populated coverage slice for when the
// library has no artists, so callers get a consistently shaped response.
func emptyFieldCoverage() []FieldCoverage {
	coverage := make([]FieldCoverage, 0, len(allFieldDefs))
	// Only include universally applicable fields for the empty case.
	for _, fd := range allFieldDefs {
		if fd.applicable("") {
			coverage = append(coverage, FieldCoverage{
				Field:      fd.name,
				Count:      0,
				Total:      0,
				Percentage: 0,
			})
		}
	}
	return coverage
}

// sortFieldCoverageAsc sorts coverage entries by percentage ascending (worst
// coverage first). Stable ordering is maintained for equal percentages by
// using the field name as a tiebreaker so results are deterministic.
func sortFieldCoverageAsc(coverage []FieldCoverage) {
	for i := 0; i < len(coverage); i++ {
		for j := i + 1; j < len(coverage); j++ {
			if coverage[j].Percentage < coverage[i].Percentage ||
				(coverage[j].Percentage == coverage[i].Percentage && coverage[j].Field < coverage[i].Field) {
				coverage[i], coverage[j] = coverage[j], coverage[i]
			}
		}
	}
}

// roundOneDecimal rounds a float64 to one decimal place.
func roundOneDecimal(f float64) float64 {
	return math.Round(f*10) / 10
}
