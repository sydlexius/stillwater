package components

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestFilterItemSingle_Selected verifies the selected state renders the
// include checkmark icon, marks data-filter-selected="true", and writes the
// count badge when the count is positive.
func TestFilterItemSingle_Selected(t *testing.T) {
	var buf bytes.Buffer
	if err := FilterItemSingle("flyout-id", "severity", "error", "Error", true, 12).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	wantSubs := []string{
		`data-filter-mode="single"`,
		`data-filter-key="severity"`,
		`data-filter-value="error"`,
		`data-filter-selected="true"`,
		`aria-pressed="true"`,
		`sw-filter-item-icon`,
		`Error`,
		`12`,
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in rendered output:\n%s", s, out)
		}
	}
}

// TestFilterItemSingle_Unselected verifies the unselected state omits the
// icon span and the count badge when count is zero.
func TestFilterItemSingle_Unselected(t *testing.T) {
	var buf bytes.Buffer
	if err := FilterItemSingle("flyout-id", "category", "image", "Image", false, 0).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, `data-filter-selected="false"`) {
		t.Errorf("expected data-filter-selected=\"false\"; got:\n%s", out)
	}
	if !strings.Contains(out, `aria-pressed="false"`) {
		t.Errorf("expected aria-pressed=\"false\"; got:\n%s", out)
	}
	if strings.Contains(out, "sw-filter-item-icon") {
		t.Errorf("unselected chip must not render an icon span; got:\n%s", out)
	}
	// Zero count is omitted -- otherwise every chip on a fresh page would
	// carry a meaningless "0" tail.
	if strings.Contains(out, ">0<") {
		t.Errorf("zero-count chip must not render a count badge; got:\n%s", out)
	}
}

// TestFilterRange verifies the paired min/max number inputs carry the
// expected data-filter-key suffixes and reflect non-zero values.
func TestFilterRange(t *testing.T) {
	var buf bytes.Buffer
	if err := FilterRange("flyout-id", "health", "Health", 40, 80).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	wantSubs := []string{
		`data-filter-range-key="health"`,
		`data-filter-range-bound="min"`,
		`data-filter-key="health_min"`,
		`value="40"`,
		`data-filter-range-bound="max"`,
		`data-filter-key="health_max"`,
		`value="80"`,
		`Health`,
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in rendered output:\n%s", s, out)
		}
	}
}

// TestFilterRange_ZeroOmitsValue confirms a zero bound renders as an empty
// input rather than "value=\"0\"", so blank inputs do not pre-populate.
func TestFilterRange_ZeroOmitsValue(t *testing.T) {
	var buf bytes.Buffer
	if err := FilterRange("flyout-id", "health", "Health", 0, 0).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `value="0"`) {
		t.Errorf("zero bound must render empty value, not \"0\"; got:\n%s", out)
	}
}

// TestFilterChip verifies a chip renders the label, includes an X-mark icon,
// and wires the dismiss button to the shared DismissFilterChip script with
// the supplied key + targetSel.
func TestFilterChip(t *testing.T) {
	var buf bytes.Buffer
	if err := FilterChip("Error", "severity", "#action-queue").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	wantSubs := []string{
		"Error",
		`aria-label="Remove filter: Error"`,
		"DismissFilterChip", // generated script function name
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in rendered output:\n%s", s, out)
		}
	}
}

// TestActiveFilters_Empty verifies that an empty chip slice yields no chip
// row at all (the wrapping <div> only appears when there is at least one chip).
func TestActiveFilters_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := ActiveFilters("#target", "/clear", "Clear All", "Active:", nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	if out != "" {
		t.Errorf("expected no markup for empty chip slice; got: %q", out)
	}
}

// TestActiveFilters_RendersChipsAndClearAll renders multiple chips plus the
// Clear All anchor and verifies the row label, every chip label, and the
// clearAll href all appear.
func TestActiveFilters_RendersChipsAndClearAll(t *testing.T) {
	chips := []FilterChipSpec{
		{Label: "Error", Key: "severity"},
		{Label: "Library 1", Key: "library_id"},
	}
	var buf bytes.Buffer
	if err := ActiveFilters("#action-queue", "/clear-all", "Clear all", "Active:", chips).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	wantSubs := []string{
		"Active:",
		"Error",
		"Library 1",
		`href="/clear-all"`,
		"Clear all",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in rendered output:\n%s", s, out)
		}
	}
}

// TestActiveFilters_PerChipTargetSelOverride confirms that a FilterChipSpec
// with a non-empty TargetSel overrides the row-wide default, while chips
// with an empty TargetSel inherit the default. The selector is embedded in
// the generated DismissFilterChip onclick payload so we test by escape-aware
// substring presence rather than exact-match.
func TestActiveFilters_PerChipTargetSelOverride(t *testing.T) {
	chips := []FilterChipSpec{
		{Label: "Inherits", Key: "severity"},
		{Label: "Overrides", Key: "category", TargetSel: "#custom-target"},
	}
	var buf bytes.Buffer
	if err := ActiveFilters("#default-target", "", "", "Active:", chips).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// templ encodes the targetSel string inside the generated onclick
	// payload; both selectors must be present (either raw or HTML-escaped).
	for _, s := range []string{"default-target", "custom-target"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in rendered output:\n%s", s, out)
		}
	}
}
