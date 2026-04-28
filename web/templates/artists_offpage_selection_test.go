package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/web/components"
)

// TestBulkActionBar_OffPageIndicatorWiring pins the markup contract that the
// client-side bulk-selection controller relies on to render the off-page
// indicator described in #1227. The toolbar must expose the split-form i18n
// templates as data attributes so the JS controller can compose
// "X selected (Y on this page, Z elsewhere)" without a server roundtrip,
// and it must include the "Show selected" affordance with the documented
// id, hidden until the client populates it. We do NOT exercise the JS
// computation itself (the toolbar count is set imperatively by updateBar);
// this test guards the templ contract those handlers depend on.
func TestBulkActionBar_OffPageIndicatorWiring(t *testing.T) {
	data := ArtistListData{}

	var buf bytes.Buffer
	if err := BulkActionBar(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// The split-form i18n keys must surface as data attributes so the
	// JS controller can build the off-page copy locally on every
	// updateBar() invocation. Without these the controller would fall
	// back to the literal placeholder template, which still works in
	// English but bypasses translation for any other locale.
	for _, attr := range []string{
		"data-i18n-selected-split-one",
		"data-i18n-selected-split-other",
	} {
		if !strings.Contains(body, attr) {
			t.Errorf("BulkActionBar missing %s attribute on toolbar; body:\n%s", attr, body)
		}
	}

	// The "Show selected" affordance must exist with the stable id the
	// controller wires the click handler to. It also starts hidden so a
	// fresh page (no selection) does not flash the button on first paint.
	// Fatal here, not Errorf: if the button is absent, the strings.Index
	// below returns -1 and body[:idx] would panic, masking the real
	// assertion failure with a stack trace.
	if !strings.Contains(body, `id="bulk-show-selected"`) {
		t.Fatalf("BulkActionBar missing #bulk-show-selected button; body:\n%s", body)
	}
	idx := strings.Index(body, `id="bulk-show-selected"`)
	if idx < 0 {
		t.Fatalf("could not locate #bulk-show-selected")
	}
	// Walk back to the opening <button to confirm it carries the hidden class.
	btnStart := strings.LastIndex(body[:idx], "<button")
	if btnStart < 0 {
		t.Fatalf("could not locate <button opener for #bulk-show-selected")
	}
	btnEnd := strings.Index(body[btnStart:], ">")
	if btnEnd < 0 {
		t.Fatalf("malformed <button tag for #bulk-show-selected")
	}
	btnTag := body[btnStart : btnStart+btnEnd+1]
	// The class list must include the `hidden` Tailwind utility. We
	// look for the token-bounded form so a hypothetical "hidden-" or
	// "_hidden" suffix would not falsely satisfy the assertion.
	if !strings.Contains(btnTag, `class="hidden `) && !strings.Contains(btnTag, ` hidden `) && !strings.Contains(btnTag, ` hidden"`) {
		t.Errorf("#bulk-show-selected must start hidden so it does not flash; got:\n%s", btnTag)
	}
	// Per memory feedback_css_cascade_display_utilities, the button must
	// not co-apply hidden + a display class that would beat it in the
	// cascade. The visible-state display class (`inline-flex`) is added
	// by JS only when selection.size > 0.
	if strings.Contains(btnTag, "inline-flex") {
		t.Errorf("#bulk-show-selected must not co-apply hidden+inline-flex (display cascade); got:\n%s", btnTag)
	}
	if !strings.Contains(btnTag, "aria-label=") {
		t.Errorf("#bulk-show-selected must carry an aria-label for screen readers; got:\n%s", btnTag)
	}
}

// TestBulkActionBar_SelectionFilterChip_Hidden verifies that the
// "Showing N selected" chip is NOT rendered when no `ids=` filter is in
// effect. Without this guard the chip would always be visible and the
// "Show all" link would have nothing to clear.
func TestBulkActionBar_SelectionFilterChip_Hidden(t *testing.T) {
	data := ArtistListData{IDs: nil}
	var buf bytes.Buffer
	if err := BulkActionBar(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if strings.Contains(body, "showing_selected") {
		t.Errorf("BulkActionBar rendered the selection-filter chip without IDs; body:\n%s", body)
	}
	// The "Show all" affordance must only appear on the chip; no chip => no "Show all".
	if strings.Contains(body, "Show all") {
		t.Errorf("BulkActionBar rendered Show-all link without IDs filter; body:\n%s", body)
	}
}

// TestBulkActionBar_SelectionFilterChip_RendersWhenActive verifies that the
// "Showing N selected" chip renders with the correct count and a "Show all"
// dismiss link when the IDs filter is active. The chip is the on-screen
// confirmation that #1227's "Show selected" affordance worked, and the
// "Show all" link is the documented way to drop the filter.
func TestBulkActionBar_SelectionFilterChip_RendersWhenActive(t *testing.T) {
	cases := []struct {
		name     string
		ids      []string
		wantCopy string
	}{
		{"singular", []string{"a-1"}, "Showing 1 selected artist"},
		{"plural", []string{"a-1", "a-2", "a-3"}, "Showing 3 selected artists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := ArtistListData{IDs: tc.ids, View: "table"}
			var buf bytes.Buffer
			if err := BulkActionBar(data).Render(testCtx(t), &buf); err != nil {
				t.Fatalf("render: %v", err)
			}
			body := buf.String()
			if !strings.Contains(body, tc.wantCopy) {
				t.Errorf("missing copy %q in rendered chip; body:\n%s", tc.wantCopy, body)
			}
			// The chip must include a Show-all link so the user can
			// drop the filter without typing the URL by hand.
			if !strings.Contains(body, "aria-label=\"Clear the selection filter and show all artists\"") {
				t.Errorf("chip missing Show-all dismiss link; body:\n%s", body)
			}
			// The dismiss link must carry the data-clear-ids="true"
			// opt-out so the htmx:configRequest hook does not
			// re-inject ids= from window.location.search and
			// silently re-engage the filter the user just cleared.
			// Without this attribute the hx-get URL still drops ids
			// (so this test would otherwise pass) but the request
			// htmx actually issues would carry ids back, defeating
			// the chip dismiss at runtime.
			if !strings.Contains(body, `data-clear-ids="true"`) {
				t.Errorf("chip dismiss link missing data-clear-ids opt-out; body:\n%s", body)
			}
			// The hx-get URL must drop the ids param so the next
			// request returns the unfiltered list. We do not pin the
			// full URL (it varies by view) but assert ids= is absent.
			hxIdx := strings.Index(body, `hx-get="`)
			if hxIdx < 0 {
				t.Fatalf("chip Show-all link missing hx-get; body:\n%s", body)
			}
			hxEnd := strings.Index(body[hxIdx+len(`hx-get="`):], `"`)
			if hxEnd < 0 {
				t.Fatalf("malformed hx-get on Show-all link; body:\n%s", body)
			}
			hxURL := body[hxIdx+len(`hx-get="`) : hxIdx+len(`hx-get="`)+hxEnd]
			if strings.Contains(hxURL, "ids=") {
				t.Errorf("Show-all hx-get must drop ids param; got %q", hxURL)
			}
		})
	}
}

// TestPaginationData_PreservesIDsThroughPaging verifies the pagination URL
// builder propagates the IDs filter so paging from page 1 to page 2 of a
// >50-item selection keeps the filter active. Without this the user would
// drop back to the unfiltered list on Next, which is the same regression
// shape #1227 calls out (selection becomes invisible during navigation).
func TestPaginationData_PreservesIDsThroughPaging(t *testing.T) {
	data := components.PaginationData{
		CurrentPage: 1,
		TotalPages:  3,
		PageSize:    50,
		TotalItems:  120,
		BaseURL:     "/artists",
		View:        "table",
		IDs:         []string{"a-1", "a-2", "a-3"},
	}
	var buf bytes.Buffer
	if err := components.Pagination(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	// The "Next" link must carry ids= so page 2 of the selected set is
	// itself a selected-set view, not a filter dropdown.
	if !strings.Contains(body, "ids=a-1%2Ca-2%2Ca-3") && !strings.Contains(body, "ids=a-1,a-2,a-3") {
		t.Errorf("Next pagination link missing ids= filter; body:\n%s", body)
	}
}
