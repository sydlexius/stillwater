package templates

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/i18n"
)

// blastRadiusNonNarrowingFields lists the artist.BlastRadiusFilter fields that
// are deliberately NOT narrowing axes, with the reason each is exempt.
//
// The drift guard below walks the filter struct by reflection and demands an
// axis for every field it does not find here. Adding a field to this map is
// therefore a DELIBERATE, reviewable act -- which is the point: the failure mode
// being guarded against is a narrowing field added with no axis, so the escape
// hatch has to be loud rather than a silent default.
var blastRadiusNonNarrowingFields = map[string]string{
	"Sort":   "reorders the same rows; it never removes one",
	"Order":  "reorders the same rows; it never removes one",
	"Limit":  "paging, not narrowing; the pager already reports the page",
	"Offset": "paging, not narrowing; the pager already reports the page",
}

// TestBlastRadiusAxes_CoverEveryNarrowingFilterField is the guard that makes
// PR 3 and PR 4 hard to get wrong.
//
// blastRadiusNarrowed decides which empty-state sentence the pane renders:
// "nothing recorded" (a claim about the WHOLE library) or "nothing matches your
// filter". A narrowing axis that reaches the query but not the axis table
// reintroduces the false all-clear for that axis -- an operator filtering to a
// field with no damage is told no tracked field was ever overwritten, over a
// library that still holds destroyed values.
//
// Nothing about adding a field to artist.BlastRadiusFilter fails on its own, so
// this walks the struct by REFLECTION rather than checking a written list. A new
// narrowing field fails here until it is either given an axis or explicitly
// declared non-narrowing above.
//
// Mutation proving teeth: deleting any entry from blastRadiusAxes fails this.
func TestBlastRadiusAxes_CoverEveryNarrowingFilterField(t *testing.T) {
	t.Parallel()

	covered := map[string]bool{}
	for _, axis := range blastRadiusAxes(BlastRadiusData{}) {
		covered[axis.StructField] = true
	}

	ft := reflect.TypeOf(artist.BlastRadiusFilter{})
	// Precondition: reflection actually saw the struct. A type that reported
	// zero fields would satisfy every assertion below without checking one.
	if ft.NumField() == 0 {
		t.Fatalf("artist.BlastRadiusFilter reported no fields; the reflection walk found nothing and this test is vacuous")
	}

	for i := 0; i < ft.NumField(); i++ {
		field := ft.Field(i)
		name := field.Name

		// REFUSE what this walk cannot see.
		//
		// reflect.NumField does NOT flatten embedded structs: for
		// struct{ Inner; Class string } it returns 2, and field 0 is
		// {Name:"Inner", Anonymous:true, Kind:struct} whose OWN fields are
		// never visited. So an implementer who groups related axes into an
		// embedded struct -- exactly the shape a multi-field axis invites --
		// would get a green suite while an inner field narrows the query and
		// blastRadiusNarrowed never learns about it. That is the false
		// all-clear walking through the front door of the guard built to stop
		// it.
		//
		// Failing loudly here turns a silent hole into an instruction at the
		// moment someone creates it, which is the same trick the exemption map
		// plays: the escape hatch is deliberate and reviewable, never a default.
		if field.Anonymous {
			t.Errorf("artist.BlastRadiusFilter embeds %s: reflect.NumField does not flatten embedded "+
				"structs, so this guard cannot see its inner fields and a narrowing one would pass "+
				"unnoticed. Flatten the field, or teach this walk to recurse.", name)
			continue
		}

		if _, exempt := blastRadiusNonNarrowingFields[name]; exempt {
			if covered[name] {
				t.Errorf("artist.BlastRadiusFilter.%s is declared non-narrowing but ALSO has an axis; "+
					"one of the two is wrong and the empty-state wording depends on which", name)
			}
			continue
		}
		if !covered[name] {
			t.Errorf("artist.BlastRadiusFilter.%s narrows the query but has no entry in blastRadiusAxes. "+
				"blastRadiusNarrowed therefore returns false for a request narrowed only on %s, and the pane "+
				"renders the LIBRARY-WIDE all-clear over a filtered view. Add an axis, or declare the field "+
				"non-narrowing in blastRadiusNonNarrowingFields with a reason.", name, name)
		}
	}
}

