package templates

import (
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/rule"
)

func TestImageSubtypeLabel(t *testing.T) {
	ctx := testCtx(t)
	cases := []struct {
		ruleID string
		want   string
	}{
		{"thumb_exists", "Thumbnail"},
		{"thumb_min_res", "Thumbnail"},
		{"thumb_square", "Thumbnail"},
		{"fanart_exists", "Fanart"},
		{"fanart_min_res", "Fanart"},
		{"fanart_aspect", "Fanart"},
		{"logo_exists", "Logo"},
		{"logo_min_res", "Logo"},
		{"logo_padding", "Logo"},
		{"banner_exists", "Banner"},
		{"banner_min_res", "Banner"},
		{"backdrop_sequencing", "Backdrop"},
		{"backdrop_min_count", "Backdrop"},
		{"extraneous_images", "General"},
		{"image_duplicate", "General"},
		{"unknown_rule", "General"},
		{"", "General"},
	}
	for _, tc := range cases {
		got := imageSubtypeLabel(ctx, tc.ruleID)
		if got != tc.want {
			t.Errorf("imageSubtypeLabel(%q) = %q; want %q", tc.ruleID, got, tc.want)
		}
	}
}

func TestRulesForImageSubtype(t *testing.T) {
	rules := []rule.Rule{
		{ID: "thumb_exists"},
		{ID: "thumb_square"},
		{ID: "fanart_exists"},
		{ID: "logo_exists"},
		{ID: "banner_exists"},
		{ID: "backdrop_sequencing"},
		{ID: "extraneous_images"},
		{ID: "image_duplicate"},
	}

	cases := []struct {
		prefix  string
		wantIDs []string
	}{
		{"thumb_", []string{"thumb_exists", "thumb_square"}},
		{"fanart_", []string{"fanart_exists"}},
		{"logo_", []string{"logo_exists"}},
		{"banner_", []string{"banner_exists"}},
		{"backdrop_", []string{"backdrop_sequencing"}},
		{"nfo_", nil},
	}

	for _, tc := range cases {
		got := rulesForImageSubtype(rules, tc.prefix)
		if len(got) != len(tc.wantIDs) {
			t.Errorf("rulesForImageSubtype(rules, %q) returned %d rules; want %d", tc.prefix, len(got), len(tc.wantIDs))
			continue
		}
		for i, r := range got {
			if r.ID != tc.wantIDs[i] {
				t.Errorf("rulesForImageSubtype(rules, %q)[%d].ID = %q; want %q", tc.prefix, i, r.ID, tc.wantIDs[i])
			}
		}
	}
}

func TestRulesForImageSubtypeFallback(t *testing.T) {
	rules := []rule.Rule{
		{ID: "thumb_exists"},
		{ID: "fanart_exists"},
		{ID: "logo_exists"},
		{ID: "banner_exists"},
		{ID: "backdrop_sequencing"},
		{ID: "extraneous_images"},
		{ID: "image_duplicate"},
	}

	got := rulesForImageSubtypeFallback(rules)
	wantIDs := []string{"extraneous_images", "image_duplicate"}

	if len(got) != len(wantIDs) {
		t.Fatalf("rulesForImageSubtypeFallback returned %d rules; want %d", len(got), len(wantIDs))
	}
	for i, r := range got {
		if r.ID != wantIDs[i] {
			t.Errorf("rulesForImageSubtypeFallback[%d].ID = %q; want %q", i, r.ID, wantIDs[i])
		}
	}
}

func TestRulesForImageSubtypeFallback_AllRecognized(t *testing.T) {
	// Every rule has a recognized prefix; fallback must be empty so the
	// "General" sub-heading does not render with zero rows under it.
	rules := []rule.Rule{
		{ID: "thumb_exists"},
		{ID: "fanart_exists"},
		{ID: "logo_exists"},
		{ID: "banner_exists"},
		{ID: "backdrop_sequencing"},
	}
	if got := rulesForImageSubtypeFallback(rules); len(got) != 0 {
		t.Errorf("rulesForImageSubtypeFallback returned %d rules; want 0", len(got))
	}
}

func TestRulesForImageSubtypeFallback_EmptyInput(t *testing.T) {
	if got := rulesForImageSubtypeFallback(nil); len(got) != 0 {
		t.Errorf("rulesForImageSubtypeFallback(nil) returned %d rules; want 0", len(got))
	}
	if got := rulesForImageSubtypeFallback([]rule.Rule{}); len(got) != 0 {
		t.Errorf("rulesForImageSubtypeFallback([]rule.Rule{}) returned %d rules; want 0", len(got))
	}
}

// TestImageRuleCatalogue_AllOnAllowList walks the canonical rule catalogue
// and asserts every image-category rule ID either matches a known sub-type
// prefix (renders under Thumbnail/Fanart/Logo/Banner/Backdrop) or appears on
// the intentional "General" allow-list. A new image rule with an unknown
// prefix would otherwise silently fall through to "General" without anyone
// noticing the sub-grouping needs an update.
func TestImageRuleCatalogue_AllOnAllowList(t *testing.T) {
	knownPrefixes := imageSubtypePrefixes
	intentionalGeneral := map[string]bool{
		"extraneous_images": true,
		"image_duplicate":   true,
	}

	for _, r := range rule.DefaultRules() {
		if r.Category != rule.RuleCategoryImage {
			continue
		}
		matched := false
		for _, pfx := range knownPrefixes {
			if strings.HasPrefix(r.ID, pfx) {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if !intentionalGeneral[r.ID] {
			t.Errorf("image rule %q has no recognized sub-type prefix and is not on the General allow-list; either add a prefix to knownPrefixes in settings.templ or add the ID to intentionalGeneral in this test", r.ID)
		}
	}
}
