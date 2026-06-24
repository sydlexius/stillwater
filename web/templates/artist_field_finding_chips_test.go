package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestFieldFindingChips_ReadAndEditViews pins that inline finding chips render in
// BOTH the read view (FieldDisplay) and the edit view (FieldEdit) for the field
// a live violation touches (#1860). Before #1860 the chips rendered only in the
// read view; removing the dedicated Open Findings section makes the inline chip
// the only surface that keeps a violation visible while editing the field, so the
// edit-view rendering must be locked.
func TestFieldFindingChips_ReadAndEditViews(t *testing.T) {
	const probe = "ORIGIN_FINDING_PROBE"
	// origin_missing maps to the "origin" field (rule.RuleFields); inject a chip
	// for it via the same context key the next/ handler uses.
	ctx := WithFieldFindings(testCtx(t), map[string][]FieldFinding{
		"origin": {{Severity: "warning", Message: probe}},
	})
	a := &artist.Artist{ID: "ar-chip-1", Name: "Chip Artist", Type: "person", Origin: "Berlin"}

	render := func(t *testing.T, edit bool) string {
		t.Helper()
		var buf bytes.Buffer
		var err error
		if edit {
			err = FieldEdit(a, "origin", nil, nil, false).Render(ctx, &buf)
		} else {
			err = FieldDisplay(a, "origin", nil).Render(ctx, &buf)
		}
		if err != nil {
			t.Fatalf("render (edit=%v): %v", edit, err)
		}
		return buf.String()
	}

	for _, tc := range []struct {
		name string
		edit bool
	}{
		{"read view", false},
		{"edit view", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := render(t, tc.edit)
			if !strings.Contains(got, "sw-field-chip") {
				t.Errorf("%s: expected an inline finding chip; none present:\n%s", tc.name, got)
			}
			if !strings.Contains(got, probe) {
				t.Errorf("%s: chip tooltip message %q absent:\n%s", tc.name, probe, got)
			}
		})
	}

	// A field with NO injected finding renders no chip in either view (guards
	// against a chip leaking onto every row).
	for _, edit := range []bool{false, true} {
		var buf bytes.Buffer
		var err error
		if edit {
			err = FieldEdit(a, "name", nil, nil, false).Render(ctx, &buf)
		} else {
			err = FieldDisplay(a, "name", nil).Render(ctx, &buf)
		}
		if err != nil {
			t.Fatalf("render name (edit=%v): %v", edit, err)
		}
		if strings.Contains(buf.String(), "sw-field-chip") {
			t.Errorf("name field (edit=%v) leaked a finding chip with no finding injected", edit)
		}
	}
}
