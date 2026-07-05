package templates

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/web/components"
)

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{int64(1024) * 1024 * 1024, "1.0 GiB"},
		{int64(1024) * 1024 * 1024 * 1024, "1.0 TiB"},
		// Saturates at TiB: anything bigger renders with the TiB suffix
		// even though the displayed magnitude shrinks because div has been
		// scaled higher than the suffix slot suggests. The numeric
		// rendering is therefore not exact for petabyte-scale inputs;
		// foreign-file artwork never reaches that scale in practice.
	}
	for _, c := range cases {
		got := humanBytes(c.in)
		if got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ffHxEndpointRe extracts the value of any hx-post/hx-get/hx-delete attribute so
// the dismiss-conditional test can assert on the rendered action contracts.
var ffHxEndpointRe = regexp.MustCompile(`hx-(?:post|get|delete)="([^"]*)"`)

// foreignEndpoints returns the set of hx-post/hx-get/hx-delete endpoint values
// that reference the foreign-files feature (filtered by the "foreign-file"
// substring so layout/sidebar chrome endpoints are excluded).
func foreignEndpoints(html string) map[string]bool {
	set := map[string]bool{}
	for _, m := range ffHxEndpointRe.FindAllStringSubmatch(html, -1) {
		if strings.Contains(m[1], "foreign-file") {
			set[m[1]] = true
		}
	}
	return set
}

// multiPageAllowlistView builds an allowlist view whose Pagination reports more
// than one page, matching what handleForeignAllowlistPage produces for a large
// allowlist.
func multiPageAllowlistView() ForeignAllowlistPageView {
	return ForeignAllowlistPageView{
		Rows: []ForeignAllowlistRow{
			{ID: "a", Scope: "global", FileName: "x.jpg", CreatedAt: "now"},
			{ID: "b", Scope: "global", FileName: "y.jpg", CreatedAt: "now"},
		},
		Pagination: components.PaginationData{
			CurrentPage: 1,
			TotalPages:  3,
			PageSize:    10,
			TotalItems:  25,
			BaseURL:     "/reports/foreign-files/allowlist",
			TargetID:    "foreign-allowlist-body",
		},
	}
}

// TestForeignAllowlistBody_SingleKeyboardWiredPager guards against the
// double-pager regression: ForeignAllowlistBody wraps the shared
// ForeignAllowlistTable (which embeds the components.Pagination "X-of-Y"
// counter) and ALSO appends the keyboard-wired NextPagination. Because the
// handler populates view.Pagination, the shared table's counter pager would
// render UNLESS it is suppressed (stableTablePagerSuppressed). The design
// mandates the keyboard-wired NextPagination only ("NO X-of-Y counter"), so
// exactly one pager must appear and the "Showing page X of Y" counter must not.
func TestForeignAllowlistBody_SingleKeyboardWiredPager(t *testing.T) {
	var buf bytes.Buffer
	if err := ForeignAllowlistBody(multiPageAllowlistView()).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if n := strings.Count(out, "Showing page"); n != 0 {
		t.Errorf("stable counter pager leaked onto the allowlist body: %d occurrence(s); want 0", n)
	}
	// Exactly one keyboard-wired Prev/Next pair (the NextPagination control).
	if n := strings.Count(out, `id="sw-page-prev"`); n != 1 {
		t.Errorf("sw-page-prev count = %d; want exactly 1 (the keyboard-wired pager)", n)
	}
	if n := strings.Count(out, `id="sw-page-next"`); n != 1 {
		t.Errorf("sw-page-next count = %d; want exactly 1", n)
	}
	// The roving boundary controls must reference the rendered pager ids so the
	// keyboard helper (keyboard.js boundaryControl) can resolve h/l page nav.
	if !strings.Contains(out, `data-sw-roving-boundary-prev="#sw-page-prev"`) {
		t.Error("missing data-sw-roving-boundary-prev pointing at #sw-page-prev")
	}
	if !strings.Contains(out, `data-sw-roving-boundary-next="#sw-page-next"`) {
		t.Error("missing data-sw-roving-boundary-next pointing at #sw-page-next")
	}
}

// TestForeignAllowlistBody_SinglePageNoPager verifies that a single-page
// allowlist renders no pager at all (neither the counter nor the keyboard-wired
// control), and no h/l boundary attributes (nothing to page to).
func TestForeignAllowlistBody_SinglePageNoPager(t *testing.T) {
	view := multiPageAllowlistView()
	view.Pagination.TotalPages = 1

	var buf bytes.Buffer
	if err := ForeignAllowlistBody(view).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "Showing page") {
		t.Error("counter pager rendered for a single-page allowlist")
	}
	if strings.Contains(out, `id="sw-page-prev"`) || strings.Contains(out, `id="sw-page-next"`) {
		t.Error("keyboard-wired pager rendered for a single-page allowlist")
	}
	if strings.Contains(out, "data-sw-roving-boundary-prev") || strings.Contains(out, "data-sw-roving-boundary-next") {
		t.Error("h/l boundary attributes present for a single-page allowlist")
	}
	// j/k roving + Enter activation still apply even on a single page.
	if !strings.Contains(out, "data-sw-roving-list") {
		t.Error("missing data-sw-roving-list on single-page allowlist body")
	}
}

