package i18n

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// drift_test.go holds the two i18n drift guards from the 2026-06-15 audit
// (#2009 #9 and #10). They are deliberately filesystem-driven (reading the
// locale JSON and the templ sources directly) rather than going through the
// embedded bundle, so they assert the on-disk source of truth a contributor
// edits, not a snapshot.

// repoRoot returns the repository root relative to this package
// (internal/i18n), so the guards can read web/templates and the locale files
// regardless of where `go test` is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	// internal/i18n -> ../.. is the repo root.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return root
}

// loadLocaleKeys reads internal/i18n/locales/<loc>.json and returns its key set.
func loadLocaleKeys(t *testing.T, root, loc string) map[string]struct{} {
	t.Helper()
	path := filepath.Join(root, "internal", "i18n", "locales", loc+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	keys := make(map[string]struct{}, len(m))
	for k := range m {
		keys[k] = struct{}{}
	}
	return keys
}

// TestLocaleCompleteness guards #2009 #10: every key in a non-English locale
// must exist in en.json. en is the fallback and the canonical key set; a key
// removed from en.json would otherwise become a silent orphan in fr/ja (the
// fallback chain would return the bare key). The reverse direction is NOT
// asserted -- fr/ja are intentionally partial (they translate a subset and
// fall back to en for the rest), so a key present in en but absent from fr/ja
// is expected, not drift.
func TestLocaleCompleteness(t *testing.T) {
	root := repoRoot(t)
	en := loadLocaleKeys(t, root, "en")

	// Discover the non-English locales from the locales directory rather than
	// hard-coding them: a newly added locale file must be checked automatically,
	// otherwise this guard recreates the duplicate-truth problem it exists to
	// prevent (a new locale silently unguarded until someone edits this slice).
	localesDir := filepath.Join(root, "internal", "i18n", "locales")
	entries, err := os.ReadDir(localesDir)
	if err != nil {
		t.Fatalf("reading %s: %v", localesDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		loc := strings.TrimSuffix(entry.Name(), ".json")
		if loc == "en" {
			continue // en is the canonical key set, not a target.
		}
		t.Run(loc, func(t *testing.T) {
			var orphans []string
			for k := range loadLocaleKeys(t, root, loc) {
				if _, ok := en[k]; !ok {
					orphans = append(orphans, k)
				}
			}
			sort.Strings(orphans)
			if len(orphans) > 0 {
				t.Errorf("%s.json has %d key(s) not present in en.json (orphaned -- the fallback chain will return the bare key); remove them or add the key to en.json: %v",
					loc, len(orphans), orphans)
			}
		})
	}
}

// templKeyRE matches the canonical translation call in templ sources:
// t(ctx, "some.key"). Only string-literal keys are extracted; dynamically
// built keys (t(ctx, "prefix."+x)) are intentionally out of scope.
var templKeyRE = regexp.MustCompile(`\bt\(ctx,\s*"([^"]+)"\)`)

// stripLineComments removes `//` line comments from templ source so that an
// example call written in a doc comment (e.g. `// fetched via t(ctx, "key")`)
// is not mistaken for a real translation call. A `//` immediately preceded by
// `:` is treated as part of a URL (https://...), not a comment.
func stripLineComments(src []byte) []byte {
	lines := strings.Split(string(src), "\n")
	for i, line := range lines {
		for j := 0; j+1 < len(line); j++ {
			if line[j] == '/' && line[j+1] == '/' && (j == 0 || line[j-1] != ':') {
				lines[i] = line[:j]
				break
			}
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// TestTranslationKeysDefined guards #2009 #9 (the used-but-undefined direction):
// every string-literal key passed to t(ctx, "...") in a templ source must exist
// in en.json. A missing key renders the bare key string in the UI -- a visible,
// user-facing defect that nothing else catches.
//
// Only the used-but-undefined direction is asserted as a hard failure. The
// reverse (defined-but-unused) is deliberately omitted: keys are also consumed
// from Go handlers and via dynamically constructed names, so a templ-only scan
// would flag many false positives, and the maintainer prefers a precise guard
// over a noisy one.
func TestTranslationKeysDefined(t *testing.T) {
	root := repoRoot(t)
	en := loadLocaleKeys(t, root, "en")

	missing := map[string][]string{} // key -> files referencing it

	// Scan every templ surface, not just web/templates: web/components holds
	// reusable templ fragments that also call t(ctx, "..."), so an undefined key
	// there would otherwise ship unguarded.
	for _, templDir := range []string{
		filepath.Join(root, "web", "templates"),
		filepath.Join(root, "web", "components"),
	} {
		err := filepath.WalkDir(templDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".templ" {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			src = stripLineComments(src)
			for _, m := range templKeyRE.FindAllSubmatch(src, -1) {
				key := string(m[1])
				if _, ok := en[key]; !ok {
					missing[key] = append(missing[key], rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", templDir, err)
		}
	}

	if len(missing) > 0 {
		keys := make([]string, 0, len(missing))
		for k := range missing {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Errorf("translation key %q used in %v is not defined in en.json (renders as the bare key in the UI)", k, missing[k])
		}
	}
}
