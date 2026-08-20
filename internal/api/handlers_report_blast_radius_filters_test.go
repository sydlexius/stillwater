package api

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
)

// handlers_report_blast_radius_filters_test.go covers the pane's filter
// controls (#3093): the flyout that sets class/attribution/field, the toolbar
// selects that set sort/order, the active-filter badge, and the chips.
//
// The controls drive query parameters blastRadiusFilterFromRequest already
// read. What is new is the UI, so what these tests have to prove is that the UI
// asks for the SAME THING the query parameters mean -- a control that narrows
// differently from the hand-built URL would give the operator a row set that
// answers a question they did not ask, on a surface whose whole purpose is
// telling them what was destroyed.

// seedAPIBlastMixedFixture seeds a fixture with BOTH damage classes, BOTH
// attribution buckets, several artists, and ONE ROW FOR EVERY TRACKABLE FIELD.
//
// The per-field coverage is the load-bearing part. The field control's options
// are generated from artist.TrackableFields(), so a fixture that covers only a
// few fields leaves most of those options matching nothing -- and a test that
// skips a control matching nothing is a conditional skip wearing a different
// hat: it reports green forever while the majority of the control goes
// unverified. Generating the fixture from the same list means a field added to
// history tracking is exercised here the moment it exists, with no edit.
//
// Class and attribution alternate by index so both values of both axes have
// rows, and the split is UNEVEN against the field axis (each field has exactly
// one row) so an assertion cannot be satisfied by a filter that narrowed on the
// wrong axis.
func seedAPIBlastMixedFixture(t *testing.T, r *Router) {
	t.Helper()

	fields := artist.TrackableFields()
	// Precondition: the field list is non-empty and large enough that a
	// single-field filter is genuinely narrowing. With one field there would be
	// no difference between filtered and unfiltered.
	if len(fields) < 2 {
		t.Fatalf("artist.TrackableFields() returned %d fields; the field axis cannot be shown to narrow", len(fields))
	}

	artists := []struct{ id, name string }{
		{"f-art-1", "Alpha Fixture"},
		{"f-art-2", "Bravo Fixture"},
		{"f-art-3", "Charlie Fixture"},
	}

	for i, field := range fields {
		a := artists[i%len(artists)]
		// Even index: blanked (a value replaced by nothing) and scan-sourced,
		// which the report attributes to an automated writer. Odd index:
		// replaced (a value replaced by a different value) and recorded
		// "manual", which is unattributable.
		newValue, source := "", "scan"
		if i%2 == 1 {
			newValue, source = "value the writer put there", "manual"
		}
		seedAPIBlastChange(t, r,
			fmt.Sprintf("f-%02d", i), a.id, a.name,
			field, "the operator's value", newValue, source,
			apiBlastBase.Add(time.Duration(i)*time.Minute))
	}
}

