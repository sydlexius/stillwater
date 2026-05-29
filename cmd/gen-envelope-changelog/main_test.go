package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseVersionEntries_RealSourceFile verifies that the parser can
// extract all envelope-version entries from the actual source file it
// targets in production. It confirms that:
//   - at least one entry is returned,
//   - version "1.0" (the original) is present,
//   - version "1.5" (the current highest) is present (closes #1710), and
//   - every entry has a non-empty description.
func TestParseVersionEntries_RealSourceFile(t *testing.T) {
	// The test runs from the package directory; walk up to the repo root.
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read source file %s: %v", srcPath, err)
	}

	entries, err := parseVersionEntries(raw)
	if err != nil {
		t.Fatalf("parseVersionEntries: %v", err)
	}

	if len(entries) == 0 {
		t.Fatal("expected at least one version entry, got none")
	}

	// Build a version -> description map for easy lookup.
	byVersion := make(map[string]string, len(entries))
	for _, e := range entries {
		byVersion[e.Version] = e.Description
	}

	// All entries must have non-empty descriptions.
	for _, e := range entries {
		if strings.TrimSpace(e.Description) == "" {
			t.Errorf("version %q has an empty description", e.Version)
		}
	}

	// Spot-check known versions.
	for _, want := range []string{"1.0", "1.5"} {
		if _, ok := byVersion[want]; !ok {
			t.Errorf("expected version %q to be present in parsed entries", want)
		}
	}

	// "1.5" is the current envelope version; the list must not stop at 1.3
	// (the drift documented in issue #1710).
	if _, ok := byVersion["1.4"]; !ok {
		t.Error("expected version 1.4 to be present; list appears truncated")
	}
}

// TestParseVersionEntries_WellFormed verifies the parser against a minimal
// synthetic source snippet that covers the expected comment format.
func TestParseVersionEntries_WellFormed(t *testing.T) {
	src := []byte(`
// CurrentEnvelopeVersion is the version emitted by Export.
//   - "1.0": original format (settings, connections)
//   - "1.1": adds rules and user preferences
//   - "2.0": hypothetical future major bump with
//     multi-line continuation text
const CurrentEnvelopeVersion = "2.0"
`)

	entries, err := parseVersionEntries(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Check each entry.
	cases := []struct {
		ver  string
		desc string
	}{
		{"1.0", "original format (settings, connections)"},
		{"1.1", "adds rules and user preferences"},
		{"2.0", "hypothetical future major bump with multi-line continuation text"},
	}
	for i, tc := range cases {
		if entries[i].Version != tc.ver {
			t.Errorf("entry[%d].Version = %q, want %q", i, entries[i].Version, tc.ver)
		}
		if got := strings.TrimSpace(entries[i].Description); got != tc.desc {
			t.Errorf("entry[%d].Description = %q, want %q", i, got, tc.desc)
		}
	}
}

// TestParseVersionEntries_EmptyBlock confirms that a source file with
// no doc-comment before the const returns an error rather than silently
// emitting an empty list.
func TestParseVersionEntries_EmptyBlock(t *testing.T) {
	src := []byte(`
const CurrentEnvelopeVersion = "1.0"
`)
	_, err := parseVersionEntries(src)
	if err == nil {
		t.Fatal("expected an error for a missing doc-comment block, got nil")
	}
}

// TestParseVersionEntries_NoBullets confirms that a doc-comment that exists
// but contains no version bullet items returns an error.
func TestParseVersionEntries_NoBullets(t *testing.T) {
	src := []byte(`
// CurrentEnvelopeVersion is the version emitted by Export.
// It does not have any bullet entries.
const CurrentEnvelopeVersion = "1.0"
`)
	_, err := parseVersionEntries(src)
	if err == nil {
		t.Fatal("expected an error for a doc-comment with no bullet entries, got nil")
	}
}

// TestParseVersionEntries_ConstNotFound confirms the parser returns an error
// when the source file does not contain the expected constant.
func TestParseVersionEntries_ConstNotFound(t *testing.T) {
	src := []byte(`
// some other constant
const SomethingElse = "1.0"
`)
	_, err := parseVersionEntries(src)
	if err == nil {
		t.Fatal("expected an error when const CurrentEnvelopeVersion is absent, got nil")
	}
}

// TestRenderList verifies that renderList produces the expected Markdown
// bulleted-list output for a small set of entries.
func TestRenderList(t *testing.T) {
	entries := []versionEntry{
		{Version: "1.0", Description: "original format"},
		{Version: "1.1", Description: "adds rules"},
	}

	got := renderList(entries)
	want := "- `1.0`: original format\n- `1.1`: adds rules\n"
	if got != want {
		t.Errorf("renderList output mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestRun_Idempotent verifies that calling run twice produces no diff:
// the second call must detect that the file is already up to date and
// skip the write, leaving the content unchanged.
func TestRun_Idempotent(t *testing.T) {
	// Use the actual source file so the test reflects real content.
	srcPath := filepath.Join("..", "..", defaultSourcePath)

	tmpDir := t.TempDir()
	outPath := filepath.Join(tmpDir, "envelope-versions.md")

	// First run: should write the file.
	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("first run: %v", err)
	}

	content1, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read after first run: %v", err)
	}

	// Second run: should be a no-op.
	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("second run: %v", err)
	}

	content2, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}

	if string(content1) != string(content2) {
		t.Errorf("content changed between runs (not idempotent):\nfirst:  %q\nsecond: %q", content1, content2)
	}
}

