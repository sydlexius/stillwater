package next

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

// nextTestCtx returns a context with the embedded English translator loaded so
// i18n lookups in the next/ templates resolve to real strings during tests.
func nextTestCtx(tb testing.TB) context.Context {
	tb.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		tb.Fatalf("loading i18n bundle: %v", err)
	}
	return i18n.WithTranslator(context.Background(), bundle.Translator("en"))
}

// TestArtistsPage_ComposesSharedBehaviorAndChrome verifies that the next/
// artists page (M55 #1335) is a chrome refresh that preserves every behavior
// by composing the shared, exported partials and components rather than forking
// them. It asserts the scoping class, the reused body container, the shared
// flyout panel, the bulk-progress-pill, the behavior script, the preserved
// JS-hook ids, and full bulk-action parity (all 5 actions incl. Lock/Unlock).
func TestArtistsPage_ComposesSharedBehaviorAndChrome(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Artists: []artist.Artist{
			{ID: "a1", Name: "Alpha"},
			{ID: "a2", Name: "Bravo"},
		},
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 2,
			BaseURL: "/next/artists", View: "table",
		},
		View:      "table",
		Libraries: []library.Library{{ID: "l1", Name: "Lib One"}, {ID: "l2", Name: "Lib Two"}},
	}

	var buf bytes.Buffer
	if err := ArtistsPage(templates.AssetPaths{IsAdmin: true}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	markers := map[string]string{
		"scoping class":          "sw-next-artists",
		"single-row toolbar":     "sw-next-toolbar",
		"reused body container":  `id="artist-content"`,
		"bulk progress pill":     `id="bulk-progress-pill"`,
		"filter trigger":         `id="artist-filter-trigger"`,
		"shared filter flyout":   `id="artist-filters-flyout"`,
		"hidden view input":      `id="artist-view-input"`,
		"hidden sort input":      `id="artist-sort-input"`,
		"behavior script (sort)": "setSortColumn",
		"htmx filter-sync hook":  "htmx:configRequest",
		"library dropdown":       `name="library_id"`,
		"scan button":            `id="scan-btn"`,
	}
	for name, want := range markers {
		if !strings.Contains(out, want) {
			t.Errorf("next.ArtistsPage missing %s (%q)", name, want)
		}
	}

	// Full bulk-action parity (decision 6): all 5 actions, including Lock and
	// Unlock, surfaced via the shared BulkProgressPill i18n carrier.
	for _, verb := range []string{
		"data-i18n-verb-run-rules",
		"data-i18n-verb-re-identify",
		"data-i18n-verb-fetch-images",
		"data-i18n-verb-lock",
		"data-i18n-verb-unlock",
	} {
		if !strings.Contains(out, verb) {
			t.Errorf("next.ArtistsPage missing bulk verb carrier %q (parity)", verb)
		}
	}

	// The toolbar must target the next/ fragment endpoint so HTMX swaps render
	// the next-specific table/grid into #artist-content -- never the stable
	// table (M55 #1335 routing fix; the prior assertion codified the bug).
	if !strings.Contains(out, `hx-get="/next/artists"`) {
		t.Errorf("next toolbar must target the /next/artists fragment endpoint")
	}
	if strings.Contains(out, `hx-get="/artists"`) {
		t.Errorf("next toolbar must not target the stable /artists endpoint (would swap the stable table)")
	}

	// Sortable Type/Origin columns carry over from the reused ArtistTable.
	for _, col := range []string{`data-col="type"`, `data-col="origin"`} {
		if !strings.Contains(out, col) {
			t.Errorf("next.ArtistsPage table missing sortable column %q", col)
		}
	}
}