// TestBlastRadiusPane_ReloadTargetContainsEverythingAFilterChanges pins the
// swap contract.
//
// The pane's reload replaces #blast-radius-pane, and /reports/blast-radius has
// no fragment handler, so the response is a full page the swap selects out of.
// What matters is the BOUNDARY: every surface whose content depends on the
// filter has to be INSIDE the swapped container, or the swap leaves a stale
// version of it standing over fresh rows.
//
// The caveat band is the dangerous one. It reports the attribution split and
// the coverage lists for the current request, so a band left behind after a
// filter change makes a wrong statement about how much of the library was
// destroyed -- on the one surface where that is the whole point.
//
// Mutation proving teeth, BOTH directions. Moving the
// <div id="blast-radius-pane"> to open AFTER the caveat band fails this,
// because the band then falls outside the container the reload replaces. So
// does moving the container's </div> UP above the pagination or the results
// table, because the markers then sit after the container closes. The second
// direction is the one an endpoint-by-landmark measurement cannot see, so the
// extent is computed by matching the container's own close tag.
func TestBlastRadiusPane_ReloadTargetContainsEverythingAFilterChanges(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastMixedFixture(t, r)

	// A page size BELOW the fixture's row count, so the pager actually renders.
	// blastRadiusPagination emits nothing at TotalPages <= 1, and a marker that
	// is absent from the whole document would read as "outside the container"
	// for the wrong reason -- the assertion would fail without proving anything
	// about the boundary. The precondition below pins that.
	body := renderBlastPane(t, r, "?page_size=10")

	inside := blastPaneContainerExtent(t, body)
	// The same view of the document the extent was measured in, for the
	// preconditions below: comparing a stripped region against an unstripped
	// document would let a marker that survives only inside a comment satisfy
	// the precondition for a marker the page does not actually render.
	markup := stripHTMLComments(stripHTMLScripts(body))

	// Everything whose content depends on the active filter.
	for _, want := range []struct{ marker, why string }{
		{`class="sw-rep-blast-caveat"`, "the attribution split and coverage lists describe the CURRENT filter; a stale band misstates how much was destroyed"},
		{`id="blast-radius-results"`, "the rows themselves"},
		{`id="blast-radius-filter-trigger"`, "the active-filter badge counts the current narrowing"},
		{`id="blast-radius-pager"`, "the page count and the prev/next hrefs are computed from the ACTIVE filter"},
	} {
		// PRECONDITION, not decoration. Each marker has to be in the document at
		// all before "is it inside the container" means anything: a marker the
		// page never rendered is trivially outside every region, so without this
		// the loop below would report a boundary defect for a missing element.
		if !strings.Contains(markup, want.marker) {
			t.Fatalf("%s is absent from the rendered page entirely, so the boundary assertion below would be "+
				"vacuous; the fixture or the template no longer renders it", want.marker)
		}
		if !strings.Contains(inside, want.marker) {
			t.Errorf("%s is OUTSIDE the reload target #blast-radius-pane, so a filter change leaves it stale: %s",
				want.marker, want.why)
		}
	}

	// And the flyout is outside, so a swap cannot replace the panel while the
	// operator is using it.
	if strings.Contains(inside, `id="blast-radius-filter-flyout"`) {
		t.Error("the filter flyout is inside the reload target; applying a filter would replace the panel " +
			"out from under the operator mid-interaction")
	}

	// The sw:filter-applied listener is bound to document.body, NOT to the
	// container the reload replaces.
	//
	// This is the one property of the reload wiring with no other defense, and
	// rebinding it onto #blast-radius-results survives every other test in the
	// repo. It is load-bearing: the swap replaces the container node, so a
	// listener bound to it dies with the first swap and every filter
	// application after the first is a silent no-op. Verified by node identity
	// across a swap and by a live simulation where the second apply never
	// fired.
	//
	// The operator-facing shape of that regression is the dangerous part: on a
	// 3,234-row damage report they change a filter, the table does not move, and
	// they conclude those really are the matching rows.
	if !strings.Contains(body, `document.body.addEventListener('sw:filter-applied'`) {
		t.Error("the sw:filter-applied listener is not bound to document.body; a listener on the swapped " +
			"container dies with the first swap and every filter application after the first is a no-op")
	}

	// Both capability checks report LOUDLY rather than returning silently.
	//
	// htmx and swFilterFlyout are loaded globally by the layout, so their
	// absence is a broken page rather than a supported configuration. Replacing
	// either console.error with a bare `return` leaves the pane looking normal
	// while every filter action does nothing, and that shape survived the suite
	// until this assertion existed. The repo forbids the silent no-op.
	for _, want := range []struct{ marker, why string }{
		{`console.error('blast-radius: htmx is not loaded`,
			"a missing htmx would silently do nothing on every filter apply"},
		{`console.error('blast-radius: swFilterFlyout is not loaded`,
			"a missing flyout controller would silently leave the panel unhydrated"},
	} {
		if !strings.Contains(body, want.marker) {
			t.Errorf("the pane script lost a loud capability check (%s): %s", want.marker, want.why)
		}
	}

	// The script really does target that container. A container nothing points
	// at is not a swap contract.
	if !strings.Contains(body, `target: '#blast-radius-pane'`) || !strings.Contains(body, `select: '#blast-radius-pane'`) {
		t.Error("blastRadiusReload does not target and select #blast-radius-pane; the wrapper above is not the " +
			"thing the reload actually replaces")
	}
}

// --- helpers ---------------------------------------------------------------

