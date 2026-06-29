package templates

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readSettingsModule returns the source of a settings JS module extracted out of
// settings.templ (M55 #1808). Tests run with the working directory set to the
// package dir (web/templates), so the vendored module sits one level up under
// web/static/js/settings.
func readSettingsModule(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "static", "js", "settings", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestSettingsSearchEntryJSON verifies that SettingsSearchEntry marshals to the
// expected JSON shape and that all required fields are present.
func TestSettingsSearchEntryJSON(t *testing.T) {
	entry := SettingsSearchEntry{
		ID:       "help-platform-profile",
		Label:    "Platform Profile",
		HelpText: "Profiles bundle the NFO format and image conventions.",
		TabID:    "general",
	}

	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	checks := map[string]string{
		"id":    "help-platform-profile",
		"label": "Platform Profile",
		"help":  "Profiles bundle the NFO format and image conventions.",
		"tab":   "general",
	}
	for field, want := range checks {
		if got[field] != want {
			t.Errorf("field %q: got %q, want %q", field, got[field], want)
		}
	}
}

// TestBuildSettingsSearchIndex verifies the search index contains entries for
// every settings tab and that all entries are well-formed.
func TestBuildSettingsSearchIndex(t *testing.T) {
	ctx := testCtx(t)
	index := BuildSettingsSearchIndex(ctx)

	if len(index) == 0 {
		t.Fatal("BuildSettingsSearchIndex returned empty index")
	}

	// All tabs that should have at least one entry.
	expectedTabs := []SettingsTabID{
		TabGeneral, TabProviders, TabConnections, TabLibraries,
		TabAutomation, TabRules, TabUsers, TabAuthProviders,
		TabMaintenance, TabLogs, TabUpdates,
	}
	tabsSeen := make(map[SettingsTabID]int)

	for i, e := range index {
		if e.ID == "" {
			t.Errorf("entry[%d]: empty ID", i)
		}
		if e.Label == "" {
			t.Errorf("entry[%d] (id=%q): empty Label", i, e.ID)
		}
		if e.HelpText == "" {
			t.Errorf("entry[%d] (id=%q): empty HelpText", i, e.ID)
		}
		if e.TabID == "" {
			t.Errorf("entry[%d] (id=%q): empty TabID", i, e.ID)
		}
		// All IDs should follow the "help-" convention used by ContextHelp.
		if !strings.HasPrefix(e.ID, "help-") {
			t.Errorf("entry[%d] (id=%q): ID should start with \"help-\"", i, e.ID)
		}
		tabsSeen[e.TabID]++
	}

	for _, tab := range expectedTabs {
		if tabsSeen[tab] == 0 {
			t.Errorf("no entries for tab %q", tab)
		}
	}
}

// TestBuildSettingsSearchIndex_UniqueIDs guards against accidental duplicate
// entries in the hand-curated index; a duplicate ID would silently make one
// anchor unreachable when a search result is clicked.
func TestBuildSettingsSearchIndex_UniqueIDs(t *testing.T) {
	ctx := testCtx(t)
	seen := make(map[string]int)
	for _, e := range BuildSettingsSearchIndex(ctx) {
		seen[e.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("duplicate entry ID %q appears %d times", id, n)
		}
	}
}

// TestBuildSettingsSearchIndex_TabIDsMatchSettingsTabs cross-checks each
// entry's TabID against the canonical settingsTabs() list. A typoed TabID
// otherwise greys out the entry's tab during search without surfacing the
// mismatch.
func TestBuildSettingsSearchIndex_TabIDsMatchSettingsTabs(t *testing.T) {
	ctx := testCtx(t)
	valid := make(map[SettingsTabID]bool)
	for _, tab := range settingsTabs() {
		valid[tab.ID] = true
	}
	for _, e := range BuildSettingsSearchIndex(ctx) {
		if !valid[e.TabID] {
			t.Errorf("entry %q has TabID %q not in settingsTabs()", e.ID, e.TabID)
		}
	}
}

// TestSettingsSearchIndexScript_EmitsWindowGlobal renders the
// settingsSearchIndexScript templ and verifies the inline script emits the
// expected `window.swSettingsSearchIndex` assignment with the marshaled JSON
// of the supplied index. Regressions to the global name or the inline
// emission strategy would silently break the client-side filter.
func TestSettingsSearchIndexScript_EmitsWindowGlobal(t *testing.T) {
	var buf bytes.Buffer
	index := []SettingsSearchEntry{
		{ID: "help-x", Label: "X", HelpText: "x help", TabID: TabGeneral},
	}
	if err := SettingsSearchIndexScript(index).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "window.swSettingsSearchIndex") {
		t.Errorf("output missing window.swSettingsSearchIndex assignment: %s", out)
	}
	if !strings.Contains(out, `"id":"help-x"`) {
		t.Errorf("output missing entry id: %s", out)
	}
	if !strings.Contains(out, `"tab":"general"`) {
		t.Errorf("output missing entry tab: %s", out)
	}
}

