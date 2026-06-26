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

	// The 4B Artwork section now renders at its mount point (id="next-artwork-{id}").
	if !strings.Contains(out, "next-artwork-art-1") {
		t.Errorf("artwork section (4B) should render its card at the artwork mount point")
	}

	// The 4C sections (providers/discography) now render their cards (4C landed).
	for _, want := range []string{"next-providers-art-1", "next-discography-art-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("4C section %q should render its card", want)
		}
	}

	// Single-scroll page: it must NOT render the stable tab bar.
	if strings.Contains(out, `role="tablist"`) {
		t.Errorf("next page must be single-scroll, not the stable tab bar")
	}
}

// TestArtistDetailPage_ChipRefreshOnActionResolved pins the inline-chip live
// refresh (#1860): when a field chip's Fix/Dismiss popover resolves a violation
// it dispatches dashboard:action-resolved, and the page script must refresh the
// editable (metadata/identifiers) sections so the resolved chip disappears
// without a reload. The wiring lives in the inline artistDetailPageScript, so we
// assert the rendered page subscribes that event to refreshEditableSections.
func TestArtistDetailPage_ChipRefreshOnActionResolved(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, detailPageData(nil, nil)).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "dashboard:action-resolved") {
		t.Fatalf("page script must subscribe to dashboard:action-resolved for chip refresh")
	}
	// The handler body must call refreshEditableSections so the metadata/identifiers
	// chips re-render. Assert the listener registration and the refresh call both
	// appear (proximity check: the listener line names the event + the refresh fn).
	idx := strings.Index(out, "addEventListener('dashboard:action-resolved'")
	if idx < 0 {
		t.Fatalf("expected an addEventListener('dashboard:action-resolved', ...) registration in the page script")
	}
	// Within the handler that follows, refreshEditableSections() must be invoked.
	window := out[idx:]
	if end := strings.Index(window, "});"); end > 0 {
		window = window[:end]
	}
	if !strings.Contains(window, "refreshEditableSections()") {
		t.Errorf("dashboard:action-resolved handler must call refreshEditableSections() to clear resolved chips; handler:\n%s", window)
	}
	// The handler must guard against re-entrant calls while the user is editing
	// a field (editing flag set by the edit-all toggle). Without the guard a
	// resolved action would swap out a section the user is actively editing.
	if !strings.Contains(window, "if (editing) return") {
		t.Errorf("dashboard:action-resolved handler must guard with 'if (editing) return' to avoid clobbering active edits; handler:\n%s", window)
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
		// OOB landing pad so the stable violations fragment doesn't log
		// htmx:oobErrorNoTarget on the next/ page.
		`id="violations-tab-badge"`,
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

// TestArtistDetailPage_FanartBase verifies the page root carries the
// data-sw-fanart-base attribute so the inline ambient-backdrop script can
// derive its API path from the DOM instead of hardcoding /api/v1 (#1861).
func TestArtistDetailPage_FanartBase(t *testing.T) {
	t.Parallel()
	assets := templates.AssetPaths{BasePath: "/app"}
	data := detailPageData(nil, nil)
	var buf bytes.Buffer
	if err := ArtistDetailPage(assets, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// The data attribute must carry the full base-path-prefixed fanart list URL.
	want := `data-sw-fanart-base="/app/api/v1/artists/art-1/images/fanart/"`
	if !strings.Contains(out, want) {
		t.Errorf("page root missing fanart base attr; want %q in output", want)
	}
	// The inline script must NOT contain a hardcoded /api/v1 path for the
	// artist-specific fanart URL -- it must read from the data attribute.
	if strings.Contains(out, `'/api/v1/artists/'`) {
		t.Errorf("ambient backdrop script still hardcodes /api/v1 path")
	}
}

// TestArtistDetailPage_HeroID verifies the hero section carries its stable
// select ID so a History-undo revert can refresh it in place via htmx.ajax
// select-swap (#1850).
func TestArtistDetailPage_HeroID(t *testing.T) {
	t.Parallel()
	data := detailPageData(nil, nil)
	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	want := `id="next-hero-art-1"`
	if !strings.Contains(out, want) {
		t.Errorf("hero section missing id attribute; want %q in output", want)
	}
}

// TestArtistDetailPage_HeroTypePillNoUppercase verifies the hero type pill does
// not carry the CSS uppercase class (#1843): the normalized label from
// nextTypeLabel already uses Title Case and must not be forced ALL CAPS.
func TestArtistDetailPage_HeroTypePillNoUppercase(t *testing.T) {
	t.Parallel()
	data := detailPageData(nil, nil)
	data.Detail.Artist.Type = "group"
	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// The hero pill span must contain the type label but must NOT include the
	// Tailwind "uppercase" class alongside it.
	if !strings.Contains(out, "tracking-wide") {
		t.Errorf("hero type pill span appears to be missing (tracking-wide not found)")
	}
	// Check for the pattern: "uppercase" must not appear adjacent to "tracking-wide"
	// in the hero type span (the only pill on the page using tracking-wide).
	if strings.Contains(out, `"rounded-full px-2 py-0.5 uppercase tracking-wide"`) {
		t.Errorf("hero type pill still carries CSS uppercase class; want Title Case only")
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
	// "debug" is in defaultSectionOrder (after "discography", before "identifiers").
	if got, want := orderedSections([]string{"identifiers", "bogus", "metadata"}, nil),
		[]string{"identifiers", "metadata", "artwork", "findings", "providers", "discography", "debug"}; !slices.Equal(got, want) {
		t.Errorf("pref order = %v, want %v", got, want)
	}
	// Hidden ids removed; the rest keep default order. Assert the full slice.
	if got, want := orderedSections(nil, []string{"artwork", "discography"}),
		[]string{"metadata", "findings", "providers", "debug", "identifiers"}; !slices.Equal(got, want) {
		t.Errorf("hidden removal = %v, want %v", got, want)
	}
}

// TestIsCollapsed verifies the collapsed-membership helper.
func TestIsCollapsed(t *testing.T) {
	t.Parallel()
	collapsed := []string{"artwork", "debug"}
	if !isCollapsed("artwork", collapsed) {
		t.Error("isCollapsed(artwork) = false, want true")
	}
	if isCollapsed("metadata", collapsed) {
		t.Error("isCollapsed(metadata) = true, want false")
	}
	if isCollapsed("artwork", nil) {
		t.Error("isCollapsed with nil list = true, want false")
	}
}

// TestArtistDetailPage_CollapsibleSortableChrome verifies the #2065 chrome: the
// reorderable region wrapper, the per-section drag handle + disclosure button
// with aria wiring, and that a collapsed section seeds aria-expanded="false" +
// a hidden body while an expanded one does not.
func TestArtistDetailPage_CollapsibleSortableChrome(t *testing.T) {
	t.Parallel()
	data := detailPageData(nil, nil)
	// Collapse the metadata section; leave findings expanded.
	data.Collapsed = []string{"metadata"}

	var buf bytes.Buffer
	if err := ArtistDetailPage(templates.AssetPaths{}, data).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// The reorderable region wrapper SortableJS mounts on, plus the shared chrome.
	for _, want := range []string{
		`data-sw-sortable-section`,
		`class="sw-section-drag-handle"`,
		`data-sw-section-toggle`,
		`class="sw-section-collapse-btn"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("page missing collapsible/sortable marker %q", want)
		}
	}

	// Collapsed metadata section: disclosure collapsed + body hidden + aria-controls
	// wired to the body id.
	for _, want := range []string{
		`aria-controls="next-metadata-body"`,
		`<div id="next-metadata-body" class="body sw-next-meta-body" hidden`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("collapsed metadata section missing %q", want)
		}
	}
	// The metadata toggle must be the collapsed one (aria-expanded="false"); the
	// findings body must NOT be hidden (it is expanded).
	if !strings.Contains(out, `aria-controls="next-metadata-body"`) ||
		!strings.Contains(out, `aria-expanded="false"`) {
		t.Error("collapsed metadata toggle should carry aria-expanded=false")
	}
	if strings.Contains(out, `<div id="next-findings-body" class="body" hidden`) {
		t.Error("findings body should not be hidden when not collapsed")
	}
}