// blastPaneContainerExtent returns the markup between the <div
// id="blast-radius-pane"> open tag and ITS OWN matching close tag.
//
// WHY NOT A LANDMARK ENDPOINT. The obvious measurement -- slice from the open
// tag to the filter flyout, which is deliberately rendered after the container
// -- measures a region LARGER than the container. The pane closes before
// #blast-bulk-bar and #blast-radius-i18n, both of which sit between it and the
// flyout, so that slice includes markup genuinely outside the swap target.
// That asymmetry makes the test one-sided: moving the OPEN tag down past the
// caveat band fails it, but moving the CLOSE tag UP above the pagination or
// the results table does NOT -- the markers still land inside the oversized
// slice, so the test stays green while the swap region no longer covers what a
// filter change invalidates. The operator-facing shape of that regression is a
// stale caveat band standing over fresh rows: a wrong statement about how much
// of the library was destroyed, on the one surface whose whole purpose is
// saying so.
//
// TWO THINGS ARE STRIPPED BEFORE MEASURING, and both are load-bearing:
//
//   - HTML COMMENTS. templ inlines comment prose VERBATIM into the rendered
//     output, so a text matcher over the response matches PROSE before it
//     matches CODE. The templ source above this container carries a long
//     comment that names the container and the flyout; leaving it in makes the
//     measurement depend on how the comment is worded.
//   - SCRIPT BODIES. A <script> block may contain the characters "<div" or
//     "</div" inside a string literal (the cheat-sheet modal script on this
//     page does), which would throw off the depth count.
//
// Neither strip can hide a real defect: the markers this test looks for are
// element attributes, and an element that exists only inside a comment or a
// script string is not rendered markup.
func blastPaneContainerExtent(t *testing.T, body string) string {
	t.Helper()

	markup := stripHTMLComments(stripHTMLScripts(body))

	idAt := strings.Index(markup, `id="blast-radius-pane"`)
	if idAt < 0 {
		t.Fatalf("#blast-radius-pane is absent from the rendered markup; the filter reload has no target " +
			"and every filter would be a no-op")
	}
	start := strings.LastIndex(markup[:idAt], "<div")
	if start < 0 {
		t.Fatalf("#blast-radius-pane's id is not carried by a <div open tag; the container's extent cannot be measured")
	}

	// Walk div open/close tags from the container's own open tag, tracking
	// depth. The close tag that brings depth back to zero is the container's.
	depth := 0
	for i := start; i < len(markup); {
		next := strings.Index(markup[i:], "<div")
		closeNext := strings.Index(markup[i:], "</div")
		switch {
		case next < 0 && closeNext < 0:
			i = len(markup)
		case closeNext >= 0 && (next < 0 || closeNext < next):
			depth--
			if depth == 0 {
				return markup[start : i+closeNext]
			}
			i += closeNext + len("</div")
		default:
			depth++
			i += next + len("<div")
		}
	}

	t.Fatalf("#blast-radius-pane is never closed (depth %d at end of document); the container's extent "+
		"cannot be measured and the swap target is not a well-formed element", depth)
	return ""
}

// stripHTMLScripts removes every <script>...</script> block, so a depth scan
// over div tags cannot be thrown off by markup-shaped text inside a JS string.
func stripHTMLScripts(s string) string {
	return stripBetween(s, "<script", "</script>")
}

// stripHTMLComments removes every <!-- ... --> block. templ renders comment
// prose verbatim, so any text matcher over rendered output has to drop them
// first or it matches the comment describing the markup instead of the markup.
func stripHTMLComments(s string) string {
	return stripBetween(s, "<!--", "-->")
}

// stripBetween removes every open..close span, inclusive. An unterminated open
// truncates the remainder, which is the safe direction: a malformed document
// yields a SHORTER measured region, so a marker that should be inside the
// container reads as missing rather than as present.
func stripBetween(s, open, close string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, open)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		rest := s[i+len(open):]
		j := strings.Index(rest, close)
		if j < 0 {
			return b.String()
		}
		s = rest[j+len(close):]
	}
}

// blastRowIDsInBody extracts the per-row DOM ids the pane rendered, in render
// order. Reading the RENDERED rows rather than the loader's slice is the point:
// a filter that reached the query but not the markup, or a template that
// rendered rows the filter excluded, is exactly the divergence under test.
var blastRowIDRE = regexp.MustCompile(`id="blast-row-([^"]+)"`)

