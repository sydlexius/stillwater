package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/web/components"
)

// TestArtistFilterFlyout_RendersAllFamilies verifies the flyout partial
// extracted from ArtistsPage (M55 #1335) still renders every tri-state filter
// family. The next/ channel composes this exact partial, so its content is the
// single source of truth for filter behavior on both channels.
func TestArtistFilterFlyout_RendersAllFamilies(t *testing.T) {
	t.Parallel()
	// Two libraries so the per-library filter section renders (the v1 page
	// gates that section behind len(Libraries) > 1).
	data := ArtistListData{Libraries: []library.Library{{ID: "l1", Name: "Lib One"}, {ID: "l2", Name: "Lib Two"}}}
	var buf bytes.Buffer
	if err := ArtistFilterFlyout(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, marker := range []string{
		`id="artist-filters-flyout"`,
		"filter_missing_meta",   // metadata family
		"filter_has_biography",  // metadata-fields family
		"filter_missing_images", // images family
		"filter_in_emby",        // platform family
		"filter_excluded",       // status family
		"filter_type_person",    // artist-type family
		"filter_library_l1",     // per-library family (Libraries > 1 path)
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("ArtistFilterFlyout missing %q", marker)
		}
	}
}

// TestArtistsPageScripts_CarriesBehavior verifies the page-behavior script
// extracted from ArtistsPage (M55 #1335) still carries the selection, sort,
// view, and off-page-selection hooks the list depends on. The next/ channel
// composes this same script so its behavior is identical, not forked.
func TestArtistsPageScripts_CarriesBehavior(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := ArtistsPageScripts().Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, marker := range []string{
		"setSortColumn",      // table header sort
		"setView",            // table/grid toggle
		"setSort",            // sort dropdown
		"setOrder",           // order dropdown
		"htmx:configRequest", // filter + off-page id injection hook
		"data-clear-ids",     // off-page selection opt-out (#1227)
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("ArtistsPageScripts missing %q", marker)
		}
	}
}

// TestArtistsPage_StillComposesExtractedPartials guards the stable page after
// the extraction: ArtistsPage must still emit the flyout panel, the behavior
// script, and the bulk-progress-pill (now via the shared partials), so the
// stable channel is unregressed by the #1335 refactor.
func TestArtistsPage_StillComposesExtractedPartials(t *testing.T) {
	t.Parallel()
	data := ArtistListData{
		Pagination: components.PaginationData{View: "table", BaseURL: "/artists"},
		View:       "table",
	}
	var buf bytes.Buffer
	if err := ArtistsPage(AssetPaths{}, data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, marker := range []string{
		`id="artist-filters-flyout"`,
		"setSortColumn",
		`id="bulk-progress-pill"`,
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("stable ArtistsPage missing %q after extraction", marker)
		}
	}
}