// TestBlastRadiusNarrowed_TrueForEverySingleAxisNarrowing walks the axis table
// and asserts each axis ALONE flips blastRadiusNarrowed, plus the neutral case.
//
// Table-driven over blastRadiusAxes rather than over a hand-written list of
// four cases, so an axis added later is exercised here the moment it exists.
// Pairing it with the reflection guard above closes the loop: that test proves
// every narrowing filter field HAS an axis, this one proves every axis actually
// narrows.
//
// Mutations proving teeth: (a) making blastRadiusNarrowed return false
// unconditionally fails every narrowing case; (b) returning true
// unconditionally fails the neutral case; (c) dropping any single axis from
// blastRadiusAxes fails that axis's subtest.
func TestBlastRadiusNarrowed_TrueForEverySingleAxisNarrowing(t *testing.T) {
	t.Parallel()

	// A value that is narrowing on the axis under test, chosen to differ from
	// the axis's own neutral value. Real values where the axis has a closed
	// vocabulary, so a case cannot be satisfied by a string the query layer
	// would coerce away.
	narrowingValue := map[string]string{
		"class":       artist.BlastClassBlanked,
		"attribution": artist.BlastAttributionUnknown,
		"field":       "biography",
		"artist_id":   "some-artist-id",
	}

	axes := blastRadiusAxes(BlastRadiusData{})
	// Precondition: the table is not empty. An empty table makes the loop body
	// run zero times, so every assertion below would be skipped and the test
	// would report green while verifying nothing -- the exact vacuity this
	// repo has been bitten by.
	if len(axes) == 0 {
		t.Fatalf("blastRadiusAxes returned no axes; the loop below would not execute and this test is vacuous")
	}

	// The unnarrowed baseline. Class and Attribution are set to their neutral
	// "all" (which is what the loader produces after Validate, never an empty
	// string), so a narrowed==true here would mean the neutral case itself is
	// broken.
	neutral := BlastRadiusData{Class: artist.BlastScopeAll, Attribution: artist.BlastScopeAll}
	if blastRadiusNarrowed(neutral) {
		t.Fatalf("blastRadiusNarrowed reported an unfiltered request as narrowed; "+
			"the pane could never render the library-wide sentence. data: %+v", neutral)
	}

	for _, axis := range axes {
		t.Run(axis.Param, func(t *testing.T) {
			value, ok := narrowingValue[axis.Param]
			if !ok {
				t.Fatalf("axis %q has no narrowing value in this test's table; a new axis was added to "+
					"blastRadiusAxes without a case here, so it is UNVERIFIED rather than passing", axis.Param)
			}
			// Precondition: the value chosen really is narrowing on this axis.
			// A value equal to the axis's neutral would make the assertion
			// below a test of the neutral case wearing the wrong name.
			if value == axis.Neutral {
				t.Fatalf("axis %q narrowing value %q equals its neutral value; the case proves nothing", axis.Param, value)
			}

			data := blastRadiusDataWithAxis(neutral, axis.Param, value)
			// Precondition: setting the field took. A typo'd param name would
			// leave data identical to neutral and the failure below would read
			// as a defect in blastRadiusNarrowed rather than in this table.
			if blastRadiusAxisValue(data, axis.Param) != value {
				t.Fatalf("axis %q: setting the value did not reach BlastRadiusData; the case is testing the neutral data", axis.Param)
			}

			if !blastRadiusNarrowed(data) {
				t.Errorf("blastRadiusNarrowed(%+v) = false with only %s narrowed. The pane will render the "+
					"LIBRARY-WIDE all-clear over a filtered view, telling the operator nothing was destroyed "+
					"while damage sits behind the filter.", data, axis.Param)
			}
			if got := blastRadiusFilterCount(data); got != 1 {
				t.Errorf("blastRadiusFilterCount(%+v) = %d with exactly one axis narrowed, want 1; "+
					"the Filters badge disagrees with the empty-state decision", data, got)
			}
		})
	}
}

