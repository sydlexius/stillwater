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
	// a box. role=status + aria-live=polite is what announces it -- but it must
	// be scoped to the COUNT, not the whole bar; see the dedicated test below.
	countTag := blastBulkCountTag(t, body)
	if !strings.Contains(countTag, `role="status"`) || !strings.Contains(countTag, `aria-live="polite"`) {
		t.Errorf("bulk selection count is not an announced live region (want role=status aria-live=polite); tag: %s", countTag)
	}

}

// blastBulkCountTag returns the opening tag of #blast-bulk-count.
func blastBulkCountTag(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, `id="blast-bulk-count"`)
	if idx < 0 {
		t.Fatalf("#blast-bulk-count absent; the bulk bar has no count element at all")
	}
	end := strings.Index(body[idx:], ">")
	if end < 0 {
		t.Fatalf("malformed #blast-bulk-count tag")
	}
	return body[idx : idx+end]
}

// TestBlastRadiusPane_LiveRegionIsScopedToTheCount pins FIX 3.
//
// role="status" implies aria-atomic="true": a change anywhere inside the live
// region re-announces the region's ENTIRE text. With the role on
// #blast-bulk-bar, every checkbox tick re-announced the bar's whole subtree --
// the count PLUS both button labels ("3 rows selectedRestore Selected Cancel").
// Ticking twenty rows produced twenty announcements each ending in the button
// names, which buries the only part that actually changed.
//
// Scoped to #blast-bulk-count, the reveal still announces (the count is written
// after unhiding) and each subsequent change announces only the number.
//
// The mutation that gives it teeth: moving role="status" aria-live="polite"
// back onto the #blast-bulk-bar tag fails both halves of this.
func TestBlastRadiusPane_LiveRegionIsScopedToTheCount(t *testing.T) {
	t.Parallel()
	body := renderBlastRadiusPane(t)

	// Precondition: the bar and its buttons rendered. Without the buttons
	// inside the bar there is nothing for an over-broad live region to
	// over-announce, and this test would pass vacuously.
	barIdx := strings.Index(body, `id="blast-bulk-bar"`)
	if barIdx < 0 {
		t.Fatalf("#blast-bulk-bar absent")
	}
	barEnd := strings.Index(body[barIdx:], ">")
	if barEnd < 0 {
		t.Fatalf("malformed #blast-bulk-bar tag")
	}
	barTag := body[barIdx : barIdx+barEnd]
	// The buttons must actually live inside the bar for the over-announcement
	// to be possible; assert on the region between the bar tag and the i18n
	// element that follows it.
	after := body[barIdx:]
	if i18nIdx := strings.Index(after, `id="blast-radius-i18n"`); i18nIdx > 0 {
		after = after[:i18nIdx]
	}
	if !strings.Contains(after, "blastRestoreSelected()") || !strings.Contains(after, "blastClearSelection()") {
		t.Fatalf("the bulk bar's action buttons are not inside it; an over-broad live region could not over-announce, so this test would be vacuous")
	}

	// The assertion: the bar itself must NOT be the live region.
	if strings.Contains(barTag, `role="status"`) || strings.Contains(barTag, `aria-live=`) {
		t.Errorf("#blast-bulk-bar is itself a live region, so role=status's implicit aria-atomic re-announces the buttons "+
			"on every selection change; move role=status/aria-live onto #blast-bulk-count. tag: %s", barTag)
	}

	// And the count must be, or the reveal announces nothing at all.
	countTag := blastBulkCountTag(t, body)
	if !strings.Contains(countTag, `role="status"`) || !strings.Contains(countTag, `aria-live="polite"`) {
		t.Errorf("#blast-bulk-count is not a live region (want role=status aria-live=polite), so ticking a checkbox announces nothing; tag: %s", countTag)
	}
}

// blastScriptFunc returns the body of a top-level function in the rendered
// blast-radius script, bounded at the next top-level function or the script
// close so assertions cannot leak into a neighboring function.
func blastScriptFunc(t *testing.T, body, name string) string {
	t.Helper()
	start := strings.Index(body, "function "+name+"(")
	if start < 0 {
		t.Fatalf("%s is not defined in the rendered page", name)
	}
	fn := body[start:]
	if end := strings.Index(fn[1:], "\n\t\tfunction "); end >= 0 {
		fn = fn[:end]
	} else if end := strings.Index(fn, "</script>"); end >= 0 {
		fn = fn[:end]
	}
	return fn
}

