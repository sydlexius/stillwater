package templates

// settings_s2_golden_test.go -- byte-identical pre/post extraction gate for
// M55 issue #1809 (S2 Data: extract the stable Providers + Rules tab cards into
// shared Section* templ funcs).
//
// Phase 1 (pre-extraction): render SettingsPage for each fixture and write the
// HTML to /tmp/m55-1809-s2/before_<n>.html (TestS2_WriteBeforeHTML).
// Phase 2 (post-extraction): run the same fixtures, write after_<n>.html
// (TestS2_WriteAfterHTML).  The diff between before and after MUST be empty for
// every fixture (TestS2_DiffBeforeAfter).
//
// The committed per-section golden tests (TestSection*_S2_Golden) render each
// extracted Section* func in isolation and compare against
// web/templates/testdata/section_*.golden.html.  Generate them with -update.
//
// This S2 slice is a PURE behavior-preserving extraction: the markup is moved
// verbatim, no a11y/i18n/logic changes.  The per-section goldens therefore
// capture exactly the same bytes the inline cards produced.
//
// Shared test helpers (updateGolden flag, goldenPath, checkOrUpdateGolden,
// s1GoldenDir, testCtx) are defined in settings_s1_golden_test.go /
// helpers_test.go and reused here.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/tagdict"
	"github.com/sydlexius/stillwater/internal/rule"
)

// s2TempDir is where the before/after full-page renders live.  These are
// ephemeral artifacts used only for the extraction byte-diff and are NOT
// committed to git.
const s2TempDir = "/tmp/m55-1809-s2"

// twoProviderKeys provides populated ProviderKeys data for the Provider Keys
// card.  Two providers with differing access tiers / key state exercise the
// ProviderKeyCard branches.
var twoProviderKeys = []provider.ProviderKeyStatus{
	{
		Name:        "musicbrainz",
		DisplayName: "MusicBrainz",
		RequiresKey: false,
		HasKey:      false,
		Status:      "not_required",
		AccessTier:  provider.TierFree,
	},
	{
		Name:        "fanart",
		DisplayName: "Fanart.tv",
		RequiresKey: true,
		HasKey:      true,
		Status:      "ok",
		AccessTier:  provider.TierFreeKey,
	},
}

// twoWebSearchProviders provides populated WebSearchProviders data.
var twoWebSearchProviders = []provider.WebSearchProviderStatus{
	{Name: "duckduckgo", DisplayName: "DuckDuckGo", Enabled: true},
	{Name: "qwant", DisplayName: "Qwant", Enabled: false},
}

// twoPriorities provides populated Priorities data for the Provider Priorities
// card.
var twoPriorities = []provider.FieldPriority{
	{Field: "genres", Providers: []provider.ProviderName{"musicbrainz", "fanart"}},
	{Field: "images", Providers: []provider.ProviderName{"fanart"}, Disabled: []provider.ProviderName{"musicbrainz"}},
}

// vocabFull is a populated VocabConfig for the Tag Sources card.
var vocabFull = &tagdict.VocabConfig{
	Exclude:   []string{"christian*", "holiday"},
	MaxGenres: 5,
	MaxStyles: 3,
	MaxMoods:  4,
}

// ruleNFO is an NFO-category rule (not filesystem-dependent).
var ruleNFO = rule.Rule{
	ID:             "nfo-present",
	Name:           "NFO present",
	Description:    "Artist NFO file exists",
	Category:       rule.RuleCategoryNFO,
	Enabled:        true,
	AutomationMode: "manual",
}

// ruleImageThumb is an image-category rule in the thumb_ subtype, marked
// filesystem-dependent so it exercises the requires-local help gating.
var ruleImageThumb = rule.Rule{
	ID:                  "thumb_present",
	Name:                "Thumb present",
	Description:         "Thumb artwork exists",
	Category:            rule.RuleCategoryImage,
	Enabled:             true,
	AutomationMode:      "auto",
	FilesystemDependent: true,
}

// ruleImageGeneral is an image-category rule that does NOT match any subtype
// prefix, exercising the subtype-fallback grouping.
var ruleImageGeneral = rule.Rule{
	ID:             "image-quality",
	Name:           "Image quality",
	Description:    "Artwork meets minimum dimensions",
	Category:       rule.RuleCategoryImage,
	Enabled:        false,
	AutomationMode: "manual",
}

// ruleMetadata is a metadata-category rule.
var ruleMetadata = rule.Rule{
	ID:             "genres-present",
	Name:           "Genres present",
	Description:    "Artist has at least one genre",
	Category:       rule.RuleCategoryMetadata,
	Enabled:        true,
	AutomationMode: "auto",
}

