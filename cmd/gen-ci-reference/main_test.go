package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalWorkflowYAML is a reduced workflow fixture with two jobs and one shard.
const minimalWorkflowYAML = `
jobs:
  changes:
    name: Detect Changes
    runs-on: ubuntu-latest
  test:
    name: Test Shard
    runs-on: ubuntu-latest
    needs: [changes]
    strategy:
      matrix:
        shard: [api-1, api-2, services, rest]
    steps:
      - name: Resolve shard package list
        run: |
          declare -A SHARDS=(
            [api]="internal/api"
            [services]="internal/auth internal/image"
          )
          echo "done"
  test-summary:
    name: Test
    runs-on: ubuntu-latest
    needs: [changes, test]
    if: always()
`

// TestParseWorkflow_MinimalFixture verifies basic YAML parsing extracts jobs,
// needs, matrix shards, and the SHARDS bash map from a minimal fixture.
func TestParseWorkflow_MinimalFixture(t *testing.T) {
	wf, err := parseWorkflow([]byte(minimalWorkflowYAML))
	if err != nil {
		t.Fatalf("parseWorkflow: %v", err)
	}
	if len(wf.Jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d: %v", len(wf.Jobs), wf.Jobs)
	}

	byID := make(map[string]*ciJob, len(wf.Jobs))
	for i := range wf.Jobs {
		byID[wf.Jobs[i].ID] = &wf.Jobs[i]
	}

	changes := byID["changes"]
	if changes == nil {
		t.Fatal("missing 'changes' job")
	}
	if changes.Name != "Detect Changes" {
		t.Errorf("changes.Name = %q, want 'Detect Changes'", changes.Name)
	}
	if len(changes.Needs) != 0 {
		t.Errorf("changes.Needs = %v, want []", changes.Needs)
	}

	testJob := byID["test"]
	if testJob == nil {
		t.Fatal("missing 'test' job")
	}
	if len(testJob.Needs) != 1 || testJob.Needs[0] != "changes" {
		t.Errorf("test.Needs = %v, want [changes]", testJob.Needs)
	}
	wantShards := []string{"api-1", "api-2", "services", "rest"}
	if len(testJob.Shards) != len(wantShards) {
		t.Errorf("test.Shards = %v, want %v", testJob.Shards, wantShards)
	}
	if v := testJob.ShardMap["api"]; v != "internal/api" {
		t.Errorf("ShardMap[api] = %q, want 'internal/api'", v)
	}
	if v := testJob.ShardMap["services"]; v != "internal/auth internal/image" {
		t.Errorf("ShardMap[services] = %q, want 'internal/auth internal/image'", v)
	}

	summary := byID["test-summary"]
	if summary == nil {
		t.Fatal("missing 'test-summary' job")
	}
	if summary.If != "always()" {
		t.Errorf("test-summary.If = %q, want 'always()'", summary.If)
	}
	if len(summary.Needs) != 2 {
		t.Errorf("test-summary.Needs = %v, want [changes test]", summary.Needs)
	}
}

// TestParseWorkflow_RealCI verifies the real ci.yml parses without error and
// contains a few well-known jobs.
func TestParseWorkflow_RealCI(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}
	wf, err := parseWorkflow(src)
	if err != nil {
		t.Fatalf("parseWorkflow: %v", err)
	}

	byID := make(map[string]*ciJob, len(wf.Jobs))
	for i := range wf.Jobs {
		byID[wf.Jobs[i].ID] = &wf.Jobs[i]
	}
	for _, want := range []string{"changes", "test", "test-summary", "lint", "build"} {
		if byID[want] == nil {
			t.Errorf("expected job %q to be present", want)
		}
	}
	testJob := byID["test"]
	if testJob != nil && len(testJob.ShardMap) == 0 {
		t.Error("expected SHARDS map to be extracted from test job steps")
	}
}