func blastRowIDsInBody(body string) []string {
	var out []string
	for _, m := range blastRowIDRE.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestBlastRadiusPane_ControlsMatchTheAPIAndRoundTrip covers requirements 1
// and 2 together, because they are two assertions about one walk.
//
// For every control the flyout renders: the rows it produces must equal the
// rows the equivalent hand-built URL produces (the UI must not narrow
// differently from the API), and after the reload the control must render
// itself as selected while its siblings on the same axis do not.
//
// The query strings are built from the control's OWN data-filter-key /
// data-filter-value attributes, read out of the rendered markup, rather than
// from a list written down beside the test. A control that shipped with a
// mismatched key or value ("damage" instead of "class", "blank" instead of
// "blanked") therefore diverges from the documented parameter and fails here,
// and a control added later is covered with no edit.
//
// Mutations proving teeth: changing the class chip's value to a literal
// "blank" fails the parity half (the URL the control writes returns the whole
// unfiltered report); hardcoding FilterItemSingle's selected argument to false
// fails the round-trip half.
func TestBlastRadiusPane_ControlsMatchTheAPIAndRoundTrip(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastMixedFixture(t, r)

	base := renderBlastPane(t, r, "")
	unfiltered := blastRowIDsInBody(base)
	// Precondition: the fixture reached the markup, one row per trackable
	// field, so a filter returning everything is distinguishable from one that
	// works.
	if want := len(artist.TrackableFields()); len(unfiltered) != want {
		t.Fatalf("precondition: unfiltered pane rendered %d rows, want %d (one per trackable field)", len(unfiltered), want)
	}
	controls := blastFilterControls(t, base)
	if len(controls) == 0 {
		t.Fatalf("the filter flyout rendered no single-select controls; this test would verify nothing")
	}
	// Precondition: with no filter in force NO control claims to be selected,
	// or the round-trip half cannot tell a real selection from the default.
	for _, c := range controls {
		if c.selected {
			t.Fatalf("control %s=%s renders as selected on an unfiltered request", c.key, c.value)
		}
	}

	for _, c := range controls {
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			query := "?" + c.key + "=" + c.value
			body := renderBlastPane(t, r, query)
			got := blastRowIDsInBody(body)

			// Preconditions: this control genuinely narrows against the
			// fixture. Matching everything, or nothing, makes the equality
			// below vacuous.
			if len(got) == len(unfiltered) {
				t.Fatalf("control %s=%s matched all %d rows; the fixture does not exercise this axis", c.key, c.value, len(got))
			}
			if len(got) == 0 {
				t.Fatalf("control %s=%s matched no rows; any two empty results are equal", c.key, c.value)
			}

			// PARITY: every row the control produced really satisfies that
			// narrowing, checked against the unfiltered report rather than by
			// recomputing the class/attribution rules here (recomputing would
			// let the test and the query agree on the same mistake).
			for _, id := range got {
				if !blastRowMatchesAxis(t, r, id, c.key, c.value) {
					t.Errorf("control %s=%s rendered row %q, which does not match that filter; the UI narrows "+
						"differently from the API", c.key, c.value, id)
				}
			}
			// And the loader recorded the axis the control asked for, which is
			// what makes the chips and the empty-state wording right.
			if v := blastLoadedAxisValue(loadBlastPane(t, r, query), c.key); v != c.value {
				t.Errorf("control %s=%s: the loader recorded %q on that axis", c.key, c.value, v)
			}

			// ROUND-TRIP: the control renders as selected after the reload, and
			// its siblings on the same axis do not (the axis is single-select,
			// so two selected chips is an impossible state on screen).
			var found bool
			for _, after := range blastFilterControls(t, body) {
				if after.key == c.key && after.value == c.value {
					found = true
					if !after.selected {
						t.Errorf("after a reload with %s, the %s=%s control is not selected; the operator "+
							"reopens the panel to an empty filter over a narrowed table", query, c.key, c.value)
					}
					continue
				}
				if after.key == c.key && after.selected {
					t.Errorf("control %s=%s is also selected while %s=%s is in force; the panel shows an "+
						"impossible state", after.key, after.value, c.key, c.value)
				}
			}
			if !found {
				t.Fatalf("control %s=%s vanished from the panel after a reload with %s in force", c.key, c.value, query)
			}
		})
	}
}