// TestSettingsSearchScript_EntryPointAndShortcut reads the extracted search.js
// module (M55 #1808) and verifies the deferred-init entry point and the `/`
// shortcut handler are both present. A future refactor that drops either
// would silently break the search box wiring or the keyboard shortcut.
func TestSettingsSearchScript_EntryPointAndShortcut(t *testing.T) {
	out := readSettingsModule(t, "search.js")
	if !strings.Contains(out, "swInitSettingsSearch") {
		t.Errorf("output missing swInitSettingsSearch entry point: %s", out)
	}
	if !strings.Contains(out, "DOMContentLoaded") {
		t.Errorf("output missing DOMContentLoaded gating: %s", out)
	}
	if !strings.Contains(out, "e.key !== '/'") {
		t.Errorf("output missing '/' shortcut guard: %s", out)
	}
}

// TestSettingsTabBar_RendersSearchBoxAndTabs renders settingsTabBar and
// verifies it emits the search input element plus every tab from
// settingsTabs() with a matching `data-tab` attribute. The client filter
// targets these attributes; missing any one silently makes that tab
// unreachable via search-driven greying.
func TestSettingsTabBar_RendersSearchBoxAndTabs(t *testing.T) {
	var buf bytes.Buffer
	if err := settingsTabBar(TabGeneral, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `id="settings-search-input"`) {
		t.Errorf("output missing search input element: %s", out)
	}
	for _, tab := range settingsTabs() {
		want := `data-tab="` + string(tab.ID) + `"`
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s", want)
		}
	}
}

// TestJsonSearchIndex verifies that jsonSearchIndex produces valid JSON and
// falls back to "[]" on an un-marshalable value.
func TestJsonSearchIndex(t *testing.T) {
	ctx := testCtx(t)
	index := BuildSettingsSearchIndex(ctx)
	out := jsonSearchIndex(index)

	if !json.Valid([]byte(out)) {
		preview := out
		if len(preview) > 80 {
			preview = preview[:80]
		}
		t.Errorf("jsonSearchIndex output is not valid JSON: %s", preview)
	}

	// Spot-check one known entry is present.
	if !strings.Contains(out, "help-platform-profile") {
		t.Error("expected \"help-platform-profile\" in JSON output")
	}

	// Empty slice should produce "[]".
	empty := jsonSearchIndex([]SettingsSearchEntry{})
	if empty != "[]" {
		t.Errorf("empty index: got %q, want \"[]\"", empty)
	}
}

// TestSettingsSearchScript_HighlightsMatchedControls verifies that the
// search.js module (M55 #1808) includes the per-control highlight logic. A
// regression that dropped data-search-match would silently break the CSS
// outline on matched controls without any visible JS error.
func TestSettingsSearchScript_HighlightsMatchedControls(t *testing.T) {
	out := readSettingsModule(t, "search.js")
	if !strings.Contains(out, "data-search-match") {
		t.Error("script missing data-search-match attribute handling")
	}
	// The JS should set the attribute on matched control elements.
	if !strings.Contains(out, `setAttribute('data-search-match'`) {
		t.Error("script missing setAttribute for data-search-match")
	}
}

// TestSettingsSearchScript_ClearFilterResets verifies that the clearFilter
// function in search.js (M55 #1808) removes both data-search-match and
// data-search-flash attributes and resets the per-tab chrome. If clearFilter
// omits either cleanup, stale highlights remain visible after the query is
// erased.
func TestSettingsSearchScript_ClearFilterResets(t *testing.T) {
	out := readSettingsModule(t, "search.js")
	if !strings.Contains(out, "data-search-match") {
		t.Error("clearFilter: script missing data-search-match cleanup")
	}
	if !strings.Contains(out, "data-search-flash") {
		t.Error("clearFilter: script missing data-search-flash cleanup")
	}
	if !strings.Contains(out, "removeAttribute('data-match-count')") {
		t.Error("clearFilter: script missing removeAttribute for data-match-count")
	}
}