// TestArtistsPage_GridViewSelectsCardGrid verifies the view switch renders the
// reused card grid (not the table) when data.View is "grid".
func TestArtistsPage_GridViewSelectsCardGrid(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Artists: []artist.Artist{{ID: "a1", Name: "Alpha"}},
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 1,
			BaseURL: "/artists", View: "grid",
		},
		View: "grid",
	}
	var buf bytes.Buffer
	if err := ArtistsPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "grid-cols-2") {
		t.Errorf("grid view should render the reused card grid (grid-cols-2 absent)")
	}
}

// TestArtistsPage_HeaderChromeAndDensity verifies the next/artists chrome (M55
// #1335): the data-density root attribute, the sr-only document heading that
// replaced the ditched per-screen PageHead (maintainer 2026-05-30 -- the visible
// title + "N of M" count were dropped as redundant with the sidebar highlight and
// the pagination footer), and the completed 4-facet artist-type family
// (Orchestra/Choir + Other) reused from the shared flyout.
func TestArtistsPage_HeaderChromeAndDensity(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Artists: []artist.Artist{{ID: "a1", Name: "Alpha"}},
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 3,
			BaseURL: "/artists", View: "table",
		},
		View:    "table",
		Filters: map[string]string{"type_group": "include"},
	}
	var buf bytes.Buffer
	if err := ArtistsPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, `data-density="comfy"`) {
		t.Errorf("next.ArtistsPage root must carry data-density for the comfy/compact model")
	}
	// The per-screen PageHead was ditched: only an sr-only document heading
	// remains for the a11y outline, and no visible "N of M" count is rendered
	// even when a filter narrows the set.
	if !strings.Contains(out, `class="sr-only"`) {
		t.Errorf("next.ArtistsPage must keep an sr-only document heading after the PageHead was ditched")
	}
	if strings.Contains(out, "3 of 42") {
		t.Errorf("header must NOT show an N-of-M count (the PageHead metric was removed)")
	}
	// Completed artist-type coverage reused from the shared flyout.
	for _, want := range []string{"filter_type_other", "Orchestra/Choir"} {
		if !strings.Contains(out, want) {
			t.Errorf("next.ArtistsPage flyout missing type-facet marker %q", want)
		}
	}

	// Non-narrowed: when nothing narrows the set, the subtitle is a plain library
	// count, not an "N of M" metric.
	plain := templates.ArtistListData{
		Artists: []artist.Artist{{ID: "a1", Name: "Alpha"}},
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 7,
			BaseURL: "/artists", View: "table",
		},
		View: "table",
	}
	var pbuf bytes.Buffer
	if err := ArtistsPage(templates.AssetPaths{}, plain).Render(nextTestCtx(t), &pbuf); err != nil {
		t.Fatalf("render plain: %v", err)
	}
	if strings.Contains(pbuf.String(), "7 of 7") {
		t.Errorf("non-narrowed header must not show an N-of-M metric")
	}
}

// TestArtistsTable_SourcesCoverageScore verifies the next-specific table renders
// the prototype's Sources / Coverage / Score cells (consolidating the stable
// page's verbose badge columns) while preserving the selection hooks, and that
// the stable badge columns are gone.
func TestArtistsTable_SourcesCoverageScore(t *testing.T) {
	t.Parallel()
	evaluated := time.Now()
	data := templates.ArtistListData{
		Artists: []artist.Artist{{
			ID: "a1", Name: "Alpha", Type: "group", Origin: "US",
			ThumbExists:       true,
			MusicBrainzID:     "mbid-1",
			DiscogsID:         "d-1",
			HealthScore:       85,
			HealthEvaluatedAt: &evaluated,
		}},
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 1,
			BaseURL: "/artists", View: "table",
		},
		View: "table",
	}
	var buf bytes.Buffer
	if err := ArtistsTable(data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	markers := map[string]string{
		"sources column":  `data-col="sources"`,
		"coverage column": `data-col="coverage"`,
		"score column":    `data-col="score"`,
		"score percent":   "85%",
		"provider IDs":    "2/6", // MBID + Discogs set, of 6 checked
		"selection hook":  "sw-bulk-select",
		"content wrapper": `id="artist-content"`,
		"sort hook":       "setSortColumn",
	}
	for name, want := range markers {
		if !strings.Contains(out, want) {
			t.Errorf("ArtistsTable missing %s (%q)", name, want)
		}
	}
	// The stable page's verbose badge columns must be consolidated away.
	if strings.Contains(out, `data-col="thumb"`) || strings.Contains(out, `data-col="mbid"`) {
		t.Errorf("next table must not keep the stable verbose badge columns (thumb/mbid)")
	}
}

