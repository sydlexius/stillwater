package rule

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// maxFindingTitleLen is the chip-label budget for a finding title. Issue #1342:
// titles surface as chip labels on the artist-detail screen (#1336) where
// truncation is unacceptable, so the title must stand alone within this bound.
const maxFindingTitleLen = 60

// TestFindingsI18nCoverage asserts that every built-in rule has an i18n-resolved
// finding title and fix hint under the findings.{rule}.{title|fix} key path, and
// that each title fits the chip-label budget. This is the lint gate from #1342:
// it fails the build if a rule is added without its finding strings, or if a
// title grows past the budget and would truncate on a chip.
func TestFindingsI18nCoverage(t *testing.T) {
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("loading embedded i18n bundle: %v", err)
	}
	en := bundle.Translator("en")
	if en == nil {
		t.Fatal("bundle has no English translator")
	}

	for _, r := range defaultRules {
		titleKey := "findings." + r.ID + ".title"
		fixKey := "findings." + r.ID + ".fix"

		// Translator.T returns the key verbatim on a miss, so key == result
		// means the translation is absent.
		if title := en.T(titleKey); title == titleKey {
			t.Errorf("rule %q: missing translation key %q", r.ID, titleKey)
		} else if n := len([]rune(title)); n > maxFindingTitleLen {
			t.Errorf("rule %q: title %q is %d chars; must be <= %d (chip-label budget)",
				r.ID, title, n, maxFindingTitleLen)
		}

		if fix := en.T(fixKey); fix == fixKey {
			t.Errorf("rule %q: missing translation key %q", r.ID, fixKey)
		}
	}
}
