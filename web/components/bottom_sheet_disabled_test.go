package components

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestBottomSheet_DisabledHrefIsNotFocusable pins the markup half of the
// disabled contract for a link row (#2382 review).
//
// BottomSheetItemData.Disabled is documented as preventing interaction, but an
// <a> takes no `disabled` attribute -- so a disabled link needs aria-disabled
// AND tabindex="-1" to be unreachable by pointer and by keyboard. Without the
// tabindex the sheet's focus queries, which select `a[href]`, would still hand
// it initial focus and include it in the Tab cycle.
//
// The button row is asserted alongside it because the two branches carry the
// disabled state differently, and a fix applied to only one is the likely
// regression.
func TestBottomSheet_DisabledHrefIsNotFocusable(t *testing.T) {
	var buf bytes.Buffer
	items := []BottomSheetItemData{
		{Label: "Live Link", Href: "/live"},
		{Label: "Dead Link", Href: "/dead", Disabled: true},
		{Label: "Dead Button", Disabled: true},
	}
	if err := BottomSheet("probe", "Probe actions", items).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// Precondition: all three rows really rendered. Without this the
	// assertions below could pass on an empty sheet.
	for _, label := range []string{"Live Link", "Dead Link", "Dead Button"} {
		if !strings.Contains(body, label) {
			t.Fatalf("row %q absent; the test asserts nothing:\n%s", label, body)
		}
	}

	deadLink := anchorFor(t, body, "/dead")
	if !strings.Contains(deadLink, `tabindex="-1"`) {
		t.Errorf("a disabled link is missing tabindex=\"-1\", so the sheet's "+
			"`a[href]` focus queries will still focus it; got: %s", deadLink)
	}
	if !strings.Contains(deadLink, `aria-disabled="true"`) {
		t.Errorf("a disabled link is missing aria-disabled; got: %s", deadLink)
	}
	if !strings.Contains(deadLink, "context-menu-item-disabled") {
		t.Errorf("a disabled link is missing the disabled class (pointer-events: none); got: %s", deadLink)
	}

	// The ENABLED link must keep its normal focusability -- a fix that
	// disabled everything would satisfy the assertions above.
	liveLink := anchorFor(t, body, "/live")
	if strings.Contains(liveLink, `tabindex="-1"`) || strings.Contains(liveLink, `aria-disabled="true"`) {
		t.Errorf("an enabled link was marked unfocusable; got: %s", liveLink)
	}
}

// anchorFor returns the single <a> tag whose href matches, so an assertion
// cannot accidentally be satisfied by a different row's attributes.
func anchorFor(t *testing.T, body, href string) string {
	t.Helper()
	needle := `href="` + href + `"`
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("no anchor with %s in:\n%s", needle, body)
	}
	start := strings.LastIndex(body[:i], "<a")
	end := strings.Index(body[i:], ">")
	if start < 0 || end < 0 {
		t.Fatalf("could not delimit the <a> tag for %s", needle)
	}
	return body[start : i+end+1]
}
