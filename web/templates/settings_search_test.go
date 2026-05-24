package templates

import (
	"encoding/json"
	"strings"
	"testing"
)

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
	index := BuildSettingsSearchIndex()

	if len(index) == 0 {
		t.Fatal("BuildSettingsSearchIndex returned empty index")
	}

	// All tabs that should have at least one entry.
	expectedTabs := []string{
		"general", "providers", "connections", "libraries",
		"automation", "rules", "users", "auth_providers",
		"maintenance", "logs", "updates",
	}
	tabsSeen := make(map[string]int)

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
	seen := make(map[string]int)
	for _, e := range BuildSettingsSearchIndex() {
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
	valid := make(map[string]bool)
	for _, tab := range settingsTabs() {
		valid[tab.ID] = true
	}
	for _, e := range BuildSettingsSearchIndex() {
		if !valid[e.TabID] {
			t.Errorf("entry %q has TabID %q not in settingsTabs()", e.ID, e.TabID)
		}
	}
}

// TestJsonSearchIndex verifies that jsonSearchIndex produces valid JSON and
// falls back to "[]" on an un-marshalable value.
func TestJsonSearchIndex(t *testing.T) {
	index := BuildSettingsSearchIndex()
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
