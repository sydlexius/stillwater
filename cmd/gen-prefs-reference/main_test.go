package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api"
)

// ---- helpers ----------------------------------------------------------------

// writeFixtureDoc creates a minimal Markdown file with begin/end markers
// wrapping the given stale body text and returns its path. The file lives in a
// temp dir so tests never touch the real docs tree.
func writeFixtureDoc(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.md")
	content := "intro\n" + beginMarker + "\n" + body + "\n" + endMarker + "\nfooter\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	return path
}

// writeI18nFixture writes a minimal en.json file and returns its path.
func writeI18nFixture(t *testing.T, entries map[string]string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("{\n")
	i := 0
	for k, v := range entries {
		comma := ","
		if i == len(entries)-1 {
			comma = ""
		}
		b.WriteString(`  "` + k + `": "` + v + `"` + comma + "\n")
		i++
	}
	b.WriteString("}\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "en.json")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("seed i18n fixture: %v", err)
	}
	return path
}

// syntheticEntry returns a minimal enum-type PreferenceDef for use in tests
// that need a registry entry without calling the real PreferenceRegistry.
func syntheticEntry(key, def string, allowed ...string) api.PreferenceDef {
	return api.PreferenceDef{Key: key, Default: def, AllowedValues: allowed}
}

// ---- escapeMarkdownCell -----------------------------------------------------

func TestEscapeMarkdownCell_Pipe(t *testing.T) {
	got := escapeMarkdownCell("a|b")
	if got != `a\|b` {
		t.Errorf("pipe not escaped: got %q", got)
	}
}

func TestEscapeMarkdownCell_Newline(t *testing.T) {
	got := escapeMarkdownCell("line1\nline2")
	if got != "line1<br>line2" {
		t.Errorf("newline not replaced with <br>: got %q", got)
	}
}

func TestEscapeMarkdownCell_MultiplePipes(t *testing.T) {
	got := escapeMarkdownCell("a|b|c")
	if got != `a\|b\|c` {
		t.Errorf("multiple pipes not escaped: got %q", got)
	}
}

func TestEscapeMarkdownCell_Clean(t *testing.T) {
	got := escapeMarkdownCell("no special chars")
	if got != "no special chars" {
		t.Errorf("clean string should pass through unchanged: got %q", got)
	}
}

// ---- formatAllowed ----------------------------------------------------------

func TestFormatAllowed_Enum(t *testing.T) {
	e := syntheticEntry("theme", "dark", "light", "dark", "system")
	got := formatAllowed(e)
	if !strings.Contains(got, "`light`") || !strings.Contains(got, "`dark`") || !strings.Contains(got, "`system`") {
		t.Errorf("enum values not all backtick-formatted: got %q", got)
	}
}

func TestFormatAllowed_RangeWithStep(t *testing.T) {
	e := api.PreferenceDef{Key: "bg_opacity", Default: "85", RangeMin: 20, RangeMax: 100, RangeStep: 5}
	got := formatAllowed(e)
	want := "20..100 (step 5)"
	if got != want {
		t.Errorf("formatAllowed range+step = %q, want %q", got, want)
	}
}

func TestFormatAllowed_RangeNoStep(t *testing.T) {
	e := api.PreferenceDef{Key: "page_size", Default: "50", RangeMin: 10, RangeMax: 500, RangeStep: 1}
	got := formatAllowed(e)
	want := "10..500"
	if got != want {
		t.Errorf("formatAllowed range (step 1) = %q, want %q", got, want)
	}
}

func TestFormatAllowed_Empty(t *testing.T) {
	e := api.PreferenceDef{Key: "orphan", Default: "x"}
	got := formatAllowed(e)
	if got != "" {
		t.Errorf("entry with no AllowedValues and no range should return empty string; got %q", got)
	}
}

// ---- resolveLabel -----------------------------------------------------------

func TestResolveLabel_PrefsNamespace(t *testing.T) {
	i18n := map[string]string{"prefs.theme.label": "Theme"}
	got := resolveLabel("theme", i18n)
	if got != "Theme" {
		t.Errorf("resolveLabel(theme) = %q, want %q", got, "Theme")
	}
}

func TestResolveLabel_SettingsAppearanceFallback(t *testing.T) {
	// No prefs.density.label; falls back to settings.appearance.density.label.
	i18n := map[string]string{"settings.appearance.density.label": "Layout Density"}
	got := resolveLabel("density", i18n)
	if got != "Layout Density" {
		t.Errorf("resolveLabel(density) fallback = %q, want %q", got, "Layout Density")
	}
}

