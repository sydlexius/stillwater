package next

import (
	"context"
	"fmt"
	"net/url"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/i18n"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/web/templates"
)

// The next/ channel is a separate Go package from web/templates, so it cannot
// reach the stable package's unexported i18n and presentation helpers. Rather
// than export them from the stable package (which would churn the v1 hot file
// across many call sites for a purely cosmetic preview screen), the small,
// presentation-only helpers the next/ artists toolbar needs are reimplemented
// here. They are deliberate, low-risk duplicates of trivial label/class logic;
// the pre-promotion parity gate (M55 W6) re-checks them against the stable
// originals. The behavior-bearing pieces (the filter flyout and the page
// JS) are NOT duplicated -- they are composed from the shared exported
// templates.ArtistFilterFlyout / templates.ArtistsPageScripts partials.

// t returns the translated string for the request locale. Mirrors
// templates.t so copied toolbar markup reads identically.
func t(ctx context.Context, key string) string {
	return i18n.TFromCtx(ctx).T(key)
}

// tn returns a pluralized translation (key.one / key.other with {count}).
func tn(ctx context.Context, key string, count int) string {
	return i18n.TFromCtx(ctx).Tn(key, count)
}

// tf returns a translated string with fmt.Sprintf-style interpolation.
func tf(ctx context.Context, key string, args ...any) string {
	tmpl := i18n.TFromCtx(ctx).T(key)
	if tmpl == key {
		return key
	}
	return fmt.Sprintf(tmpl, args...)
}

// Glass/outline button convention for the next/ channel.
//
// glassButton is the single shared class-string for all glass/outline buttons
// across the next/ dashboard, artists, and bulk screens. It uses Tailwind v4
// arbitrary-value utilities to reference the --swd-line and --swd-ink-2 CSS
// variable tokens. Those tokens are defined (light + dark) on the next/ screen
// markers .sw-next-dashboard, .sw-next-artist-detail, .sw-next-foreign-files,
// and .sw-next-artists (see the --swd-* token blocks in web/static/css/
// input.css); the buttons must render inside one of those scopes for the tokens
// to resolve. Border and text color then adapt automatically to both themes and
// the user's Background Opacity setting -- no inline style= attribute required.
// Combine with layout utilities (inline-flex, gap, padding, font-*, disabled:*)
// at each call site.
const glassButton = "rounded-md border border-[var(--swd-line)] text-[var(--swd-ink-2)] hover:bg-white/5 focus:outline-none focus:ring-2 focus:ring-blue-500"

// glassButtonHalo is appended (via templ.KV or concatenation) when a glass
// button is in its active/pressed state. An inset ring keeps the halo inside
// the element's border, matching the artist-detail kind-tab convention.
// focus:ring-4 focus:ring-blue-700 override the base glassButton focus ring
// (ring-2 ring-blue-500) with a heavier, darker indicator so keyboard focus
// is distinguishable from the always-on halo (WCAG 2.4.7).
const glassButtonHalo = "ring-2 ring-blue-500 ring-inset focus:ring-4 focus:ring-blue-700"

// Filter-trigger class sets for the next/ toolbar. The glass base is already
// on the element (glassButton); these constants only carry the toggled overlays
// the JS data-active-classes / data-neutral-classes swap switches between.
const (
	filterTriggerActive  = glassButtonHalo
	filterTriggerNeutral = "hover:bg-white/5"
	filterTriggerBadge   = "sw-filter-trigger-badge ml-0.5 inline-flex h-4 w-4 items-center justify-center rounded-full bg-blue-600 text-[10px] font-bold text-white dark:bg-blue-400 dark:text-gray-900"
)

// activeFilterCount counts include/exclude (non-neutral) filter facets.
func activeFilterCount(filters map[string]string) int {
	count := 0
	for _, v := range filters {
		if v == "include" || v == "exclude" {
			count++
		}
	}
	return count
}

// sortLabel returns a short display label for the active sort field.
func sortLabel(ctx context.Context, sort string) string {
	switch sort {
	case "sort_name":
		return t(ctx, "artists.sort.sort_name")
	case "type":
		return t(ctx, "artists.sort.type")
	case "origin":
		return t(ctx, "artists.sort.origin")
	case "health_score":
		return t(ctx, "artists.sort.health_score")
	case "updated_at":
		return t(ctx, "artists.sort.last_updated")
	case "created_at":
		return t(ctx, "artists.sort.date_added")
	default:
		return t(ctx, "artists.sort.name")
	}
}

