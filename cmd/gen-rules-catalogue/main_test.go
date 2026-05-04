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

	if c := strings.Count(got, "No automated fix."); c != wantDetectionOnly {
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
// annotation appears for nfo_exists (the only rule in that set today).
func TestRenderCatalogue_FilesystemDependent(t *testing.T) {
	got := renderCatalogue(rule.DefaultRules())

	if !strings.Contains(got, "Filesystem-dependent:** Yes") {
		t.Error("expected filesystem-dependent annotation in output")
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
	if !strings.Contains(got, "No automated fix.") {
		t.Error("expected detection-only fix text")
	}
	if !strings.Contains(got, "100") {
		t.Error("expected resolution value in configurable block")
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