// TestExtractShardsMap verifies the regex-based extraction of bash SHARDS maps.
func TestExtractShardsMap(t *testing.T) {
	script := `declare -A SHARDS=(
  [api]="internal/api"
  [services]="internal/auth internal/image internal/artist"
  [providers]="internal/provider internal/connection"
)
echo done`

	m := extractShardsMap(script)
	if m == nil {
		t.Fatal("expected non-nil map, got nil")
	}
	cases := map[string]string{
		"api":       "internal/api",
		"services":  "internal/auth internal/image internal/artist",
		"providers": "internal/provider internal/connection",
	}
	for k, want := range cases {
		if got := m[k]; got != want {
			t.Errorf("m[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestExtractShardsMap_NoDeclaration returns nil when script lacks SHARDS.
func TestExtractShardsMap_NoDeclaration(t *testing.T) {
	if m := extractShardsMap("echo hello"); m != nil {
		t.Errorf("expected nil for script without SHARDS, got %v", m)
	}
}

// TestRenderMermaid verifies the Mermaid output contains expected structural
// markers (flowchart directive, node entries, edge arrows).
func TestRenderMermaid(t *testing.T) {
	wf, err := parseWorkflow([]byte(minimalWorkflowYAML))
	if err != nil {
		t.Fatalf("parseWorkflow: %v", err)
	}

	out := renderMermaid(wf)

	if !strings.Contains(out, "```mermaid") {
		t.Error("expected mermaid code fence open")
	}
	if !strings.Contains(out, "```\n") {
		t.Error("expected mermaid code fence close")
	}
	if !strings.Contains(out, "flowchart TD") {
		t.Error("expected 'flowchart TD' directive")
	}
	if !strings.Contains(out, "-->") {
		t.Error("expected at least one edge arrow '-->'")
	}
	// test-summary needs changes and test.
	if !strings.Contains(out, "changes") {
		t.Error("expected 'changes' node")
	}
}

// TestRenderShardSection verifies the shard table contains expected rows and
// annotations for partitioned, named, and rest shards.
func TestRenderShardSection(t *testing.T) {
	wf, err := parseWorkflow([]byte(minimalWorkflowYAML))
	if err != nil {
		t.Fatalf("parseWorkflow: %v", err)
	}

	out, err := renderShardSection(wf)
	if err != nil {
		t.Fatalf("renderShardSection: %v", err)
	}

	if !strings.Contains(out, "## Test Matrix Shards") {
		t.Error("expected section heading")
	}
	// Partitioned shard api-1
	if !strings.Contains(out, "`api-1`") {
		t.Error("expected api-1 row")
	}
	if !strings.Contains(out, "Partitioned shard") {
		t.Error("expected partitioned-shard annotation for api-1")
	}
	// Named shard services
	if !strings.Contains(out, "`services`") {
		t.Error("expected services row")
	}
	if !strings.Contains(out, "Named shard") {
		t.Error("expected named-shard annotation")
	}
	// Rest shard
	if !strings.Contains(out, "`rest`") {
		t.Error("expected rest row")
	}
	if !strings.Contains(out, "Remainder") {
		t.Error("expected dynamic-remainder annotation for rest shard")
	}
}

// TestRenderShardSection_MissingTestJob verifies that an error is returned when
// the expected test job is absent from the workflow.
func TestRenderShardSection_MissingTestJob(t *testing.T) {
	const noTestJobYAML = `
jobs:
  changes:
    name: Detect Changes
    runs-on: ubuntu-latest
`
	wf, err := parseWorkflow([]byte(noTestJobYAML))
	if err != nil {
		t.Fatalf("parseWorkflow: %v", err)
	}
	if _, err := renderShardSection(wf); err == nil {
		t.Error("expected error when test job is absent, got nil")
	}
}

// TestParseWorkflow_MissingSHARDSStep verifies that a job with a shard matrix
// but no SHARDS declaration fails at parse time rather than silently producing
// an empty map.
func TestParseWorkflow_MissingSHARDSStep(t *testing.T) {
	const missingSHARDS = `
jobs:
  changes:
    name: Detect Changes
    runs-on: ubuntu-latest
  test:
    name: Test Shard
    runs-on: ubuntu-latest
    needs: [changes]
    strategy:
      matrix:
        shard: [api-1, api-2]
    steps:
      - name: Run tests
        run: go test ./...
`
	if _, err := parseWorkflow([]byte(missingSHARDS)); err == nil {
		t.Error("expected error when shard matrix exists but no SHARDS declare step is found, got nil")
	}
}

// TestParseWorkflow_SingleNeedsString verifies that a single-element needs
// value expressed as a bare string (not a list) is parsed correctly.
func TestParseWorkflow_SingleNeedsString(t *testing.T) {
	const singleNeeds = `
jobs:
  changes:
    name: Detect Changes
    runs-on: ubuntu-latest
  test:
    name: Test
    runs-on: ubuntu-latest
    needs: changes
    strategy:
      matrix:
        shard: [rest]
    steps:
      - name: Resolve shard package list
        run: |
          declare -A SHARDS=(
            [api]="internal/api"
          )
          echo done
`
	wf, err := parseWorkflow([]byte(singleNeeds))
	if err != nil {
		t.Fatalf("parseWorkflow: %v", err)
	}
	var testJob *ciJob
	for i := range wf.Jobs {
		if wf.Jobs[i].ID == "test" {
			testJob = &wf.Jobs[i]
		}
	}
	if testJob == nil {
		t.Fatal("test job not found")
	}
	if len(testJob.Needs) != 1 || testJob.Needs[0] != "changes" {
		t.Errorf("test.Needs = %v, want [changes]", testJob.Needs)
	}
}

// TestRun_Idempotent verifies two consecutive runs produce identical output.
func TestRun_Idempotent(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	outPath := filepath.Join(t.TempDir(), "ci-reference.md")

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
		t.Error("content changed between runs (not idempotent)")
	}
}

// TestRun_CheckMode verifies -check exits nil when fresh and errors when stale.
func TestRun_CheckMode(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	outPath := filepath.Join(t.TempDir(), "ci-reference.md")

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

// TestRun_SourceNotFound covers the source-read error branch.
func TestRun_SourceNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := run(filepath.Join(dir, "no-such.yml"), filepath.Join(dir, "out.md"), false); err == nil {
		t.Fatal("expected an error for a missing source file, got nil")
	}
}

// TestParseWorkflow_Empty ensures the parser rejects a workflow with no jobs.
func TestParseWorkflow_Empty(t *testing.T) {
	if _, err := parseWorkflow([]byte("name: Empty\n")); err == nil {
		t.Fatal("expected an error for a workflow with no jobs, got nil")
	}
}

// TestParseWorkflow_ListRunsOn verifies that a job whose runs-on field is a YAML
// sequence (label list) is parsed without error and produces a comma-joined
// RunsOn string on the resulting ciJob.
func TestParseWorkflow_ListRunsOn(t *testing.T) {
	const listRunsOnYAML = `
jobs:
  changes:
    name: Detect Changes
    runs-on: ubuntu-latest
  test:
    name: Test
    runs-on: [self-hosted, linux]
    needs: changes
    strategy:
      matrix:
        shard: [rest]
    steps:
      - name: Resolve shard package list
        run: |
          declare -A SHARDS=(
            [api]="internal/api"
          )
          echo done
`
	wf, err := parseWorkflow([]byte(listRunsOnYAML))
	if err != nil {
		t.Fatalf("parseWorkflow: %v", err)
	}
	var testJob *ciJob
	for i := range wf.Jobs {
		if wf.Jobs[i].ID == "test" {
			testJob = &wf.Jobs[i]
		}
	}
	if testJob == nil {
		t.Fatal("test job not found")
	}
	const want = "self-hosted, linux"
	if testJob.RunsOn != want {
		t.Errorf("test.RunsOn = %q, want %q", testJob.RunsOn, want)
	}
}

// TestRun_CreatesNestedOutputDir covers the MkdirAll branch.
func TestRun_CreatesNestedOutputDir(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	outPath := filepath.Join(t.TempDir(), "nested", "deeper", "ci-reference.md")
	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("run with nested output dir: %v", err)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("expected output file to be created: %v", err)
	}
}