// blastControl is one single-select filter chip as the pane rendered it.
type blastControl struct {
	key      string
	value    string
	selected bool
}

// blastFilterControlRE matches a FilterItemSingle button inside the pane. The
// attribute order is fixed by the component, so a positional match is safe and
// keeps the helper from silently matching a different control shape.
//
// aria-pressed is captured POSITIONALLY alongside data-filter-selected, so this
// regex fails to match at all if the attribute is dropped from the markup. That
// is deliberate and it is the only place the server-side attribute is checked:
// the browser spec reads the LIVE DOM, and filter-flyout.js unconditionally
// sets aria-pressed on every single-mode chip at DOMContentLoaded, so it always
// observes a repaired attribute. Deleting aria-pressed from FilterItemSingle
// left that spec passing on both engines while the server sent none.
var blastFilterControlRE = regexp.MustCompile(
	`data-filter-flyout="blast-radius-filter-flyout" data-filter-key="([^"]+)" data-filter-value="([^"]+)" data-filter-mode="single" data-filter-selected="(true|false)" aria-pressed="(true|false)"`)

// blastFilterControls reads every single-select control the pane's flyout
// rendered, out of the markup. Tests build their query strings from what the
// markup SAYS rather than from a list written down beside them, so a control
// whose key or value drifted is caught rather than silently untested.
func blastFilterControls(t *testing.T, body string) []blastControl {
	t.Helper()
	var out []blastControl
	for _, m := range blastFilterControlRE.FindAllStringSubmatch(body, -1) {
		// The two state attributes must agree. They are written from the same
		// bool, so a divergence means one was hand-edited: the visual state and
		// the announced state would then disagree, and a screen-reader user
		// would be told the opposite of what the panel shows.
		if m[3] != m[4] {
			t.Errorf("control %s=%s renders data-filter-selected=%q but aria-pressed=%q; a screen-reader "+
				"user is told the opposite of what the panel displays", m[1], m[2], m[3], m[4])
		}
		out = append(out, blastControl{key: m[1], value: m[2], selected: m[3] == "true"})
	}
	return out
}

// blastLoadedAxisValue reads back what the loader recorded on one axis, so a
// test can check the pane's own view of the request rather than re-parsing the
// URL it built.
func blastLoadedAxisValue(data templates.BlastRadiusData, key string) string {
	switch key {
	case "class":
		return data.Class
	case "attribution":
		return data.Attribution
	case "field":
		return data.Field
	case "artist_id":
		return data.ArtistID
	}
	return ""
}

// blastTriggerBadgeCount reads the number in the Filters trigger badge, or 0
// when no badge rendered.
var blastBadgeRE = regexp.MustCompile(`<span class="sw-filter-trigger-badge">(\d+)</span>`)

func blastTriggerBadgeCount(body string) int {
	m := blastBadgeRE.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	var n int
	fmt.Sscanf(m[1], "%d", &n)
	return n
}

// blastRowMatchesAxis reports whether the row with this change id satisfies the
// named narrowing, read back from the unfiltered report rather than recomputed
// here (recomputing the class/attribution rules in the test would let the test
// and the query agree on the same mistake).
func blastRowMatchesAxis(t *testing.T, r *Router, changeID, key, value string) bool {
	t.Helper()
	for _, row := range loadBlastPane(t, r, "?page_size=500").Rows {
		if row.ID != changeID {
			continue
		}
		switch key {
		case "class":
			return row.Class == value
		case "attribution":
			return row.Attribution == value
		case "field":
			return row.Field == value
		}
		return false
	}
	t.Fatalf("row %q is not in the unfiltered report at all; the filter rendered a row the report does not hold", changeID)
	return false
}

