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

// TestArtistsPage_KeyboardShortcuts verifies the shared keyboard surface
// (/ focus-search, Cmd/Ctrl+A select-all-visible, Esc clear) is wired into the
// next/ page.
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
	for _, want := range []string{"__swArtistsKbd", "artist-search", "Escape", "metaKey"} {
		if !strings.Contains(out, want) {
			t.Errorf("ArtistsPage keyboard surface missing %q", want)
		}
	}
}
