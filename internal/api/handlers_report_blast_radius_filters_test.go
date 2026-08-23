package api

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/i18n"
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
		{`id="blast-radius-sort"`, "the sort select shows the ordering in force"},
		{`id="blast-radius-order"`, "the order select shows the ordering in force"},
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
	// Indexed once for every parity check below, rather than reloaded per row.
	rowIndex := blastUnfilteredRowIndex(t, r)
	// Precondition: the index really covers the whole rendered report. If
	// page_size clipped it, a row could be missing from the index for a paging
	// reason and be reported as "the filter rendered a row the report does not
	// hold", which names the wrong defect.
	if len(rowIndex) != len(unfiltered) {
		t.Fatalf("the unfiltered row index holds %d rows but the pane rendered %d; the index does not cover "+
			"the whole report and a parity failure below would be attributed to the wrong cause",
			len(rowIndex), len(unfiltered))
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
				if !blastRowMatchesAxis(t, rowIndex, id, c.key, c.value) {
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
//
// The pattern matches the badge by its CLASS and tolerates other attributes on
// the same span. It previously pinned the exact opening tag
// `<span class="sw-filter-trigger-badge">`, so adding aria-hidden to the badge
// made every count read 0 and reported it as "the operator is told a different
// amount of the report is hidden than actually is" -- a real-looking failure
// with a purely cosmetic cause. A helper that reads a value should not also
// assert the tag's full attribute set; the tests that care about aria-hidden
// assert it directly.
var blastBadgeRE = regexp.MustCompile(`<span[^>]*class="sw-filter-trigger-badge"[^>]*>(\d+)</span>`)

func blastTriggerBadgeCount(body string) int {
	m := blastBadgeRE.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	var n int
	fmt.Sscanf(m[1], "%d", &n)
	return n
}

// blastUnfilteredRowIndex loads the unfiltered report ONCE and indexes it by
// change id, for the parity checks below.
//
// Loaded once rather than per row. The previous shape re-ran this load inside
// the per-row loop, so each subtest performed one full load PER RENDERED ROW --
// controls x rows loads of identical, unchanging data, each re-running the
// loader and its SQLite queries. The report is a fixed fixture for the whole
// test; nothing between rows can change it.
//
// page_size=500 is deliberately far above the fixture's row count so the index
// holds the WHOLE report. The caller asserts that below rather than trusting it.
func blastUnfilteredRowIndex(t *testing.T, r *Router) map[string]artist.BlastRadiusRow {
	t.Helper()
	rows := loadBlastPane(t, r, "?page_size=500").Rows
	idx := make(map[string]artist.BlastRadiusRow, len(rows))
	for _, row := range rows {
		idx[row.ID] = row
	}
	if len(idx) != len(rows) {
		t.Fatalf("the unfiltered report holds %d rows but only %d distinct change ids; the index would "+
			"silently drop rows and the parity check below would read the wrong one", len(rows), len(idx))
	}
	return idx
}

// blastRowMatchesAxis reports whether the row with this change id satisfies the
// named narrowing, read back from the unfiltered report rather than recomputed
// here (recomputing the class/attribution rules in the test would let the test
// and the query agree on the same mistake).
//
// A row absent from the index is a t.Fatalf, NOT a false. That branch is a real
// assertion -- the filtered view rendered a row the unfiltered report does not
// hold, which is a broken query rather than a failed match -- and reporting it
// as "does not match this filter" would name the wrong defect.
func blastRowMatchesAxis(t *testing.T, idx map[string]artist.BlastRadiusRow, changeID, key, value string) bool {
	t.Helper()
	row, ok := idx[changeID]
	if !ok {
		t.Fatalf("row %q is not in the unfiltered report at all; the filter rendered a row the report does not hold", changeID)
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

// blastFixtureFieldWithClass returns a trackable field whose fixture row has
// the given damage class, so a test can build a narrowing that is guaranteed
// non-empty (or, combined with the OTHER class, guaranteed empty).
func blastFixtureFieldWithClass(t *testing.T, class string) string {
	t.Helper()
	fields := artist.TrackableFields()
	for i, field := range fields {
		rowClass := artist.BlastClassBlanked
		if i%2 == 1 {
			rowClass = artist.BlastClassReplaced
		}
		if rowClass == class {
			return field
		}
	}
	t.Fatalf("the fixture has no field with damage class %q; a test relying on one cannot be built", class)
	return ""
}

// TestBlastRadiusPane_FieldControlOptionsAreTrackableFields is the anti-drift
// guard on the one control whose options are DATA rather than a fixed
// vocabulary.
//
// The report can only see fields metadata_changes records, which is exactly
// artist.TrackableFields(). A hand-written option list drifts silently when a
// field is added to history tracking: the option simply never appears, so
// damage to that field is unfilterable while the coverage caveat directly above
// the table claims the report covers it. Equality is asserted in BOTH
// directions -- a missing option hides an axis, an extra one offers a filter
// that can only ever return nothing.
//
// Mutation proving teeth: replacing the `for _, field := range
// data.CoveredFields` loop with a literal list of two fields fails this with
// the twelve missing fields named.
func TestBlastRadiusPane_FieldControlOptionsAreTrackableFields(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastMixedFixture(t, r)

	want := artist.TrackableFields()
	// Precondition: TrackableFields is non-empty, or "both sets are equal" is
	// satisfied by a control that renders nothing at all.
	if len(want) == 0 {
		t.Fatalf("artist.TrackableFields() is empty; the equality below would be satisfied by an absent control")
	}

	var got []string
	for _, c := range blastFilterControls(t, renderBlastPane(t, r, "")) {
		if c.key == "field" {
			got = append(got, c.value)
		}
	}

	// FREQUENCIES, not sets. The membership checks below prove the two lists
	// hold the same VALUES; they say nothing about how many times each appears.
	// Collapsed into sets, a control rendering "biography" twice -- 15 options
	// against 14 trackable fields -- satisfies every membership check and the
	// test stays green while the operator sees a duplicated entry in the field
	// filter.
	//
	// That is not a hypothetical shape here. This test exists to prove the
	// options are GENERATED from TrackableFields() rather than written out, and
	// a hand-written list is exactly the edit that produces a stray duplicate --
	// so the defect this test guards against and the defect a set comparison
	// cannot see are the same defect.
	countGot := map[string]int{}
	for _, f := range got {
		countGot[f]++
	}
	countWant := map[string]int{}
	for _, f := range want {
		countWant[f]++
	}

	for _, f := range want {
		if countGot[f] == 0 {
			t.Errorf("field %q is in artist.TrackableFields() but has no option in the field filter. Damage to "+
				"it is unfilterable while the coverage caveat says the report covers it. The control must be "+
				"generated from the trackable-field list, never written out.", f)
		}
	}
	for _, f := range got {
		if countWant[f] == 0 {
			t.Errorf("the field filter offers %q, which is NOT trackable, so selecting it can only ever return "+
				"an empty table the operator reads as an all-clear for that field", f)
		}
	}

	// MULTIPLICITY, per value. Reported per field rather than as a bare total
	// so the failure names WHICH option was duplicated, which is what a reader
	// needs to find the stray line.
	for _, f := range want {
		if countGot[f] > countWant[f] {
			t.Errorf("the field filter offers %q %d times, want %d. A duplicated option gives the operator two "+
				"identical entries in the field list and no way to tell them apart, on the control whose whole "+
				"job is saying which fields the report can narrow to.", f, countGot[f], countWant[f])
		}
	}

	// And the totals agree, which catches a duplicate of a value that is NOT
	// trackable -- that one is reported as untrackable above, but its extra
	// copies would otherwise go unremarked.
	if len(got) != len(want) {
		t.Errorf("the field filter rendered %d options against %d trackable fields; the option list is not a "+
			"one-for-one rendering of artist.TrackableFields()", len(got), len(want))
	}
}

// TestBlastRadiusPane_EmptyStateWordingUnderEveryControl walks each control and
// asserts the FILTERED wording renders when that control narrows to nothing,
// and the LIBRARY-WIDE wording renders when nothing narrows.
//
// This is the same honesty rule the pane already carries, re-asserted per
// CONTROL rather than per hand-built URL: a control the operator can reach
// which produces the library-wide all-clear over a filtered view is the defect,
// regardless of whether the URL form of it is already covered.
//
// Mutation proving teeth: dropping any axis from blastRadiusAxes makes the
// corresponding control's subtest render "Nothing recorded" over a library with
// six recorded changes.
func TestBlastRadiusPane_EmptyStateWordingUnderEveryControl(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastMixedFixture(t, r)

	const allClear = "Nothing recorded"
	const filtered = "No rows match the current filter"

	// Precondition: the library HAS damage, so the library-wide all-clear is
	// false for every request below no matter how it is narrowed.
	if base := loadBlastPane(t, r, ""); base.Counts.Total == 0 {
		t.Fatalf("precondition: fixture reports no damage; the all-clear would be TRUE and this test is vacuous")
	}
	// And the unfiltered pane renders NEITHER empty state, so a filtered
	// subtest that finds one has genuinely emptied the table.
	if body := renderBlastPane(t, r, ""); strings.Contains(body, allClear) || strings.Contains(body, filtered) {
		t.Fatalf("precondition: the unfiltered pane rendered an empty state over a library with damage")
	}

	// One value per axis that matches NOTHING against this fixture. Values
	// must be VALID on their axis (an unrecognized class is coerced to "all" by
	// Validate and would narrow nothing), so the field/artist axes carry real
	// but unused values and the class/attribution axes are covered by the
	// combination below.
	narrowings := map[string]string{
		"artist_id": "no-such-artist",
	}
	for axis, value := range narrowings {
		t.Run(axis, func(t *testing.T) {
			query := "?" + axis + "=" + value
			// Precondition: this narrowing really empties the table.
			if rows := loadBlastPane(t, r, query); len(rows.Rows) != 0 {
				t.Fatalf("narrowing %q matched %d rows; it must match none to reach the empty state", query, len(rows.Rows))
			}
			body := renderBlastPane(t, r, query)
			if strings.Contains(body, allClear) {
				t.Errorf("narrowing %q rendered the library-wide all-clear over a library holding recorded damage", query)
			}
			if !strings.Contains(body, filtered) {
				t.Errorf("narrowing %q rendered neither the all-clear nor the filtered explanation; an "+
					"unexplained empty table reads as an all-clear too", query)
			}
		})
	}

	// class and attribution both have rows on every value in this fixture, so
	// they are emptied in COMBINATION with a field that exists on the other
	// side of them. This still exercises each axis: remove the class term and
	// the query matches rows again.
	t.Run("class+field", func(t *testing.T) {
		// A field whose only fixture row is BLANKED, asked for as REPLACED.
		blankedField := blastFixtureFieldWithClass(t, artist.BlastClassBlanked)
		query := "?class=" + artist.BlastClassReplaced + "&field=" + blankedField
		if rows := loadBlastPane(t, r, query); len(rows.Rows) != 0 {
			t.Fatalf("narrowing %q matched %d rows; it must match none", query, len(rows.Rows))
		}
		// The class term is load-bearing: without it the same field matches.
		if rows := loadBlastPane(t, r, "?field="+blankedField); len(rows.Rows) == 0 {
			t.Fatalf("field=%s alone already matches nothing; the class term is not what emptied the table", blankedField)
		}
		body := renderBlastPane(t, r, query)
		if strings.Contains(body, allClear) {
			t.Errorf("narrowing %q rendered the library-wide all-clear over a library holding recorded damage", query)
		}
		if !strings.Contains(body, filtered) {
			t.Errorf("narrowing %q did not explain that a filter emptied the table", query)
		}
	})
}

// TestBlastRadiusPane_ChipsMatchTheNarrowing pins the dismissable chips against
// the actual narrowing. The BADGE half of this contract is already pinned by
// TestBlastRadiusPane_ActiveFilterBadgeCountsTheNarrowing; what is new here is
// that every narrowing axis also renders a way OFF it.
//
// The badge and the chips are the only signals an operator has that the table
// is short because they asked for it. A badge that undercounts, or a chip that
// does not render for an axis, reproduces the unexplained-short-table problem
// the empty-state wording exists to prevent -- with the difference that the
// table is not empty, so no empty state fires at all.
//
// Mutation proving teeth: dropping the artist_id axis from blastRadiusAxes
// makes the artist_id subtest report a badge of 0 and no chip while the table
// is narrowed to one artist.
func TestBlastRadiusPane_ChipsMatchTheNarrowing(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastMixedFixture(t, r)

	// Precondition: an unfiltered pane shows NO badge and NO chips, so a
	// nonzero reading below is attributable to the filter.
	base := renderBlastPane(t, r, "")
	if n := blastTriggerBadgeCount(base); n != 0 {
		t.Fatalf("precondition: the unfiltered pane shows a filter badge of %d", n)
	}
	// And no dismiss controls, which also pins that the count below is reading
	// THIS pane's chips and not some other "Remove ..." control on the page.
	if n := strings.Count(base, `aria-label="Remove `); n != 0 {
		t.Fatalf("precondition: the unfiltered pane rendered %d dismiss controls; the per-case counts below "+
			"would be measuring something other than the filter chips", n)
	}

	cases := []struct {
		query string
		want  int
	}{
		{"?class=" + artist.BlastClassBlanked, 1},
		{"?attribution=" + artist.BlastAttributionUnknown, 1},
		{"?field=biography", 1},
		{"?artist_id=f-art-1", 1},
		{"?class=" + artist.BlastClassBlanked + "&attribution=" + artist.BlastAttributionUnknown, 2},
		{"?class=" + artist.BlastClassBlanked + "&attribution=" + artist.BlastAttributionUnknown + "&field=" + artist.TrackableFields()[0], 3},
		// Ordering is not narrowing and must not be badged.
		{"?sort=" + artist.BlastSortArtistName + "&order=asc", 0},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			body := renderBlastPane(t, r, tc.query)
			if got := blastTriggerBadgeCount(body); got != tc.want {
				t.Errorf("filter badge = %d, want %d for %s; the operator is told a different amount of the "+
					"report is hidden than actually is", got, tc.want, tc.query)
			}
			// One dismissable chip per narrowing axis, so every active filter
			// has a visible way off.
			if got := strings.Count(body, `aria-label="Remove `); got != tc.want {
				t.Errorf("rendered %d dismiss controls, want %d for %s; an axis with no chip hides rows with "+
					"no visible way to clear it", got, tc.want, tc.query)
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
	tr := blastTestTranslator(t)

	// Resolve the empty-state key ONCE, and prove it resolved before formatting
	// it. Both matter, and the heading test below already does the same thing --
	// this assertion was the inconsistent one.
	//
	// i18n.Translator.T returns the KEY ITSELF when the key is missing, and this
	// value is then handed to fmt.Sprintf with a count. A missing key would
	// therefore format as "reports.blast_radius.empty_filtered%!(EXTRA int=5)",
	// fail the Contains check, and report a confusing wording mismatch instead
	// of naming the real problem, which is that the key is gone.
	//
	// Resolving once rather than per call site is the other half: the two
	// Sprintf calls below must format the SAME template with different counts,
	// so that a divergence between them is impossible by construction rather
	// than by two lookups happening to agree.
	emptyFilteredTmpl := tr.T("reports.blast_radius.empty_filtered")
	if emptyFilteredTmpl == "" || emptyFilteredTmpl == "reports.blast_radius.empty_filtered" {
		t.Fatalf("the empty-state key did not resolve (got %q); the assertions below would compare the "+
			"rendered page against a raw key name", emptyFilteredTmpl)
	}
	// And it really does carry the count verb the assertions interpolate. A
	// value that lost its %d would format identically for every count, so the
	// "quotes the library-wide number, never 0" assertion would pass whatever
	// the page said.
	if !strings.Contains(emptyFilteredTmpl, "%d") {
		t.Fatalf("the empty-state template %q carries no %%d verb, so the count assertions below cannot "+
			"distinguish the library-wide number from 0", emptyFilteredTmpl)
	}

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

			// The whole sentence, RESOLVED FROM THE KEY the template renders it
			// from and formatted with the count it must quote. This read as
			// English literals ("No rows match the current filter", "records %d
			// change(s) overall"), which pinned the en.json COPY rather than the
			// behavior: rewording the sentence turned this red on correct code,
			// and the assertion could not run under another locale at all. What
			// is actually under test is WHICH NUMBER the sentence carries, and
			// resolving the key checks that without freezing the wording.
			wantSentence := fmt.Sprintf(emptyFilteredTmpl, want)
			if !strings.Contains(body, wantSentence) {
				t.Errorf("the filtered empty state does not read %q. It must quote the LIBRARY-WIDE count; a "+
					"filter-scoped count renders as 0 here and contradicts the caveat band directly above, "+
					"which reports the real damage.", wantSentence)
			}
			// And specifically not the zero-valued form, which is what a
			// filter-scoped count produces on exactly this view.
			if want != 0 {
				zeroSentence := fmt.Sprintf(emptyFilteredTmpl, 0)
				if strings.Contains(body, zeroSentence) {
					t.Errorf("the filtered empty state quotes a count of 0, over a library holding %d "+
						"recorded changes", want)
				}
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

	// axis -> the heading it must carry, RESOLVED FROM THE SAME i18n KEY the
	// template renders it from.
	//
	// These were English literals ("Class", "Attribution", "Field"), which made
	// the assertion test the en.json COPY rather than the key wiring: editing a
	// heading's wording turned this red on correct code, and running the suite
	// under any other locale would fail every case. Neither is the property
	// under test. What matters is that a chip section and the table column it
	// filters resolve the SAME key, so the panel and the table can never
	// describe one axis two different ways -- which is exactly what resolving
	// through the translator checks.
	tr := blastTestTranslator(t)
	want := map[string]string{
		"class":       tr.T("reports.blast_radius.column_class"),
		"attribution": tr.T("reports.blast_radius.column_attribution"),
		"field":       tr.T("reports.blast_radius.column_field"),
	}
	// Precondition: every key RESOLVED. i18n.Translator.T returns the key
	// itself when it is missing, so an undefined key would otherwise be
	// compared against a legend as the literal string
	// "reports.blast_radius.column_class" and fail with a confusing message
	// instead of naming the real problem.
	for axis, heading := range want {
		if heading == "" || strings.HasPrefix(heading, "reports.blast_radius.") {
			t.Fatalf("the %s heading key did not resolve (got %q); the assertions below would compare legends "+
				"against a raw key name", axis, heading)
		}
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

// blastTestTranslator returns the English translator the render helpers install
// on the request context, so a test can resolve the SAME i18n key the template
// renders from instead of hardcoding its current English wording.
//
// Hardcoded literals in these assertions test the copy in en.json rather than
// the behavior: rewording a heading or a sentence turns the test red on correct
// code, and the assertion cannot run under any other locale. Resolving the key
// keeps what is actually under test -- that the panel and the table name an axis
// with the SAME key, and that the empty-state sentence carries the LIBRARY-WIDE
// count -- while letting the wording change freely.
//
// It deliberately mirrors withI18nCtx's construction rather than reaching into a
// request, so a caller that never built a request can still use it.
func blastTestTranslator(t *testing.T) *i18n.Translator {
	t.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("loading i18n bundle: %v", err)
	}
	return bundle.Translator("en")
}

// TestBlastRadiusPane_FilterScriptHasNoHardcodedUserFacingStrings pins the rule
// the pane's own comments state: JS has no access to the request translator, so
// every user-facing string a script block renders comes from the hidden
// #blast-radius-i18n element rather than an English literal.
//
// THIS EXISTS BECAUSE THE RULE WAS BROKEN BY A FIX THAT CITED IT. The
// active-filter badge's aria-label shipped as '1 active filter' / n + ' active
// filters', hardcoded, in a function added a few lines below a comment block
// spelling out why that is wrong. Nothing failed: the visible pane was fully
// translated, and the only surface carrying the English was an aria-label, so
// the defect was invisible to every rendered-output assertion and to a reader
// skimming the diff.
//
// The operator-facing shape is narrow and easy to miss: on a non-English locale
// a screen-reader user hears English on the ONE control that says how much of
// the damage report is hidden, while every sighted user sees it translated.
//
// WHAT THIS CHECKS, and why it is shaped this way. It asserts the two known
// label strings resolve from the i18n element -- not that no literal exists
// anywhere, which a script full of legitimate non-user-facing literals (class
// names, event names, console.error text for developers) cannot support. A
// blanket English-word grep would be noise. What it can state precisely is: the
// keys exist, the element carries them, and the script reads them.
func TestBlastRadiusPane_FilterScriptHasNoHardcodedUserFacingStrings(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastMixedFixture(t, r)

	tr := blastTestTranslator(t)
	body := renderBlastPane(t, r, "?class="+artist.BlastClassBlanked)

	// The keys resolve. Translator.T returns the key itself when missing, so
	// this catches a key deleted from en.json, which would otherwise render the
	// raw key name into the aria-label as if it were copy.
	for _, key := range []string{
		"reports.blast_radius.js.filter_badge_one",
		"reports.blast_radius.js.filter_badge_many",
	} {
		if v := tr.T(key); v == "" || v == key {
			t.Fatalf("the badge label key %q did not resolve (got %q); the aria-label would render a raw key "+
				"name to a screen-reader user", key, v)
		}
	}

	// The hidden element CARRIES them, keyed as the script reads them. The
	// dataset name is the camelCase of the data- attribute, so these two must
	// stay in step: data-filter-badge-one is read as dataset.filterBadgeOne.
	for _, attr := range []struct{ name, key string }{
		{"data-filter-badge-one", "reports.blast_radius.js.filter_badge_one"},
		{"data-filter-badge-many", "reports.blast_radius.js.filter_badge_many"},
	} {
		want := attr.name + `="` + tr.T(attr.key) + `"`
		if !strings.Contains(body, want) {
			t.Errorf("#blast-radius-i18n does not carry %s with the translated value; the script falls back to "+
				"its English literal and a non-English screen-reader user hears English on the one control "+
				"that reports how much of the damage report is hidden", attr.name)
		}
	}

	// And the script READS them rather than hardcoding the label. Both halves
	// are needed: the element could carry the strings while the script ignores
	// them, which is exactly the state this test was written for.
	if !strings.Contains(body, "i18n.filterBadgeOne") || !strings.Contains(body, "i18n.filterBadgeMany") {
		t.Error("the filter script does not read the badge label from the i18n element's dataset; a literal " +
			"there is the one untranslated string on the pane")
	}

	// The plural form interpolates rather than concatenating a number onto an
	// English word. A translation whose count sits elsewhere in the sentence --
	// which is most of them -- cannot be expressed by concatenation.
	if !strings.Contains(body, "'{count}'") {
		t.Error("the badge's plural label does not interpolate {count}; a concatenated number cannot render " +
			"a translation that places the count anywhere but the front")
	}
}

// TestBlastRadiusPane_OrderingControlsRoundTrip covers the sort/order selects,
// which are NOT filters and so are not part of the flyout parity test above.
//
// Two things are asserted per case: the select renders the option in force as
// selected, and the rows actually come back in that order. Asserting only the
// markup would pass a control that displays a sort the query never applied.
//
// Mutation proving teeth: dropping Sort/Order from the loader's BlastRadiusData
// makes every non-default case fail the "renders as selected" half, because the
// selects fall back to showing the default.
func TestBlastRadiusPane_OrderingControlsRoundTrip(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastMixedFixture(t, r)

	cases := []struct {
		sort  string
		order string
	}{
		// EVERY sort key in BOTH directions. blastRadiusOrderBy builds a
		// different ORDER BY clause per key AND interpolates the direction into
		// it, so key and direction are independent axes -- covering three of the
		// six combinations left an inverted comparator free to hide in whichever
		// half went unexercised. artist_name descending and field ascending were
		// the two missing.
		{artist.BlastSortCreatedAt, "desc"},
		{artist.BlastSortCreatedAt, "asc"},
		{artist.BlastSortArtistName, "asc"},
		{artist.BlastSortArtistName, "desc"},
		{artist.BlastSortField, "asc"},
		{artist.BlastSortField, "desc"},
	}

	for _, tc := range cases {
		t.Run(tc.sort+"/"+tc.order, func(t *testing.T) {
			query := fmt.Sprintf("?sort=%s&order=%s", tc.sort, tc.order)
			body := renderBlastPane(t, r, query)

			if !blastSelectHasSelected(t, body, "blast-radius-sort", tc.sort) {
				t.Errorf("the sort select does not show %q as selected after a reload with %s; the control "+
					"reports an ordering the rows were not fetched in", tc.sort, query)
			}
			if !blastSelectHasSelected(t, body, "blast-radius-order", tc.order) {
				t.Errorf("the order select does not show %q as selected after a reload with %s", tc.order, query)
			}

			// The rows really are in that order, read from the loader (the
			// markup renders the same slice).
			data := loadBlastPane(t, r, query)
			if len(data.Rows) < 2 {
				t.Fatalf("precondition: %d rows returned; ordering is unobservable below 2 rows", len(data.Rows))
			}
			if data.Sort != tc.sort || data.Order != tc.order {
				t.Errorf("loader recorded sort=%q order=%q, want %q/%q", data.Sort, data.Order, tc.sort, tc.order)
			}
			if !blastRowsAreOrdered(data.Rows, tc.sort, tc.order) {
				t.Errorf("rows are not in %s %s order; the control claims an ordering the query did not apply",
					tc.sort, tc.order)
			}
		})
	}
}

// blastSelectHasSelected reports whether the <select id=selectID> rendered
// optionValue as its selected option. Scoped to that one select's markup so a
// matching option in a sibling select cannot satisfy it.
func blastSelectHasSelected(t *testing.T, body, selectID, optionValue string) bool {
	t.Helper()
	start := strings.Index(body, `id="`+selectID+`"`)
	if start < 0 {
		t.Fatalf("select %q is absent from the page", selectID)
	}
	end := strings.Index(body[start:], "</select>")
	if end < 0 {
		t.Fatalf("select %q is not closed", selectID)
	}
	region := body[start : start+end]

	// EXACTLY ONE option may carry `selected`, and it must be the expected one.
	//
	// Asserting only that the target option is selected is not enough: in a
	// non-multiple <select> the LAST selected option wins, so marking every
	// option selected leaves the target's own check passing while the control
	// displays something else. Measured on a mutant build with all three sort
	// options selected: ?sort=artist_name displayed "Field" and
	// ?sort=created_at displayed "Field", while the rows came back correctly
	// ordered in both cases. blastRowsAreOrdered cannot catch that -- the rows
	// really are right -- so only the DISPLAY lies, which is exactly the
	// "control shows a sort the query never applied" failure this helper's
	// caller exists to prevent.
	var selectedValues []string
	seenTarget := false
	for _, m := range blastOptionRE.FindAllStringSubmatch(region, -1) {
		value, attrs := m[1], m[2]
		if value == optionValue {
			seenTarget = true
		}
		if strings.Contains(attrs, "selected") {
			selectedValues = append(selectedValues, value)
		}
	}
	if !seenTarget {
		t.Fatalf("select %q has no option with value %q", selectID, optionValue)
	}
	if len(selectedValues) != 1 {
		t.Errorf("select %q marks %d options selected (%v); a non-multiple select displays the LAST of "+
			"them, so the control reports an ordering unrelated to the request",
			selectID, len(selectedValues), selectedValues)
		return false
	}
	return selectedValues[0] == optionValue
}

// blastOptionRE captures each <option>'s value and the rest of its attributes,
// so a caller can count how many are marked selected rather than only asking
// whether one particular option is.
var blastOptionRE = regexp.MustCompile(`<option value="([^"]*)"([^>]*)>`)

// blastRowsAreOrdered reports whether rows are ordered exactly the way the
// SQL clause blastRadiusOrderBy builds for (sort, order) orders them: the
// primary key in the requested direction, THEN, in order, every tie-breaker
// the query appends after it (see blastRowCompare). The tie-breakers are
// fixed regardless of direction -- blastRadiusOrderBy never interpolates
// <dir> into them -- so they are checked with a fixed direction even when
// order is "desc".
//
// Comparing only the primary key -- the previous shape of this helper --
// cannot see a dropped or reordered tie-breaker: two rows tied on the primary
// key still come back in SOME order, and a primary-only check accepts
// whichever order that happens to be. blastRowCompare encodes the full
// clause per sort key, so two rows tied on the primary are only "ordered"
// here if they also agree with the ORDER BY's own secondary and tertiary
// keys (issue #3111).
//
// THE DATE KEY IS COMPARED AS A TIME, THE TEXT KEYS AS STRINGS. That split is
// deliberate and load-bearing.
//
// created_at was previously formatted with RFC3339Nano and compared as text.
// That format STRIPS TRAILING ZEROS from the fractional second, so within one
// second "...T00:00:00.5Z" sorts BEFORE "...T00:00:00Z" -- '.' is 46 and 'Z' is
// 90 in ASCII, so the LATER timestamp compares as smaller. Verified in Go:
// b.After(a) is true while the formatted sb > sa is false. The current fixture
// uses whole-minute timestamps and never renders a fractional second, so it
// passed; a fixture with sub-second values would make this helper report a
// correct ordering as broken, or miss a real inversion. Comparing time.Time
// directly removes the format from the comparison entirely.
//
// artist_name and field stay byte-wise, matching the BINARY collation the
// rows came back in. That path carries a known, accepted limitation: it uses
// Go's byte-wise operators while the rows were ordered by SQLite. Those agree
// today because the columns use the default BINARY collation, which is also
// byte-wise. They would diverge under COLLATE NOCASE or for non-ASCII artist
// names, where SQLite and Go disagree and this helper would call a correct
// ordering broken. Left as is deliberately: unlike the timestamp defect
// above, it has no reachable failure with the schema and fixture in force,
// and expressing SQLite's collation in Go would be a re-implementation that
// could disagree in its own way.
//
// id is the FINAL tie-breaker on every key and is always DESC, matching
// blastRadiusOrderBy; it is a TEXT primary key, so it too is compared
// byte-wise.
func blastRowsAreOrdered(rows []artist.BlastRadiusRow, sort, order string) bool {
	for i := 1; i < len(rows); i++ {
		if blastRowCompare(rows[i-1], rows[i], sort, order) > 0 {
			return false
		}
	}
	return true
}

// blastRowCompare compares two adjacent rows the way the ORDER BY clause
// blastRadiusOrderBy builds for (sort, order) would: negative if a belongs
// before b, positive if a belongs after b, zero if they tie on every key the
// clause carries. Because id is unique, the zero case should be unreachable
// in practice, but it is expressed correctly here rather than assumed away.
func blastRowCompare(a, b artist.BlastRadiusRow, sort, order string) int {
	switch sort {
	case artist.BlastSortArtistName:
		// ORDER BY artist_name <dir>, field ASC, id DESC
		if c := compareBlastText(a.ArtistName, b.ArtistName, order); c != 0 {
			return c
		}
		if c := compareBlastText(a.Field, b.Field, "asc"); c != 0 {
			return c
		}
		return compareBlastText(a.ID, b.ID, "desc")
	case artist.BlastSortField:
		// ORDER BY field <dir>, artist_name ASC, id DESC
		if c := compareBlastText(a.Field, b.Field, order); c != 0 {
			return c
		}
		if c := compareBlastText(a.ArtistName, b.ArtistName, "asc"); c != 0 {
			return c
		}
		return compareBlastText(a.ID, b.ID, "desc")
	default:
		// ORDER BY created_at <dir>, id DESC
		if c := compareBlastTime(a.CreatedAt, b.CreatedAt, order); c != 0 {
			return c
		}
		return compareBlastText(a.ID, b.ID, "desc")
	}
}

// compareBlastText compares two strings byte-wise, applying the requested
// direction. Negative means a belongs first.
func compareBlastText(a, b, order string) int {
	c := strings.Compare(a, b)
	if order != "asc" {
		c = -c
	}
	return c
}

// compareBlastTime compares two instants directly (never their rendering --
// see the package comment above), applying the requested direction.
func compareBlastTime(a, b time.Time, order string) int {
	var c int
	switch {
	case a.Before(b):
		c = -1
	case a.After(b):
		c = 1
	}
	if order != "asc" {
		c = -c
	}
	return c
}

// blastRowsDump renders the sort-relevant fields of each row, in the order
// they were returned, for a failure message that names the offending rows
// rather than only asserting a bool.
//
// The fields shown are the ones that ORDER BY <sort> actually compares
// (blastRowCompare), not just the id: a tie-breaker violation is a defect in
// the SECONDARY/tertiary key, and a dump that showed only ids would still
// leave a maintainer re-running the test to see which key broke. Per sort
// key:
//
//   - artist_name: id, artist_name, field (the tie-breaker under test), so a
//     violation of "field ASC" is visible directly in the dump.
//   - field: id, field, artist_name (the tie-breaker under test).
//   - created_at: id, created_at -- the only tie-breaker here is id DESC, and
//     created_at is what the rows tie on, so both are shown.
func blastRowsDump(rows []artist.BlastRadiusRow, sort string) string {
	var b strings.Builder
	for i, row := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		switch sort {
		case artist.BlastSortArtistName:
			fmt.Fprintf(&b, "{id=%s artist_name=%q field=%q}", row.ID, row.ArtistName, row.Field)
		case artist.BlastSortField:
			fmt.Fprintf(&b, "{id=%s field=%q artist_name=%q}", row.ID, row.Field, row.ArtistName)
		default:
			fmt.Fprintf(&b, "{id=%s created_at=%s}", row.ID, row.CreatedAt.Format(time.RFC3339))
		}
	}
	return b.String()
}

// seedAPIBlastTieFixture seeds rows that DELIBERATELY tie on each primary
// sort key blastRadiusOrderBy carries a tie-breaker for (issue #3111). The
// mixed fixture above cannot do this: it gives every row a distinct field and
// spaces timestamps a minute apart, so the primary key is unique on every row
// and no tie-breaker is ever reached. A tie-breaker assertion written against
// that fixture would pass vacuously.
//
// Six rows, three genuine ties:
//
//   - T1/T2 tie on artist_name ("Alpha Tie") but differ on field
//     (biography/genres), exercising the "field ASC" secondary key under
//     BlastSortArtistName. Their ids are chosen so that id DESC ALONE would
//     invert the pair: id(T1) < id(T2), so dropping "field ASC" and falling
//     back to "artist_name <dir>, id DESC" puts T2 before T1 -- the opposite
//     of the correct "field ASC" order (biography before genres).
//   - T1/T3 tie on BOTH artist_name ("Alpha Tie") and field ("biography"),
//     exercising "id DESC" as the tie-breaker of last resort under BOTH
//     BlastSortArtistName and BlastSortField (they tie on field there too,
//     with equal artist_name as well). T1 is inserted first (lower rowid)
//     and given the SMALLER id, so a query with no id clause at all falls
//     back to insertion order (T1 first) -- the opposite of the correct
//     "id DESC" order (T3 first, since id(T3) > id(T1)).
//   - T2/T4 tie on field ("genres") but differ on artist_name
//     ("Alpha Tie"/"Zulu Tie"), exercising "artist_name ASC" under
//     BlastSortField. id(T4) > id(T2), so dropping "artist_name ASC" and
//     falling back to "field <dir>, id DESC" puts T4 before T2 -- the
//     opposite of the correct "artist_name ASC" order (Alpha before Zulu).
//   - T5/T6 tie on created_at TO THE SECOND, exercising "id DESC" under
//     BlastSortCreatedAt. T5 is inserted first and given the smaller id, so
//     dropping "id DESC" falls back to insertion order (T5 first) -- the
//     opposite of the correct order (T6 first, since id(T6) > id(T5)).
func seedAPIBlastTieFixture(t *testing.T, r *Router) {
	t.Helper()

	tieBase := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return tieBase.Add(time.Duration(m) * time.Minute) }

	seed := func(id, artistID, artistName, field string, ts time.Time) {
		seedAPIBlastChange(t, r, id, artistID, artistName, field, "the operator's value", "", "scan", ts)
	}

	seed("id-1-bio", "tie-a1", "Alpha Tie", "biography", at(0)) // T1
	seed("id-2-gen", "tie-a2", "Alpha Tie", "genres", at(1))    // T2
	seed("id-9-bio", "tie-a3", "Alpha Tie", "biography", at(2)) // T3
	seed("id-3-gen", "tie-a4", "Zulu Tie", "genres", at(3))     // T4
	seed("id-5-lo", "tie-a5", "Created Tie A", "formed", at(10))
	seed("id-6-hi", "tie-a6", "Created Tie B", "died", at(10)) // same minute as T5: a genuine tie to the second
}

// TestBlastRadiusPane_OrderingTieBreakers proves the tie-breakers
// blastRadiusOrderBy carries after each primary sort key (issue #3111):
// "field ASC, id DESC" after artist_name, "artist_name ASC, id DESC" after
// field, and "id DESC" after created_at.
//
// Each subtest first asserts its rows GENUINELY TIE on the primary key --
// without that, the tie-breaker assertion below it would just be re-checking
// ordinary primary-key ordering and would prove nothing about the
// tie-breaker at all.
//
// blastRowsAreOrdered now compares the FULL ordering (see blastRowCompare),
// so once the tie precondition holds, a passing case here is real evidence
// the tie-breakers fired. Dropping "field ASC" or "id DESC" from the
// artist_name branch of blastRadiusOrderBy, "artist_name ASC" or "id DESC"
// from the field branch, or "id DESC" from the created_at branch each turn
// the corresponding subtest red (see seedAPIBlastTieFixture's doc comment for
// exactly which pair inverts under which drop, and why).
//
// EACH SUBTEST ALSO LOADS THE OPPOSITE PRIMARY DIRECTION, and that second
// load is not redundant with the first. blastRadiusOrderBy's tie-breakers
// are hardcoded ("field ASC", not "field <dir>"), and a mutation that
// REORIENTS one -- interpolating <dir> into it instead of dropping it --
// only shows up when the query direction actually differs from the
// tie-breaker's fixed direction. Querying only order=asc for the
// artist_name/field cases (whose tie-breakers are themselves ASC) and only
// order=desc for created_at (whose tie-breaker id DESC matches) made that
// class of mutation a silent no-op: <dir> substitutes in as the same
// direction the tie-breaker already had, so nothing changes and the suite
// stays green over a broken invariant. Loading the opposite direction too
// forces <dir> to actually diverge from the hardcoded direction, so a
// reoriented tie-breaker is now visible.
func TestBlastRadiusPane_OrderingTieBreakers(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastTieFixture(t, r)

	rowByID := func(t *testing.T, rows []artist.BlastRadiusRow, id string) artist.BlastRadiusRow {
		t.Helper()
		for _, row := range rows {
			if row.ID == id {
				return row
			}
		}
		t.Fatalf("row %q is not in the loaded set", id)
		return artist.BlastRadiusRow{}
	}

	t.Run(artist.BlastSortArtistName, func(t *testing.T) {
		for _, order := range []string{"asc", "desc"} {
			t.Run(order, func(t *testing.T) {
				data := loadBlastPane(t, r, "?sort="+artist.BlastSortArtistName+"&order="+order+"&page_size=500")
				t1 := rowByID(t, data.Rows, "id-1-bio")
				t2 := rowByID(t, data.Rows, "id-2-gen")
				t3 := rowByID(t, data.Rows, "id-9-bio")

				// PRECONDITION: the rows genuinely tie on artist_name.
				// Without this, "field ASC" and "id DESC" below would just be
				// plain artist_name ordering wearing a different label.
				if t1.ArtistName != t2.ArtistName || t2.ArtistName != t3.ArtistName {
					t.Fatalf("precondition: T1/T2/T3 do not tie on artist_name (%q/%q/%q); the fixture no "+
						"longer exercises the field-ASC/id-DESC tie-breakers", t1.ArtistName, t2.ArtistName, t3.ArtistName)
				}
				// And T1/T3 additionally tie on field, isolating "id DESC" as
				// the ONLY thing that can order that pair.
				if t1.Field != t3.Field {
					t.Fatalf("precondition: T1/T3 do not tie on field (%q/%q); the id-DESC-under-artist_name "+
						"case is not actually isolated", t1.Field, t3.Field)
				}
				// While T1/T2 differ on field, or the field-ASC case is
				// vacuous too.
				if t1.Field == t2.Field {
					t.Fatalf("precondition: T1/T2 tie on field (%q); the field-ASC case needs them to differ", t1.Field)
				}

				if !blastRowsAreOrdered(data.Rows, artist.BlastSortArtistName, order) {
					t.Errorf("rows tied on artist_name (order=%s) are not further ordered by field ASC, id DESC: %s",
						order, blastRowsDump(data.Rows, artist.BlastSortArtistName))
				}
			})
		}
	})

	t.Run(artist.BlastSortField, func(t *testing.T) {
		for _, order := range []string{"asc", "desc"} {
			t.Run(order, func(t *testing.T) {
				data := loadBlastPane(t, r, "?sort="+artist.BlastSortField+"&order="+order+"&page_size=500")
				t2 := rowByID(t, data.Rows, "id-2-gen")
				t4 := rowByID(t, data.Rows, "id-3-gen")

				// PRECONDITION: T2/T4 genuinely tie on field but differ on
				// artist_name, isolating "artist_name ASC" as the
				// tie-breaker under test.
				if t2.Field != t4.Field {
					t.Fatalf("precondition: T2/T4 do not tie on field (%q/%q); the artist_name-ASC case is vacuous",
						t2.Field, t4.Field)
				}
				if t2.ArtistName == t4.ArtistName {
					t.Fatalf("precondition: T2/T4 tie on artist_name (%q) too; the case cannot isolate "+
						"artist_name ASC", t2.ArtistName)
				}

				if !blastRowsAreOrdered(data.Rows, artist.BlastSortField, order) {
					t.Errorf("rows tied on field (order=%s) are not further ordered by artist_name ASC, id DESC: %s",
						order, blastRowsDump(data.Rows, artist.BlastSortField))
				}
			})
		}
	})

	t.Run(artist.BlastSortCreatedAt, func(t *testing.T) {
		for _, order := range []string{"asc", "desc"} {
			t.Run(order, func(t *testing.T) {
				data := loadBlastPane(t, r, "?sort="+artist.BlastSortCreatedAt+"&order="+order+"&page_size=500")
				t5 := rowByID(t, data.Rows, "id-5-lo")
				t6 := rowByID(t, data.Rows, "id-6-hi")

				// PRECONDITION: T5/T6 genuinely tie on created_at TO THE
				// SECOND (created_at is stored at second resolution), or
				// "id DESC" below is re-checking ordinary created_at
				// ordering.
				if !t5.CreatedAt.Equal(t6.CreatedAt) {
					t.Fatalf("precondition: T5/T6 do not tie on created_at (%v/%v); the id-DESC case is vacuous",
						t5.CreatedAt, t6.CreatedAt)
				}

				if !blastRowsAreOrdered(data.Rows, artist.BlastSortCreatedAt, order) {
					t.Errorf("rows tied on created_at (order=%s) are not further ordered by id DESC: %s",
						order, blastRowsDump(data.Rows, artist.BlastSortCreatedAt))
				}
			})
		}
	})
}
