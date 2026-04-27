package templates

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestActivityRow_RuleFixSourceHidesRevert pins the contract that rule
// auto-fix entries (field "rule_fix", source "rule:<rule_id>") render in the
// activity feed without a Revert button. The feed shows Revert only for
// trackable fields, and rule auto-fixes mutate disk state (NFO files, image
// files, directory renames) that cannot be safely undone via the field-revert
// path. A future addition of "rule_fix" to trackableFields would silently
// introduce a broken Revert affordance, and this test guards against that
// regression. Issue #1106.
func TestActivityRow_RuleFixSourceHidesRevert(t *testing.T) {
	c := artist.MetadataChangeWithArtist{
		MetadataChange: artist.MetadataChange{
			ID:        "rule-fix-1",
			ArtistID:  "a-1",
			Field:     "rule_fix",
			OldValue:  "",
			NewValue:  "created artist.nfo for Test Artist",
			Source:    "rule:nfo_exists",
			CreatedAt: time.Now().UTC(),
		},
		ArtistName: "Test Artist",
	}

	var buf bytes.Buffer
	if err := ActivityChangeRowFragment(c, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// The fragment must mark the row with the activity-change- id prefix so
	// the feed's load-more / counter logic finds it.
	if !strings.Contains(body, `id="activity-change-rule-fix-1"`) {
		t.Errorf("rendered row missing activity-change-<id> wrapper; body:\n%s", body)
	}

	// The source label must surface the rule ID so users can tell which
	// rule produced the entry. The historySourceLabel helper formats
	// "rule:<id>" as "Rule: <id>".
	if !strings.Contains(body, "Rule: nfo_exists") {
		t.Errorf("rendered row missing rule source label; body:\n%s", body)
	}

	// Critical: the Revert button must NOT render for rule_fix entries. The
	// button is the htmx POST against /api/v1/history/<id>/revert; if the
	// trackableFields gate ever leaks rule_fix, this assertion catches it.
	if strings.Contains(body, "/api/v1/history/rule-fix-1/revert") {
		t.Errorf("rule_fix activity row rendered a Revert button; rule auto-fixes mutate disk state and cannot be reverted via the field-revert path. Body:\n%s", body)
	}

	// The fixer's human-readable message must surface in the row body so
	// the user sees what changed without expanding the details panel.
	if !strings.Contains(body, "created artist.nfo for Test Artist") {
		t.Errorf("rendered row missing fixer message in NewValue display; body:\n%s", body)
	}
}

// TestActivityPage_RenderRuleFixFilterChip verifies that the activity page
// surfaces a "Rule auto-fix" filter chip in the change-type filter section
// so users can scope the feed to engine-driven repairs. Issue #1106.
func TestActivityPage_RenderRuleFixFilterChip(t *testing.T) {
	data := ActivityPageData{
		Limit:    50,
		BasePath: "",
	}
	var buf bytes.Buffer
	if err := ActivityPage(AssetPaths{}, data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// FilterItem renders an option with value="rule_fix" inside the
	// activity-filters-flyout filter group so users can include/exclude
	// rule auto-fix entries from the feed view.
	if !strings.Contains(body, `value="rule_fix"`) {
		t.Errorf("activity page missing rule_fix filter chip; body excerpt:\n%s", excerpt(body, "rule_fix", 200))
	}
	if !strings.Contains(body, "Rule auto-fix") {
		t.Errorf("activity page missing Rule auto-fix filter label; the i18n key field.rule_fix is expected. Body excerpt:\n%s", excerpt(body, "rule_fix", 200))
	}
}

// excerpt returns up to ctx characters around the first occurrence of needle
// in body, or the full body if needle is missing. Used to keep failure logs
// readable when a large rendered template is involved.
func excerpt(body, needle string, ctx int) string {
	idx := strings.Index(body, needle)
	if idx < 0 {
		return body
	}
	start := idx - ctx
	if start < 0 {
		start = 0
	}
	end := idx + len(needle) + ctx
	if end > len(body) {
		end = len(body)
	}
	return body[start:end]
}