// TestRun_CheckMode verifies that -check mode returns nil when the file is
// already up to date, and returns an error when the file is stale.
func TestRun_CheckMode(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	tmpDir := t.TempDir()
	outPath := filepath.Join(tmpDir, "envelope-versions.md")

	// Write the file first.
	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	// Check mode should return nil (file is fresh).
	if err := run(srcPath, outPath, true); err != nil {
		t.Errorf("check on fresh file: expected nil, got %v", err)
	}

	// Corrupt the file and re-check; should return an error.
	if err := os.WriteFile(outPath, []byte("stale content"), 0o644); err != nil {
		t.Fatalf("corrupt file: %v", err)
	}
	if err := run(srcPath, outPath, true); err == nil {
		t.Error("check on stale file: expected error, got nil")
	}
}

// TestMain_HappyPath drives main() end-to-end through the flag parser so the
// CLI entry point is exercised: it points -source at a synthetic file and
// -output at a temp path, then confirms the generated file is written.
func TestMain_HappyPath(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "export.go")
	const sampleSource = `
// CurrentEnvelopeVersion is the version emitted by Export.
//   - "1.0": original format
//   - "1.1": adds rules
const CurrentEnvelopeVersion = "1.1"
`
	if err := os.WriteFile(srcPath, []byte(sampleSource), 0o644); err != nil {
		t.Fatalf("write sample source: %v", err)
	}
	outPath := filepath.Join(dir, "envelope-versions.md")

	// main() registers flags on and parses the global flag.CommandLine using
	// os.Args. Save and restore both, and swap in a fresh FlagSet so the flags
	// are not redefined against the default CommandLine.
	oldArgs := os.Args
	oldFlags := flag.CommandLine
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldFlags
	})
	flag.CommandLine = flag.NewFlagSet("gen-envelope-changelog", flag.ContinueOnError)
	os.Args = []string{"gen-envelope-changelog", "-source", srcPath, "-output", outPath}

	main()

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("expected main() to write %s: %v", outPath, err)
	}
	if !strings.Contains(string(got), "`1.1`") {
		t.Errorf("generated output missing version 1.1:\n%s", got)
	}
}

// TestRun_SourceNotFound covers the source-read error branch in run().
func TestRun_SourceNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := run(filepath.Join(dir, "does-not-exist.go"), filepath.Join(dir, "out.md"), false); err == nil {
		t.Fatal("expected an error for a missing source file, got nil")
	}
}

