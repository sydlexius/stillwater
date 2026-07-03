package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// Consolidated from web/templates/next/helpers_test.go with the artists-list
// promotion (#1757 PR-3a): these cover the canonical copies of the coverage /
// score helpers backing coverage.templ. The next/ package keeps its own copies
// (and their tests) until artist-detail promotes in PR-3b.

// TestCoverageItems covers metadata grading and image state mapping.
func TestCoverageItems(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)

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
	items := coverageItems(ctx, full, "emby")
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
	if got := coverageItems(ctx, partial, "")[0].State; got != "partial" {
		t.Errorf("partial M state = %q, want partial", got)
	}

	// Empty group -> M state missing.
	empty := artist.Artist{Type: "group"}
	if got := coverageItems(ctx, empty, "")[0].State; got != "missing" {
		t.Errorf("empty M state = %q, want missing", got)
	}

	// Image low-res flag without the exists flag is treated as missing, not low:
	// a stale ThumbLowRes on an absent image must not light the amber bubble.
	lowButAbsent := artist.Artist{Type: "group", ThumbExists: false, ThumbLowRes: true}
	if got := coverageItems(ctx, lowButAbsent, "")[1].State; got != "missing" {
		t.Errorf("absent-but-low thumb state = %q, want missing", got)
	}
}

// TestMetadataFields covers person vs ensemble applicability.
func TestMetadataFields(t *testing.T) {
	t.Parallel()

	// Person: born/died/gender apply, formed/disbanded do not.
	person := artist.Artist{Type: "person", Biography: "b", Born: "1970"}
	have, total := metadataFields(person)
	if have != 2 { // biography + born
		t.Errorf("person have = %d, want 2", have)
	}
	if total != 9 { // 6 universal + born/died/gender
		t.Errorf("person total = %d, want 9", total)
	}

	// Ensemble (empty type): formed/disbanded apply, born/died/gender do not.
	group := artist.Artist{Type: "", Formed: "1980", Genres: []string{"jazz"}}
	have, total = metadataFields(group)
	if have != 2 { // genres + formed
		t.Errorf("group have = %d, want 2", have)
	}
	if total != 8 { // 6 universal + formed/disbanded
		t.Errorf("group total = %d, want 8", total)
	}

	// Character is a person variant: born/died/gender apply (total 9), same as
	// a "person" type.
	character := artist.Artist{Type: "character", Born: "1939"}
	have, total = metadataFields(character)
	if have != 1 { // born only
		t.Errorf("character have = %d, want 1", have)
	}
	if total != 9 { // 6 universal + born/died/gender
		t.Errorf("character total = %d, want 9", total)
	}
}

// TestCoverageBubbleClass covers each state branch.
func TestCoverageBubbleClass(t *testing.T) {
	t.Parallel()
	present := coverageBubbleClass("present")
	low := coverageBubbleClass("low")
	partial := coverageBubbleClass("partial")
	missing := coverageBubbleClass("missing")

	if !strings.Contains(present, "green") {
		t.Errorf("present bubble not green: %q", present)
	}
	if low != partial {
		t.Errorf("low and partial should share amber styling")
	}
	if !strings.Contains(low, "yellow") {
		t.Errorf("low bubble not amber: %q", low)
	}
	if !strings.Contains(missing, "gray") {
		t.Errorf("missing bubble not muted: %q", missing)
	}
}

// TestProviderIDCount covers some, all, and none.
func TestProviderIDCount(t *testing.T) {
	t.Parallel()
	have, total := providerIDCount(artist.Artist{MusicBrainzID: "mb", SpotifyID: "sp"})
	if have != 2 || total != 6 {
		t.Errorf("partial provider IDs = %d/%d, want 2/6", have, total)
	}
	have, _ = providerIDCount(artist.Artist{})
	if have != 0 {
		t.Errorf("no provider IDs have = %d, want 0", have)
	}
	// All six provider IDs set: have == total == 6.
	allSix := artist.Artist{
		MusicBrainzID: "mb", AudioDBID: "adb", DiscogsID: "dg",
		SpotifyID: "sp", DeezerID: "dz", WikidataID: "wd",
	}
	if have, total := providerIDCount(allSix); have != 6 || total != 6 {
		t.Errorf("all provider IDs = %d/%d, want 6/6", have, total)
	}
}

// TestScoreTextAndDot covers every threshold band.
func TestScoreTextAndDot(t *testing.T) {
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
		if got := scoreText(b.score); !strings.Contains(got, b.textColor) {
			t.Errorf("scoreText(%v) = %q, want %s", b.score, got, b.textColor)
		}
		if got := scoreDot(b.score); got != b.dotColor {
			t.Errorf("scoreDot(%v) = %q, want %q", b.score, got, b.dotColor)
		}
	}
}

// TestArtistScored covers scored vs unscored.
func TestArtistScored(t *testing.T) {
	t.Parallel()
	if artistScored(artist.Artist{}) {
		t.Errorf("unscored artist should report false")
	}
	now := time.Now()
	if !artistScored(artist.Artist{HealthEvaluatedAt: &now}) {
		t.Errorf("scored artist should report true")
	}
}

// TestScorePercentAndIDText covers the formatting helpers.
func TestScorePercentAndIDText(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	if got := scorePercent(87.4); got != "87%" {
		t.Errorf("scorePercent(87.4) = %q, want 87%%", got)
	}
	// %.0f rounds half away from zero, so 87.5 rounds up to 88.
	if got := scorePercent(87.5); got != "88%" {
		t.Errorf("scorePercent(87.5) = %q, want 88%%", got)
	}
	a := artist.Artist{MusicBrainzID: "mb"}
	if got := idCountText(a); got != "1/6" {
		t.Errorf("idCountText = %q, want 1/6", got)
	}
	if got := idCountTitle(ctx, a); got == "" {
		t.Errorf("idCountTitle empty")
	}
}
