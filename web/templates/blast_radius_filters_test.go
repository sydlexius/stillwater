package templates

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
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
