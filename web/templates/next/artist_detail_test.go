package next

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
)

// detailPageData builds a minimal ArtistDetailPageData for render tests.
func detailPageData(sectionOrder, hidden []string) ArtistDetailPageData {
	return ArtistDetailPageData{
		Detail: templates.ArtistDetailData{
			Artist: artist.Artist{
				ID:   "art-1",
				Name: "Render Test Artist",
				Type: "Group",
				Path: "/music/Render Test Artist",
			},
			FieldProviders: map[string][]string{},
		},
		SectionOrder: sectionOrder,
		Hidden:       hidden,
	}
}

// TestArtistDetailPage_RendersHeroAndSections verifies the next/ artist-detail
// page renders the hero (name + on-disk path), the full metadata field set, and
// the findings anchor, all under the next-channel scope class. The 4B/4C
// mount-point sections are SUPPRESSED until those features build them (maintainer
// UAT: empty cards read as broken), so this also asserts they render no card.
func TestArtistDetailPage_RendersHeroAndSections(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, detailPageData(nil, nil)).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	markers := map[string]string{
		"next-channel scope": "sw-next-artist-detail",
		"hero artist name":   "Render Test Artist",
		"on-disk path":       "/music/Render Test Artist",
		"name field":         "field-name-art-1",
		"genres field":       "field-genres-art-1",
		"moods field":        "field-moods-art-1",
		"musicbrainz id":     "field-musicbrainz_id-art-1",
		"findings anchor":    `id="next-findings"`,
	}
	for label, want := range markers {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page missing %s (%q)", label, want)
		}
	}

	// The unbuilt 4B (artwork) and 4C (providers/discography) sections must not
	// render an empty card. They stay in defaultSectionOrder (so they slot back
	// in without reflow) but emit no visible markup until their features land.
	for _, absent := range []string{"next-artwork-art-1", "next-providers-art-1", "next-discography-art-1"} {
		if strings.Contains(out, absent) {
			t.Errorf("suppressed mount-point section %q should render no card", absent)
		}
	}

	// Single-scroll page: it must NOT render the stable tab bar.
	if strings.Contains(out, `role="tablist"`) {
		t.Errorf("next page must be single-scroll, not the stable tab bar")
	}
}

