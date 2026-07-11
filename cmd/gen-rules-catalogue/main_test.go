package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/rule"
)

// TestRenderCatalogue_AllRulesPresent verifies that every rule returned by
// DefaultRules() produces a heading in the rendered output.
func TestRenderCatalogue_AllRulesPresent(t *testing.T) {
	rules := rule.DefaultRules()
	got := renderCatalogue(rules)

	for _, r := range rules {
		// Each rule must have a ## heading.
		needle := "## " + r.Name
		if !strings.Contains(got, needle) {
			t.Errorf("expected heading %q for rule %s; not found in output", needle, r.ID)
		}
	}
}

// TestRenderCatalogue_AtAGlanceTable checks structural elements of the summary
// table.
func TestRenderCatalogue_AtAGlanceTable(t *testing.T) {
	got := renderCatalogue(rule.DefaultRules())

	if !strings.Contains(got, "## At a glance") {
		t.Error(`expected "## At a glance" heading`)
	}
	if !strings.Contains(got, "| Rule | Category | Default | Fixable |") {
		t.Error("expected table header row")
	}
	// All three category labels must appear.
	for _, cat := range []string{"NFO", "Metadata", "Image"} {
		if !strings.Contains(got, "| "+cat+" |") {
			t.Errorf("expected category %q in table", cat)
		}
	}
}

// TestRenderCatalogue_CategoryOrder verifies NFO rules appear before Metadata,
// which appears before Image (both in the table and the detail sections).
func TestRenderCatalogue_CategoryOrder(t *testing.T) {
	got := renderCatalogue(rule.DefaultRules())

	nfoIdx := strings.Index(got, "## NFO file exists")
	metaIdx := strings.Index(got, "## Biography exists")
	imgIdx := strings.Index(got, "## Thumbnail image exists")

	if nfoIdx < 0 || metaIdx < 0 || imgIdx < 0 {
		t.Fatalf("one or more expected rule headings not found (nfo=%d meta=%d img=%d)", nfoIdx, metaIdx, imgIdx)
	}
	if nfoIdx >= metaIdx || metaIdx >= imgIdx {
		t.Errorf("expected NFO < Metadata < Image order; nfo=%d meta=%d img=%d", nfoIdx, metaIdx, imgIdx)
	}
}

// TestRenderCatalogue_FixableVsDetectionOnly checks that the rendered "Fix"
// section count matches the number of detection-only rules in the registry,
// and that the at-a-glance table assigns the right "Fixable" label per rule.
//
// The previous form of this test only asserted that "No automated fix."
// appeared anywhere in the output, which would silently pass if a fixable
// rule regressed to detection-only as long as one detection-only rule still
// rendered. Counting equality catches per-rule rendering regressions.
func TestRenderCatalogue_FixableVsDetectionOnly(t *testing.T) {
	rules := rule.DefaultRules()
	got := renderCatalogue(rules)

	wantDetectionOnly := 0
	wantSometimes := 0
	wantYes := 0
	for _, r := range rules {
		entry := rule.CatalogueEntry(r.ID)
		switch {
		case entry.FixBehavior == "":
			wantDetectionOnly++
		case entry.Conditional:
			wantSometimes++
		default:
			wantYes++
		}
	}

	if c := strings.Count(got, "**Fix:** No automated fix."); c != wantDetectionOnly {
		t.Errorf("rendered detection-only count = %d, want %d", c, wantDetectionOnly)
	}
	// "| Sometimes |" and "| Yes |" appear in the at-a-glance table only
	// (one row per rule). Count both columns to verify the binary "any
	// non-empty FixBehavior is Yes" regression cannot recur.
	if c := strings.Count(got, "| Sometimes |"); c != wantSometimes {
		t.Errorf("rendered Sometimes count = %d, want %d", c, wantSometimes)
	}
	if c := strings.Count(got, "| Yes |"); c != wantYes {
		t.Errorf("rendered Yes count = %d, want %d", c, wantYes)
	}
}

// TestRenderCatalogue_FilesystemDependent verifies the filesystem-dependent
// annotation count matches the number of rules in DefaultRules() flagged with
// FilesystemDependent: true. A loose Contains check would silently pass even
// if the renderer dropped the annotation for some rules.
func TestRenderCatalogue_FilesystemDependent(t *testing.T) {
	rules := rule.DefaultRules()
	got := renderCatalogue(rules)

	want := 0
	for _, r := range rules {
		if r.FilesystemDependent {
			want++
		}
	}
	if c := strings.Count(got, "Filesystem-dependent:** Yes"); c != want {
		t.Errorf("filesystem-dependent annotation count = %d, want %d", c, want)
	}
}

