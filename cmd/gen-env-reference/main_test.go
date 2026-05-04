package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixtureGood is a minimal struct that exercises every supported field shape:
// a primitive int with a default, a nested struct, a string with a path-style
// name, a boolean, and a slice. Tests reflect over this fixture rather than
// the real config.Config so the codegen contract is verified in isolation.
type fixtureGood struct {
	Port  int `env:"FX_PORT" default:"8080" desc:"TCP port the server listens on."`
	Inner fixtureInner
}

type fixtureInner struct {
	DBPath  string   `env:"FX_DB_PATH" default:"/data/x.db" desc:"Path to the database file."`
	Enabled bool     `env:"FX_ENABLED" default:"true" desc:"Whether the feature is enabled."`
	List    []string `env:"FX_LIST" default:"a,b" desc:"Comma-separated list."`
	Secret  string   `env:"FX_SECRET" default:"unset" desc:"Generated on first run when unset."`
	Bare    string   `env:"FX_BARE" desc:"Bare value with no documented default."`
	Skipped string   // no env tag, must be ignored
}

// fixtureMissingDesc has an env tag but no desc tag; collectRows must reject
// it so the docs page can never silently ship with an empty description.
type fixtureMissingDesc struct {
	Port int `env:"FX_PORT" default:"1"`
}

func TestCollectRows_Fixture(t *testing.T) {
	rows, err := collectRows(reflect.TypeOf(fixtureGood{}))
	if err != nil {
		t.Fatalf("collectRows: %v", err)
	}
	wantNames := []string{"FX_BARE", "FX_DB_PATH", "FX_ENABLED", "FX_LIST", "FX_PORT", "FX_SECRET"}
	if len(rows) != len(wantNames) {
		t.Fatalf("expected %d rows, got %d (%v)", len(wantNames), len(rows), rows)
	}
	for i, want := range wantNames {
		if rows[i].Name != want {
			t.Errorf("rows[%d].Name = %q, want %q (output not alphabetically sorted)", i, rows[i].Name, want)
		}
	}

	// Spot-check type mapping.
	byName := map[string]envRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if got := byName["FX_PORT"].Type; got != "integer" {
		t.Errorf("FX_PORT type = %q, want integer", got)
	}
	if got := byName["FX_DB_PATH"].Type; got != "path" {
		t.Errorf("FX_DB_PATH type = %q, want path (PATH-named string)", got)
	}
	if got := byName["FX_ENABLED"].Type; got != "boolean" {
		t.Errorf("FX_ENABLED type = %q, want boolean", got)
	}
	if got := byName["FX_LIST"].Type; got != "list (comma-separated)" {
		t.Errorf("FX_LIST type = %q, want list label", got)
	}

	// Default rendering: backticked literal, "unset" pass-through, and "(none)"
	// for an absent default.
	if got := byName["FX_PORT"].Default; got != "`8080`" {
		t.Errorf("FX_PORT default = %q, want `8080`", got)
	}
	if got := byName["FX_SECRET"].Default; got != "unset" {
		t.Errorf("FX_SECRET default = %q, want unset", got)
	}
	if got := byName["FX_BARE"].Default; got != "(none)" {
		t.Errorf("FX_BARE default = %q, want (none)", got)
	}
}

func TestCollectRows_MissingDescIsError(t *testing.T) {
	_, err := collectRows(reflect.TypeOf(fixtureMissingDesc{}))
	if err == nil {
		t.Fatal("expected error when env-tagged field has no desc tag")
	}
	if !strings.Contains(err.Error(), "FX_PORT") {
		t.Errorf("error should name the offending env var; got %v", err)
	}
}

func TestRenderTable_HeaderAndRows(t *testing.T) {
	rows := []envRow{
		{Name: "A_VAR", Type: "string", Default: "`x`", Description: "First var."},
		{Name: "B_VAR", Type: "integer", Default: "(none)", Description: "Second var."},
	}
	got := renderTable(rows)
	wantHeader := "| Variable | Type | Default | Description |\n|---|---|---|---|\n"
	if !strings.HasPrefix(got, wantHeader) {
		t.Fatalf("missing or wrong header.\ngot:\n%s", got)
	}
	if !strings.Contains(got, "| `A_VAR` | string | `x` | First var. |") {
		t.Errorf("first row not rendered as expected; got:\n%s", got)
	}
	if !strings.Contains(got, "| `B_VAR` | integer | (none) | Second var. |") {
		t.Errorf("second row not rendered as expected; got:\n%s", got)
	}
}

func TestRenderTable_EscapesPipesAndNewlines(t *testing.T) {
	rows := []envRow{
		{Name: "PIPE_VAR", Type: "string", Default: "`a|b`", Description: "Has a | pipe and\na newline."},
	}
	got := renderTable(rows)
	if !strings.Contains(got, `\|b`) {
		t.Errorf("pipe in default should be escaped; got:\n%s", got)
	}
	if !strings.Contains(got, "Has a \\| pipe and<br>a newline.") {
		t.Errorf("pipe and newline in description should be escaped; got:\n%s", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Count(line, "|")-strings.Count(line, `\|`) != 5 {
			t.Errorf("each row should have exactly 5 unescaped pipes; got line:\n%s", line)
		}
	}
}

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

// writeFixtureDoc writes a docs file with the begin/end markers wrapping
// stale body text, returns the path. Used by run() tests.
func writeFixtureDoc(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "env.md")
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
	if !strings.Contains(string(got), "| Variable | Type | Default | Description |") {
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
	infoBefore, err := os.Stat(path)
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
		t.Errorf("second run should be a no-op")
	}
	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Logf("note: file mtime changed despite no content change (acceptable)")
	}
}

func TestRun_CheckMode(t *testing.T) {
	t.Run("stale errors", func(t *testing.T) {
		path := writeFixtureDoc(t, "STALE")
		err := run(path, true)
		if err == nil {
			t.Fatal("expected error in -check mode against stale file")
		}
		if !strings.Contains(err.Error(), "stale") {
			t.Errorf("error should mention staleness; got: %v", err)
		}
	})
	t.Run("fresh succeeds", func(t *testing.T) {
		path := writeFixtureDoc(t, "STALE")
		if err := run(path, false); err != nil {
			t.Fatalf("seed regen: %v", err)
		}
		if err := run(path, true); err != nil {
			t.Errorf("check mode should pass on fresh file; got: %v", err)
		}
	})
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
	path := filepath.Join(dir, "env.md")
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
