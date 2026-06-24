package api

import (
	"sort"
	"testing"

	"github.com/sydlexius/stillwater/internal/rule"
)

// TestNonFieldViolations pins the next/ "Other findings" filter (#1860): the
// residual card lists ONLY findings whose rule does not map to a metadata field
// (rule.RuleFields empty). Field-mapped rules render as inline chips and must be
// excluded so they are not duplicated. The set of field-mapped rules is the five
// that carry a Fields tag in the catalogue; every other rule (image +
// whole-record/structural) is retained.
func TestNonFieldViolations(t *testing.T) {
	t.Parallel()

	// The five field-mapped rules -- each must be filtered OUT.
	fieldMapped := []string{
		rule.RuleBioExists,
		rule.RuleMetadataQuality,
		rule.RuleNameLanguagePref,
		rule.RuleOriginMissing,
		rule.RuleNFOHasMBID,
	}
	// A representative spread of non-field rules (image + structural) -- each must
	// be RETAINED.
	nonField := []string{
		rule.RuleNFOExists,
		rule.RuleDirectoryNameMismatch,
		rule.RuleArtistIDMismatch,
		rule.RuleDiscographyPopulated,
		rule.RuleThumbExists,
		rule.RuleFanartMinRes,
		rule.RuleLogoPadding,
		rule.RuleImageDuplicate,
	}

	// Guard the premise: the five "field-mapped" rules really do carry field tags,
	// and the "non-field" ones really do not. This catches a catalogue drift that
	// would silently move a rule between buckets.
	for _, id := range fieldMapped {
		if len(rule.RuleFields(id)) == 0 {
			t.Fatalf("precondition: rule %q expected to be field-mapped but RuleFields is empty", id)
		}
	}
	for _, id := range nonField {
		if len(rule.RuleFields(id)) != 0 {
			t.Fatalf("precondition: rule %q expected to be non-field but RuleFields = %v", id, rule.RuleFields(id))
		}
	}

	var in []rule.RuleViolation
	for _, id := range append(append([]string{}, fieldMapped...), nonField...) {
		in = append(in, rule.RuleViolation{ID: "v-" + id, RuleID: id})
	}

	got := nonFieldViolations(in)

	gotIDs := make([]string, 0, len(got))
	for i := range got {
		gotIDs = append(gotIDs, got[i].RuleID)
	}
	sort.Strings(gotIDs)
	want := append([]string{}, nonField...)
	sort.Strings(want)

	if len(gotIDs) != len(want) {
		t.Fatalf("nonFieldViolations kept %d rules %v, want %d %v", len(gotIDs), gotIDs, len(want), want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("nonFieldViolations result[%d] = %q, want %q (full: %v)", i, gotIDs[i], want[i], gotIDs)
		}
	}

	// Empty input yields a non-nil empty slice (the template's empty-state branch
	// keys off len, and a nil vs empty distinction must not leak).
	if out := nonFieldViolations(nil); out == nil {
		t.Errorf("nonFieldViolations(nil) = nil, want non-nil empty slice")
	}
}
