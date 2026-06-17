package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	icli "github.com/sydlexius/stillwater/internal/cli"
)

// fixtureGood exercises every supported field shape: a bool with a default
// and a string. Tests reflect over this fixture to verify the codegen
// contract in isolation from the real cli.Flags type.
type fixtureGood struct {
	Verbose bool   `flag:"verbose" default:"false" desc:"Enable verbose output."`
	Config  string `flag:"config" default:"/etc/app.conf" desc:"Path to the config file."`
	Skipped string // no flag tag; must be ignored
}

// fixtureMissingDesc has a flag tag but no desc tag; collectRows must reject
// it so the docs page can never silently ship with an empty description.
type fixtureMissingDesc struct {
	Verbose bool `flag:"verbose" default:"false"`
}

// ---- collectRows ------------------------------------------------------------

func TestCollectRows_Fixture(t *testing.T) {
	rows, err := collectRows(reflect.TypeOf(fixtureGood{}))
	if err != nil {
		t.Fatalf("collectRows: %v", err)
	}
	// Only the two tagged fields; Skipped has no flag: tag.
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d (%v)", len(rows), rows)
	}
	if rows[0].Name != "verbose" {
		t.Errorf("rows[0].Name = %q, want %q", rows[0].Name, "verbose")
	}
	if rows[1].Name != "config" {
		t.Errorf("rows[1].Name = %q, want %q", rows[1].Name, "config")
	}
	// Type mapping.
	if rows[0].Type != "boolean" {
		t.Errorf("verbose type = %q, want boolean", rows[0].Type)
	}
	if rows[1].Type != "string" {
		t.Errorf("config type = %q, want string", rows[1].Type)
	}
	// Default rendering.
	if rows[0].Default != "`false`" {
		t.Errorf("verbose default = %q, want `false`", rows[0].Default)
	}
	if rows[1].Default != "`/etc/app.conf`" {
		t.Errorf("config default = %q, want `/etc/app.conf`", rows[1].Default)
	}
}

func TestCollectRows_MissingDescIsError(t *testing.T) {
	_, err := collectRows(reflect.TypeOf(fixtureMissingDesc{}))
	if err == nil {
		t.Fatal("expected error when flag-tagged field has no desc tag")
	}
	if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("error should name the offending flag; got %v", err)
	}
	if !strings.Contains(err.Error(), "desc") {
		t.Errorf("error should mention the missing desc tag; got %v", err)
	}
}

func TestCollectRows_NonStruct(t *testing.T) {
	_, err := collectRows(reflect.TypeOf(42))
	if err == nil {
		t.Fatal("expected error for non-struct type")
	}
	if !strings.Contains(err.Error(), "struct") {
		t.Errorf("error should mention struct; got %v", err)
	}
}

func TestCollectRows_RealFlags(t *testing.T) {
	// The real cli.Flags must have valid desc: tags on every flag: field.
	// This test is the coverage enforcement from the test layer: if anyone
	// adds a flag: field without a desc: tag, this test catches it.
	rows, err := collectRows(reflect.TypeOf(icli.Flags{}))
	if err != nil {
		t.Fatalf("real cli.Flags has a field missing desc tag: %v", err)
	}
	if len(rows) == 0 {
		t.Error("real cli.Flags has no flag: tagged fields -- at least one expected")
	}
}

// ---- renderDefault ----------------------------------------------------------

func TestRenderDefault_Empty(t *testing.T) {
	if got := renderDefault(""); got != "(none)" {
		t.Errorf("renderDefault(\"\") = %q, want (none)", got)
	}
}

func TestRenderDefault_Literal(t *testing.T) {
	if got := renderDefault("false"); got != "`false`" {
		t.Errorf("renderDefault(false) = %q, want `false`", got)
	}
}

func TestRenderDefault_Path(t *testing.T) {
	if got := renderDefault("/config/app.db"); got != "`/config/app.db`" {
		t.Errorf("renderDefault path = %q", got)
	}
}

// ---- flagDocType ------------------------------------------------------------

func TestFlagDocType_Bool(t *testing.T) {
	if got := flagDocType(reflect.TypeOf(false)); got != "boolean" {
		t.Errorf("bool type = %q, want boolean", got)
	}
}

func TestFlagDocType_String(t *testing.T) {
	if got := flagDocType(reflect.TypeOf("")); got != "string" {
		t.Errorf("string type = %q, want string", got)
	}
}

func TestFlagDocType_Int(t *testing.T) {
	if got := flagDocType(reflect.TypeOf(0)); got != "integer" {
		t.Errorf("int type = %q, want integer", got)
	}
}

// ---- escapeMarkdownCell ----------------------------------------------------

func TestEscapeMarkdownCell_Pipe(t *testing.T) {
	if got := escapeMarkdownCell("a|b"); got != `a\|b` {
		t.Errorf("pipe not escaped: got %q", got)
	}
}

func TestEscapeMarkdownCell_Newline(t *testing.T) {
	if got := escapeMarkdownCell("line1\nline2"); got != "line1<br>line2" {
		t.Errorf("newline not replaced: got %q", got)
	}
}

func TestEscapeMarkdownCell_Clean(t *testing.T) {
	if got := escapeMarkdownCell("no special"); got != "no special" {
		t.Errorf("clean string should pass through: got %q", got)
	}
}