// TestBlastRadiusPane_ActiveFilterBadgeCountsTheNarrowing pins the trigger
// badge against the axes this slice can actually set.
//
// The badge is the operator's only signal that a SHORT TABLE is short on
// purpose. When the table is empty the pane says so in words, but a narrowed
// table that still has rows carries no other explanation, so a badge that
// undercounts leaves the operator reading a filtered report as though it were
// the whole one.
//
// Ordering deliberately gets a case here with an expected count of ZERO even
// though this slice ships no sort control: ?sort= and ?order= are reachable by
// hand today and the loader honors them, so a badge that counted them would
// already be wrong. Pinning it now means the slice that ADDS those controls
// cannot regress it silently.
//
// Mutation proving teeth: dropping the axis.active() check in
// blastRadiusFilterCount makes the unfiltered case report 4 instead of 0.
func TestBlastRadiusPane_ActiveFilterBadgeCountsTheNarrowing(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastMixedFixture(t, r)

	// Precondition: an unfiltered pane shows NO badge, so a nonzero reading
	// below is attributable to the filter rather than to a badge that is
	// always rendered.
	if n := blastTriggerBadgeCount(renderBlastPane(t, r, "")); n != 0 {
		t.Fatalf("precondition: the unfiltered pane shows a filter badge of %d", n)
	}

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"?class=" + artist.BlastClassBlanked, 1},
		{"?attribution=" + artist.BlastAttributionUnknown, 1},
		{"?class=" + artist.BlastClassBlanked + "&attribution=" + artist.BlastAttributionUnknown, 2},
		// Ordering is not narrowing and must never be badged.
		{"?sort=" + artist.BlastSortArtistName + "&order=asc", 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if got := blastTriggerBadgeCount(renderBlastPane(t, r, tc.query)); got != tc.want {
				t.Errorf("filter badge = %d, want %d for %s; the operator is told a different amount of the "+
					"report is hidden than actually is", got, tc.want, tc.query)
			}
		})
	}
}

// TestBlastRadiusPane_FilteredEmptyStateQuotesALibraryWideCount is the
// operator-facing half of the TotalUnfiltered change.
//
// The sentence reads "No rows match the current filter. The report still
// records N change(s) overall -- clear the filter to see them." Its whole job is
// to tell an operator that the report holds damage BEHIND the filter that just
// matched nothing. Quoting a filter-scoped count made N structurally zero on
// every view that renders it: the branch fires only when the narrowing matched
// nothing, and a count scoped to that narrowing is therefore zero.
//
// The dangerous shape is not the zero on its own, it is the CONTRADICTION. The
// caveat band sits a few pixels above this sentence and reports the real
// bucket counts, so the pane asserted "0 overall" directly beneath "N of
// unknown origin" -- and on a data-destruction recovery surface the reassuring
// half is the one an operator acts on.
//
// Asserted against the RENDERED page rather than the loader, because the defect
// was a template quoting the wrong field of a correct struct.
//
// Mutation proving teeth: reverting the template to data.Counts.Total makes
// every case below render "0 change(s)".
func TestBlastRadiusPane_FilteredEmptyStateQuotesALibraryWideCount(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastMixedFixture(t, r)

	// The honest library-wide number, taken from an unfiltered load.
	base := loadBlastPane(t, r, "")
	want := base.Counts.TotalUnfiltered
	if want == 0 {
		t.Fatalf("precondition: the fixture reports TotalUnfiltered=0, so there is nothing for a filter to " +
			"hide and this test cannot distinguish an honest count from a filter-scoped one")
	}

	// Every narrowing here is verified to empty the table against this fixture,
	// which seeds one row per trackable field alternating blanked/scan and
	// replaced/manual. They are NOT skipped when they fail to empty it: a
	// conditional skip reports green forever while verifying nothing, and the
	// precondition below turns a fixture change into a loud failure instead.
	//
	// The first case is the dangerous one, and the reason this test exists in
	// this shape. It empties the table while the caveat band directly above
	// still reports a non-zero automated bucket, so the pane is asserting "the
	// report records N overall" a few pixels beneath "N of automated origin" --
	// the two statements have to agree, and before this fix the sentence said 0.
	for _, query := range []string{
		"?class=" + artist.BlastClassBlanked + "&attribution=" + artist.BlastAttributionUnknown,
		"?class=" + artist.BlastClassReplaced + "&attribution=" + artist.BlastAttributionAutomated,
		"?artist_id=no-such-artist",
		"?field=biography&class=" + artist.BlastClassReplaced,
	} {
		t.Run(query, func(t *testing.T) {
			// Precondition: this narrowing really empties the table, or the
			// filtered empty state never renders and the assertion is vacuous.
			if got := loadBlastPane(t, r, query); len(got.Rows) != 0 {
				t.Fatalf("narrowing %q matched %d rows against this fixture, so it never reaches the "+
					"filtered empty state and this case verifies nothing. The fixture changed; pick a "+
					"narrowing that empties the table.", query, len(got.Rows))
			}

			body := renderBlastPane(t, r, query)
			if !strings.Contains(body, "No rows match the current filter") {
				t.Fatalf("narrowing %q did not render the filtered empty state", query)
			}
			// The sentence must quote the library-wide count, never zero.
			wantPhrase := fmt.Sprintf("records %d change(s) overall", want)
			if !strings.Contains(body, wantPhrase) {
				t.Errorf("the filtered empty state does not quote the library-wide count. Want %q. A "+
					"filter-scoped count renders as 0 here and contradicts the caveat band directly above, "+
					"which reports the real damage.", wantPhrase)
			}
			if strings.Contains(body, "records 0 change(s) overall") && want != 0 {
				t.Errorf("the filtered empty state claims the report records 0 change(s) overall, over a "+
					"library holding %d recorded changes", want)
			}
		})
	}
}