func TestResolveLabel_SnakeTitleFallback(t *testing.T) {
	// Neither i18n namespace has an entry; snakeToTitle kicks in.
	got := resolveLabel("my_pref_key", map[string]string{})
	if got != "My Pref Key" {
		t.Errorf("resolveLabel fallback = %q, want %q", got, "My Pref Key")
	}
}

func TestResolveLabel_Override_AutoFetch(t *testing.T) {
	// auto_fetch_images is overridden to prefs.auto_fetch.* namespace.
	i18n := map[string]string{
		"prefs.auto_fetch.label":        "Prefetch Images",
		"prefs.auto_fetch_images.label": "WRONG - should not be used",
	}
	got := resolveLabel("auto_fetch_images", i18n)
	if got != "Prefetch Images" {
		t.Errorf("resolveLabel(auto_fetch_images) = %q, want %q (override not applied)", got, "Prefetch Images")
	}
}

func TestResolveLabel_Override_SidebarState(t *testing.T) {
	// sidebar_state is overridden to settings.appearance.sidebar_default_state.* namespace.
	i18n := map[string]string{
		"settings.appearance.sidebar_default_state.label": "Sidebar Default State",
		"settings.appearance.sidebar_state.label":         "WRONG - should not be used",
	}
	got := resolveLabel("sidebar_state", i18n)
	if got != "Sidebar Default State" {
		t.Errorf("resolveLabel(sidebar_state) = %q, want %q (override not applied)", got, "Sidebar Default State")
	}
}

// ---- resolveDescription -----------------------------------------------------

func TestResolveDescription_PrefsDescription(t *testing.T) {
	i18n := map[string]string{"prefs.theme.description": "Choose color scheme."}
	got := resolveDescription("theme", i18n)
	if got != "Choose color scheme." {
		t.Errorf("resolveDescription(theme) = %q, want description", got)
	}
}

func TestResolveDescription_PrefsHelpFallback(t *testing.T) {
	// No description; falls back to prefs.{key}.help.
	i18n := map[string]string{"prefs.theme.help": "Tip: system follows your OS."}
	got := resolveDescription("theme", i18n)
	if got != "Tip: system follows your OS." {
		t.Errorf("resolveDescription help fallback = %q, want help text", got)
	}
}

func TestResolveDescription_Override_AutoFetch(t *testing.T) {
	// auto_fetch_images override resolves description from prefs.auto_fetch.* namespace.
	i18n := map[string]string{
		"prefs.auto_fetch.description":        "Automatically search providers for images.",
		"prefs.auto_fetch_images.description": "WRONG - should not be used",
	}
	got := resolveDescription("auto_fetch_images", i18n)
	if got != "Automatically search providers for images." {
		t.Errorf("resolveDescription(auto_fetch_images) = %q, override not applied", got)
	}
}

func TestResolveDescription_MissingReturnsEmpty(t *testing.T) {
	got := resolveDescription("unknown_key", map[string]string{})
	if got != "" {
		t.Errorf("missing description should return empty string; got %q", got)
	}
}

// ---- buildRows --------------------------------------------------------------

func TestBuildRows_Sorted(t *testing.T) {
	entries := []api.PreferenceDef{
		syntheticEntry("z_key", "z", "z1", "z2"),
		syntheticEntry("a_key", "a", "a1"),
	}
	i18n := map[string]string{
		"prefs.z_key.label": "Z Key",
		"prefs.a_key.label": "A Key",
	}
	rows := buildRows(entries, i18n)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Key != "a_key" || rows[1].Key != "z_key" {
		t.Errorf("rows not sorted alphabetically: got %v, %v", rows[0].Key, rows[1].Key)
	}
}

func TestBuildRows_LabelAndDescription(t *testing.T) {
	entries := []api.PreferenceDef{syntheticEntry("theme", "dark", "light", "dark", "system")}
	i18n := map[string]string{
		"prefs.theme.label":       "Theme",
		"prefs.theme.description": "Pick a color scheme.",
	}
	rows := buildRows(entries, i18n)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Label != "Theme" {
		t.Errorf("Label = %q, want %q", r.Label, "Theme")
	}
	if r.Description != "Pick a color scheme." {
		t.Errorf("Description = %q, want description text", r.Description)
	}
	if !strings.Contains(r.AllowedStr, "`dark`") {
		t.Errorf("AllowedStr should contain backtick-formatted value; got %q", r.AllowedStr)
	}
}

func TestBuildRows_PipeInDescription(t *testing.T) {
	entries := []api.PreferenceDef{syntheticEntry("theme", "dark", "light", "dark")}
	i18n := map[string]string{
		"prefs.theme.label":       "Theme",
		"prefs.theme.description": "Light | dark mode toggle.",
	}
	rows := buildRows(entries, i18n)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	// The description is stored raw in prefRow; escaping happens in renderTable.
	// Verify the raw value so the pipeline is clear.
	if rows[0].Description != "Light | dark mode toggle." {
		t.Errorf("description stored with unescaped pipe: got %q", rows[0].Description)
	}
}