// TestBlastRadiusFilterCount_MatchesTheNumberOfNarrowingAxes pins the badge
// against multi-axis states, which the single-axis loop above cannot reach.
//
// Mutation proving teeth: counting axes unconditionally (dropping the
// axis.active() check) makes the neutral case report 4 instead of 0 and the
// two-axis case report 4 instead of 2.
func TestBlastRadiusFilterCount_MatchesTheNumberOfNarrowingAxes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data BlastRadiusData
		want int
	}{
		// The two neutral cases (all-scopes and empty-scopes) are NOT repeated
		// here: TestBlastRadiusNarrowed_TrueForEverySingleAxisNarrowing already
		// asserts both, and the count-agreement check below ties the badge to
		// the same verdict. What this table adds that `narrowed` cannot express
		// is CARDINALITY -- one, two, four -- so it carries only the cases where
		// the number itself is the property.
		{
			name: "one axis",
			data: BlastRadiusData{Class: artist.BlastClassBlanked, Attribution: artist.BlastScopeAll},
			want: 1,
		},
		{
			name: "two axes",
			data: BlastRadiusData{Class: artist.BlastClassBlanked, Attribution: artist.BlastAttributionUnknown},
			want: 2,
		},
		{
			name: "every axis",
			data: BlastRadiusData{
				Class:       artist.BlastClassReplaced,
				Attribution: artist.BlastAttributionAutomated,
				Field:       "genres",
				ArtistID:    "art-1",
			},
			want: 4,
		},
		{
			// Ordering is not narrowing. A badge that counted it would tell the
			// operator rows are hidden when a re-sort hides none.
			name: "sort and order are not counted",
			data: BlastRadiusData{
				Class:       artist.BlastScopeAll,
				Attribution: artist.BlastScopeAll,
				Sort:        artist.BlastSortArtistName,
				Order:       "asc",
			},
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blastRadiusFilterCount(tc.data); got != tc.want {
				t.Errorf("blastRadiusFilterCount = %d, want %d for %+v", got, tc.want, tc.data)
			}
			// The badge and the empty-state wording read the same table, so
			// they must never disagree about whether anything is narrowing.
			if narrowed := blastRadiusNarrowed(tc.data); narrowed != (tc.want > 0) {
				t.Errorf("blastRadiusNarrowed = %v but the count is %d; the badge and the empty-state "+
					"wording disagree about whether rows are being hidden", narrowed, tc.want)
			}
		})
	}
}

// blastRadiusDataWithAxis returns base with the field behind param set to value.
// Kept beside the axis table so a new axis that this helper cannot set fails
// loudly in the test above rather than silently testing the neutral data.
func blastRadiusDataWithAxis(base BlastRadiusData, param, value string) BlastRadiusData {
	switch param {
	case "class":
		base.Class = value
	case "attribution":
		base.Attribution = value
	case "field":
		base.Field = value
	case "artist_id":
		base.ArtistID = value
	}
	return base
}

// blastRadiusAxisValue reads one axis's current value back out of a
// BlastRadiusData, used only to assert the write above took effect.
func blastRadiusAxisValue(data BlastRadiusData, param string) string {
	for _, axis := range blastRadiusAxes(data) {
		if axis.Param == param {
			return axis.Value
		}
	}
	return ""
}

