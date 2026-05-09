package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSlugify verifies the slugifier matches python-markdown's default toc
// extension behavior. Each case corresponds to a heading in the docs pages
// that the artist detail ContextHelp icons link to.
func TestSlugify(t *testing.T) {
	cases := []struct {
		heading string
		want    string
	}{
		// Simple heading.
		{"How evaluation runs", "how-evaluation-runs"},
		// Mixed-case with article.
		{"What rules check", "what-rules-check"},
		// Heading with colon and parentheses -- python-markdown strips both.
		{"Layer 1: artist lock (the big switch)", "layer-1-artist-lock-the-big-switch"},
		// Heading with colon, parentheses, and hyphenated word.
		{"Layer 2: field locks (per-field protection)", "layer-2-field-locks-per-field-protection"},
		// Heading with article.
		{"What a refresh does", "what-a-refresh-does"},
		// Heading with no punctuation.
		{"When you get the disambiguation prompt", "when-you-get-the-disambiguation-prompt"},
		// Per-field contains a hyphen already.
		{"Per-field priority", "per-field-priority"},
		// Simple short heading.
		{"The four slots", "the-four-slots"},
		// Mixed-case.
		{"Three modes per rule", "three-modes-per-rule"},
		// Heading with colon and comma.
		{"Resolution and aspect: rule-driven, configurable", "resolution-and-aspect-rule-driven-configurable"},
		// Accented characters must be stripped (NFKD normalization drops the
		// combining diacritic after decomposition).
		{"Cafe au lait", "cafe-au-lait"},
		// Underscore is preserved (python-markdown's slugify keeps it via \w);
		// only whitespace and hyphens collapse to a single hyphen.
		{"snake_case heading", "snake_case-heading"},
		// SCREAMING_SNAKE preserves the underscores after lowercasing.
		{"SW_TLS_PORT and friends", "sw_tls_port-and-friends"},
		// Leading and trailing punctuation gets stripped.
		{"  Padded with spaces  ", "padded-with-spaces"},
		// Numbers survive verbatim.
		{"HTTP/3 over QUIC", "http3-over-quic"},
	}

	for _, tc := range cases {
		t.Run(tc.heading, func(t *testing.T) {
			got := slugify(tc.heading)
			if got != tc.want {
				t.Errorf("slugify(%q) = %q, want %q", tc.heading, got, tc.want)
			}
		})
	}
}

// writeTempMD writes content to a temporary file and returns the path.
func writeTempMD(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.md")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp md: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp md: %v", err)
	}
	return f.Name()
}

// TestDuplicateSlugDedup verifies that a second heading producing the same
// base slug receives a -1 suffix, and a third gets -2, consistent with the
// python-markdown toc de-duplication algorithm.
func TestDuplicateSlugDedup(t *testing.T) {
	md := "## What you don't need to think about\n\nSome text.\n\n" +
		"## What you don't need to think about\n\nMore text.\n\n" +
		"## What you don't need to think about\n\nEven more text.\n"

	mdPath := writeTempMD(t, md)
	got, err := headingsToAnchors(mdPath, "test/page")
	if err != nil {
		t.Fatalf("headingsToAnchors: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 anchors, got %d: %v", len(got), got)
	}
	want := []string{
		"test/page#what-you-dont-need-to-think-about",
		"test/page#what-you-dont-need-to-think-about-1",
		"test/page#what-you-dont-need-to-think-about-2",
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("anchor[%d]: got %q, want %q", i, got[i], w)
		}
	}
}

// TestFencedCodeBlocksSkipped verifies that lines beginning with # inside a
// fenced code block are not treated as headings. Both backtick and tilde
// fences are recognized.
func TestFencedCodeBlocksSkipped(t *testing.T) {
	md := "## Real heading\n\n```bash\n# this is a comment\n```\n\n" +
		"~~~\n# tilde-fenced comment\n~~~\n\n" +
		"## Another heading\n"
	mdPath := writeTempMD(t, md)

	got, err := headingsToAnchors(mdPath, "test/page")
	if err != nil {
		t.Fatalf("headingsToAnchors: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 anchors, got %d: %v", len(got), got)
	}
	for _, a := range got {
		if strings.Contains(a, "this-is-a-comment") || strings.Contains(a, "tilde-fenced-comment") {
			t.Errorf("fenced code comment was treated as a heading: %v", got)
		}
	}
}