// orderLabel returns a short label for the sort order.
func orderLabel(ctx context.Context, order string) string {
	if order == "desc" {
		return t(ctx, "artists.order.desc")
	}
	return t(ctx, "artists.order.asc")
}

// sortDropdownItem returns the class string for a sort-dropdown menu item,
// highlighting the active selection.
func sortDropdownItem(active bool) string {
	base := "w-full text-left px-4 py-1.5 text-sm hover:bg-gray-50 dark:hover:bg-gray-700 "
	if active {
		return base + "font-semibold text-blue-600 dark:text-blue-400"
	}
	return base + "text-gray-700 dark:text-gray-200"
}

// -- next/ artists table cells (M55 #1335) -----------------------------------
// The next/ table forks the ROW rendering to adopt the prototype's
// Sources / Coverage / Score cells, so it needs a few presentation helpers the
// stable package keeps unexported. These are deliberate, low-risk
// reimplementations of trivial class/label logic (the pre-promotion parity gate
// re-checks them against the stable originals). The behavior-bearing pieces
// (platform badges, active-filter chips, bulk bar, pagination) are REUSED from
// the exported templates.* / components.* partials, never duplicated.

// nextLibraryName returns the display name of the artist's primary library
// (Artist.LibraryID, hydrated from the M:N membership table), or "" when the
// artist has no resolvable library. Drives the next/ table's Library column,
// which the prototype surfaces but the stable page exposes only via the filter.
func nextLibraryName(a artist.Artist, libs []library.Library) string {
	if a.LibraryID == "" {
		return ""
	}
	for i := range libs {
		if libs[i].ID == a.LibraryID {
			return libs[i].Name
		}
	}
	return ""
}

// nextTypeLabel maps a raw artist type to the localized filter-flyout facet
// label (Person / Group / Orchestra / Other) so the Type column reads with the
// same vocabulary as the type filter, instead of exposing raw values like
// "solo" or "" that have no matching facet and confuse the reader. Mirrors the
// server-side facet grouping in internal/artist/scan.go: type_person =
// {person, solo}, type_group = {group}, type_orchestra = {orchestra, choir},
// type_other = the negation facet that catches everything else (including
// untyped artists).
func nextTypeLabel(ctx context.Context, rawType string) string {
	// Delegate to the shared templates helper so the artists list, the
	// artist-detail hero tag, and the metadata Type row can never diverge.
	return templates.ArtistTypeLabel(ctx, rawType)
}

// nextShowAllPath mirrors the stable showAllPath: it rebuilds the list URL
// without the ids= filter so the "Show all" affordance (shown while a
// show-selected view is active) drops the selection filter and returns to the
// full list, preserving search/sort/order/filter/library/view. Channel-aware
// via data.Pagination.BaseURL (set by buildArtistListData), so in next/ it
// targets /next/artists, not the stable /artists.
func nextShowAllPath(data templates.ArtistListData) string {
	v := url.Values{}
	if data.Search != "" {
		v.Set("search", data.Search)
	}
	if data.Sort != "" {
		v.Set("sort", data.Sort)
	}
	if data.Order != "" {
		v.Set("order", data.Order)
	}
	if data.Filter != "" {
		v.Set("filter", data.Filter)
	}
	if data.LibraryID != "" {
		v.Set("library_id", data.LibraryID)
	}
	if data.View != "" {
		v.Set("view", data.View)
	}
	for k, state := range data.Filters {
		switch state {
		case "include":
			v.Set("filter_"+k, "+y")
		case "exclude":
			v.Set("filter_"+k, "-y")
		}
	}
	base := data.Pagination.BaseURL
	if base == "" {
		base = "/next/artists"
	}
	enc := v.Encode()
	if enc == "" {
		return base
	}
	return base + "?" + enc
}

// nextIsFilterActive mirrors templates.isFilterActive: it reports whether a
// narrowing filter/search/library is active, which gates the bulk-action safety
// rail via the #artist-content data-filter-active flag.
func nextIsFilterActive(data templates.ArtistListData) bool {
	return activeFilterCount(data.Filters) > 0 || data.Search != "" || data.LibraryID != "" || data.Filter != ""
}