// TestBlastRadiusFilterBadgeLabel covers the sentence that carries the
// active-filter count to assistive technology.
//
// This label is the ONLY form in which a screen-reader user learns the damage
// report is narrowed: the badge renders a bare numeral, which announces nothing
// useful on its own, and the trigger's accessible name is the stable action
// ("Open filter panel") rather than the count. So a wrong or unsubstituted
// value here is not cosmetic -- it is the operator being told nothing about how
// much of a data-destruction report is hidden.
//
// The Go side and the pane's JS blastFilterBadgeLabel must produce the SAME
// string from the SAME keys, because the count is written twice: server render
// on first paint, then blastRestoreServerFilterCount after hydration. A
// divergence would make the visible badge and the announced sentence disagree.
// This test pins the Go half; a Playwright spec asserts the computed accessible
// description after hydration, which is the JS half.
func TestBlastRadiusFilterBadgeLabel(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)

	// The singular key is a distinct string, not the plural with "1" in it, so
	// a locale can inflect it. Asserted separately for that reason.
	if got, want := blastRadiusFilterBadgeLabel(ctx, 1), "1 active filter"; got != want {
		t.Errorf("blastRadiusFilterBadgeLabel(1) = %q, want %q", got, want)
	}

	for _, n := range []int{0, 2, 4, 11} {
		got := blastRadiusFilterBadgeLabel(ctx, n)
		want := fmt.Sprintf("%d active filters", n)
		if got != want {
			t.Errorf("blastRadiusFilterBadgeLabel(%d) = %q, want %q", n, got, want)
		}
		// The placeholder is SUBSTITUTED, never emitted. A template returned
		// verbatim would read "{count} active filters" aloud, and the assertion
		// above would already have caught it -- this names the cause when it
		// happens rather than reporting a wording mismatch.
		if strings.Contains(got, "{count}") {
			t.Errorf("blastRadiusFilterBadgeLabel(%d) left the {count} placeholder unsubstituted: %q", n, got)
		}
	}

	// The count really varies the output. Two different counts producing the
	// same sentence would mean the number never reaches the reader, which is
	// the whole defect this label exists to fix.
	if a, b := blastRadiusFilterBadgeLabel(ctx, 2), blastRadiusFilterBadgeLabel(ctx, 3); a == b {
		t.Errorf("counts 2 and 3 produce the same label %q, so the number is not reaching the sentence", a)
	}
}

// TestBlastRadiusFieldControlLabels_MatchTheTableAndAreNeverBareKeys pins the
// field chips' labels to the SAME function the pane's own table column uses.
//
// The pane briefly carried a private blastRadiusFieldLabel whose fallback
// returned the raw field name, justified in its docstring as matching "the name
// the operator sees in the table's Field column". That was factually wrong: the
// table renders historyFieldLabel -> fieldLabel, which HUMANIZES an untranslated
// name. Measured for an untranslated "new_thing": the chip said "new_thing"
// while the table said "New Thing". Two labels for one field, on one screen, is
// the divergence a shared helper exists to prevent, so the copy is gone and both
// surfaces call fieldLabel.
//
// The precondition loop here previously compared fieldLabel's OUTPUT against the
// i18n key, which no fallback can ever return -- so it was false in both
// branches and caught nothing. Proven: adding an untranslated field to
// trackableFields left the whole suite green while the control rendered a bare
// name. It now asks the TRANSLATOR directly, which is the only thing that knows
// whether a key exists.
func TestBlastRadiusFieldControlLabels_MatchTheTableAndAreNeverBareKeys(tt *testing.T) {
	tt.Parallel()
	ctx := testCtx(tt)

	// Every trackable field has a translation today, so every chip renders a
	// real label rather than a humanized fallback. Asked of the translator, not
	// of fieldLabel: fieldLabel never returns the key, so comparing its output
	// against the key can only ever be false.
	for _, field := range artist.TrackableFields() {
		if key := "field." + field; t(ctx, key) == key {
			tt.Errorf("trackable field %q has no %s translation, so its filter chip renders a humanized "+
				"fallback rather than the curated label", field, key)
		}
	}

	// The chip and the table must agree, because they name the same field a few
	// pixels apart.
	for _, field := range artist.TrackableFields() {
		if chip, table := fieldLabel(ctx, field), historyFieldLabel(ctx, field); chip != table {
			tt.Errorf("field %q renders as %q in the filter chip and %q in the table column; one field must "+
				"not have two names on one screen", field, chip, table)
		}
	}

	// And the fallback is humanized, not raw, for a field with no key yet.
	const unknown = "not_a_real_field_xyz"
	if got := fieldLabel(ctx, unknown); got != "Not A Real Field Xyz" {
		tt.Errorf("fieldLabel(%q) = %q, want the humanized form; an untranslated field must not render as a "+
			"bare snake_case name in a control", unknown, got)
	}
}