// TestArtistsTable_UnratedScore verifies an artist that has not been scored
// shows a muted placeholder rather than a misleading 0%.
func TestArtistsTable_UnratedScore(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Artists: []artist.Artist{{ID: "a1", Name: "Alpha"}}, // HealthEvaluatedAt nil
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 1,
			BaseURL: "/artists", View: "table",
		},
		View: "table",
	}
	var buf bytes.Buffer
	if err := ArtistsTable(data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "0%") {
		t.Errorf("unscored artist must not render a misleading 0%% score")
	}
}

// TestArtistsPage_KeyboardShortcuts verifies the next/ page declares its
// keyboard contract for the shared vendored helper (web/static/js/keyboard.js)
// via data-sw-* attributes (/ focus-search, f filters, r scan, j/k/Enter
// roving, bulk scope) and that the old inline ArtistsKeyboardShortcuts mount
// (__swArtistsKbd) has been retired from next/.
func TestArtistsPage_KeyboardShortcuts(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 0,
			BaseURL: "/artists", View: "table",
		},
		View: "table",
	}
	var buf bytes.Buffer
	if err := ArtistsPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`data-sw-shortcut="/"`, `data-sw-shortcut="f"`, `data-sw-shortcut="r"`,
		"data-sw-roving-list", "data-sw-bulk-scope",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("next artists keyboard contract missing %q", want)
		}
	}
	if strings.Contains(out, "__swArtistsKbd") {
		t.Errorf("inline ArtistsKeyboardShortcuts must be retired from next/ artists")
	}

	// Roving layer (#1791), container half: the list advertises the j/k/Enter
	// roving labels for the shared helper's registry even on an empty list.
	for _, want := range []string{
		"data-sw-roving-label-j", "data-sw-roving-label-k", "data-sw-roving-label-Enter",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("next artists roving container contract missing %q", want)
		}
	}
}

// TestArtistsTable_RovingItemContract verifies the #1791 per-row half of the
// roving contract: with artists present, each row carries data-sw-roving-item +
// a stable data-sw-roving-key (so focus survives HTMX swaps of the list) and an
// inner data-sw-roving-activate target (so Enter opens the focused artist's
// detail). These are absent on an empty list, so this test seeds rows.
func TestArtistsTable_RovingItemContract(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Artists: []artist.Artist{{ID: "a1", Name: "Alpha"}, {ID: "a2", Name: "Bravo"}},
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 2,
			BaseURL: "/next/artists", View: "table",
		},
		View: "table",
	}
	var buf bytes.Buffer
	if err := ArtistsTable(data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"data-sw-roving-list",
		"data-sw-roving-item",
		`data-sw-roving-key="a1"`,
		"data-sw-roving-activate",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("next artists row roving contract missing %q", want)
		}
	}
}

