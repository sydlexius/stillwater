package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
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