// allCategoryRules spans all three categories plus an image subtype and an image
// fallback rule, so the SectionRules loop renders all category cards and both
// image sub-groupings.
var allCategoryRules = []rule.Rule{ruleNFO, ruleImageThumb, ruleImageGeneral, ruleMetadata}

// s2Fixtures returns the full set of SettingsData fixtures exercising every
// conditional branch in the 8 extracted Data cards.
func s2Fixtures() []struct {
	name string
	data SettingsData
} {
	return []struct {
		name string
		data SettingsData
	}{
		{
			// Fixture 0: Providers tab -- all three conditional cards populated,
			// romanization on, vocab populated, non-zero similarity threshold.
			name: "providers-all-populated-romanization-on",
			data: SettingsData{
				ActiveTab:                TabProviders,
				ProviderKeys:             twoProviderKeys,
				WebSearchProviders:       twoWebSearchProviders,
				Priorities:               twoPriorities,
				MetadataLanguages:        []string{"en", "ja"},
				NameRomanizationFallback: true,
				NameSimilarityThreshold:  85,
				VocabConfig:              vocabFull,
			},
		},
		{
			// Fixture 1: Providers tab -- all three conditional cards EMPTY
			// (provider keys / web search / priorities), romanization off, nil
			// vocab, zero threshold, empty languages.
			name: "providers-all-empty-romanization-off",
			data: SettingsData{
				ActiveTab:                TabProviders,
				ProviderKeys:             nil,
				WebSearchProviders:       nil,
				Priorities:               nil,
				MetadataLanguages:        nil,
				NameRomanizationFallback: false,
				NameSimilarityThreshold:  0,
				VocabConfig:              nil,
			},
		},
		{
			// Fixture 2: Rules tab -- rules across nfo/image/metadata with an
			// FS-dependent image rule and HasLocalLibrary false (exercises the
			// fsHelp + severityHelp first-occurrence gating), schedule = 60.
			name: "rules-all-categories-no-local-lib-schedule-60",
			data: SettingsData{
				ActiveTab:           TabRules,
				Rules:               allCategoryRules,
				HasLocalLibrary:     false,
				RuleScheduleMinutes: 60,
			},
		},
		{
			// Fixture 3: Rules tab -- empty rules (empty-state card), schedule
			// disabled (0).
			name: "rules-empty-schedule-disabled",
			data: SettingsData{
				ActiveTab:           TabRules,
				Rules:               nil,
				HasLocalLibrary:     false,
				RuleScheduleMinutes: 0,
			},
		},
		{
			// Fixture 4: Rules tab -- image rules with subtype + fallback, with
			// HasLocalLibrary true (FS-dependent rule available), schedule daily
			// (1440).
			name: "rules-image-subtypes-local-lib-schedule-daily",
			data: SettingsData{
				ActiveTab:           TabRules,
				Rules:               []rule.Rule{ruleImageThumb, ruleImageGeneral},
				HasLocalLibrary:     true,
				RuleScheduleMinutes: 1440,
			},
		},
	}
}

// TestS2_WriteBeforeHTML renders SettingsPage for every s2Fixture and writes
// before_<n>.html to s2TempDir.  Run BEFORE the extraction.
func TestS2_WriteBeforeHTML(t *testing.T) {
	if err := os.MkdirAll(s2TempDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", s2TempDir, err)
	}
	ctx := testCtx(t)
	for i, fx := range s2Fixtures() {
		var buf bytes.Buffer
		if err := SettingsPage(AssetPaths{}, fx.data).Render(ctx, &buf); err != nil {
			t.Fatalf("fixture %d %q render: %v", i, fx.name, err)
		}
		out := filepath.Join(s2TempDir, fmt.Sprintf("before_%d.html", i))
		if err := os.WriteFile(out, buf.Bytes(), 0644); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
		t.Logf("wrote %s (%d bytes)", out, buf.Len())
	}
}

// TestS2_WriteAfterHTML renders SettingsPage for every s2Fixture and writes
// after_<n>.html to s2TempDir.  Run AFTER the extraction.
func TestS2_WriteAfterHTML(t *testing.T) {
	if err := os.MkdirAll(s2TempDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", s2TempDir, err)
	}
	ctx := testCtx(t)
	for i, fx := range s2Fixtures() {
		var buf bytes.Buffer
		if err := SettingsPage(AssetPaths{}, fx.data).Render(ctx, &buf); err != nil {
			t.Fatalf("fixture %d %q render: %v", i, fx.name, err)
		}
		out := filepath.Join(s2TempDir, fmt.Sprintf("after_%d.html", i))
		if err := os.WriteFile(out, buf.Bytes(), 0644); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
		t.Logf("wrote %s (%d bytes)", out, buf.Len())
	}
}

