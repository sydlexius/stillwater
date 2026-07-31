package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

// renderBlastRadiusPane serves the blast-radius report page and returns the
// rendered HTML. The fixture is seeded first so the table has real rows: a
// selection control that only exists per row is invisible on an empty table,
// and every assertion below would pass vacuously without it.
func renderBlastRadiusPane(t *testing.T) string {
	t.Helper()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastFixture(t, r)

	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "blast-radius")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	http.HandlerFunc(r.handleReportPage).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Precondition. Without rows there are no per-row checkboxes to find, so a
	// green result would mean "nothing rendered" rather than "nothing broken".
	if !strings.Contains(body, `id="blast-radius-tbl"`) {
		t.Fatalf("blast-radius table absent; the page did not render the pane at all")
	}
	if !strings.Contains(body, `id="blast-row-`) {
		t.Fatalf("no blast-radius rows rendered; the fixture did not reach the pane, so this test would pass vacuously")
	}
	return body
}

// blastInlineHandlerRefs returns every function name this page invokes from an
// inline event attribute (onclick/onchange/...) whose name starts with the
// pane's "blast" prefix, deduplicated and sorted.
//
// The prefix filter is deliberate: the page also carries chrome handlers
// (toggleHelpOverlay, searchHelp) that live in a vendored static .js file this
// server-side render cannot see. Asserting over those would fail for a reason
// that has nothing to do with this pane. Everything the blast-radius pane
// itself wires is inline, in blastRadiusScript, and is therefore in scope.
func blastInlineHandlerRefs(html string) []string {
	re := regexp.MustCompile(`on(?:click|change|input|submit)="(blast[A-Za-z0-9_$]*)\(`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		seen[m[1]] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestBlastRadiusPane_EveryBulkHandlerIsDefinedOnThisPane is THE regression
// test for the defect that got the bulk affordance deleted from this pane in
// the first place (see the doc comment on repPaneBlastRadius).
//
// The earlier cut emitted .bulk-select checkboxes, a select-all and a
// #bulk-action-bar whose handlers -- toggleSelectAll, updateBulkBar,
// clearBulkSelection -- are defined ONLY inside repComplianceScript. The pane
// dispatch is an else-if chain, so on the blast-radius pane that script never
// renders: every one of those onchange/onclick attributes named a function that
// did not exist. Ticking a checkbox threw ReferenceError and the bar never
// unhid. The markup rendered perfectly and the control was inert.
//
// So this test does not check that the markup exists (the test below does
// that). It checks the PAIRING: for every handler this pane's markup invokes,
// the SAME response must also carry that function's definition. A control wired
// to a function defined in some other pane's script fails here.
//
// The mutation that gives it teeth: renaming any of the four bulk functions in
// blastRadiusScript (or moving them into repComplianceScript, which is what the
// original defect amounted to) leaves the attribute pointing at a name the
// document no longer defines, and this fails naming exactly that handler.
func TestBlastRadiusPane_EveryBulkHandlerIsDefinedOnThisPane(t *testing.T) {
	t.Parallel()
	body := renderBlastRadiusPane(t)

	refs := blastInlineHandlerRefs(body)

	// Precondition on the SET, not just on its size: this test is only
	// meaningful if the bulk controls are actually wired. If a future edit
	// silently drops the bulk markup, "no undefined handlers" would be
	// trivially true, and this pane would quietly lose bulk restore with a
	// green suite. Naming the required handlers makes that a failure.
	required := []string{
		"blastRestoreRow",      // per-row restore, the pre-existing control
		"blastToggleSelectAll", // select-all
		"blastUpdateBulkBar",   // per-row checkbox
		"blastRestoreSelected", // bulk action bar: preview
		"blastClearSelection",  // bulk action bar: cancel
	}
	have := make(map[string]bool, len(refs))
	for _, name := range refs {
		have[name] = true
	}
	var notWired []string
	for _, name := range required {
		if !have[name] {
			notWired = append(notWired, name)
		}
	}
	if len(notWired) > 0 {
		t.Fatalf("blast-radius pane markup never invokes %v (found handlers: %v); the bulk affordance is missing, not merely broken",
			notWired, refs)
	}

	// The assertion itself: every referenced handler must be DEFINED in this
	// same response. A definition in another pane's script is not reachable
	// here, which is precisely the original defect.
	var undefined []string
	for _, name := range refs {
		if !strings.Contains(body, "function "+name+"(") {
			undefined = append(undefined, name)
		}
	}
	if len(undefined) > 0 {
		t.Errorf("blast-radius pane wires handlers that are NOT defined in the page it renders: %v.\n"+
			"Clicking those controls throws ReferenceError and the affordance is inert. "+
			"Define them in blastRadiusScript (which repPaneBlastRadius renders) rather than in another pane's script.",
			undefined)
	}

	// And the inverse direction of the same defect: the compliance pane's
	// selection functions must NOT be what this pane's controls point at. If
	// they were, this pane would depend on a script it does not render.
	for _, foreign := range []string{"toggleSelectAll", "updateBulkBar", "clearBulkSelection"} {
		if regexp.MustCompile(`on(?:click|change)="` + foreign + `\(`).MatchString(body) {
			t.Errorf("blast-radius pane markup invokes %q, which is defined only in repComplianceScript and never renders on this pane", foreign)
		}
	}
}

// TestBlastRadiusPane_BulkSelectionMarkupAndA11y asserts the controls exist and
// carry the attributes the automated a11y tier requires.
//
// Each assertion states the failure it rules out; none of them is a
// "does the string appear" check for its own sake.
func TestBlastRadiusPane_BulkSelectionMarkupAndA11y(t *testing.T) {
	t.Parallel()
	body := renderBlastRadiusPane(t)

	// Per-row checkboxes: one per rendered row. Counting them against the rows
	// rules out a select-all with nothing to select, and a header-only control.
	rows := strings.Count(body, `id="blast-row-`)
	boxes := strings.Count(body, `class="blast-select `)
	if rows == 0 {
		t.Fatalf("no rows rendered")
	}
	if boxes != rows {
		t.Errorf("per-row selection checkboxes = %d, want one per rendered row (%d)", boxes, rows)
	}

	// Select-all, exactly one. Two would make the "check all" state ambiguous
	// and would double-fire blastToggleSelectAll.
	if got := strings.Count(body, `id="blast-select-all"`); got != 1 {
		t.Errorf("select-all controls = %d, want exactly 1", got)
	}

	// Accessible names. axe-core fails an unlabeled checkbox, and a
	// screen-reader user selecting rows for a DESTRUCTIVE write with no name
	// announced cannot tell what they are selecting. Every checkbox on this
	// pane must carry aria-label.
	labeled := strings.Count(body, `aria-label="Select `)
	if labeled < boxes+1 {
		t.Errorf("checkboxes carrying an aria-label = %d, want at least %d (every row plus select-all); an unlabeled checkbox is an axe-core violation",
			labeled, boxes+1)
	}

	// The bulk bar must render HIDDEN. A bar visible with nothing selected
	// offers a destructive action over an empty selection.
	barIdx := strings.Index(body, `id="blast-bulk-bar"`)
	if barIdx < 0 {
		t.Fatalf("bulk action bar (#blast-bulk-bar) absent")
	}
	// Read the bar's own opening tag rather than searching the whole document,
	// so an unrelated element's class attribute cannot satisfy this.
	end := strings.Index(body[barIdx:], ">")
	if end < 0 {
		t.Fatalf("malformed #blast-bulk-bar tag")
	}
	barTag := body[barIdx : barIdx+end]
	if !strings.Contains(barTag, "hidden") {
		t.Errorf("bulk action bar renders VISIBLE with no selection; tag: %s", barTag)
	}
	// Live region. The bar is revealed by script, and a control that appears
	// with no announcement is invisible to a screen-reader user who just ticked
	// a box. role=status + aria-live=polite is what announces it.
	if !strings.Contains(barTag, `role="status"`) || !strings.Contains(barTag, `aria-live="polite"`) {
		t.Errorf("bulk action bar is not an announced live region (want role=status aria-live=polite); tag: %s", barTag)
	}

}

// TestCompliancePane_SelectionHandlersStillResolve is the other half of the
// script-scoping decision recorded on repPaneBlastRadius.
//
// Bulk selection was NOT hoisted into a shared partial; the blast-radius pane
// carries its own blast-prefixed handlers. That keeps the compliance pane's
// toggleSelectAll/updateBulkBar/clearBulkSelection exactly where they were, in
// repComplianceScript. This test is the evidence for "the compliance pane still
// works": it applies the identical wired-vs-defined check to that pane, so a
// future edit that DOES try to hoist them cannot quietly break the other
// surface -- moving them out of repComplianceScript fails here.
func TestCompliancePane_SelectionHandlersStillResolve(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	req := httptest.NewRequest(http.MethodGet, "/reports/compliance", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "compliance")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	http.HandlerFunc(r.handleReportPage).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Precondition: this really is the compliance pane, with its selection
	// markup rendered. Without it the loop below iterates nothing.
	if !strings.Contains(body, `id="compliance-tbl"`) {
		t.Fatalf("compliance table absent; dispatch did not reach the compliance pane")
	}
	if !strings.Contains(body, `id="select-all"`) {
		t.Fatalf("compliance select-all absent, so its selection wiring is not under test")
	}

	// The compliance pane's own selection handlers must be defined in the page
	// it renders -- the same property the blast-radius pane is held to.
	for _, name := range []string{"toggleSelectAll", "updateBulkBar", "clearBulkSelection", "bulkAction"} {
		if !strings.Contains(body, "function "+name+"(") {
			t.Errorf("compliance pane wires %q but does not define it in the page it renders; "+
				"its selection controls are inert (this is what hoisting these out of repComplianceScript would cause)", name)
		}
	}

	// And the compliance pane must not have picked up the blast-radius pane's
	// selectors. The two selection systems are deliberately disjoint; sharing a
	// class name would let one pane's select-all tick the other's rows.
	if strings.Contains(body, "blast-select") || strings.Contains(body, "blast-bulk-bar") {
		t.Errorf("compliance pane rendered blast-radius selection markup; the two panes' selectors must stay disjoint")
	}
}

// TestBlastRadiusPane_EmptyStateSpansTheSelectionColumn covers the column the
// selection checkbox adds to the header.
//
// It needs its OWN render because the empty-state cell only exists when there
// are no rows: asserting it on the seeded page above would have been vacuous
// (measured -- reverting the colspan there left the test green, because the
// cell was never emitted). This renders a router with NO fixture, so the
// empty-state row is the row under test.
func TestBlastRadiusPane_EmptyStateSpansTheSelectionColumn(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	// Deliberately NOT seeded.

	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "blast-radius")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	http.HandlerFunc(r.handleReportPage).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Preconditions: the pane rendered, and it rendered EMPTY. Without both,
	// the colspan assertion below would be checking a cell that is not there.
	if !strings.Contains(body, `id="blast-radius-tbl"`) {
		t.Fatalf("blast-radius table absent; the page did not render the pane")
	}
	if strings.Contains(body, `id="blast-row-`) {
		t.Fatalf("rows rendered on an unseeded router, so the empty state is not under test")
	}

	// The header now has 8 columns (selection + the 7 data columns). An
	// empty-state cell that still spans 7 leaves the table visibly ragged and
	// misaligned under the selection column.
	if !strings.Contains(body, `colspan="8"`) {
		t.Errorf("empty-state cell does not span all 8 columns (selection + 7 data columns); body contains colspan=7: %v",
			strings.Contains(body, `colspan="7"`))
	}
	if strings.Contains(body, `colspan="7"`) {
		t.Errorf("empty-state cell still spans 7 columns; the selection column makes it 8")
	}
}

// TestBlastRadiusPane_BulkRestorePreviewsBeforeCommitting asserts the two-step
// contract in the shipped script: the bulk entry point must issue the DRY RUN
// first and reach the write only from inside the confirm dialog's callback.
//
// This is asserted on the rendered script text because that script is what the
// browser runs; there is no server-side seam to test it through. It is narrow
// on purpose -- it checks the ORDER of the two requests and that the commit
// sits inside showConfirmDialog, which is the property that stops a single
// click from writing.
//
// The mutation that gives it teeth: changing blastRestoreSelected's first
// request to commit:true, or hoisting the commit call out of the
// showConfirmDialog callback, breaks it.
func TestBlastRadiusPane_BulkRestorePreviewsBeforeCommitting(t *testing.T) {
	t.Parallel()
	body := renderBlastRadiusPane(t)

	start := strings.Index(body, "function blastRestoreSelected(")
	if start < 0 {
		t.Fatalf("blastRestoreSelected is not defined in the rendered page")
	}
	// Bound the slice at the next top-level function or the script close, so
	// this reads blastRestoreSelected's body and not the rest of the file.
	fn := body[start:]
	if end := strings.Index(fn[1:], "\n\t\tfunction "); end >= 0 {
		fn = fn[:end]
	} else if end := strings.Index(fn, "</script>"); end >= 0 {
		fn = fn[:end]
	}

	preview := strings.Index(fn, "blastRestoreRequest(ids, false")
	confirm := strings.Index(fn, "showConfirmDialog(")
	commit := strings.Index(fn, "blastRestoreRequest(ids, true")

	if preview < 0 {
		t.Fatalf("bulk restore never issues the commit:false dry run; the first click would write:\n%s", fn)
	}
	if commit < 0 {
		t.Fatalf("bulk restore never issues the commit:true write, so it cannot restore anything:\n%s", fn)
	}
	if confirm < 0 {
		t.Fatalf("bulk restore never opens a confirm dialog, so nothing asks for the second affirmative action:\n%s", fn)
	}
	if preview >= confirm || confirm >= commit {
		t.Errorf("bulk restore does not preview -> confirm -> commit (offsets preview=%d confirm=%d commit=%d); "+
			"a commit that is not gated behind the dialog writes on the first click", preview, confirm, commit)
	}

	// A committed bulk restore must clear the selection, or the operator is
	// left with rows ticked that no longer exist.
	if !strings.Contains(fn, "blastClearSelection()") {
		t.Errorf("bulk restore does not clear the selection after committing:\n%s", fn)
	}
	// And it must drop only the rows the server reports as restored: removing
	// every requested row would hide refusals, which are rows STILL DAMAGED.
	if !strings.Contains(fn, "restore_status") {
		t.Errorf("bulk restore does not consult per-item restore_status, so refused rows would be removed as though they had been restored:\n%s", fn)
	}
}
