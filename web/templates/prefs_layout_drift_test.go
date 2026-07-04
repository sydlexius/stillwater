package templates

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// jsDefaultOrderRE captures the contents of every
// `var DEFAULT_ORDER = [ ... ];` array literal in prefs-drawer.js. The array
// is declared in more than one function scope there, and each copy must match
// the Go default order, so all occurrences are checked.
var jsDefaultOrderRE = regexp.MustCompile(`var DEFAULT_ORDER\s*=\s*\[([^\]]*)\]`)

// TestPrefsLayoutSectionOrderParity guards #2009 #2: the artist-detail section
// order is declared in Go (defaultPrefsLayoutSections in prefs_drawer.templ) and
// duplicated as DEFAULT_ORDER in web/static/js/prefs-drawer.js (used by the
// reset button and the full-reset PATCH). A reorder or rename in one place that
// is not mirrored in the other silently breaks reset/full-reset, with no guard
// today. This asserts every JS DEFAULT_ORDER copy equals the Go order exactly.
func TestPrefsLayoutSectionOrderParity(t *testing.T) {
	// Go source of truth (in-package access to the unexported var).
	want := make([]string, len(defaultPrefsLayoutSections))
	for i, s := range defaultPrefsLayoutSections {
		want[i] = s.ID
	}

	// web/templates -> ../.. is the repo root.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	jsPath := filepath.Join(root, "web", "static", "js", "prefs-drawer.js")
	src, err := os.ReadFile(jsPath)
	if err != nil {
		t.Fatalf("reading %s: %v", jsPath, err)
	}

	matches := jsDefaultOrderRE.FindAllSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatalf("no `var DEFAULT_ORDER = [...]` literal found in %s -- did the JS rename it? Update this guard.", jsPath)
	}

	for i, m := range matches {
		got := parseJSStringArray(string(m[1]))
		if !equalStrings(got, want) {
			t.Errorf("DEFAULT_ORDER copy #%d in prefs-drawer.js = %v, want %v (must match defaultPrefsLayoutSections in prefs_drawer.templ)", i+1, got, want)
		}
	}
}

// parseJSStringArray turns `'a', 'b', 'c'` (the inside of a JS array literal of
// single- or double-quoted strings) into []string{"a","b","c"}.
func parseJSStringArray(inner string) []string {
	var out []string
	for _, part := range strings.Split(inner, ",") {
		s := strings.TrimSpace(part)
		s = strings.Trim(s, `'"`)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