// TestHeadingDepthsAndEdgeCases covers H1-H6 plus malformed lines that look
// like headings. The walker accepts any depth equally; the edge cases below
// must be ignored.
func TestHeadingDepthsAndEdgeCases(t *testing.T) {
	md := "# H1 heading\n" +
		"## H2 heading\n" +
		"### H3 heading\n" +
		"#### H4 heading\n" +
		"##### H5 heading\n" +
		"###### H6 heading\n" +
		"#missing-space\n" + // not a heading -- python-markdown requires the space
		"#  \n" + // empty heading after stripping trim -- ignored
		"\n" +
		"   ### indented (still not a heading per common-mark inside files we care about)\n"

	mdPath := writeTempMD(t, md)
	got, err := headingsToAnchors(mdPath, "test/depths")
	if err != nil {
		t.Fatalf("headingsToAnchors: %v", err)
	}
	want := []string{
		"test/depths#h1-heading",
		"test/depths#h2-heading",
		"test/depths#h3-heading",
		"test/depths#h4-heading",
		"test/depths#h5-heading",
		"test/depths#h6-heading",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d anchors, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("anchor[%d]: got %q, want %q", i, got[i], w)
		}
	}
}

// TestCollectAnchorsWalksAndSkips verifies that collectAnchors walks every
// .md file under the docs root, skips the reference/ subdirectory, ignores
// non-Markdown files, and returns sorted output.
func TestCollectAnchorsWalksAndSkips(t *testing.T) {
	root := t.TempDir()

	// Create a small fixture tree:
	//   root/core-concepts/rules.md
	//   root/how-to/refresh.md
	//   root/reference/auto-generated.md   (must be skipped)
	//   root/extra.txt                      (non-markdown, must be skipped)
	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	mustWrite("core-concepts/rules.md", "# Rules\n\n## Three modes per rule\n")
	mustWrite("how-to/refresh.md", "# Refresh\n\n## What a refresh does\n")
	mustWrite("reference/auto-generated.md", "# Auto-generated -- must be skipped\n")
	mustWrite("extra.txt", "Not a markdown file.\n")

	got, err := collectAnchors(root)
	if err != nil {
		t.Fatalf("collectAnchors: %v", err)
	}

	want := []string{
		"core-concepts/rules#rules",
		"core-concepts/rules#three-modes-per-rule",
		"how-to/refresh#refresh",
		"how-to/refresh#what-a-refresh-does",
	}

	if len(got) != len(want) {
		t.Fatalf("expected %d anchors, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("anchor[%d]: got %q, want %q", i, got[i], w)
		}
	}

	// Defense-in-depth: ensure no anchor came from the reference subdirectory.
	for _, a := range got {
		if strings.HasPrefix(a, "reference/") {
			t.Errorf("reference/ anchor leaked into output: %q", a)
		}
	}
}

// TestWriteOrCheckCreatesFile verifies the writer creates the destination
// (including a missing parent directory) and emits a deterministic body.
func TestWriteOrCheckCreatesFile(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "nested", "dir", "_doc-anchors.txt")
	anchors := []string{"a/page#one", "a/page#two"}

	if err := writeOrCheck(dest, anchors, false); err != nil {
		t.Fatalf("writeOrCheck: %v", err)
	}

	body, err := os.ReadFile(dest) //nolint:gosec // G304: test reads a path it just wrote
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	want := fileHeader + "a/page#one\na/page#two\n"
	if string(body) != want {
		t.Errorf("body mismatch:\n got %q\nwant %q", string(body), want)
	}
}

// TestWriteOrCheckIdempotent verifies that re-running with identical content
// is a no-op (does not return an error and does not rewrite the file).
func TestWriteOrCheckIdempotent(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "_doc-anchors.txt")
	anchors := []string{"x/y#z"}

	if err := writeOrCheck(dest, anchors, false); err != nil {
		t.Fatalf("first writeOrCheck: %v", err)
	}
	statBefore, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Second run with identical content; check mode false should still no-op.
	if err := writeOrCheck(dest, anchors, false); err != nil {
		t.Fatalf("second writeOrCheck: %v", err)
	}
	statAfter, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !statAfter.ModTime().Equal(statBefore.ModTime()) {
		t.Errorf("file was rewritten despite identical content; modtime changed")
	}
}