// TestBlastRadiusPane_FilterSectionsAreLabelledForTheirAxis pins each flyout
// section's heading to the axis its chips actually carry.
//
// Nothing read these headings before: the parity tests match on
// data-filter-key/value, and the a11y spec reads chip labels and aria-pressed.
// So swapping the Attribution section's legend for the Class key left every
// package green while the panel rendered TWO sections both titled "Class" --
// one holding Blanked/Replaced, the other Automated/Unknown.
//
// The operator consequence is specific rather than cosmetic: someone looking
// for the attribution axis finds no heading naming it and concludes the report
// cannot filter by attribution. On this pane that is the axis that matters
// most, because it is the one that separates "an automated writer destroyed
// this" from "we cannot tell who did".
//
// The assertion pairs each section's LEGEND with the data-filter-key of the
// chips inside it, so a heading can only be right by naming its own axis.
//
// Mutation proving teeth: rendering the class heading for the attribution
// section fails this and nothing else in the repo.
func TestBlastRadiusPane_FilterSectionsAreLabelledForTheirAxis(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastMixedFixture(t, r)
	body := renderBlastPane(t, r, "")

	// Each <fieldset> is one section: its <legend> is the heading and the
	// data-filter-key of the chips inside it names the axis.
	sectionRE := regexp.MustCompile(`(?s)<fieldset class="sw-filter-section">.*?<legend[^>]*>([^<]*)</legend>(.*?)</fieldset>`)
	matches := sectionRE.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatalf("no filter sections rendered; this test would verify nothing")
	}

	// axis -> the heading it must carry, keyed by the same i18n strings the
	// table's own column headers use, so a chip section and the column it
	// filters can never describe the same axis differently.
	want := map[string]string{
		"class":       "Class",
		"attribution": "Attribution",
		"field":       "Field",
	}

	seen := map[string]bool{}
	keyRE := regexp.MustCompile(`data-filter-key="([^"]+)"`)
	for _, m := range matches {
		legend := strings.TrimSpace(m[1])
		keys := map[string]bool{}
		for _, k := range keyRE.FindAllStringSubmatch(m[2], -1) {
			keys[k[1]] = true
		}
		if len(keys) == 0 {
			continue // a section with no chips names no axis; nothing to check.
		}
		if len(keys) > 1 {
			t.Errorf("a single filter section holds chips for more than one axis (%v); its heading %q cannot "+
				"be correct for all of them", keys, legend)
			continue
		}
		var axis string
		for k := range keys {
			axis = k
		}
		seen[axis] = true
		if w, ok := want[axis]; ok && legend != w {
			t.Errorf("the %s section is headed %q, want %q. An operator scanning the panel for that axis "+
				"finds no heading naming it and concludes the report cannot filter on it.", axis, legend, w)
		}
	}

	// Precondition: the axes this slice ships were actually found and checked.
	for _, axis := range []string{"class", "attribution"} {
		if !seen[axis] {
			t.Errorf("no filter section carrying %s chips was found, so its heading went unchecked", axis)
		}
	}
}