// TestArtistsPage_ColumnsIconOnly verifies the #1792 toolbar change: the Columns
// control on the next/ artists toolbar is icon-only (the literal "Columns" text
// node is dropped) but still exposes the label via title + aria-label for
// pointer + assistive-tech users, mirroring the toolbar's other icon-only
// buttons. The shared ColumnToggle component keeps its text-bearing form on the
// stable channel (iconOnly=false), so only the next/ caller is affected.
func TestArtistsPage_ColumnsIconOnly(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Artists: []artist.Artist{{ID: "a1", Name: "Alpha"}},
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 1,
			BaseURL: "/next/artists", View: "table",
		},
		View: "table",
	}
	var buf bytes.Buffer
	if err := ArtistsPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Isolate the Columns control so the assertions can't be satisfied by an
	// unrelated "Columns"/title elsewhere on the page.
	start := strings.Index(out, `data-col-toggle="artists"`)
	if start < 0 {
		t.Fatalf("next artists toolbar missing the Columns control (data-col-toggle)")
	}
	// The control's <button> opens with the toggle onclick; scope the slice to it.
	seg := out[start:]
	if end := strings.Index(seg, "</button>"); end >= 0 {
		seg = seg[:end]
	}
	if !strings.Contains(seg, `title="Columns"`) {
		t.Errorf("icon-only Columns button must carry a title=\"Columns\" tooltip")
	}
	if !strings.Contains(seg, `aria-label="Columns"`) {
		t.Errorf("icon-only Columns button must carry an aria-label=\"Columns\"")
	}
	// Icon-only: the visible "Columns" text node must be gone (only the SVG +
	// the title/aria-label carry the meaning). The substring "Columns" still
	// appears inside the title/aria-label attribute values above; assert there
	// is no bare text node by checking it does not appear immediately before the
	// button close (where the label text used to render).
	if strings.Contains(seg, `>Columns<`) || strings.Contains(seg, "</svg>\n\t\t\tColumns") || strings.Contains(seg, "</svg>Columns") {
		t.Errorf("icon-only Columns button must not render the visible label text node")
	}
}