// nextComplianceAvailable mirrors templates.complianceAvailable: a nil map means
// compliance could not be loaded, so the dot is suppressed (no grey placeholder).
func nextComplianceAvailable(m map[string]artist.ComplianceStatus) bool { return m != nil }

// nextArtistCompliance looks up an artist's compliance status, defaulting to
// compliant when absent (the artist has no active violations).
func nextArtistCompliance(id string, m map[string]artist.ComplianceStatus) artist.ComplianceStatus {
	if s, ok := m[id]; ok {
		return s
	}
	return artist.ComplianceCompliant
}

// nextComplianceDotClass / nextComplianceDotTitle mirror the stable dot mapping.
func nextComplianceDotClass(status artist.ComplianceStatus) string {
	switch status {
	case artist.ComplianceError:
		return "bg-red-500"
	case artist.ComplianceWarning:
		return "bg-yellow-500"
	default:
		return "bg-green-500"
	}
}

func nextComplianceDotTitle(ctx context.Context, status artist.ComplianceStatus) string {
	switch status {
	case artist.ComplianceError:
		return t(ctx, "artists.compliance.error")
	case artist.ComplianceWarning:
		return t(ctx, "artists.compliance.warning")
	default:
		return t(ctx, "artists.compliance.compliant")
	}
}

// nextCoverageItem is one metadata/image presence bubble in the Coverage cell.
type nextCoverageItem struct {
	Label   string // single-letter glyph (M / T / F / L)
	State   string // "present" | "low" | "missing"
	Tooltip string // localized "<term>: <state>" hover text
}

// nextCoverageItems consolidates the stable page's Metadata / Thumb / Fanart /
// Logo badge columns into one compact set of bubbles. Image bubbles keep the
// stable three-state distinction (present / low-res / missing) so no signal is
// lost; tooltips reuse the active profile's image terminology.
func nextCoverageItems(ctx context.Context, a artist.Artist, profileName string) []nextCoverageItem {
	mk := func(label, term, state string) nextCoverageItem {
		return nextCoverageItem{Label: label, State: state, Tooltip: term + ": " + t(ctx, "artists.coverage."+state)}
	}
	imgState := func(exists, low bool) string {
		switch {
		case exists && low:
			return "low"
		case exists:
			return "present"
		default:
			return "missing"
		}
	}
	// M = metadata completeness, GRADED by how many applicable descriptive fields
	// are populated. Per the maintainer: users care about metadata completeness,
	// not NFO-file presence -- so M is a fraction (full/partial/empty), not a
	// binary flag. T/F/L are the three image slots (amber = low-res). Images and
	// provider IDs have their own indicators and are excluded from the metadata
	// tally to avoid double-counting; overall rule compliance is the Score column.
	have, total := nextMetadataFields(a)
	mState := "missing"
	if total > 0 {
		switch {
		case have == total:
			mState = "present"
		case have > 0:
			mState = "partial"
		}
	}
	return []nextCoverageItem{
		{Label: "M", State: mState, Tooltip: tf(ctx, "artists.coverage.metadata_fields", have, total)},
		mk("T", img.ImageTermFor("thumb", profileName), imgState(a.ThumbExists, a.ThumbLowRes)),
		mk("F", img.ImageTermFor("fanart", profileName), imgState(a.FanartExists, a.FanartLowRes)),
		mk("L", img.ImageTermFor("logo", profileName), imgState(a.LogoExists, a.LogoLowRes)),
	}
}

