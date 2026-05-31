package next

import (
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

// TestTranslationHelpers exercises the small i18n wrappers t / tn / tf,
// including the tf passthrough when a key is missing. Named tt to avoid
// shadowing the package-level translation helper t.
func TestTranslationHelpers(tt *testing.T) {
	tt.Parallel()
	ctx := nextTestCtx(tt)

	if got := t(ctx, "artists.sort.name"); got == "" {
		tt.Fatalf("t returned empty for a known key")
	}

	// tn pluralizes; both forms should resolve to a non-key string for a real key.
	if got := tn(ctx, "artists.count", 1); got == "" {
		tt.Errorf("tn returned empty for known key")
	}
	if got := tn(ctx, "artists.count", 5); got == "" {
		tt.Errorf("tn returned empty for known key plural")
	}

	// tf with a missing key returns the key unchanged (no Sprintf applied).
	const missing = "this.key.definitely.does.not.exist"
	if got := tf(ctx, missing, 1, 2); got != missing {
		tt.Errorf("tf(missing) = %q, want passthrough %q", got, missing)
	}

	// tf with a real format key interpolates without panicking.
	if got := tf(ctx, "artists.coverage.ids", 3, 6); got == "" {
		tt.Errorf("tf interpolation returned empty")
	}
}

// TestActiveFilterCount covers include/exclude counting and neutral skips.
func TestActiveFilterCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   map[string]string
		want int
	}{
		{"nil", nil, 0},
		{"empty", map[string]string{}, 0},
		{"all neutral", map[string]string{"a": "neutral", "b": ""}, 0},
		{"mixed", map[string]string{"a": "include", "b": "exclude", "c": "neutral"}, 2},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := activeFilterCount(c.in); got != c.want {
				t.Errorf("activeFilterCount(%v) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestSortLabel covers every sort branch including the default.
func TestSortLabel(t *testing.T) {
	t.Parallel()
	ctx := nextTestCtx(t)
	for _, sort := range []string{
		"sort_name", "type", "origin", "health_score",
		"updated_at", "created_at", "name", "bogus", "",
	} {
		if got := sortLabel(ctx, sort); got == "" {
			t.Errorf("sortLabel(%q) returned empty", sort)
		}
	}
}

// TestOrderLabel covers asc and desc.
func TestOrderLabel(t *testing.T) {
	t.Parallel()
	ctx := nextTestCtx(t)
	asc := orderLabel(ctx, "asc")
	desc := orderLabel(ctx, "desc")
	if asc == "" || desc == "" {
		t.Fatalf("orderLabel empty: asc=%q desc=%q", asc, desc)
	}
	if asc == desc {
		t.Errorf("orderLabel asc and desc should differ: %q", asc)
	}
	// Unknown order falls through to asc.
	if got := orderLabel(ctx, "weird"); got != asc {
		t.Errorf("orderLabel(weird) = %q, want asc %q", got, asc)
	}
}

// TestSortDropdownItem covers the active and inactive class branches.
func TestSortDropdownItem(t *testing.T) {
	t.Parallel()
	active := sortDropdownItem(true)
	inactive := sortDropdownItem(false)
	if active == inactive {
		t.Fatalf("active and inactive sort-dropdown classes should differ")
	}
	if !contains(active, "font-semibold") {
		t.Errorf("active item missing font-semibold: %q", active)
	}
	if contains(inactive, "font-semibold") {
		t.Errorf("inactive item should not be bold: %q", inactive)
	}
}

// TestNextLibraryName covers the empty-id, hit, and miss branches.
func TestNextLibraryName(t *testing.T) {
	t.Parallel()
	libs := []library.Library{{ID: "l1", Name: "Lib One"}, {ID: "l2", Name: "Lib Two"}}
	cases := []struct {
		name string
		a    artist.Artist
		want string
	}{
		{"no library id", artist.Artist{}, ""},
		{"resolves", artist.Artist{LibraryID: "l2"}, "Lib Two"},
		{"unresolvable", artist.Artist{LibraryID: "missing"}, ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := nextLibraryName(c.a, libs); got != c.want {
				t.Errorf("nextLibraryName = %q, want %q", got, c.want)
			}
		})
	}
}

// TestNextTypeLabel covers all facet groupings and the default catch-all.
func TestNextTypeLabel(t *testing.T) {
	t.Parallel()
	ctx := nextTestCtx(t)
	person := nextTypeLabel(ctx, "person")
	group := nextTypeLabel(ctx, "group")
	orchestra := nextTypeLabel(ctx, "orchestra")
	other := nextTypeLabel(ctx, "")

	if person == "" || group == "" || orchestra == "" || other == "" {
		t.Fatalf("nextTypeLabel returned an empty label")
	}
	// solo maps to person, choir to orchestra (case-insensitive + trim).
	if got := nextTypeLabel(ctx, "  SOLO "); got != person {
		t.Errorf("nextTypeLabel(solo) = %q, want person %q", got, person)
	}
	if got := nextTypeLabel(ctx, "Choir"); got != orchestra {
		t.Errorf("nextTypeLabel(choir) = %q, want orchestra %q", got, orchestra)
	}
	if got := nextTypeLabel(ctx, "wat"); got != other {
		t.Errorf("nextTypeLabel(wat) = %q, want other %q", got, other)
	}
}

// TestNextShowAllPath covers the full and empty query permutations.
func TestNextShowAllPath(t *testing.T) {
	t.Parallel()

	// All fields set -> every param plus the include/exclude filters.
	full := templates.ArtistListData{
		Search:     "bach",
		Sort:       "type",
		Order:      "desc",
		Filter:     "incomplete",
		LibraryID:  "l1",
		View:       "grid",
		Filters:    map[string]string{"type_person": "include", "type_group": "exclude", "noise": "neutral"},
		Pagination: components.PaginationData{BaseURL: "/next/artists"},
	}
	got := nextShowAllPath(full)
	for _, want := range []string{
		"/next/artists?", "search=bach", "sort=type", "order=desc",
		"filter=incomplete", "library_id=l1", "view=grid",
		"filter_type_person=%2By", "filter_type_group=-y",
	} {
		if !contains(got, want) {
			t.Errorf("nextShowAllPath full missing %q in %q", want, got)
		}
	}
	if contains(got, "noise") {
		t.Errorf("neutral filter should not appear: %q", got)
	}

	// No fields and no BaseURL -> default base, no query string.
	if got := nextShowAllPath(templates.ArtistListData{}); got != "/next/artists" {
		t.Errorf("nextShowAllPath empty = %q, want /next/artists", got)
	}
}

// TestNextIsFilterActive covers each predicate branch.
func TestNextIsFilterActive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		data templates.ArtistListData
		want bool
	}{
		{"none", templates.ArtistListData{}, false},
		{"facet", templates.ArtistListData{Filters: map[string]string{"x": "include"}}, true},
		{"search", templates.ArtistListData{Search: "q"}, true},
		{"library", templates.ArtistListData{LibraryID: "l1"}, true},
		{"filter", templates.ArtistListData{Filter: "incomplete"}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := nextIsFilterActive(c.data); got != c.want {
				t.Errorf("nextIsFilterActive = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNextComplianceAvailable covers nil vs non-nil maps.
func TestNextComplianceAvailable(t *testing.T) {
	t.Parallel()
	if nextComplianceAvailable(nil) {
		t.Errorf("nil map should be unavailable")
	}
	if !nextComplianceAvailable(map[string]artist.ComplianceStatus{}) {
		t.Errorf("non-nil map should be available")
	}
}

// TestNextArtistCompliance covers present and absent lookups.
func TestNextArtistCompliance(t *testing.T) {
	t.Parallel()
	m := map[string]artist.ComplianceStatus{"a1": artist.ComplianceError}
	if got := nextArtistCompliance("a1", m); got != artist.ComplianceError {
		t.Errorf("present lookup = %q, want error", got)
	}
	if got := nextArtistCompliance("missing", m); got != artist.ComplianceCompliant {
		t.Errorf("absent lookup = %q, want compliant default", got)
	}
}

// TestNextComplianceDotClassAndTitle covers all three severity branches.
func TestNextComplianceDotClassAndTitle(t *testing.T) {
	t.Parallel()
	ctx := nextTestCtx(t)
	cases := []struct {
		status    artist.ComplianceStatus
		wantClass string
	}{
		{artist.ComplianceError, "bg-red-500"},
		{artist.ComplianceWarning, "bg-yellow-500"},
		{artist.ComplianceCompliant, "bg-green-500"},
	}
	titles := map[string]bool{}
	for _, c := range cases {
		if got := nextComplianceDotClass(c.status); got != c.wantClass {
			t.Errorf("nextComplianceDotClass(%q) = %q, want %q", c.status, got, c.wantClass)
		}
		title := nextComplianceDotTitle(ctx, c.status)
		if title == "" {
			t.Errorf("nextComplianceDotTitle(%q) empty", c.status)
		}
		titles[title] = true
	}
	if len(titles) != 3 {
		t.Errorf("expected 3 distinct compliance titles, got %d: %v", len(titles), titles)
	}
}

// TestNextCoverageItems covers metadata grading and image state mapping.
func TestNextCoverageItems(t *testing.T) {
	t.Parallel()
	ctx := nextTestCtx(t)

	// Fully populated person with all images present and high-res.
	full := artist.Artist{
		Type:         "person",
		Biography:    "bio",
		Genres:       []string{"rock"},
		Styles:       []string{"indie"},
		Moods:        []string{"happy"},
		Origin:       "US",
		YearsActive:  "1990-2000",
		Born:         "1970",
		Died:         "2020",
		Gender:       "male",
		ThumbExists:  true,
		FanartExists: true, FanartLowRes: true,
		LogoExists: false,
	}
	items := nextCoverageItems(ctx, full, "emby")
	if len(items) != 4 {
		t.Fatalf("expected 4 coverage items, got %d", len(items))
	}
	if items[0].Label != "M" || items[0].State != "present" {
		t.Errorf("M bubble = %+v, want present", items[0])
	}
	if items[1].State != "present" { // thumb exists, high-res
		t.Errorf("thumb state = %q, want present", items[1].State)
	}
	if items[2].State != "low" { // fanart exists but low-res
		t.Errorf("fanart state = %q, want low", items[2].State)
	}
	if items[3].State != "missing" { // logo absent
		t.Errorf("logo state = %q, want missing", items[3].State)
	}

	// Partial metadata -> M state partial.
	partial := artist.Artist{Type: "group", Biography: "bio"}
	if got := nextCoverageItems(ctx, partial, "")[0].State; got != "partial" {
		t.Errorf("partial M state = %q, want partial", got)
	}

	// Empty group -> M state missing.
	empty := artist.Artist{Type: "group"}
	if got := nextCoverageItems(ctx, empty, "")[0].State; got != "missing" {
		t.Errorf("empty M state = %q, want missing", got)
	}

	// Image low-res flag without the exists flag is treated as missing, not low:
	// a stale ThumbLowRes on an absent image must not light the amber bubble.
	lowButAbsent := artist.Artist{Type: "group", ThumbExists: false, ThumbLowRes: true}
	if got := nextCoverageItems(ctx, lowButAbsent, "")[1].State; got != "missing" {
		t.Errorf("absent-but-low thumb state = %q, want missing", got)
	}
}

// TestNextMetadataFields covers person vs ensemble applicability.
func TestNextMetadataFields(t *testing.T) {
	t.Parallel()

	// Person: born/died/gender apply, formed/disbanded do not.
	person := artist.Artist{Type: "person", Biography: "b", Born: "1970"}
	have, total := nextMetadataFields(person)
	if have != 2 { // biography + born
		t.Errorf("person have = %d, want 2", have)
	}
	if total != 9 { // 6 universal + born/died/gender
		t.Errorf("person total = %d, want 9", total)
	}

	// Ensemble (empty type): formed/disbanded apply, born/died/gender do not.
	group := artist.Artist{Type: "", Formed: "1980", Genres: []string{"jazz"}}
	have, total = nextMetadataFields(group)
	if have != 2 { // genres + formed
		t.Errorf("group have = %d, want 2", have)
	}
	if total != 8 { // 6 universal + formed/disbanded
		t.Errorf("group total = %d, want 8", total)
	}

	// Character is a person variant: born/died/gender apply (total 9), same as
	// a "person" type.
	character := artist.Artist{Type: "character", Born: "1939"}
	have, total = nextMetadataFields(character)
	if have != 1 { // born only
		t.Errorf("character have = %d, want 1", have)
	}
	if total != 9 { // 6 universal + born/died/gender
		t.Errorf("character total = %d, want 9", total)
	}
}

// TestNextCoverageBubbleClass covers each state branch.
func TestNextCoverageBubbleClass(t *testing.T) {
	t.Parallel()
	present := nextCoverageBubbleClass("present")
	low := nextCoverageBubbleClass("low")
	partial := nextCoverageBubbleClass("partial")
	missing := nextCoverageBubbleClass("missing")

	if !contains(present, "green") {
		t.Errorf("present bubble not green: %q", present)
	}
	if low != partial {
		t.Errorf("low and partial should share amber styling")
	}
	if !contains(low, "yellow") {
		t.Errorf("low bubble not amber: %q", low)
	}
	if !contains(missing, "gray") {
		t.Errorf("missing bubble not muted: %q", missing)
	}
}

// TestNextProviderIDCount covers some, all, and none.
func TestNextProviderIDCount(t *testing.T) {
	t.Parallel()
	have, total := nextProviderIDCount(artist.Artist{MusicBrainzID: "mb", SpotifyID: "sp"})
	if have != 2 || total != 6 {
		t.Errorf("partial provider IDs = %d/%d, want 2/6", have, total)
	}
	have, _ = nextProviderIDCount(artist.Artist{})
	if have != 0 {
		t.Errorf("no provider IDs have = %d, want 0", have)
	}
	// All six provider IDs set: have == total == 6.
	allSix := artist.Artist{
		MusicBrainzID: "mb", AudioDBID: "adb", DiscogsID: "dg",
		SpotifyID: "sp", DeezerID: "dz", WikidataID: "wd",
	}
	if have, total := nextProviderIDCount(allSix); have != 6 || total != 6 {
		t.Errorf("all provider IDs = %d/%d, want 6/6", have, total)
	}
}

// TestNextScoreTextAndDot covers every threshold band.
func TestNextScoreTextAndDot(t *testing.T) {
	t.Parallel()
	bands := []struct {
		score     float64
		textColor string
		dotColor  string
	}{
		{100, "green", "bg-green-500"},
		{99, "blue", "bg-blue-500"}, // just below the 100 ok threshold
		{85, "blue", "bg-blue-500"},
		{70, "blue", "bg-blue-500"}, // exact >=70 info boundary
		{50, "yellow", "bg-yellow-500"},
		{40, "yellow", "bg-yellow-500"}, // exact >=40 warn boundary
		{10, "red", "bg-red-500"},
	}
	for _, b := range bands {
		if got := nextScoreText(b.score); !contains(got, b.textColor) {
			t.Errorf("nextScoreText(%v) = %q, want %s", b.score, got, b.textColor)
		}
		if got := nextScoreDot(b.score); got != b.dotColor {
			t.Errorf("nextScoreDot(%v) = %q, want %q", b.score, got, b.dotColor)
		}
	}
}

// TestNextArtistScored covers scored vs unscored.
func TestNextArtistScored(t *testing.T) {
	t.Parallel()
	if nextArtistScored(artist.Artist{}) {
		t.Errorf("unscored artist should report false")
	}
	now := time.Now()
	if !nextArtistScored(artist.Artist{HealthEvaluatedAt: &now}) {
		t.Errorf("scored artist should report true")
	}
}

// TestNextScorePercentAndIDText covers the formatting helpers.
func TestNextScorePercentAndIDText(t *testing.T) {
	t.Parallel()
	ctx := nextTestCtx(t)
	if got := nextScorePercent(87.4); got != "87%" {
		t.Errorf("nextScorePercent(87.4) = %q, want 87%%", got)
	}
	// %.0f rounds half away from zero, so 87.5 rounds up to 88.
	if got := nextScorePercent(87.5); got != "88%" {
		t.Errorf("nextScorePercent(87.5) = %q, want 88%%", got)
	}
	a := artist.Artist{MusicBrainzID: "mb"}
	if got := nextIDCountText(a); got != "1/6" {
		t.Errorf("nextIDCountText = %q, want 1/6", got)
	}
	if got := nextIDCountTitle(ctx, a); got == "" {
		t.Errorf("nextIDCountTitle empty")
	}
}

// contains is a tiny strings.Contains alias to keep the assertions terse.
func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