// TestArtistsTable_SharedNextPaginationAndRovingBoundary verifies the next/
// artists table adopts the shared NextPagination footer (M55 #1791) and wires
// the roving page-nav boundary contract instead of the stable components.
// Pagination. With more than one page, the footer must render the shared
// sw-page-prev/sw-page-next controls, the roving container must declare the
// boundary selectors and the h/l page-nav labels, and the enabled control must
// swap the WHOLE #artist-content fragment via outerHTML (matching the channel's
// sort/filter/search swaps) rather than the dashboard's innerHTML rows-only +
// OOB footer.
func TestArtistsTable_SharedNextPaginationAndRovingBoundary(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Artists: []artist.Artist{{ID: "a1", Name: "Alpha"}, {ID: "a2", Name: "Bravo"}},
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 2, PageSize: 50, TotalItems: 80,
			BaseURL: "/next/artists", View: "table", Sort: "name", Order: "asc",
		},
		View: "table",
	}

	var buf bytes.Buffer
	if err := ArtistsTable(data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Shared NextPagination footer + its fixed control ids the keyboard helper
	// resolves via the boundary selectors.
	for name, want := range map[string]string{
		"shared pagination nav":   `id="sw-pagination"`,
		"prev control id":         `id="sw-page-prev"`,
		"next control id":         `id="sw-page-next"`,
		"roving boundary next":    `data-sw-roving-boundary-next="#sw-page-next"`,
		"roving boundary prev":    `data-sw-roving-boundary-prev="#sw-page-prev"`,
		"page-prev label (h)":     `data-sw-roving-label-h="prev page"`,
		"page-next label (l)":     `data-sw-roving-label-l="next page"`,
		"outerHTML fragment swap": `hx-swap="outerHTML"`,
		"footer targets fragment": `hx-target="#artist-content"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ArtistsTable missing %s (%q)", name, want)
		}
	}

	// The stable page-counter component must be gone (no "Showing page X of Y").
	if strings.Contains(out, "Showing page ") {
		t.Errorf("ArtistsTable must not render the stable components.Pagination counter")
	}
	// On page 1 the previous control is disabled (no hx-get) while next is a real
	// link, so the enabled Next must carry an hx-get to page 2.
	if !strings.Contains(out, "page=2") {
		t.Errorf("ArtistsTable Next control must hx-get the next page (page=2)")
	}
	// Artists replaces the whole fragment, so the footer must NOT be emitted
	// out-of-band (that is the dashboard's rows-only model).
	if strings.Contains(out, `hx-swap-oob="true"`) {
		t.Errorf("ArtistsTable NextPagination must not use an out-of-band footer (outerHTML fragment swap carries it)")
	}
}

// TestArtistsTable_ArtistLinkTargetsNextRoute verifies that artist name links in
// the next/ table row resolve to /next/artists/<id> and not the stable /artists/<id>
// route (M55 #1888 regression guard).
func TestArtistsTable_ArtistLinkTargetsNextRoute(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Artists: []artist.Artist{{ID: "art-42", Name: "Test Artist"}},
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 1,
			BaseURL: "/next/artists", View: "table",
		},
		View: "table",
	}
	var buf bytes.Buffer
	if err := ArtistsTable(data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `href="/next/artists/art-42"`) {
		t.Errorf("table row link must target /next/artists/<id>, not the stable route")
	}
	if strings.Contains(out, `href="/artists/art-42"`) {
		t.Errorf("table row must not leak to the stable /artists/<id> route")
	}
}

// TestArtistsGrid_ArtistLinkTargetsNextRoute verifies that artist card links in
// the next/ grid resolve to /next/artists/<id> and not the stable /artists/<id>
// route (M55 #1888 regression guard).
func TestArtistsGrid_ArtistLinkTargetsNextRoute(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Artists: []artist.Artist{{ID: "art-99", Name: "Grid Artist"}},
		Pagination: components.PaginationData{
			CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 1,
			BaseURL: "/next/artists", View: "grid",
		},
		View: "grid",
	}
	var buf bytes.Buffer
	if err := ArtistsGrid(data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `href="/next/artists/art-99"`) {
		t.Errorf("grid card link must target /next/artists/<id>, not the stable route")
	}
	if strings.Contains(out, `href="/artists/art-99"`) {
		t.Errorf("grid card must not leak to the stable /artists/<id> route")
	}
}

// TestArtistsPage_NoControlPinnedKeycaps verifies step 4 of M55 #1791: the
// inline "/" keycap pinned in the search box and the "f" keycap on the filter
// button are removed (matching the next/ dashboard; the #1789 registry owns the
// hints via data-sw-shortcut). The tip-line legend keeps its / and f keycaps.
func TestArtistsPage_NoControlPinnedKeycaps(t *testing.T) {
	t.Parallel()
	data := templates.ArtistListData{
		Artists:    []artist.Artist{{ID: "a1", Name: "Alpha"}},
		Pagination: components.PaginationData{CurrentPage: 1, TotalPages: 1, PageSize: 50, TotalItems: 1, BaseURL: "/next/artists", View: "table"},
		View:       "table",
	}
	var buf bytes.Buffer
	if err := ArtistsPage(templates.AssetPaths{IsAdmin: true}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// The search-box pinned keycap used a unique absolute-position class set.
	if strings.Contains(out, "pointer-events-none absolute right-2") {
		t.Errorf("search box must not pin an inline / keycap (hint owned by the #1789 registry)")
	}
	// The filter-button keycap used the unique "sw-kbd ml-1 hidden" class set.
	if strings.Contains(out, `class="sw-kbd ml-1 hidden sm:inline-flex"`) {
		t.Errorf("filter button must not render an inline f keycap")
	}
	// The data-sw-shortcut attributes that advertise / and f to the registry must
	// remain, and the tip-line legend must still teach both keys.
	for name, want := range map[string]string{
		"search shortcut attr": `data-sw-shortcut="/"`,
		"filter shortcut attr": `data-sw-shortcut="f"`,
		"legend keeps search":  `<kbd class="sw-kbd inline-flex">/</kbd>`,
		"legend keeps filter":  `<kbd class="sw-kbd inline-flex">f</kbd>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ArtistsPage missing %s (%q)", name, want)
		}
	}
}
