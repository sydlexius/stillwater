package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// renderField renders a field in read (edit=false) or edit (edit=true) view with
// the given findings context.
func renderField(t *testing.T, ctx context.Context, a *artist.Artist, field string, edit bool) string {
	t.Helper()
	var buf bytes.Buffer
	var err error
	if edit {
		err = FieldEdit(a, field, nil, nil, false).Render(ctx, &buf)
	} else {
		err = FieldDisplay(a, field, nil).Render(ctx, &buf)
	}
	if err != nil {
		t.Fatalf("render %s (edit=%v): %v", field, edit, err)
	}
	return buf.String()
}

// TestFieldFindingChips_ReadAndEditViews pins that inline finding chips render in
// BOTH the read view (FieldDisplay) and the edit view (FieldEdit) for the field a
// live violation touches (#1860), and that a field with no finding renders no
// chip in either view.
func TestFieldFindingChips_ReadAndEditViews(t *testing.T) {
	const probe = "ORIGIN_FINDING_PROBE"
	ctx := WithFieldFindings(testCtx(t), map[string][]FieldFinding{
		"origin": {{ID: "v1", ArtistID: "ar-chip-1", RuleID: "origin_missing", Severity: "warning", Message: probe, Fixable: true}},
	})
	a := &artist.Artist{ID: "ar-chip-1", Name: "Chip Artist", Type: "person", Origin: "Berlin"}

	for _, edit := range []bool{false, true} {
		got := renderField(t, ctx, a, "origin", edit)
		if !strings.Contains(got, "sw-field-chip") {
			t.Errorf("origin (edit=%v): expected an inline finding chip; none present:\n%s", edit, got)
		}
		if !strings.Contains(got, probe) {
			t.Errorf("origin (edit=%v): chip message %q absent:\n%s", edit, probe, got)
		}
	}

	// A field with NO injected finding renders no chip in either view.
	for _, edit := range []bool{false, true} {
		got := renderField(t, ctx, a, "name", edit)
		if strings.Contains(got, "sw-field-chip") {
			t.Errorf("name field (edit=%v) leaked a finding chip with no finding injected:\n%s", edit, got)
		}
	}
}

// TestFieldFindingChip_Popover pins the chip's click-popover (#1860): it carries
// the message, a unique per-field menu id, a Dismiss action with the confirm
// hook, and -- when the finding is fixable -- a Fix action. A non-fixable finding
// renders Dismiss only (no Fix). Both actions carry the violation+artist ids the
// reused artistViolationFix/Dismiss scripts key off.
func TestFieldFindingChip_Popover(t *testing.T) {
	render := func(t *testing.T, f FieldFinding) string {
		t.Helper()
		ctx := WithFieldFindings(testCtx(t), map[string][]FieldFinding{"origin": {f}})
		a := &artist.Artist{ID: f.ArtistID, Name: "Chip Artist", Type: "person", Origin: "Berlin"}
		return renderField(t, ctx, a, "origin", false)
	}

	fixable := FieldFinding{ID: "viol-99", ArtistID: "ar-9", RuleID: "origin_missing", Severity: "warning", Message: "needs origin", Fixable: true}
	got := render(t, fixable)

	// Unique popover menu id scoped to field + violation (so a multi-field finding
	// does not collide).
	if !strings.Contains(got, `data-context-menu="ff-origin-viol-99"`) {
		t.Errorf("popover missing unique menu id ff-origin-viol-99:\n%s", got)
	}
	if !strings.Contains(got, `id="ctx-panel-ff-origin-viol-99"`) {
		t.Errorf("popover panel id missing:\n%s", got)
	}
	if !strings.Contains(got, `role="menu"`) {
		t.Errorf("popover not marked role=menu:\n%s", got)
	}
	if !strings.Contains(got, "needs origin") {
		t.Errorf("popover body missing the message:\n%s", got)
	}
	// Both actions carry the violation + artist ids the reused scripts look up.
	if c := strings.Count(got, `data-violation-id="viol-99"`); c != 2 {
		t.Errorf("fixable finding: expected 2 action buttons with data-violation-id (Fix + Dismiss), got %d:\n%s", c, got)
	}
	if !strings.Contains(got, `data-confirm=`) {
		t.Errorf("Dismiss action missing its confirm hook:\n%s", got)
	}
	// The trigger is a button that opens the menu, not a link to the old section.
	if strings.Contains(got, `href="#next-findings"`) {
		t.Errorf("chip should no longer link to the removed section anchor:\n%s", got)
	}

	// Non-fixable: Dismiss only (one action button), no Fix.
	nonFixable := FieldFinding{ID: "viol-7", ArtistID: "ar-7", RuleID: "origin_missing", Severity: "info", Message: "fyi", Fixable: false}
	got2 := render(t, nonFixable)
	if c := strings.Count(got2, `data-violation-id="viol-7"`); c != 1 {
		t.Errorf("non-fixable finding: expected exactly 1 action button (Dismiss only), got %d:\n%s", c, got2)
	}
}