// TestArtistDetailPage_PrototypeChrome pins the prototype-fidelity rework
// (M55 #1336, 4A UAT): the page adopts the dark sw-dash-card system, the hero is
// the restrained portrait|meta|actions layout with a compliance summary bar (no
// fanart-as-header band), the metadata fields render as key-value rows, and the
// findings/history sections load eagerly (the intersect-once trigger left them
// stuck on "Loading" in a full-page capture).
func TestArtistDetailPage_PrototypeChrome(t *testing.T) {
	t.Parallel()
	// A scored artist so the health badge renders the percent, not a class leak.
	data := detailPageData(nil, nil)
	evaluated := time.Time{} // non-nil pointer marks the artist as scored
	data.Detail.Artist.HealthEvaluatedAt = &evaluated
	data.Detail.Artist.HealthScore = 62

	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"sw-next-hero-portrait", // restrained hero portrait, not a fanart band
		"sw-next-hero-meta",     // hero meta column
		"sw-next-summary",       // compliance summary bar
		"sw-cov",                // shared coverage box-bar (from coverage.templ)
		"sw-next-fields",        // metadata key-value rows
		`hx-trigger="load"`,     // findings/history load eagerly (bug #6)
		"62%",                   // health badge shows the percent (bug #5)
		"data-sw-stickhdr",      // sticky mini-header (design section 6)
		"sw-next-stickhdr-name", // sticky header carries the artist name
		// Toolbar round 2: labeled Actions dropdown (not a bare ellipsis), and
		// Run Rules moved INTO it (still keyboard-bound R).
		`aria-controls="ctx-panel-next-artist-actions-art-1"`,
		`data-sw-shortcut="R"`,
		"/artists/art-1/run-rules",
		// Edit-all is a bidirectional toggle (aria-pressed reflects state).
		`data-sw-edit-all`,
		`aria-pressed="false"`,
		// OOB landing pads so the reused stable fragments don't log
		// htmx:oobErrorNoTarget on the next/ page (bug #4).
		`id="violations-tab-badge"`,
		`id="history-showing-counter"`,
		// A11y baseline: the Actions disclosure declares a menu popup, and the
		// alias input has a real (sr-only) accessible name, not placeholder-only.
		`aria-haspopup="menu"`,
		`aria-labelledby="alias-input-label-art-1"`,
		`role="menu"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page missing prototype-chrome marker %q", want)
		}
	}

	// The hero's Run Rules lives in the Actions dropdown (a gray menuitem right
	// after the dropdown's aria-controls), not as a hero primary button. Assert
	// the run-rules endpoint appears within the Actions dropdown panel markup.
	panelIdx := strings.Index(out, `id="ctx-panel-next-artist-actions-art-1"`)
	runIdx := strings.LastIndex(out, "/artists/art-1/run-rules")
	if panelIdx < 0 || runIdx < panelIdx {
		t.Errorf("Run Rules should render inside the Actions dropdown panel")
	}

	// Bug #5 regression: the score tone is a CLASS, not text content. The raw
	// class string must never appear as escaped text inside a >...< text node.
	if strings.Contains(out, ">text-green-600 dark:text-green-400<") ||
		strings.Contains(out, ">text-red-600 dark:text-red-400<") {
		t.Errorf("health score tone class leaked as visible text (bug #5 regression)")
	}

	// The old fanart-as-card-header band must be gone.
	if strings.Contains(out, "sw-next-hero-head") {
		t.Errorf("the dominating fanart hero header should be removed")
	}
}

// TestArtistDetailPage_FindingSeverity verifies the hero bar renders the
// per-severity finding counts when the breakdown is present.
func TestArtistDetailPage_FindingSeverity(t *testing.T) {
	t.Parallel()
	data := detailPageData(nil, nil)
	data.Detail.ViolationCount = 5
	data.Detail.ViolationsBySeverity = map[string]int{"error": 1, "warning": 4, "info": 0}

	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// Severity dots use the --swd-err / --swd-warn tokens; the counts render as
	// plain numbers linking to the findings section.
	for _, want := range []string{"var(--swd-err)", "var(--swd-warn)", "#next-findings"} {
		if !strings.Contains(out, want) {
			t.Errorf("hero finding-severity missing %q", want)
		}
	}
	// info=0 must not render an info dot.
	if strings.Contains(out, "var(--swd-info)") {
		t.Errorf("info severity dot should be absent when info count is 0")
	}
}

// TestArtistDetailPage_GenderHiddenForGroups verifies the gender field is
// suppressed for group-type artists (via the gender-wrap OOB container) and the
// container itself is still present so the type->gender OOB swap keeps working.
func TestArtistDetailPage_GenderHiddenForGroups(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, detailPageData(nil, nil)).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "gender-wrap-art-1") {
		t.Errorf("gender OOB container missing")
	}
	if strings.Contains(out, "field-gender-art-1") {
		t.Errorf("gender field should be hidden for a group-type artist")
	}
}

// TestArtistDetailPage_SectionOrderHonored verifies a custom section order is
// applied (Hero always first), and a hidden section is omitted.
func TestArtistDetailPage_SectionOrderHonored(t *testing.T) {
	t.Parallel()
	// Put identifiers first among the reorderable sections; hide discography.
	data := detailPageData([]string{"identifiers", "metadata"}, []string{"discography"})

	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	idIdx := strings.Index(out, "next-ids-heading")
	metaIdx := strings.Index(out, "next-metadata-art-1")
	if idIdx < 0 || metaIdx < 0 {
		t.Fatalf("expected both identifiers and metadata sections; ids=%d meta=%d", idIdx, metaIdx)
	}
	if idIdx > metaIdx {
		t.Errorf("identifiers should render before metadata per the custom order")
	}
	if strings.Contains(out, "next-discography-art-1") {
		t.Errorf("hidden discography section should be omitted")
	}
}

// TestArtistDetailPage_Neighbors verifies prev/next-artist links and their
// keyboard-shortcut attributes render when neighbor ids are present.
func TestArtistDetailPage_Neighbors(t *testing.T) {
	t.Parallel()
	data := detailPageData(nil, nil)
	data.PrevArtistID = "prev-id"
	data.NextArtistID = "next-id"

	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"/next/artists/prev-id",
		"/next/artists/next-id",
		`data-sw-shortcut="h"`,
		`data-sw-shortcut="l"`,
		`data-sw-prev-artist`,
		`data-sw-next-artist`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page missing neighbor marker %q", want)
		}
	}
}

// TestOrderedSections covers the pref/default merge + hide logic directly.
func TestOrderedSections(t *testing.T) {
	t.Parallel()
	// Default order, nothing hidden: the full slice must equal defaultSectionOrder.
	if got := orderedSections(nil, nil); !slices.Equal(got, defaultSectionOrder) {
		t.Errorf("default order = %v, want %v", got, defaultSectionOrder)
	}
	// Pref reorders known ids; unknown id ("bogus") dropped; ids missing from the
	// pref are appended in default order. Assert the FULL resulting slice so a
	// regression in the trailing (appended) order is caught, not just the prefix.
	if got, want := orderedSections([]string{"identifiers", "bogus", "metadata"}, nil),
		[]string{"identifiers", "metadata", "artwork", "findings", "history", "providers", "discography"}; !slices.Equal(got, want) {
		t.Errorf("pref order = %v, want %v", got, want)
	}
	// Hidden ids removed; the rest keep default order. Assert the full slice.
	if got, want := orderedSections(nil, []string{"artwork", "discography"}),
		[]string{"metadata", "findings", "history", "providers", "identifiers"}; !slices.Equal(got, want) {
		t.Errorf("hidden removal = %v, want %v", got, want)
	}
}
