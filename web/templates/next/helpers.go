package next

import (
	"context"
	"fmt"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/i18n"
	img "github.com/sydlexius/stillwater/internal/image"
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

// -- next/ artist-detail presentation helpers (M55 #1335/#1336) ---------------
// The artists LIST promoted to the canonical templates package in #1757 PR-3a;
// the helpers below remain only for the not-yet-promoted next/ artist-detail
// screen (coverage.templ + artist_detail.templ) and are deleted with PR-3b /
// the PR-6 lane teardown. The canonical package carries its own copies.

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