// TestBlastRadiusPane_CommitOutcomeBranchesOnTheCounts pins FIX 1.
//
// commitBlastRestore re-reads the live value immediately before each write, so
// the server can refuse at commit time an item the dry-run plan listed as
// eligible. {restored:0, refused:3} is a designed-for response.
//
// Both commit callbacks used to call showSuccessToast unconditionally, so an
// operator who selected three rows, previewed "3 of 3 selected change(s) can be
// restored." and confirmed got a GREEN success banner reading "0 value(s)
// restored." on a recovery surface where nothing had been recovered.
//
// The mutation that gives it teeth: replacing either callback's
// blastRestoreOutcomeToast(result) with a bare showSuccessToast(...) fails the
// per-callback assertions; deleting any one of the three branches inside
// blastRestoreOutcomeToast fails the branch assertions.
func TestBlastRadiusPane_CommitOutcomeBranchesOnTheCounts(t *testing.T) {
	t.Parallel()
	body := renderBlastRadiusPane(t)

	outcome := blastScriptFunc(t, body, "blastRestoreOutcomeToast")

	// Precondition: the helper reads the counts at all. A helper that ignores
	// refused could not branch on it, and every assertion below would be
	// asserting on dead text.
	for _, field := range []string{"result.restored", "result.refused"} {
		if !strings.Contains(outcome, field) {
			t.Fatalf("blastRestoreOutcomeToast never reads %s, so it cannot branch on the outcome:\n%s", field, outcome)
		}
	}

	// All three variants must be reachable from this one helper. The variant
	// is the only signal an operator gets at a glance, so a missing branch is
	// a misreport, not a cosmetic gap.
	if !strings.Contains(outcome, "showToast(") {
		t.Errorf("a commit that restored NOTHING does not raise the error toast; a zero would be reported in a success color:\n%s", outcome)
	}
	if !strings.Contains(outcome, "showWarningToast(") {
		t.Errorf("a partial restore (some restored, some refused) does not raise the warning toast; an incomplete recovery would read as clean:\n%s", outcome)
	}
	if !strings.Contains(outcome, "showSuccessToast(") {
		t.Errorf("a fully successful restore never raises the success toast:\n%s", outcome)
	}

	// The zero-restored branch must come FIRST. If the success branch could be
	// reached with restored === 0, the fix is defeated regardless of which
	// toasts the function can raise.
	zeroGuard := strings.Index(outcome, "restored === 0")
	success := strings.Index(outcome, "showSuccessToast(")
	if zeroGuard < 0 {
		t.Fatalf("blastRestoreOutcomeToast has no restored === 0 guard, so a zero-restore reaches a non-error toast:\n%s", outcome)
	}
	if zeroGuard > success {
		t.Errorf("the restored === 0 guard (offset %d) comes AFTER the success toast (offset %d); a zero-restore can still report success",
			zeroGuard, success)
	}

	// Both commit callbacks must route through the helper, and neither may
	// call the success toast directly -- that is the exact defect.
	for _, name := range []string{"blastRestoreRow", "blastRestoreSelected"} {
		fn := blastScriptFunc(t, body, name)
		if !strings.Contains(fn, "blastRestoreOutcomeToast(result)") {
			t.Errorf("%s does not report its commit outcome through blastRestoreOutcomeToast, so it cannot distinguish a refused restore from a successful one:\n%s", name, fn)
		}
		if strings.Contains(fn, "showSuccessToast(") {
			t.Errorf("%s calls showSuccessToast directly; a commit that restored nothing would be reported in green:\n%s", name, fn)
		}
	}
}

