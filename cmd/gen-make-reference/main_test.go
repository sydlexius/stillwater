package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseTargets_RealMakefile verifies the parser extracts help entries from
// the actual repository Makefile and that a few well-known targets are present.
func TestParseTargets_RealMakefile(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read Makefile %s: %v", srcPath, err)
	}

	targets, err := parseTargets(raw)
	if err != nil {
		t.Fatalf("parseTargets: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("expected at least one target, got none")
	}

	byName := make(map[string]string, len(targets))
	for _, tg := range targets {
		if strings.TrimSpace(tg.Description) == "" {
			t.Errorf("target %q has an empty description", tg.Name)
		}
		byName[tg.Name] = tg.Description
	}

	for _, want := range []string{"build", "test", "lint", "generate-docs", "help"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("expected target %q to be present in parsed help comments", want)
		}
	}
}

// TestParseTargets_WellFormed verifies the parser against a minimal synthetic
// Makefile snippet covering a target, a continuation line, and a non-help line.
func TestParseTargets_WellFormed(t *testing.T) {
	src := []byte("## build: Build the binary\n" +
		"build:\n" +
		"\tgo build ./...\n" +
		"## worktree: Create a sibling worktree\n" +
		"##   Usage: make worktree NAME=<slug>\n" +
		"worktree:\n" +
		"\t@echo wt\n")

	targets, err := parseTargets(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets (continuation line skipped), got %d: %+v", len(targets), targets)
	}
	if targets[0].Name != "build" || targets[0].Description != "Build the binary" {
		t.Errorf("target[0] = %+v, want {build, Build the binary}", targets[0])
	}
	if targets[1].Name != "worktree" || targets[1].Description != "Create a sibling worktree" {
		t.Errorf("target[1] = %+v, want {worktree, Create a sibling worktree}", targets[1])
	}
}

// TestParseTargets_NoComments confirms a Makefile without help comments yields
// an error rather than an empty table.
func TestParseTargets_NoComments(t *testing.T) {
	src := []byte("build:\n\tgo build ./...\n# a normal comment\n")
	if _, err := parseTargets(src); err == nil {
		t.Fatal("expected an error for a Makefile with no help comments, got nil")
	}
}

// TestRenderTable verifies the Markdown table output, including pipe escaping.
func TestRenderTable(t *testing.T) {
	got := renderTable([]makeTarget{
		{Name: "build", Description: "Build the binary"},
		{Name: "test", Description: "Run tests (unit | race)"},
	})
	want := "| Command | Description |\n" +
		"|---|---|\n" +
		"| `make build` | Build the binary |\n" +
		"| `make test` | Run tests (unit \\| race) |\n"
	if got != want {
		t.Errorf("renderTable mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestRun_Idempotent verifies two consecutive runs produce no diff.
func TestRun_Idempotent(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	outPath := filepath.Join(t.TempDir(), "make-commands.md")

	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	content1, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read after first run: %v", err)
	}
	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	content2, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}
	if string(content1) != string(content2) {
		t.Errorf("content changed between runs (not idempotent)")
	}
}

// TestRun_CheckMode verifies -check returns nil when fresh and errors when stale.
func TestRun_CheckMode(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	outPath := filepath.Join(t.TempDir(), "make-commands.md")

	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	if err := run(srcPath, outPath, true); err != nil {
		t.Errorf("check on fresh file: expected nil, got %v", err)
	}
	if err := os.WriteFile(outPath, []byte("stale content"), 0o644); err != nil {
		t.Fatalf("corrupt file: %v", err)
	}
	if err := run(srcPath, outPath, true); err == nil {
		t.Error("check on stale file: expected error, got nil")
	}
}

// TestRun_SourceNotFound covers the source-read error branch in run().
func TestRun_SourceNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := run(filepath.Join(dir, "no-such-makefile"), filepath.Join(dir, "out.md"), false); err == nil {
		t.Fatal("expected an error for a missing Makefile, got nil")
	}
}

// TestMain_HappyPath drives main() through the flag parser to cover the CLI
// entry point end to end.
func TestMain_HappyPath(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(srcPath, []byte("## build: Build the binary\nbuild:\n\tgo build ./...\n"), 0o644); err != nil {
		t.Fatalf("write sample Makefile: %v", err)
	}
	outPath := filepath.Join(dir, "make-commands.md")

	oldArgs := os.Args
	oldFlags := flag.CommandLine
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldFlags
	})
	flag.CommandLine = flag.NewFlagSet("gen-make-reference", flag.ContinueOnError)
	os.Args = []string{"gen-make-reference", "-source", srcPath, "-output", outPath}

	main()

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("expected main() to write %s: %v", outPath, err)
	}
	if !strings.Contains(string(got), "`make build`") {
		t.Errorf("generated output missing build target:\n%s", got)
	}
}

// TestParseTargets_SkipsNonTargetComments confirms that "## " comment lines
// without a valid "<name>: <description>" shape are skipped rather than
// producing empty or malformed rows.
func TestParseTargets_SkipsNonTargetComments(t *testing.T) {
	src := []byte("## build: Build the binary\n" +
		"## a note without a colon\n" + // no colon -> skipped
		"## : empty name\n" + // empty name -> skipped
		"## empty-desc:\n" + // empty description -> skipped
		"build:\n\tgo build\n")
	targets, err := parseTargets(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 1 || targets[0].Name != "build" {
		t.Fatalf("expected only the valid build target, got %+v", targets)
	}
}

// TestRun_CreatesNestedOutputDir covers the MkdirAll branch by writing to an
// output path whose parent directories do not yet exist.
func TestRun_CreatesNestedOutputDir(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	outPath := filepath.Join(t.TempDir(), "nested", "deeper", "make-commands.md")
	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("run with nested output dir: %v", err)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("expected output file to be created: %v", err)
	}
}
