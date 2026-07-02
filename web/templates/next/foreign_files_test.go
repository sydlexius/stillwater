package next

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

// hxEndpointRe extracts the value of any hx-post/hx-get/hx-delete attribute.
// hrefRe extracts href values. Both feed the stable-vs-next action-parity test.
var (
	hxEndpointRe = regexp.MustCompile(`hx-(?:post|get|delete)="([^"]*)"`)
	hrefRe       = regexp.MustCompile(`href="([^"]*)"`)
	// mainBodyRe extracts the <main id="sw-main"> ... </main> region -- the page
	// BODY, excluding the shared promoted sidebar/chrome. The promoted Layout
	// (M55 #1757) renders exactly one such landmark; (?s) lets it span newlines
	// and .*? stops at the first </main>.
	mainBodyRe = regexp.MustCompile(`(?s)<main[^>]*id="sw-main".*?</main>`)
)

// foreignEndpoints returns the set of hx-post/hx-get/hx-delete endpoint values
// that reference the foreign-files feature (filtered by the "foreign-file"
// substring so layout/sidebar chrome endpoints, which legitimately differ
// between Layout and LayoutNext, are excluded). The /api/v1 paths are
// channel-invariant, so this set is what must match across stable and next/.
func foreignEndpoints(html string) map[string]bool {
	set := map[string]bool{}
	for _, m := range hxEndpointRe.FindAllStringSubmatch(html, -1) {
		if strings.Contains(m[1], "foreign-file") {
			set[m[1]] = true
		}
	}
	return set
}

// foreignNavHrefCount counts the DISTINCT foreign-files navigation TARGETS in
// the page BODY (the <main id="sw-main"> region, excluding the shared sidebar).
// It unions href and hx-get/hx-post/hx-delete values containing "foreign-files"
// into a set, so an element carrying both attributes with the same value (a
// progressive-enhancement <a href hx-get> link) counts as ONE target.
//
// Body-scoping + distinct-value is required after the M55 #1757 shell
// promotion. Stable and next now render the SAME promoted sidebar, so a
// whole-page tally (a) double-counts the shared chrome and (b) is confounded by
// next's back-link URL (/next/reports/foreign-files) coinciding with the
// sidebar's foreign link. Scoping to <main> removes both. And counting raw
// attribute OCCURRENCES wrongly scored the SAME "next page" nav across channels:
// stable's @components.Pagination renders it as <a href+hx-get> (two matching
// attributes) while next's @NextPagination renders it as a <button hx-get>
// (one) -- the deliberate minimal Prev/Next-no-counter pager (#1790). Comparing
// DISTINCT body targets measures the real intent: next/ must offer at least as
// many foreign-files nav targets as stable. The channel path prefix still
// differs (/settings/... vs /next/reports/...), so the check compares COUNTS,
// not literal values.
func foreignNavHrefCount(html string) int {
	body := mainBodyRe.FindString(html)
	if body == "" {
		panic("foreignNavHrefCount: <main id=\"sw-main\"> landmark not found in rendered HTML")
	}
	targets := map[string]bool{}
	for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[1], "foreign-files") {
			targets[m[1]] = true
		}
	}
	for _, m := range hxEndpointRe.FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[1], "foreign-files") {
			targets[m[1]] = true
		}
	}
	return len(targets)
}

// multiPageAllowlistView builds an allowlist view whose Pagination reports more
// than one page, matching what handleNextForeignAllowlistPage produces for a
// large allowlist. The next/ handler always populates Pagination (the stable
// handler leaves it zero), so this is the case where a stable-pager leak would
// surface.
func multiPageAllowlistView() templates.ForeignAllowlistPageView {
	return templates.ForeignAllowlistPageView{
		Rows: []templates.ForeignAllowlistRow{
			{ID: "a", Scope: "global", FileName: "x.jpg", CreatedAt: "now"},
			{ID: "b", Scope: "global", FileName: "y.jpg", CreatedAt: "now"},
		},
		Pagination: components.PaginationData{
			CurrentPage: 1,
			TotalPages:  3,
			PageSize:    10,
			TotalItems:  25,
			BaseURL:     "/next/reports/foreign-files/allowlist",
			TargetID:    "foreign-allowlist-body",
		},
	}
}