// TestRenderCatalogue_FixtureSingleRule exercises the rendering of a single
// fixture rule to verify the per-rule section format independently of the real
// registry.
func TestRenderCatalogue_FixtureSingleRule(t *testing.T) {
	rules := []rule.Rule{
		{
			ID:          "test_rule",
			Name:        "Test rule",
			Description: "A test rule for unit testing.",
			Category:    rule.RuleCategoryNFO,
			Enabled:     true,
			Config:      rule.RuleConfig{Severity: "error", MinWidth: 100, MinHeight: 100},
		},
	}

	// Override the catalog entry for this fixture by testing the renderer
	// directly. Because CatalogueEntry("test_rule") returns a zero value,
	// the renderer will emit "No automated fix." -- which is the correct
	// detection-only path we want to exercise here.
	got := renderCatalogue(rules)

	if !strings.Contains(got, "## Test rule") {
		t.Error("expected ## Test rule heading")
	}
	if !strings.Contains(got, "**Category:** NFO") {
		t.Error("expected NFO category")
	}
	if !strings.Contains(got, "**Default:** Enabled, auto") {
		t.Error("expected Enabled, auto default state")
	}
	if !strings.Contains(got, "**Severity:** error") {
		t.Error("expected severity error")
	}
	if !strings.Contains(got, "A test rule for unit testing.") {
		t.Error("expected description")
	}
	if !strings.Contains(got, "**Fix:** No automated fix.") {
		t.Error("expected detection-only fix text")
	}
	if !strings.Contains(got, "Minimum resolution (default 100 &times; 100 px)") {
		t.Error("expected exact minimum resolution line in configurable block")
	}
}

// TestRenderCatalogue_GuardsAndExamples verifies that a rule with Guards and
// Examples renders both sections.
func TestRenderCatalogue_GuardsAndExamples(t *testing.T) {
	rules := rule.DefaultRules()
	got := renderCatalogue(rules)

	// NFO file exists has Guards and Examples set.
	if !strings.Contains(got, "## NFO file exists") {
		t.Fatal("expected ## NFO file exists heading")
	}
	// Guards paragraph should appear after the description.
	if !strings.Contains(got, "Media servers like Emby and Jellyfin read artist information") {
		t.Error("expected Guards prose for NFO file exists rule")
	}
	// When this fires block should appear.
	if !strings.Contains(got, "**When this fires:**") {
		t.Error("expected 'When this fires' heading")
	}
}

// TestRenderCatalogue_WhenFiresOmittedWhenNoExamples verifies that a detection-only
// fixture rule with no Examples does not emit the "When this fires" block.
func TestRenderCatalogue_WhenFiresOmittedWhenNoExamples(t *testing.T) {
	// Use a fixture rule with empty catalogue entry (no Guards, no Examples).
	rules := []rule.Rule{
		{
			ID:          "no_examples_rule",
			Name:        "No examples rule",
			Description: "A rule with no examples.",
			Category:    rule.RuleCategoryNFO,
			Enabled:     false,
			Config:      rule.RuleConfig{Severity: "info"},
		},
	}
	got := renderCatalogue(rules)

	if strings.Contains(got, "**When this fires:**") {
		t.Error("expected 'When this fires' block to be absent when Examples is empty")
	}
}

// TestRenderCatalogue_FixExampleOmittedWhenEmpty verifies that a fixable rule
// with no FixExample does not emit the fenced before/after block.
func TestRenderCatalogue_FixExampleOmittedWhenEmpty(t *testing.T) {
	rules := rule.DefaultRules()
	got := renderCatalogue(rules)

	// thumb_exists has FixBehavior but no FixExample (fetch-from-empty).
	// Verify the fix lead-in is present but no fenced block is adjacent.
	thumbFixIdx := strings.Index(got, "## Thumbnail image exists")
	if thumbFixIdx < 0 {
		t.Fatal("expected ## Thumbnail image exists heading")
	}
	thumbSection := got[thumbFixIdx:]
	// Find the next rule heading to bound the search.
	nextH2 := strings.Index(thumbSection[3:], "\n## ")
	var section string
	if nextH2 >= 0 {
		section = thumbSection[:nextH2+3]
	} else {
		section = thumbSection
	}
	if !strings.Contains(section, "**What the fix does:**") {
		t.Error("expected 'What the fix does' lead-in for thumb_exists")
	}
	// No fenced block should appear for this rule (fetch-from-empty: no before).
	if strings.Contains(section, "```\nBefore:") {
		t.Error("expected no fenced FixExample block for thumb_exists (fetch-from-empty)")
	}
}