// TestWriteOrCheckCheckMode verifies that -check returns an error when the
// destination is stale (or missing) and succeeds when it matches.
func TestWriteOrCheckCheckMode(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "_doc-anchors.txt")
	anchors := []string{"a/b#c"}

	// Missing destination: check should fail.
	if err := writeOrCheck(dest, anchors, true); err == nil {
		t.Errorf("expected check-mode error for missing destination, got nil")
	}

	// Write the file, then check mode should succeed.
	if err := writeOrCheck(dest, anchors, false); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := writeOrCheck(dest, anchors, true); err != nil {
		t.Errorf("expected check-mode success after seed, got: %v", err)
	}

	// Mutate the file, check should fail.
	if err := os.WriteFile(dest, []byte("stale\n"), 0o644); err != nil { //nolint:gosec // G306: test fixture
		t.Fatalf("mutate: %v", err)
	}
	if err := writeOrCheck(dest, anchors, true); err == nil {
		t.Errorf("expected check-mode error for stale destination, got nil")
	}
}

// TestCollectAnchorsSkipsNonMarkdown verifies the walker ignores files that
// don't end in .md (the suffix gate above the headings parser).
func TestCollectAnchorsSkipsNonMarkdown(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil { //nolint:gosec // G306: test fixture
			t.Fatalf("write: %v", err)
		}
	}

	// One real .md file plus two non-.md noise files (the kinds of artifacts
	// that legitimately live alongside docs: a generated anchors list and a
	// JSON manifest).
	mustWrite("how-to/refresh.md", "# Refresh\n")
	mustWrite("how-to/_doc-anchors.txt", "# Looks like a heading but is .txt\n")
	mustWrite("how-to/manifest.json", `{"unrelated": true}`)

	got, err := collectAnchors(root)
	if err != nil {
		t.Fatalf("collectAnchors: %v", err)
	}
	if len(got) != 1 || got[0] != "how-to/refresh#refresh" {
		t.Errorf("unexpected anchors after skip: %v", got)
	}
}

// TestCollectAnchorsErrorPropagation verifies that a non-existent docsRoot
// surfaces as a walk error rather than silently emitting an empty list.
func TestCollectAnchorsErrorPropagation(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	_, err := collectAnchors(missing)
	if err == nil {
		t.Fatal("expected error for missing docsRoot, got nil")
	}
}

// TestRunErrorOnMissingDocsRoot verifies run() propagates the walker error
// (covers the error branch around the collectAnchors call).
func TestRunErrorOnMissingDocsRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	dest := filepath.Join(t.TempDir(), "out", "_doc-anchors.txt")
	if err := run(missing, dest, false); err == nil {
		t.Error("expected run() to fail on missing docsRoot")
	}
}

// TestWriteOrCheckReadError verifies writeOrCheck surfaces a non-IsNotExist
// read error (not just a missing file). Passing a directory as the dest
// triggers an EISDIR-class error inside os.ReadFile.
func TestWriteOrCheckReadError(t *testing.T) {
	dir := t.TempDir() // a directory, not a file
	if err := writeOrCheck(dir, []string{"a/b#c"}, false); err == nil {
		t.Error("expected error when dest is a directory, got nil")
	}
}

// TestRunEndToEnd exercises run() against a fixture docs root and a custom
// canonical output path. Routing canonicalPath through -output skips the
// components mirror, isolating this test from the real repo tree.
func TestRunEndToEnd(t *testing.T) {
	root := t.TempDir()
	docsRoot := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(docsRoot, "core-concepts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsRoot, "core-concepts", "field-locks.md"),
		[]byte("# Field locks\n\n## Layer 1: artist lock (the big switch)\n"), 0o644); err != nil { //nolint:gosec // G306: test fixture
		t.Fatalf("write fixture: %v", err)
	}

	canonical := filepath.Join(root, "out", "_doc-anchors.txt")
	if err := run(docsRoot, canonical, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	body, err := os.ReadFile(canonical) //nolint:gosec // G304: path constructed above
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"core-concepts/field-locks#field-locks",
		"core-concepts/field-locks#layer-1-artist-lock-the-big-switch",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing anchor %q; full body:\n%s", want, got)
		}
	}

	// Re-run in check mode on the same canonical path should succeed.
	if err := run(docsRoot, canonical, true); err != nil {
		t.Errorf("run(check): %v", err)
	}
}
