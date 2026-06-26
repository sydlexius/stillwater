package api

// TestJSDefaultsMatchGoRegistry validates that the DEFAULTS object in
// web/static/js/preferences.js is in sync with the Go preference registry
// (PreferenceRegistry). This catches drift between the two sources that
// previously had no automated link, preventing silent mismatches where the
// client applies a different default than the server stores.
//
// The test is intentionally NOT in a _test package so it can reach the same
// package-level constants (BgOpacityDefault, PageSizeDefault, etc.) and the
// internal preferenceDefaults map through PreferenceRegistry without needing
// an additional exported surface.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// jsPrefsPath is the path to preferences.js relative to this test file's
// package directory (internal/api/). Go tests run with the working directory
// set to the package being tested, so two directory levels up reaches the repo
// root, from which the web/ tree is accessible.
const jsPrefsPath = "../../web/static/js/preferences.js"

// serverOnlyKeys is the set of preference keys that exist in the Go registry
// but are intentionally absent from the JS DEFAULTS object. These keys are
// either server-side-only (page_size is applied server-side and never needed
// by client-side default logic) or structurally complex (the romanization flag
// is stored as a string but managed server-side only).
//
// Note: metadata_languages, artist_detail_section_order,
// artist_detail_hidden_sections, and artist_detail_collapsed_sections are
// excluded from PreferenceRegistry() entirely (they cannot be represented as a
// flat key/default table) and therefore never appear in the registry loop below
// -- no guard is needed for them here.
var serverOnlyKeys = map[string]bool{
	PrefPageSize:                 true,
	PrefMetadataNameRomanization: true,
}

// parseJSDefaults reads preferences.js and returns the contents of the
// var DEFAULTS = { ... }; object as a map[string]string. It uses a simple
// regex approach that is intentionally tied to the literal object syntax in
// the file -- if the file format changes substantially, this test should be
// updated alongside it.
func parseJSDefaults(t *testing.T) map[string]string {
	t.Helper()

	data, err := os.ReadFile(jsPrefsPath)
	if err != nil {
		t.Fatalf("parseJSDefaults: read %s: %v", jsPrefsPath, err)
	}

	// Extract the var DEFAULTS = { ... }; block. The (?s) flag makes "."
	// match newlines so the entire multi-line block is captured in one pass.
	blockRE := regexp.MustCompile(`(?s)var DEFAULTS = \{(.*?)\};`)
	matches := blockRE.FindSubmatch(data)
	if matches == nil {
		t.Fatalf("parseJSDefaults: could not find 'var DEFAULTS = { ... };' block in %s", jsPrefsPath)
	}

	// Parse key: 'value' pairs within the captured block. Comment lines
	// (starting with //) are skipped implicitly because they do not match
	// the (\w+): pattern. Trailing commas are ignored by the regex.
	// NOTE: pairRE assumes all DEFAULTS values are single-quoted strings. If
	// preferences.js is ever updated to use double quotes or numeric literals,
	// this regex (and the test) must be updated to match.
	pairRE := regexp.MustCompile(`\s*(\w+):\s*'([^']*)'`)
	pairMatches := pairRE.FindAllStringSubmatch(string(matches[1]), -1)

	if len(pairMatches) == 0 {
		t.Fatalf("parseJSDefaults: parsed zero key-value pairs from DEFAULTS block in %s; check the regex", jsPrefsPath)
	}

	result := make(map[string]string, len(pairMatches))
	for _, m := range pairMatches {
		result[m[1]] = m[2]
	}
	return result
}

func TestJSDefaultsMatchGoRegistry(t *testing.T) {
	jsDefaults := parseJSDefaults(t)
	goRegistry := PreferenceRegistry()

	// Build a quick-lookup map from the registry slice.
	goByKey := make(map[string]PreferenceDef, len(goRegistry))
	for _, def := range goRegistry {
		goByKey[def.Key] = def
	}

	// Every key in JS DEFAULTS must exist in the Go registry with a matching
	// default value. A JS key that has no Go counterpart indicates either a
	// dead JS key or a Go key that was removed without updating preferences.js.
	for jsKey, jsVal := range jsDefaults {
		goDef, ok := goByKey[jsKey]
		if !ok {
			t.Errorf("JS DEFAULTS key %q not found in Go PreferenceRegistry()", jsKey)
			continue
		}
		// Compare string values; bg_opacity stores "85" in both sources.
		if jsVal != goDef.Default {
			t.Errorf("JS DEFAULTS[%q] = %q, but Go registry default = %q; update preferences.js to match", jsKey, jsVal, goDef.Default)
		}
	}

	// Every Go registry key that is not server-only must appear in JS DEFAULTS.
	// A Go key that is missing from JS DEFAULTS means the client falls back to
	// an undefined default, which may differ from the server's stored default.
	for _, def := range goRegistry {
		if serverOnlyKeys[def.Key] {
			continue
		}
		// Also skip keys with the suppress_confirm_ prefix: they are
		// dynamically created by the UI and have no static DEFAULTS entry.
		if strings.HasPrefix(def.Key, PrefSuppressConfirmPrefix) {
			continue
		}
		if _, ok := jsDefaults[def.Key]; !ok {
			t.Errorf("Go registry key %q (default %q) not found in JS DEFAULTS in %s; add it or add to serverOnlyKeys", def.Key, def.Default, jsPrefsPath)
		}
	}
}