// TestRenderCatalogue_FixExampleRenderedWhenPresent verifies that a rule with
// FixExample emits the fenced before/after block.
func TestRenderCatalogue_FixExampleRenderedWhenPresent(t *testing.T) {
	rules := rule.DefaultRules()
	got := renderCatalogue(rules)

	// nfo_has_mbid has a FixExample set.
	if !strings.Contains(got, "```\nBefore: artist.nfo has no <musicbrainzartistid> element") {
		t.Error("expected fenced FixExample block for nfo_has_mbid rule")
	}
}

// TestRenderCatalogue_DetectionOnlyOmitsBothFixSections verifies that a
// detection-only rule (no FixBehavior, no FixExample) omits both fix-related
// sections and renders the "No automated fix." text instead.
func TestRenderCatalogue_DetectionOnlyOmitsBothFixSections(t *testing.T) {
	rules := rule.DefaultRules()
	got := renderCatalogue(rules)

	// artist_id_mismatch is detection-only (no FixBehavior). image_duplicate,
	// the previous subject here, became fixable, so this test moved to a rule
	// that is still detection-only.
	detIdx := strings.Index(got, "## Artist/ID mismatch")
	if detIdx < 0 {
		t.Fatal("expected ## Artist/ID mismatch heading")
	}
	section := got[detIdx:]
	nextH2 := strings.Index(section[3:], "\n## ")
	if nextH2 >= 0 {
		section = section[:nextH2+3]
	}

	if strings.Contains(section, "**What the fix does:**") {
		t.Error("expected 'What the fix does' to be absent for detection-only rule")
	}
	if !strings.Contains(section, "**Fix:** No automated fix.") {
		t.Error("expected 'No automated fix.' for detection-only rule")
	}
	if strings.Contains(section, "```\nBefore:") {
		t.Error("expected no fenced FixExample block for detection-only rule")
	}
}

// TestRenderWhenFires exercises the renderWhenFires helper directly.
func TestRenderWhenFires(t *testing.T) {
	t.Run("empty examples", func(t *testing.T) {
		entry := rule.RuleCatalogueEntry{}
		got := renderWhenFires(entry)
		if got != "" {
			t.Errorf("expected empty string for empty Examples, got: %q", got)
		}
	})

	t.Run("single example", func(t *testing.T) {
		entry := rule.RuleCatalogueEntry{
			Examples: []string{"An artist with no thumbnail file."},
		}
		got := renderWhenFires(entry)
		if !strings.Contains(got, "**When this fires:**") {
			t.Error("expected 'When this fires' heading")
		}
		if !strings.Contains(got, "- An artist with no thumbnail file.") {
			t.Error("expected example bullet")
		}
	})

	t.Run("multiple examples", func(t *testing.T) {
		entry := rule.RuleCatalogueEntry{
			Examples: []string{"First example.", "Second example.", "Third example."},
		}
		got := renderWhenFires(entry)
		if strings.Count(got, "- ") != 3 {
			t.Errorf("expected 3 bullet items, got %d", strings.Count(got, "- "))
		}
	})
}

// TestRenderFixExample exercises the renderFixExample helper directly.
func TestRenderFixExample(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		entry := rule.RuleCatalogueEntry{}
		got := renderFixExample(entry)
		if got != "" {
			t.Errorf("expected empty string for empty FixExample, got: %q", got)
		}
	})

	t.Run("non-empty", func(t *testing.T) {
		entry := rule.RuleCatalogueEntry{
			FixExample: "Before: /music/National, The/\nAfter:  /music/The National/",
		}
		got := renderFixExample(entry)
		if !strings.Contains(got, "```\n") {
			t.Error("expected fenced code block opening")
		}
		if !strings.Contains(got, "Before: /music/National, The/") {
			t.Error("expected Before line in FixExample")
		}
		if !strings.Contains(got, "After:  /music/The National/") {
			t.Error("expected After line in FixExample")
		}
	})
}

