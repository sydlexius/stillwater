package rule

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestCatalogue_AllDefaultRulesPresent locks the documentation catalogue to
// the rule registry: every rule returned by DefaultRules() must have an
// explicit entry in rulesCatalogue. Without this, a newly registered rule
// silently falls through to the zero-value RuleCatalogueEntry, and the
// generated reference page would document it as detection-only with no
// human review.
//
// To intentionally mark a rule as detection-only, add an entry with
// FixBehavior: "" -- the explicit entry is what this test enforces.
func TestCatalogue_AllDefaultRulesPresent(t *testing.T) {
	for _, r := range DefaultRules() {
		if !CatalogueEntryPresent(r.ID) {
			t.Errorf("rule %q has no entry in rulesCatalogue (add one in internal/rule/catalogue.go)", r.ID)
		}
	}
}

// TestCatalogue_NoOrphanedEntries flags catalogue entries whose rule ID is
// no longer registered. Stale entries are documentation-only debt but make
// the at-a-glance numbers wrong if anything ever counts the catalogue map.
func TestCatalogue_NoOrphanedEntries(t *testing.T) {
	known := make(map[string]struct{}, len(DefaultRules()))
	for _, r := range DefaultRules() {
		known[r.ID] = struct{}{}
	}
	for id := range rulesCatalogue {
		if _, ok := known[id]; !ok {
			t.Errorf("rulesCatalogue has entry for %q which is not in DefaultRules()", id)
		}
	}
}

// TestRuleFields_MetadataRulesTagged locks the field-tag-on-rule mapping the
// artist-detail page reads (M55 #1336). The metadata rules that inspect a
// single artist-detail field must carry that field key; image and
// whole-record / cross-field rules must carry none so they stay out of the
// inline-chip path and surface only in the Open Findings list.
func TestRuleFields_MetadataRulesTagged(t *testing.T) {
	want := map[string][]string{
		RuleBioExists:        {"biography"},
		RuleMetadataQuality:  {"biography"},
		RuleOriginMissing:    {"origin"},
		RuleNFOHasMBID:       {"musicbrainz_id"},
		RuleNameLanguagePref: {"name", "sort_name"},
	}
	for id, exp := range want {
		got := RuleFields(id)
		if len(got) != len(exp) {
			t.Errorf("RuleFields(%q) = %v, want %v", id, got, exp)
			continue
		}
		for i := range exp {
			if got[i] != exp[i] {
				t.Errorf("RuleFields(%q)[%d] = %q, want %q", id, i, got[i], exp[i])
			}
		}
	}
	// Image and whole-record rules carry no field tag.
	for _, id := range []string{RuleThumbExists, RuleFanartExists, RuleLogoExists, RuleNFOExists, RuleDiscographyPopulated} {
		if got := RuleFields(id); len(got) != 0 {
			t.Errorf("RuleFields(%q) = %v, want no field tag", id, got)
		}
	}
}

// TestRuleFields_AllKeysResolvable guards against a typo in a rule's Fields tag
// silently dropping a chip. Every field key any catalogue entry advertises must
// be a real artist-detail field key -- one that artist.FieldValueFromArtist
// resolves. A misspelled key ("biograhy") renders no chip and never errors at
// runtime; this test turns that into a build failure. Tying validity to the
// canonical resolver (rather than a hand-kept allowlist) means the check tracks
// the field set automatically.
func TestRuleFields_AllKeysResolvable(t *testing.T) {
	// A fully-populated artist: every scalar/slice field set to a non-empty
	// sentinel so FieldValueFromArtist returns "" ONLY for an unrecognized key.
	probe := &artist.Artist{
		Biography:      "b",
		Genres:         []string{"g"},
		Styles:         []string{"s"},
		Moods:          []string{"m"},
		Formed:         "f",
		Born:           "bo",
		Disbanded:      "d",
		Died:           "di",
		YearsActive:    "y",
		Type:           "group",
		Gender:         "female",
		Origin:         "o",
		Name:           "n",
		SortName:       "sn",
		Disambiguation: "da",
		MusicBrainzID:  "mb",
		AudioDBID:      "ad",
		DiscogsID:      "dc",
		WikidataID:     "wd",
		DeezerID:       "dz",
	}
	for id, entry := range rulesCatalogue {
		for _, f := range entry.Fields {
			if artist.FieldValueFromArtist(probe, f) == "" {
				t.Errorf("rule %q tags field key %q which artist.FieldValueFromArtist does not resolve (typo? rename?)", id, f)
			}
		}
	}
}