// nextMetadataFields counts how many applicable descriptive metadata fields the
// artist has populated, and how many apply, for the graded Coverage "M" bubble.
// The TYPE-APPLICABILITY guards follow internal/artist/completeness.go (born/
// died/gender for person|solo|character; formed/disbanded for group|orchestra|
// choir and the empty default). The descriptive FIELD SET, however, is chosen
// independently for this coverage bubble and is NOT the completeness report's
// field set: it adds Moods/Origin/YearsActive/Gender and excludes NFO presence,
// the MBID, and the image slots (images and provider IDs have their own
// indicators here, so they are deliberately left out of the metadata tally).
func nextMetadataFields(a artist.Artist) (have, total int) {
	isPerson := a.Type == "person" || a.Type == "solo" || a.Type == "character"
	isEnsemble := a.Type == "group" || a.Type == "orchestra" || a.Type == "choir" || a.Type == ""
	fields := []struct {
		applicable bool
		present    bool
	}{
		{true, a.Biography != ""},
		{true, len(a.Genres) > 0},
		{true, len(a.Styles) > 0},
		{true, len(a.Moods) > 0},
		{true, a.Origin != ""},
		{true, a.YearsActive != ""},
		{isEnsemble, a.Formed != ""},
		{isEnsemble, a.Disbanded != ""},
		{isPerson, a.Born != ""},
		{isPerson, a.Died != ""},
		{isPerson, a.Gender != ""},
	}
	for _, f := range fields {
		if !f.applicable {
			continue
		}
		total++
		if f.present {
			have++
		}
	}
	return have, total
}

// nextCoverageBubbleClass returns the Tailwind classes for a coverage bubble in
// the given state: present = filled green, low = filled amber, missing = muted
// outline. The .sw-cov hook lets the scoped CSS refine these.
func nextCoverageBubbleClass(state string) string {
	base := "sw-cov inline-flex h-4 w-4 items-center justify-center rounded-[3px] text-[9px] font-bold leading-none "
	switch state {
	case "present":
		return base + "bg-green-500/20 text-green-600 dark:text-green-400 ring-1 ring-inset ring-green-500/50"
	case "low", "partial":
		return base + "bg-yellow-500/20 text-yellow-600 dark:text-yellow-400 ring-1 ring-inset ring-yellow-500/50"
	default:
		return base + "text-gray-400 dark:text-gray-600 ring-1 ring-inset ring-gray-300/60 dark:ring-gray-600/60"
	}
}

// nextProviderIDCount returns how many provider IDs are set and the total
// checked, for the Coverage cell's "h / t IDs" count.
func nextProviderIDCount(a artist.Artist) (have, total int) {
	ids := []string{a.MusicBrainzID, a.AudioDBID, a.DiscogsID, a.SpotifyID, a.DeezerID, a.WikidataID}
	total = len(ids)
	for _, id := range ids {
		if id != "" {
			have++
		}
	}
	return have, total
}

// nextScoreText / nextScoreDot map a 0-100 health score to tone classes,
// mirroring the prototype ScoreCell thresholds (100 ok, >=70 info, >=40 warn,
// else err).
func nextScoreText(score float64) string {
	switch {
	case score >= 100:
		// green-700 (not -600): white-bg contrast 4.94:1 vs green-600's 3.22:1
		// (fails AA); dark keeps green-400 (7.83:1). #1784 contrast floor.
		return "text-green-700 dark:text-green-400"
	case score >= 70:
		return "text-blue-600 dark:text-blue-400"
	case score >= 40:
		// yellow-700 (not -600): white-bg contrast 4.92:1 vs yellow-600's 2.93:1
		// (fails AA); dark keeps yellow-400 (8.86:1). #1784 contrast floor.
		return "text-yellow-700 dark:text-yellow-400"
	default:
		return "text-red-600 dark:text-red-400"
	}
}

func nextScoreDot(score float64) string {
	switch {
	case score >= 100:
		return "bg-green-500"
	case score >= 70:
		return "bg-blue-500"
	case score >= 40:
		return "bg-yellow-500"
	default:
		return "bg-red-500"
	}
}

// nextArtistScored reports whether the artist has an evaluated health score, so
// the Score cell shows a muted placeholder instead of a misleading 0%.
func nextArtistScored(a artist.Artist) bool { return a.HealthEvaluatedAt != nil }

// nextScorePercent formats the health score as a whole-number percent.
func nextScorePercent(score float64) string { return fmt.Sprintf("%.0f%%", score) }

// nextIDCountText / nextIDCountTitle render the Coverage cell's provider-ID
// coverage ("h/t" with a localized hover label).
func nextIDCountText(a artist.Artist) string {
	have, total := nextProviderIDCount(a)
	return fmt.Sprintf("%d/%d", have, total)
}

func nextIDCountTitle(ctx context.Context, a artist.Artist) string {
	have, total := nextProviderIDCount(a)
	return tf(ctx, "artists.coverage.ids", have, total)
}