// TestRenderCatalogue_AnchorPreservation verifies that rule heading anchors
// are byte-identical to the known set. The nameToAnchor function must not
// change without this test failing, since external links depend on these values.
func TestRenderCatalogue_AnchorPreservation(t *testing.T) {
	knownAnchors := map[string]string{
		"NFO file exists":                        "nfo-file-exists",
		"NFO has MusicBrainz ID":                 "nfo-has-musicbrainz-id",
		"Thumbnail image exists":                 "thumbnail-image-exists",
		"Thumbnail is square":                    "thumbnail-is-square",
		"Thumbnail minimum resolution":           "thumbnail-minimum-resolution",
		"Fanart image exists":                    "fanart-image-exists",
		"Logo image exists":                      "logo-image-exists",
		"Biography exists":                       "biography-exists",
		"Fanart minimum resolution":              "fanart-minimum-resolution",
		"Fanart aspect ratio":                    "fanart-aspect-ratio",
		"Logo minimum width":                     "logo-minimum-width",
		"Banner image exists":                    "banner-image-exists",
		"Banner minimum resolution":              "banner-minimum-resolution",
		"Extraneous image files":                 "extraneous-image-files",
		"Artist/ID mismatch":                     "artistid-mismatch",
		"Directory name matches artist":          "directory-name-matches-artist",
		"No duplicate images":                    "no-duplicate-images",
		"Metadata quality":                       "metadata-quality",
		"Backdrop/fanart sequencing":             "backdropfanart-sequencing",
		"Minimum backdrop count":                 "minimum-backdrop-count",
		"Logo excessive padding":                 "logo-excessive-padding",
		"Artist name matches preferred language": "artist-name-matches-preferred-language",
	}

	for name, want := range knownAnchors {
		got := nameToAnchor(name)
		if got != want {
			t.Errorf("nameToAnchor(%q) = %q; want %q", name, got, want)
		}
	}
}

// TestRenderCatalogue_AllRulesHaveGuards verifies that every rule in the
// default registry has a non-empty Guards string in its catalogue entry.
func TestRenderCatalogue_AllRulesHaveGuards(t *testing.T) {
	for _, r := range rule.DefaultRules() {
		entry := rule.CatalogueEntry(r.ID)
		if entry.Guards == "" {
			t.Errorf("rule %s (%s) has empty Guards field", r.ID, r.Name)
		}
	}
}

// TestRenderCatalogue_AllRulesHaveExamples verifies that every rule in the
// default registry has at least one entry in its Examples slice.
func TestRenderCatalogue_AllRulesHaveExamples(t *testing.T) {
	for _, r := range rule.DefaultRules() {
		entry := rule.CatalogueEntry(r.ID)
		if len(entry.Examples) == 0 {
			t.Errorf("rule %s (%s) has empty Examples slice", r.ID, r.Name)
		}
	}
}

// TestReplaceBetweenMarkers exercises the core replacement helper.
func TestReplaceBetweenMarkers(t *testing.T) {
	src := []byte("prefix\n" + beginMarker + "\nstale body\n" + endMarker + "\nsuffix\n")
	out, err := replaceBetweenMarkers(src, beginMarker, endMarker, "fresh body")
	if err != nil {
		t.Fatal(err)
	}
	want := "prefix\n" + beginMarker + "\nfresh body\n" + endMarker + "\nsuffix\n"
	if string(out) != want {
		t.Fatalf("unexpected output\nwant:\n%s\n\ngot:\n%s", want, string(out))
	}
}

func TestReplaceBetweenMarkers_MissingBegin(t *testing.T) {
	_, err := replaceBetweenMarkers([]byte("no markers here"), beginMarker, endMarker, "body")
	if err == nil {
		t.Fatal("expected error when begin marker is missing")
	}
}

func TestReplaceBetweenMarkers_MissingEnd(t *testing.T) {
	src := []byte("prefix " + beginMarker + " no end")
	_, err := replaceBetweenMarkers(src, beginMarker, endMarker, "body")
	if err == nil {
		t.Fatal("expected error when end marker is missing")
	}
}

// TestRun_CheckMode verifies the -check mode detects staleness and accepts
// a fresh file.
func TestRun_CheckMode(t *testing.T) {
	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "rules-catalogue.md")

	// Seed the file with markers and stale content.
	seed := "intro\n" + beginMarker + "\nstale\n" + endMarker + "\n"
	if err := os.WriteFile(outFile, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write mode should fill the markers.
	if err := run(outFile, false); err != nil {
		t.Fatalf("run (write): %v", err)
	}

	// Check mode on a fresh file should pass.
	if err := run(outFile, true); err != nil {
		t.Fatalf("run -check on fresh file: %v", err)
	}

	// Manually stale the file by writing back the seed content.
	if err := os.WriteFile(outFile, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// Check mode must return an error now.
	if err := run(outFile, true); err == nil {
		t.Fatal("expected error from -check on stale file")
	}
}

// TestNameToAnchor exercises the MkDocs heading anchor helper.
func TestNameToAnchor(t *testing.T) {
	cases := []struct{ name, want string }{
		{"NFO file exists", "nfo-file-exists"},
		{"Artist/ID mismatch", "artistid-mismatch"},
		{"Backdrop/fanart sequencing", "backdropfanart-sequencing"},
		{"No duplicate images", "no-duplicate-images"},
		{"Logo excessive padding", "logo-excessive-padding"},
	}
	for _, c := range cases {
		got := nameToAnchor(c.name)
		if got != c.want {
			t.Errorf("nameToAnchor(%q) = %q; want %q", c.name, got, c.want)
		}
	}
}