// ---- replaceBetweenMarkers --------------------------------------------------

func TestReplaceBetweenMarkers_Basic(t *testing.T) {
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
		t.Errorf("error should mention begin marker; got: %v", err)
	}
}

func TestReplaceBetweenMarkers_MissingEnd(t *testing.T) {
	src := []byte("prefix " + beginMarker + " no end")
	_, err := replaceBetweenMarkers(src, beginMarker, endMarker, "body")
	if err == nil {
		t.Fatal("expected error when end marker is missing")
	}
	if !strings.Contains(err.Error(), "end marker") {
		t.Errorf("error should mention end marker; got: %v", err)
	}
}

func TestReplaceBetweenMarkers_TrailingNewlinesNormalized(t *testing.T) {
	// Body with excess trailing newlines should produce exactly one trailing
	// newline before the end marker.
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

func TestRun_RewritesStaleContent(t *testing.T) {
	docPath := writeFixtureDoc(t, "STALE TABLE")
	i18nPath := writeI18nFixture(t, map[string]string{})
	if err := run(docPath, i18nPath, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "STALE TABLE") {
		t.Errorf("stale body should be replaced; got:\n%s", got)
	}
	if !strings.Contains(string(got), "| Key | Label | Default | Allowed Values | Description |") {
		t.Errorf("regenerated table header missing; got:\n%s", got)
	}
	if !strings.Contains(string(got), "intro\n") || !strings.Contains(string(got), "footer\n") {
		t.Errorf("manual prose around markers should be preserved; got:\n%s", got)
	}
}

func TestRun_NoChangeIsNoop(t *testing.T) {
	docPath := writeFixtureDoc(t, "STALE")
	i18nPath := writeI18nFixture(t, map[string]string{})
	if err := run(docPath, i18nPath, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(docPath, i18nPath, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	after, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("second run should be a no-op (content unchanged)")
	}
}

func TestRun_CheckMode_StaleErrors(t *testing.T) {
	docPath := writeFixtureDoc(t, "STALE")
	i18nPath := writeI18nFixture(t, map[string]string{})
	err := run(docPath, i18nPath, true)
	if err == nil {
		t.Fatal("expected error in -check mode against stale file")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error should mention staleness; got: %v", err)
	}
}

func TestRun_CheckMode_FreshSucceeds(t *testing.T) {
	docPath := writeFixtureDoc(t, "STALE")
	i18nPath := writeI18nFixture(t, map[string]string{})
	if err := run(docPath, i18nPath, false); err != nil {
		t.Fatalf("seed regen: %v", err)
	}
	if err := run(docPath, i18nPath, true); err != nil {
		t.Errorf("check mode should pass on fresh file; got: %v", err)
	}
}

func TestRun_MissingOutputFile(t *testing.T) {
	i18nPath := writeI18nFixture(t, map[string]string{})
	err := run(filepath.Join(t.TempDir(), "does-not-exist.md"), i18nPath, false)
	if err == nil {
		t.Fatal("expected error when output path does not exist")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("error should mention read failure; got: %v", err)
	}
}

func TestRun_MissingI18nFile(t *testing.T) {
	docPath := writeFixtureDoc(t, "STALE")
	err := run(docPath, filepath.Join(t.TempDir(), "does-not-exist.json"), false)
	if err == nil {
		t.Fatal("expected error when i18n path does not exist")
	}
	if !strings.Contains(err.Error(), "i18n") {
		t.Errorf("error should mention i18n load failure; got: %v", err)
	}
}

func TestRun_MalformedI18nFile(t *testing.T) {
	docPath := writeFixtureDoc(t, "STALE")
	dir := t.TempDir()
	badJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSON, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(docPath, badJSON, false)
	if err == nil {
		t.Fatal("expected error for malformed i18n JSON")
	}
	if !strings.Contains(err.Error(), "i18n") {
		t.Errorf("error should mention i18n load failure; got: %v", err)
	}
}

func TestRun_MissingMarkers(t *testing.T) {
	dir := t.TempDir()
	docPath := filepath.Join(dir, "prefs.md")
	if err := os.WriteFile(docPath, []byte("no markers here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	i18nPath := writeI18nFixture(t, map[string]string{})
	err := run(docPath, i18nPath, false)
	if err == nil {
		t.Fatal("expected error when markers are absent")
	}
	if !strings.Contains(err.Error(), "marker") {
		t.Errorf("error should mention missing marker; got: %v", err)
	}
}
