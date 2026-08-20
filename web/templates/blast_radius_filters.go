package templates

import (
	"github.com/sydlexius/stillwater/internal/artist"
)

// blast_radius_filters.go holds the narrowing-axis model behind the
// blast-radius pane's filter controls (issue #3093).
//
// WHY AN AXIS TABLE AND NOT A CHAIN OF ifs
//
// The pane's empty state has to tell "this library has no damage" apart from
// "no damage matches what you asked for". Those are opposite answers on a
// recovery surface, and getting them backwards renders a library-wide
// all-clear over a library that still holds destroyed values -- an operator who
// reads that stops looking. blastRadiusNarrowed is what decides which sentence
// renders, so EVERY axis that can hide a row has to be accounted for in it.
//
// Written as a chain of `data.Field != "" || ...` it is one forgotten clause
// away from the false all-clear, and nothing fails when the clause is missing.
// Written as a TABLE, the same table is the single source for every reading of
// "what is being hidden" -- in this slice the empty-state decision and the
// active-filter count, and later the dismissable chips, which arrive with the
// slice that renders them -- and a test can walk it against the fields of
// artist.BlastRadiusFilter and fail when a newly added narrowing field has no
// axis. That test is the point of this file's shape.

// blastRadiusAxis is one axis on which the report can be narrowed: a query
// parameter, the artist.BlastRadiusFilter field it feeds, the value currently
// requested, and the value that means "not narrowing".
//
// Neutral is per-axis rather than a single sentinel because the two shapes
// genuinely differ. Class and Attribution default to the literal "all"
// (artist.BlastScopeAll) since their scope values are all/blanked/replaced and
// all/automated/unknown, so an emptiness check alone would report EVERY request
// as narrowed and the library-wide sentence could never render. Field and
// ArtistID are genuinely empty when unset. Both shapes are carried here rather
// than normalized at the loader, because "all" is the value the query layer
// wants.
type blastRadiusAxis struct {
	// Param is the query-string key the UI writes and
	// blastRadiusFilterFromRequest reads.
	Param string
	// StructField is the name of the artist.BlastRadiusFilter field this axis
	// mirrors. It exists for the drift guard in
	// TestBlastRadiusAxes_CoverEveryNarrowingFilterField, which walks the
	// filter struct by reflection and fails when a narrowing field has no
	// axis here. It is never rendered.
	StructField string
	// Value is what the current request asked for on this axis.
	Value string
	// Neutral is the value on this axis that narrows nothing.
	Neutral string
}

// active reports whether this axis is currently hiding rows.
//
// The empty check is redundant for every axis whose Neutral is itself "", and
// deleting it leaves the suite green today. KEEP IT: it is what makes this
// correct for an axis whose neutral value is a non-empty sentinel (as Class and
// Attribution's "all" already is), where an unset-and-therefore-empty Value
// must still read as "not narrowing". Removing it would work until the first
// such axis arrived and then fail as a false all-clear.
func (a blastRadiusAxis) active() bool {
	return a.Value != "" && a.Value != a.Neutral
}

// blastRadiusAxes returns every narrowing axis with the current request's value
// bound to it.
//
// Sort and Order are deliberately ABSENT. They reorder the same rows; they
// never remove one. Counting them as narrowing would make a plain re-sort of an
// undamaged library render "No rows match the current filter. The report still
// records 0 change(s) overall", which is a stranger falsehood than the one this
// function exists to prevent. Limit and Offset are absent for the same reason:
// paging is not narrowing, and the pager already reports the page it is on.
func blastRadiusAxes(data BlastRadiusData) []blastRadiusAxis {
	return []blastRadiusAxis{
		{Param: "class", StructField: "Class", Value: data.Class, Neutral: artist.BlastScopeAll},
		{Param: "attribution", StructField: "Attribution", Value: data.Attribution, Neutral: artist.BlastScopeAll},
		{Param: "field", StructField: "Field", Value: data.Field, Neutral: ""},
		{Param: "artist_id", StructField: "ArtistID", Value: data.ArtistID, Neutral: ""},
	}
}

// blastRadiusNarrowed reports whether any filter is restricting the pane's view.
//
// It exists so the empty state can tell "this library has no damage" apart from
// "no damage matches what you asked for". See the file header for why it walks
// a table rather than testing each field inline.
func blastRadiusNarrowed(data BlastRadiusData) bool {
	for _, axis := range blastRadiusAxes(data) {
		if axis.active() {
			return true
		}
	}
	return false
}

// blastRadiusFilterCount returns how many axes are narrowing, for the Filters
// trigger badge.
//
// It counts NARROWING axes only, so the badge answers the same question the
// empty state does: how much of the report is being hidden right now. The sort
// and order selects sit outside the flyout and are not counted -- badging a
// re-sort as an active filter would tell an operator that rows are being hidden
// when none are.
func blastRadiusFilterCount(data BlastRadiusData) int {
	n := 0
	for _, axis := range blastRadiusAxes(data) {
		if axis.active() {
			n++
		}
	}
	return n
}
