package api

import (
	"strings"
	"testing"
)

// TestBlastRadiusPane_CarriesContextHelpToTheDocs asserts that the
// blast-radius pane ships an in-app context-help affordance pointing at the
// blast-radius section of the how-to docs, and that the affordance is
// operable.
//
// WHY THIS PANE SPECIFICALLY: blast radius is a destructive-recovery surface.
// The rules an operator needs exactly when a restore is refused -- that a
// preview is not a promise, why a row can be refused, that refused rows stay
// in the table -- live in the docs. Without a link from the pane, an operator
// hitting a refusal has to go search a docs site from a different tab. The
// maintainer's UAT flagged the absence.
//
// The mutation that gives this teeth: delete the
// @components.ContextHelp(...) call from repPaneBlastRadius in
// web/templates/reports_page.templ (measured RED), or drop the
// @components.ContextHelpScript() beside it, which leaves the button
// rendered but its onclick handler undefined -- an inert control, the exact
// silent-failure shape this repo forbids (also measured RED).
//
// The anchor STRING is separately validated against the generated docs anchor
// set by TestContextHelpAnchors in web/components; this test does not
// re-litigate that. It asserts the affordance exists on THIS pane, is
// keyboard-operable, and carries an accessible name.
func TestBlastRadiusPane_CarriesContextHelpToTheDocs(t *testing.T) {
	t.Parallel()
	body := renderBlastRadiusPane(t)

	// The help control itself. The id is the one repPaneBlastRadius passes,
	// so this pins the affordance to this pane rather than matching any
	// ContextHelp that some shared chrome might render.
	if !strings.Contains(body, `id="help-blast-radius"`) {
		t.Fatalf("blast-radius pane renders no context-help affordance; an operator hitting a refused restore has no in-app route to the docs that explain it")
	}

	// Accessible name. ContextHelp builds it as "Help: " + label, and the
	// label passed here is the pane's own title. A "?" button with no
	// accessible name is an axe violation and unusable by a screen reader.
	if !strings.Contains(body, `aria-label="Help: Blast radius"`) {
		t.Errorf("context-help button has no accessible name of the form 'Help: Blast radius'; a bare ? button is an axe violation")
	}

	// Keyboard reachability: the control must be a real <button>, not a
	// div-with-onclick, and must not be pulled out of the tab order. The
	// component opts a help icon out of tabbing only inside a
	// .sw-help-no-tab wrapper (ARIA menus); this pane must not use one.
	if !strings.Contains(body, `class="sw-context-help-btn"`) {
		t.Errorf("context-help control is not the standard sw-context-help-btn <button>; a non-button control is not keyboard reachable")
	}
	// The opt-out is a wrapper CLASS applied to an ancestor, so it has to be
	// looked for in the markup between the pane header and the help icon --
	// not document-wide. ContextHelpScript's own source text mentions
	// .sw-help-no-tab (that is the selector it applies tabindex=-1 with), so
	// a whole-body match would fail on the script, not on real markup.
	header := strings.Index(body, `class="sw-rep-pane-header"`)
	helpAt := strings.Index(body, `id="help-blast-radius"`)
	if header < 0 || header >= helpAt {
		t.Fatalf("could not locate the pane header before the help icon (header=%d help=%d); the tab-order check below would be scoped to nothing", header, helpAt)
	}
	if strings.Contains(body[header:helpAt], "sw-help-no-tab") {
		t.Errorf("blast-radius pane wraps the help icon in .sw-help-no-tab, which sets tabindex=-1 and removes it from the tab order")
	}

	// The deep link, and where it points. contextHelpDocURL turns the
	// path-and-fragment anchor into the published docs URL.
	//
	// It targets the RESTORE subsection, not the top of the blast-radius
	// section. The operator most likely to reach for this help is one whose
	// restore was just refused, and the explanation they need -- that a
	// preview is not a promise -- lives in that subsection. Landing them at
	// the top of the section makes them scroll for it.
	if !strings.Contains(body, "how-to/view-reports#putting-a-value-back") {
		t.Errorf("context-help popover does not deep-link to the restore subsection; body has a Read more link: %v",
			strings.Contains(body, "sw-context-help-link"))
	}

	// The handlers the button's onclick/onkeydown call must be DEFINED on
	// this page. Layout does not mount them globally, so a pane that renders
	// ContextHelp without ContextHelpScript ships a button that throws
	// ReferenceError on click -- rendered, focusable, and inert.
	if !strings.Contains(body, "window.swContextHelpToggle") {
		t.Errorf("swContextHelpToggle is not defined on this page; the help button's onclick would throw ReferenceError and the popover would never open")
	}
	if !strings.Contains(body, "window.swContextHelpClose") {
		t.Errorf("swContextHelpClose is not defined on this page; Escape from the focused help button would throw ReferenceError")
	}
}