// TestParseBulletLine covers the version/description split and all
// malformed-input guards: missing opening quote, missing closing quote,
// missing colon after the closing quote, and an empty description.
func TestParseBulletLine(t *testing.T) {
	if v, d, ok := parseBulletLine(`"1.0": original format`); !ok || v != "1.0" || d != "original format" {
		t.Errorf("valid line: got (%q, %q, %v), want (%q, %q, true)", v, d, ok, "1.0", "original format")
	}
	if _, _, ok := parseBulletLine(`1.0: no opening quote`); ok {
		t.Error("expected ok=false for a line with no opening quote")
	}
	if _, _, ok := parseBulletLine(`"1.0 no closing quote`); ok {
		t.Error("expected ok=false for a line with no closing quote")
	}
	// No colon after the closing quote: must be rejected.
	if _, _, ok := parseBulletLine(`"1.0" description without colon`); ok {
		t.Error("expected ok=false for a bullet with no colon after the closing quote")
	}
	// Colon present but description is empty: must be rejected.
	if _, _, ok := parseBulletLine(`"1.0":`); ok {
		t.Error("expected ok=false for a bullet with an empty description")
	}
	// Colon present but description is only whitespace: must be rejected.
	if _, _, ok := parseBulletLine(`"1.0":   `); ok {
		t.Error("expected ok=false for a bullet with a whitespace-only description")
	}
}

// TestParseVersionEntries_SkipsBulletNoColon confirms that a bullet which is
// missing the colon after the closing quote is skipped rather than treated as
// a valid entry, and that surrounding well-formed bullets are still collected.
func TestParseVersionEntries_SkipsBulletNoColon(t *testing.T) {
	src := []byte(`
// CurrentEnvelopeVersion is the version emitted by Export.
//   - "1.0": good entry
//   - "1.1" description without colon
//   - "1.2": another good entry
const CurrentEnvelopeVersion = "1.2"
`)
	entries, err := parseVersionEntries(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (no-colon bullet skipped), got %d: %+v", len(entries), entries)
	}
	if entries[0].Version != "1.0" || entries[1].Version != "1.2" {
		t.Errorf("unexpected versions, want 1.0 and 1.2: %+v", entries)
	}
}

// TestParseVersionEntries_SkipsBulletEmptyDescription confirms that a bullet
// with a colon but an empty description is skipped rather than emitting a
// blank-description entry.
func TestParseVersionEntries_SkipsBulletEmptyDescription(t *testing.T) {
	src := []byte(`
// CurrentEnvelopeVersion is the version emitted by Export.
//   - "1.0": good entry
//   - "1.1":
//   - "1.2": another good entry
const CurrentEnvelopeVersion = "1.2"
`)
	entries, err := parseVersionEntries(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (empty-description bullet skipped), got %d: %+v", len(entries), entries)
	}
	if entries[0].Version != "1.0" || entries[1].Version != "1.2" {
		t.Errorf("unexpected versions, want 1.0 and 1.2: %+v", entries)
	}
}

// TestParseVersionEntries_SkipsMalformedBullet confirms a bullet whose quoted
// version is unterminated is skipped rather than failing the whole parse, and
// that the well-formed bullets around it are still collected.
func TestParseVersionEntries_SkipsMalformedBullet(t *testing.T) {
	src := []byte(`
// CurrentEnvelopeVersion is the version emitted by Export.
//   - "1.0": good entry
//   - "1.1 unterminated version quote
//   - "1.2": another good entry
const CurrentEnvelopeVersion = "1.2"
`)
	entries, err := parseVersionEntries(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (malformed skipped), got %d: %+v", len(entries), entries)
	}
	if entries[0].Version != "1.0" || entries[1].Version != "1.2" {
		t.Errorf("unexpected versions, want 1.0 and 1.2: %+v", entries)
	}
}