// TestBlastRadiusChips_OnePerNarrowingAxis pins the chips against the same axis
// table the badge and the empty-state wording read.
//
// The chips are the operator's only way OFF a filter that is not set by a
// control -- artist_id arrives by deep link and has no chip in the flyout -- so
// a missing chip leaves rows hidden with no visible way to clear them.
//
// Mutation proving teeth: dropping the artist_id axis from blastRadiusAxes
// makes the artist_id case render no chip; removing the axis.active() check in
// blastRadiusChips makes the neutral case render four.
func TestBlastRadiusChips_OnePerNarrowingAxis(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)

	neutral := BlastRadiusData{Class: artist.BlastScopeAll, Attribution: artist.BlastScopeAll}
	if chips := blastRadiusChips(ctx, neutral); len(chips) != 0 {
		t.Fatalf("an unfiltered request produced %d chips, want 0; the operator is shown filters that are "+
			"not in force. chips: %+v", len(chips), chips)
	}

	cases := []struct {
		name      string
		data      BlastRadiusData
		wantKeys  []string
		wantLabel string
	}{
		{
			name:      "class",
			data:      BlastRadiusData{Class: artist.BlastClassBlanked, Attribution: artist.BlastScopeAll},
			wantKeys:  []string{"class"},
			wantLabel: "Class: Blanked",
		},
		{
			name:      "attribution",
			data:      BlastRadiusData{Class: artist.BlastScopeAll, Attribution: artist.BlastAttributionUnknown},
			wantKeys:  []string{"attribution"},
			wantLabel: "Attribution: Unknown",
		},
		{
			name:      "field",
			data:      BlastRadiusData{Class: artist.BlastScopeAll, Attribution: artist.BlastScopeAll, Field: "genres"},
			wantKeys:  []string{"field"},
			wantLabel: "Field: Genres",
		},
		{
			// The axis no control sets. It must still get a chip, or a deep
			// link narrows the report with no visible way back.
			name:      "artist_id",
			data:      BlastRadiusData{Class: artist.BlastScopeAll, Attribution: artist.BlastScopeAll, ArtistID: "art-1"},
			wantKeys:  []string{"artist_id"},
			wantLabel: "Artist: art-1",
		},
		{
			name: "all four",
			data: BlastRadiusData{
				Class:       artist.BlastClassReplaced,
				Attribution: artist.BlastAttributionAutomated,
				Field:       "moods",
				ArtistID:    "art-2",
			},
			wantKeys: []string{"class", "attribution", "field", "artist_id"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chips := blastRadiusChips(ctx, tc.data)
			if len(chips) != len(tc.wantKeys) {
				t.Fatalf("got %d chips, want %d: %+v", len(chips), len(tc.wantKeys), chips)
			}
			for i, key := range tc.wantKeys {
				if chips[i].Key != key {
					t.Errorf("chip[%d].Key = %q, want %q; the dismiss button would remove the wrong "+
						"query parameter", i, chips[i].Key, key)
				}
				// Every chip reloads the PANE container, not the bare table:
				// dismissing a filter changes the caveat band too, and a band
				// left behind reports the OLD filter's attribution split over
				// the new rows.
				//
				// This assertion previously demanded "#blast-radius-results"
				// while this very comment said the pane -- so the test PINNED
				// the defect and would have failed the fix. Measured live
				// before the fix: dismissing one chip left two
				// #blast-radius-pane elements, 71 duplicated DOM ids, and a
				// stale band reading "4 of unknown origin" above 21 unfiltered
				// rows.
				if chips[i].TargetSel != "#blast-radius-pane" {
					t.Errorf("chip[%d].TargetSel = %q, want %q; dismissing this chip would leave a stale "+
						"caveat band standing over freshly loaded rows", i, chips[i].TargetSel, "#blast-radius-pane")
				}
				// SelectSel is required WITH it, not optional here. This route
				// has no fragment handler, so without a select the response is
				// a full page and htmx swaps the entire body into the target.
				if chips[i].SelectSel != "#blast-radius-pane" {
					t.Errorf("chip[%d].SelectSel = %q, want %q; without it the dismiss swaps a whole second "+
						"copy of the page into the pane", i, chips[i].SelectSel, "#blast-radius-pane")
				}
			}
			if tc.wantLabel != "" && chips[0].Label != tc.wantLabel {
				t.Errorf("chip label = %q, want %q; the chip and the table's own badges must describe the "+
					"same state in the same words", chips[0].Label, tc.wantLabel)
			}
		})
	}
}