// TestBlastRadiusPane_PerRowRemovalIsGatedOnRestoreStatus pins FIX 1b.
//
// The per-row path removed the row unconditionally after a commit. A REFUSED
// restore therefore both reported success AND made the still-damaged row vanish
// from the table until reload -- the operator loses the only on-screen evidence
// that the value still needs recovering. The bulk path already guarded this.
//
// The mutation that gives it teeth: dropping the restore_status check around
// row.remove() in blastRestoreRow.
func TestBlastRadiusPane_PerRowRemovalIsGatedOnRestoreStatus(t *testing.T) {
	t.Parallel()
	body := renderBlastRadiusPane(t)

	fn := blastScriptFunc(t, body, "blastRestoreRow")

	// Precondition: the function does remove a row. If it stopped removing
	// rows entirely, the gate assertion would be vacuously satisfied.
	remove := strings.Index(fn, "row.remove()")
	if remove < 0 {
		t.Fatalf("blastRestoreRow no longer removes the restored row, so the removal gate is not under test:\n%s", fn)
	}

	// The gate must be READ (the item's status) and must sit BEFORE the
	// removal. A status check after the fact does not stop the row vanishing.
	gate := strings.Index(fn, "restore_status")
	if gate < 0 {
		t.Fatalf("blastRestoreRow removes the row without consulting restore_status; a REFUSED restore hides a still-damaged row:\n%s", fn)
	}
	if gate > remove {
		t.Errorf("blastRestoreRow's restore_status check (offset %d) comes AFTER row.remove() (offset %d); the row is already gone",
			gate, remove)
	}
	// It must compare against the server's own "restored" token, not merely
	// mention the field.
	if !strings.Contains(fn, `'restored'`) {
		t.Errorf("blastRestoreRow does not compare restore_status against 'restored', the token commitBlastRestore writes for a landed write:\n%s", fn)
	}
}

// TestBlastRadiusPane_BulkBarMovesFocusOutBeforeHiding pins FIX 2 at the
// source level.
//
// .hidden is display:none, and hiding the element that currently holds focus
// makes the browser reset focus to <body>. Both paths that hide this bar --
// the bar's own Cancel, and blastClearSelection after a commit -- have focus
// INSIDE the bar at that moment, because the shared modal's hideModal restores
// focus to its opener (the "Restore Selected" button, in the bar) and only then
// does the callback hide it. A keyboard user who just committed a destructive
// operation was dumped at the top of the document.
//
// This is a SOURCE-ORDER assertion, not a behavioral one. It cannot observe
// focus: jsdom does not blur on hide (it reported focus STAYING on the hidden
// button), and a browser test without the real built stylesheet gets the wrong
// answer too, because .hidden is inert without it. Only Chromium plus the real
// styles.css reproduces the defect, which needs a served page. This test pins
// that the guard exists and runs BEFORE the hide; see the report for what a
// browser-level regression test would need.
func TestBlastRadiusPane_BulkBarMovesFocusOutBeforeHiding(t *testing.T) {
	t.Parallel()
	body := renderBlastRadiusPane(t)

	fn := blastScriptFunc(t, body, "blastUpdateBulkBar")

	// Precondition: the function does hide the bar by adding .hidden. That is
	// the action that drops focus; without it there is nothing to guard.
	hide := strings.Index(fn, `classList.add('hidden')`)
	if hide < 0 {
		t.Fatalf("blastUpdateBulkBar no longer hides the bar via the hidden class, so the focus guard is not under test:\n%s", fn)
	}

	// The guard: it must test whether focus is inside the bar, and move it to
	// the select-all, BEFORE the hide.
	contains := strings.Index(fn, "bar.contains(document.activeElement)")
	if contains < 0 {
		t.Fatalf("blastUpdateBulkBar hides the bar without checking whether it holds focus; focus falls to <body>:\n%s", fn)
	}
	if contains > hide {
		t.Errorf("the focus check (offset %d) runs AFTER the bar is hidden (offset %d); by then the browser has already reset focus to <body>",
			contains, hide)
	}
	refocus := strings.Index(fn, `getElementById('blast-select-all')`)
	if refocus < 0 || !strings.Contains(fn, ".focus()") {
		t.Fatalf("blastUpdateBulkBar does not move focus to #blast-select-all before hiding:\n%s", fn)
	}
	if refocus > hide {
		t.Errorf("focus is moved to #blast-select-all (offset %d) only AFTER the hide (offset %d), which is too late", refocus, hide)
	}

	// The refocus target must actually exist on the page, or the guard is a
	// no-op that reports a console error and still drops focus.
	if !strings.Contains(body, `id="blast-select-all"`) {
		t.Errorf("the focus guard targets #blast-select-all, which this page does not render")
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