// TestS2_DiffBeforeAfter compares every before/after pair and fails if any
// differ.  This is the byte-identical acceptance gate for the extraction.
func TestS2_DiffBeforeAfter(t *testing.T) {
	for i := range s2Fixtures() {
		before := filepath.Join(s2TempDir, fmt.Sprintf("before_%d.html", i))
		after := filepath.Join(s2TempDir, fmt.Sprintf("after_%d.html", i))

		bData, err := os.ReadFile(before)
		if err != nil {
			t.Skipf("fixture %d: before file missing (%v) -- run TestS2_WriteBeforeHTML first", i, err)
			continue
		}
		aData, err := os.ReadFile(after)
		if err != nil {
			t.Skipf("fixture %d: after file missing (%v) -- run TestS2_WriteAfterHTML first", i, err)
			continue
		}
		if !bytes.Equal(bData, aData) {
			diffPos := -1
			for j := 0; j < len(bData) && j < len(aData); j++ {
				if bData[j] != aData[j] {
					diffPos = j
					break
				}
			}
			t.Errorf("fixture %d differs (first diff at byte %d, before len=%d, after len=%d)",
				i, diffPos, len(bData), len(aData))
		}
	}
}

// --- Per-section golden tests ------------------------------------------------

func TestSectionProviderKeys_Populated_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{ProviderKeys: twoProviderKeys}
	var buf bytes.Buffer
	if err := SectionProviderKeys(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "provider_keys_populated", buf.String())
}

func TestSectionProviderKeys_Empty_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{ProviderKeys: nil}
	var buf bytes.Buffer
	if err := SectionProviderKeys(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "provider_keys_empty", buf.String())
}

func TestSectionWebSearch_Populated_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{WebSearchProviders: twoWebSearchProviders}
	var buf bytes.Buffer
	if err := SectionWebSearch(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "web_search_populated", buf.String())
}

func TestSectionWebSearch_Empty_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{WebSearchProviders: nil}
	var buf bytes.Buffer
	if err := SectionWebSearch(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "web_search_empty", buf.String())
}

func TestSectionProviderPriorities_Populated_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Priorities: twoPriorities, ProviderKeys: twoProviderKeys}
	var buf bytes.Buffer
	if err := SectionProviderPriorities(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "provider_priorities_populated", buf.String())
}

func TestSectionProviderPriorities_Empty_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Priorities: nil}
	var buf bytes.Buffer
	if err := SectionProviderPriorities(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "provider_priorities_empty", buf.String())
}

func TestSectionMetadataLanguages_RomanizationOn_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{MetadataLanguages: []string{"en", "ja"}, NameRomanizationFallback: true}
	var buf bytes.Buffer
	if err := SectionMetadataLanguages(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "metadata_languages_romanization_on", buf.String())
}

func TestSectionMetadataLanguages_RomanizationOff_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{MetadataLanguages: nil, NameRomanizationFallback: false}
	var buf bytes.Buffer
	if err := SectionMetadataLanguages(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "metadata_languages_romanization_off", buf.String())
}

func TestSectionAdvanced_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{NameSimilarityThreshold: 85}
	var buf bytes.Buffer
	if err := SectionAdvanced(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "advanced_name_similarity", buf.String())
}

func TestSectionTagSources_Populated_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{VocabConfig: vocabFull}
	var buf bytes.Buffer
	if err := SectionTagSources(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "tag_sources_populated", buf.String())
}

func TestSectionTagSources_Empty_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{VocabConfig: nil}
	var buf bytes.Buffer
	if err := SectionTagSources(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "tag_sources_empty", buf.String())
}

func TestSectionRules_AllCategoriesNoLocalLib_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Rules: allCategoryRules, HasLocalLibrary: false}
	var buf bytes.Buffer
	if err := SectionRules(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "rules_all_categories_no_local_lib", buf.String())
}

func TestSectionRules_ImageSubtypesLocalLib_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Rules: []rule.Rule{ruleImageThumb, ruleImageGeneral}, HasLocalLibrary: true}
	var buf bytes.Buffer
	if err := SectionRules(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "rules_image_subtypes_local_lib", buf.String())
}

func TestSectionRules_Empty_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Rules: nil}
	var buf bytes.Buffer
	if err := SectionRules(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "rules_empty", buf.String())
}

func TestSectionRuleSchedule_Hour_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{RuleScheduleMinutes: 60}
	var buf bytes.Buffer
	if err := SectionRuleSchedule(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "rule_schedule_hour", buf.String())
}

func TestSectionRuleSchedule_Disabled_S2_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{RuleScheduleMinutes: 0}
	var buf bytes.Buffer
	if err := SectionRuleSchedule(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "rule_schedule_disabled", buf.String())
}