// TestBlastRadiusChipLabels_GoThroughTheTranslator proves the chip labels are
// TRANSLATED rather than assembled from Go string literals.
//
// Replacing all four tf() calls with `"Class: " + ...` concatenation left every
// package green, because two blind spots stack. The chip assertions elsewhere
// compare against the ENGLISH rendering, which hardcoded English satisfies
// perfectly; and the i18n drift guard checks used-but-undefined, not
// defined-but-unused, so keys that stop being consumed raise nothing. (That
// direction is deliberately out of scope repo-wide -- keys are also consumed
// from Go and via dynamic names -- so this is a site-level assertion rather
// than a change to the shared guard.)
//
// The test renders each chip under a translator with a DELIBERATELY DIFFERENT
// value for every chip key. Anything that ignores the translator keeps emitting
// English and fails. That is a property no English-text comparison can express.
func TestBlastRadiusChipLabels_GoThroughTheTranslator(t *testing.T) {
	t.Parallel()

	// Sentinels, not translations: each is unmistakable in a failure message
	// and shares no substring with the English form.
	tr := i18n.NewTranslator("xx", map[string]string{
		"reports.blast_radius.chip_class":       "XLOCALE_CLASS=%s",
		"reports.blast_radius.chip_attribution": "XLOCALE_ATTR=%s",
		"reports.blast_radius.chip_field":       "XLOCALE_FIELD=%s",
		"reports.blast_radius.chip_artist":      "XLOCALE_ARTIST=%s",
		// The nested labels the chip interpolates, so a failure below is
		// attributable to the CHIP key rather than to a missing inner one.
		"reports.blast_radius.class_blanked":       "XVAL_BLANKED",
		"reports.blast_radius.attribution_unknown": "XVAL_UNKNOWN",
		"field.biography":                          "XVAL_BIO",
	})
	ctx := i18n.WithTranslator(context.Background(), tr)

	data := BlastRadiusData{
		Class:       artist.BlastClassBlanked,
		Attribution: artist.BlastAttributionUnknown,
		Field:       "biography",
		ArtistID:    "art-1",
	}
	chips := blastRadiusChips(ctx, data)
	// Precondition: all four axes produced a chip, or an absent chip would let
	// its assertion pass by never running.
	if len(chips) != 4 {
		t.Fatalf("got %d chips, want 4 (one per narrowing axis); the assertions below would not cover them all", len(chips))
	}

	want := map[string]string{
		"class":       "XLOCALE_CLASS=XVAL_BLANKED",
		"attribution": "XLOCALE_ATTR=XVAL_UNKNOWN",
		"field":       "XLOCALE_FIELD=XVAL_BIO",
		"artist_id":   "XLOCALE_ARTIST=art-1",
	}
	for _, c := range chips {
		w, ok := want[c.Key]
		if !ok {
			t.Errorf("unexpected chip key %q; this test has no expectation for it, so a new axis is "+
				"UNVERIFIED rather than passing", c.Key)
			continue
		}
		if c.Label != w {
			t.Errorf("chip %q rendered %q, want %q. The label did not come from the translator: a hardcoded "+
				"Go literal renders identical English under every locale, and no English-text assertion can "+
				"tell the difference.", c.Key, c.Label, w)
		}
	}
}