// TestForeignFilesPage_DismissCountConditional pins the `if view.Count > 0`
// guard on the bulk Dismiss control (#1773 B4): the button -- and therefore the
// /api/v1/foreign-files/dismiss endpoint -- must render ONLY when there are
// detected files. If the guard regressed to always-render (dismiss shown on an
// empty page) or never-render (the B4 port miss), this catches it.
func TestForeignFilesPage_DismissCountConditional(t *testing.T) {
	const dismiss = "/api/v1/foreign-files/dismiss"
	ctx := testCtx(t)
	assets := AssetPaths{IsAdmin: true}

	render := func(t *testing.T, v ForeignFilesPageView) map[string]bool {
		t.Helper()
		var buf bytes.Buffer
		if err := ForeignFilesPage(assets, v).Render(ctx, &buf); err != nil {
			t.Fatalf("render: %v", err)
		}
		return foreignEndpoints(buf.String())
	}

	if render(t, ForeignFilesPageView{Count: 0})[dismiss] {
		t.Errorf("Dismiss endpoint %q rendered with Count=0; the `if view.Count > 0` guard is broken (always-render)", dismiss)
	}
	nonEmpty := ForeignFilesPageView{
		Rows:  []ForeignFileRow{{ID: "f1", ArtistID: "a1", ArtistName: "Alpha", FileName: "x.jpg", DetectedAt: "now"}},
		Count: 1,
	}
	if !render(t, nonEmpty)[dismiss] {
		t.Errorf("Dismiss endpoint %q absent with Count>0; the conditional Dismiss control is missing (never-render)", dismiss)
	}
}

// TestForeignFilesPage_CanonicalNavTargets verifies the promoted page's
// header links point at the CANONICAL /reports/foreign-files paths, not the
// retired /next/ or /settings/ lanes (M55 #1757 PR-6a). The detected-files page
// links to the allowlist; the allowlist page links back to detected files.
func TestForeignFilesPage_CanonicalNavTargets(t *testing.T) {
	ctx := testCtx(t)
	assets := AssetPaths{IsAdmin: true}

	var detected bytes.Buffer
	if err := ForeignFilesPage(assets, ForeignFilesPageView{}).Render(ctx, &detected); err != nil {
		t.Fatalf("render detected: %v", err)
	}
	if !strings.Contains(detected.String(), `href="/reports/foreign-files/allowlist"`) {
		t.Error("detected-files page must link to the canonical /reports/foreign-files/allowlist")
	}
	if strings.Contains(detected.String(), "/next/reports/foreign-files") || strings.Contains(detected.String(), "/settings/foreign-files") {
		t.Error("detected-files page must not reference the retired /next/ or /settings/ foreign-files paths")
	}

	var allow bytes.Buffer
	if err := ForeignAllowlistPage(assets, multiPageAllowlistView()).Render(ctx, &allow); err != nil {
		t.Fatalf("render allowlist: %v", err)
	}
	if !strings.Contains(allow.String(), `href="/reports/foreign-files"`) {
		t.Error("allowlist page back-link must point at the canonical /reports/foreign-files")
	}
	if strings.Contains(allow.String(), "/next/reports/foreign-files") || strings.Contains(allow.String(), "/settings/foreign-files") {
		t.Error("allowlist page must not reference the retired /next/ or /settings/ foreign-files paths")
	}
}