// TestForeignAllowlistBodyNext_SingleKeyboardWiredPager guards against the
// double-pager regression: ForeignAllowlistBodyNext wraps the shared
// ForeignAllowlistTable (which embeds the stable components.Pagination) and ALSO
// appends the keyboard-wired NextPagination. Because the next/ handler populates
// view.Pagination, the shared table's stable pager would render UNLESS it is
// suppressed (stableTablePagerSuppressed). The next/ design mandates the
// keyboard-wired NextPagination only ("NO X-of-Y counter"), so exactly one pager
// (the next/ one) must appear and the stable "Showing page X of Y" counter must
// not.
func TestForeignAllowlistBodyNext_SingleKeyboardWiredPager(t *testing.T) {
	var buf bytes.Buffer
	if err := ForeignAllowlistBodyNext(multiPageAllowlistView()).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if n := strings.Count(out, "Showing page"); n != 0 {
		t.Errorf("stable pager (X-of-Y counter) leaked onto next/ allowlist page: %d occurrence(s); want 0", n)
	}
	// Exactly one keyboard-wired Prev/Next pair (the NextPagination control).
	if n := strings.Count(out, `id="sw-page-prev"`); n != 1 {
		t.Errorf("sw-page-prev count = %d; want exactly 1 (the next/ keyboard-wired pager)", n)
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

// TestForeignAllowlistBodyNext_SinglePageNoPager verifies that a single-page
// allowlist renders no pager at all (neither the stable counter nor the next/
// keyboard-wired control), and no h/l boundary attributes (nothing to page to).
func TestForeignAllowlistBodyNext_SinglePageNoPager(t *testing.T) {
	view := multiPageAllowlistView()
	view.Pagination.TotalPages = 1

	var buf bytes.Buffer
	if err := ForeignAllowlistBodyNext(view).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "Showing page") {
		t.Error("stable pager rendered for a single-page allowlist")
	}
	if strings.Contains(out, `id="sw-page-prev"`) || strings.Contains(out, `id="sw-page-next"`) {
		t.Error("next/ pager rendered for a single-page allowlist")
	}
	if strings.Contains(out, "data-sw-roving-boundary-prev") || strings.Contains(out, "data-sw-roving-boundary-next") {
		t.Error("h/l boundary attributes present for a single-page allowlist")
	}
	// j/k roving + Enter activation still apply even on a single page.
	if !strings.Contains(out, "data-sw-roving-list") {
		t.Error("missing data-sw-roving-list on single-page allowlist body")
	}
}

// TestForeignFiles_StableNextActionParity guards against the drift class that
// produced the #1773 B4 miss: a page-level action present on the stable page
// (the bulk Dismiss button) that was never ported to the next/ page. It renders
// the stable ForeignFilesPage and the next/ ForeignFilesPageNext with identical
// view data and asserts that every foreign-files ACTION CONTRACT in stable (the
// hx-post/hx-get/hx-delete endpoints) also exists in next/, plus that next/
// carries at least as many foreign-files nav links. Styling differs and is NOT
// compared; only the endpoint/href contracts are. A new stable-wrapper control
// added without its next/ counterpart fails this test.
func TestForeignFiles_StableNextActionParity(t *testing.T) {
	view := templates.ForeignFilesPageView{
		Rows: []templates.ForeignFileRow{
			{ID: "f1", ArtistID: "a1", ArtistName: "Alpha", FilePath: "/m/a/x.jpg", FileName: "x.jpg", SizeBytes: 10, DetectedAt: "now"},
		},
		Count: 1, // Count > 0 so the conditional Dismiss control renders on both pages.
	}
	ctx := nextTestCtx(t)

	var sbuf, nbuf bytes.Buffer
	if err := templates.ForeignFilesPage(templates.AssetPaths{IsAdmin: true}, view).Render(ctx, &sbuf); err != nil {
		t.Fatalf("render stable ForeignFilesPage: %v", err)
	}
	if err := ForeignFilesPageNext(templates.AssetPaths{IsAdmin: true}, view).Render(ctx, &nbuf); err != nil {
		t.Fatalf("render next ForeignFilesPageNext: %v", err)
	}
	stable, next := sbuf.String(), nbuf.String()

	stableEp, nextEp := foreignEndpoints(stable), foreignEndpoints(next)
	for ep := range stableEp {
		if !nextEp[ep] {
			t.Errorf("next/ detected-files page is missing stable action endpoint %q; every page-level action contract in stable must exist in next/", ep)
		}
	}
	// Teeth: the bulk-dismiss endpoint must be present in BOTH renders. If the
	// extraction or the port silently breaks, this catches it directly.
	const dismiss = "/api/v1/foreign-files/dismiss"
	if !stableEp[dismiss] {
		t.Fatalf("test setup wrong: stable page did not render the dismiss endpoint %q", dismiss)
	}
	if !nextEp[dismiss] {
		t.Errorf("next/ detected-files page did not render the bulk Dismiss endpoint %q (the #1773 B4 port regression)", dismiss)
	}
	if sc, nc := foreignNavHrefCount(stable), foreignNavHrefCount(next); nc < sc {
		t.Errorf("next/ detected-files foreign-files nav links (%d) < stable (%d): a header nav link was dropped on next/", nc, sc)
	}
}

// TestForeignFiles_DismissCountConditional pins the `if view.Count > 0` guard on
// the bulk Dismiss control (#1773 B4): the button -- and therefore the
// /api/v1/foreign-files/dismiss endpoint -- must render ONLY when there are
// detected files. The parity test only proves the endpoint exists at Count>0; if
// the guard regressed to always-render (dismiss shown on an empty page) or
// never-render (the B4 port miss), nothing else would catch it. Asserted on both
// the next/ ForeignFilesPageNext and the stable ForeignFilesPage (same guard).
func TestForeignFiles_DismissCountConditional(t *testing.T) {
	const dismiss = "/api/v1/foreign-files/dismiss"
	ctx := nextTestCtx(t)

	render := func(t *testing.T, c templ.Component) map[string]bool {
		t.Helper()
		var buf bytes.Buffer
		if err := c.Render(ctx, &buf); err != nil {
			t.Fatalf("render: %v", err)
		}
		return foreignEndpoints(buf.String())
	}

	emptyView := templates.ForeignFilesPageView{Count: 0}
	nonEmptyView := templates.ForeignFilesPageView{
		Rows:  []templates.ForeignFileRow{{ID: "f1", ArtistID: "a1", ArtistName: "Alpha", FileName: "x.jpg", DetectedAt: "now"}},
		Count: 1,
	}
	assets := templates.AssetPaths{IsAdmin: true}

	cases := []struct {
		name string
		comp func(templates.ForeignFilesPageView) templ.Component
	}{
		{"next", func(v templates.ForeignFilesPageView) templ.Component { return ForeignFilesPageNext(assets, v) }},
		{"stable", func(v templates.ForeignFilesPageView) templ.Component { return templates.ForeignFilesPage(assets, v) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if render(t, tc.comp(emptyView))[dismiss] {
				t.Errorf("%s: Dismiss endpoint %q rendered with Count=0; the `if view.Count > 0` guard is broken (always-render)", tc.name, dismiss)
			}
			if !render(t, tc.comp(nonEmptyView))[dismiss] {
				t.Errorf("%s: Dismiss endpoint %q absent with Count>0; the conditional Dismiss control is missing (never-render)", tc.name, dismiss)
			}
		})
	}
}

// TestForeignAllowlist_StableNextActionParity is the allowlist-page counterpart
// to TestForeignFiles_StableNextActionParity: same endpoint-set + nav-count
// parity between the stable ForeignAllowlistPage and next/ ForeignAllowlistPageNext.
func TestForeignAllowlist_StableNextActionParity(t *testing.T) {
	view := multiPageAllowlistView()
	ctx := nextTestCtx(t)

	var sbuf, nbuf bytes.Buffer
	if err := templates.ForeignAllowlistPage(templates.AssetPaths{IsAdmin: true}, view).Render(ctx, &sbuf); err != nil {
		t.Fatalf("render stable ForeignAllowlistPage: %v", err)
	}
	if err := ForeignAllowlistPageNext(templates.AssetPaths{IsAdmin: true}, view).Render(ctx, &nbuf); err != nil {
		t.Fatalf("render next ForeignAllowlistPageNext: %v", err)
	}
	stable, next := sbuf.String(), nbuf.String()

	stableEp, nextEp := foreignEndpoints(stable), foreignEndpoints(next)
	for ep := range stableEp {
		if !nextEp[ep] {
			t.Errorf("next/ allowlist page is missing stable action endpoint %q; every page-level action contract in stable must exist in next/", ep)
		}
	}
	if sc, nc := foreignNavHrefCount(stable), foreignNavHrefCount(next); nc < sc {
		t.Errorf("next/ allowlist foreign-files nav links (%d) < stable (%d): the back-navigation link was dropped on next/", nc, sc)
	}
}