// ---- renderContent ----------------------------------------------------------

func TestRenderContent_HasFlagsHeader(t *testing.T) {
	rows := []flagRow{
		{Name: "reset-password", Type: "boolean", Default: "`false`", Description: "Reset password."},
	}
	got := renderContent(rows, nil)
	if !strings.Contains(got, "| Flag | Type | Default | Description |") {
		t.Errorf("flags table header missing; got:\n%s", got)
	}
	if !strings.Contains(got, "| `--reset-password`") {
		t.Errorf("flag row missing; got:\n%s", got)
	}
}

func TestRenderContent_SubcommandsSection(t *testing.T) {
	rows := []flagRow{
		{Name: "verbose", Type: "boolean", Default: "`false`", Description: "Verbose mode."},
	}
	subs := []icli.SubcommandInfo{
		{Name: "reset-credentials", Summary: "Wipe credentials.", Details: "Full details here."},
	}
	got := renderContent(rows, subs)
	if !strings.Contains(got, "## Subcommands") {
		t.Errorf("subcommands section missing; got:\n%s", got)
	}
	if !strings.Contains(got, "reset-credentials") {
		t.Errorf("subcommand name missing; got:\n%s", got)
	}
	if !strings.Contains(got, "Full details here.") {
		t.Errorf("subcommand details missing; got:\n%s", got)
	}
}

func TestRenderContent_NoSubcommandsNoSection(t *testing.T) {
	rows := []flagRow{
		{Name: "verbose", Type: "boolean", Default: "`false`", Description: "Verbose."},
	}
	got := renderContent(rows, nil)
	if strings.Contains(got, "## Subcommands") {
		t.Errorf("subcommands section should be absent when nil; got:\n%s", got)
	}
}

// ---- replaceBetweenMarkers --------------------------------------------------

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
	if !strings.Contains(err.Error(), "begin marker") {
		t.Errorf("error should mention begin marker; got %v", err)
	}
}

func TestReplaceBetweenMarkers_MissingEnd(t *testing.T) {
	src := []byte("prefix " + beginMarker + " no end")
	_, err := replaceBetweenMarkers(src, beginMarker, endMarker, "body")
	if err == nil {
		t.Fatal("expected error when end marker is missing")
	}
	if !strings.Contains(err.Error(), "end marker") {
		t.Errorf("error should mention end marker; got %v", err)
	}
}

func TestReplaceBetweenMarkers_TrailingNewlines(t *testing.T) {
	src := []byte("a\n" + beginMarker + "\nold\n" + endMarker + "\nb\n")
	out, err := replaceBetweenMarkers(src, beginMarker, endMarker, "new\n\n\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "a\n" + beginMarker + "\nnew\n" + endMarker + "\nb\n"
	if string(out) != want {
		t.Fatalf("trailing newlines not normalized\nwant:\n%s\ngot:\n%s", want, string(out))
	}
}

// ---- run() ------------------------------------------------------------------

// writeFixtureDoc writes a docs file with the begin/end markers wrapping
// stale body text, returns the path. Used by run() tests.
func writeFixtureDoc(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cli.md")
	content := "intro\n" + beginMarker + "\n" + body + "\n" + endMarker + "\nfooter\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	return path
}

func TestRun_RewritesStaleContent(t *testing.T) {
	path := writeFixtureDoc(t, "STALE TABLE")
	if err := run(path, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "STALE TABLE") {
		t.Errorf("stale body should be replaced; got:\n%s", got)
	}
	if !strings.Contains(string(got), "| Flag | Type | Default | Description |") {
		t.Errorf("regenerated table header missing; got:\n%s", got)
	}
	if !strings.Contains(string(got), "intro\n") || !strings.Contains(string(got), "footer\n") {
		t.Errorf("manual prose around markers should be preserved; got:\n%s", got)
	}
}

func TestRun_NoChangeIsNoop(t *testing.T) {
	path := writeFixtureDoc(t, "STALE")
	if err := run(path, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(path, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("second run should be a no-op (content unchanged)")
	}
}

func TestRun_CheckMode_StaleErrors(t *testing.T) {
	path := writeFixtureDoc(t, "STALE")
	err := run(path, true)
	if err == nil {
		t.Fatal("expected error in -check mode against stale file")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error should mention staleness; got: %v", err)
	}
}

func TestRun_CheckMode_FreshSucceeds(t *testing.T) {
	path := writeFixtureDoc(t, "STALE")
	if err := run(path, false); err != nil {
		t.Fatalf("seed regen: %v", err)
	}
	if err := run(path, true); err != nil {
		t.Errorf("check mode should pass on fresh file; got: %v", err)
	}
}

func TestRun_MissingFile(t *testing.T) {
	err := run(filepath.Join(t.TempDir(), "does-not-exist.md"), false)
	if err == nil {
		t.Fatal("expected error when output path does not exist")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("error should mention read failure; got: %v", err)
	}
}

func TestRun_MissingMarkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cli.md")
	if err := os.WriteFile(path, []byte("no markers here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(path, false)
	if err == nil {
		t.Fatal("expected error when markers are absent")
	}
	if !strings.Contains(err.Error(), "marker") {
		t.Errorf("error should mention marker; got: %v", err)
	}
}
