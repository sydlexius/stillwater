package components

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// TestFilterItemSingle_Selected verifies the selected state renders the
// include checkmark icon, marks data-filter-selected="true", and writes the
// count badge when the count is positive.
func TestFilterItemSingle_Selected(t *testing.T) {
	var buf bytes.Buffer
	if err := FilterItemSingle("flyout-id", "severity", "error", "Error", true, 12).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	wantSubs := []string{
		`data-filter-mode="single"`,
		`data-filter-key="severity"`,
		`data-filter-value="error"`,
		`data-filter-selected="true"`,
		`aria-pressed="true"`,
		`sw-filter-item-icon`,
		`Error`,
		`12`,
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in rendered output:\n%s", s, out)
		}
	}
}

// TestFilterItemSingle_Unselected verifies the unselected state omits the
// icon span and the count badge when count is zero.
func TestFilterItemSingle_Unselected(t *testing.T) {
	var buf bytes.Buffer
	if err := FilterItemSingle("flyout-id", "category", "image", "Image", false, 0).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, `data-filter-selected="false"`) {
		t.Errorf("expected data-filter-selected=\"false\"; got:\n%s", out)
	}
	if !strings.Contains(out, `aria-pressed="false"`) {
		t.Errorf("expected aria-pressed=\"false\"; got:\n%s", out)
	}
	if strings.Contains(out, "sw-filter-item-icon") {
		t.Errorf("unselected chip must not render an icon span; got:\n%s", out)
	}
	// Zero count is omitted -- otherwise every chip on a fresh page would
	// carry a meaningless "0" tail.
	if strings.Contains(out, ">0<") {
		t.Errorf("zero-count chip must not render a count badge; got:\n%s", out)
	}
}

// TestFilterRange verifies the paired min/max number inputs carry the
// expected data-filter-key suffixes and reflect non-zero values.
func TestFilterRange(t *testing.T) {
	var buf bytes.Buffer
	if err := FilterRange("flyout-id", "health", "Health", 40, 80).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	wantSubs := []string{
		`data-filter-range-key="health"`,
		`data-filter-range-bound="min"`,
		`data-filter-key="health_min"`,
		`value="40"`,
		`data-filter-range-bound="max"`,
		`data-filter-key="health_max"`,
		`value="80"`,
		`Health`,
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in rendered output:\n%s", s, out)
		}
	}
}

// TestFilterRange_ZeroOmitsValue confirms a zero bound renders as an empty
// input rather than "value=\"0\"", so blank inputs do not pre-populate.
func TestFilterRange_ZeroOmitsValue(t *testing.T) {
	var buf bytes.Buffer
	if err := FilterRange("flyout-id", "health", "Health", 0, 0).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `value="0"`) {
		t.Errorf("zero bound must render empty value, not \"0\"; got:\n%s", out)
	}
}

// TestFilterChip verifies a chip renders the label, includes an X-mark icon,
// and wires the dismiss button to the shared DismissFilterChip script with
// the supplied key + targetSel. The aria-label is sourced through the
// "common.remove_filter" i18n key so the assistive text follows the request
// locale (CR feedback on PR #1653).
func TestFilterChip(t *testing.T) {
	var buf bytes.Buffer
	// Provide a translator on the context so tf() can interpolate the chip
	// label into the localized "Remove %s filter" template.
	tr := i18n.NewTranslator("en", map[string]string{
		"common.remove_filter": "Remove %s filter",
	})
	ctx := i18n.WithTranslator(context.Background(), tr)
	if err := FilterChip("Error", "severity", "#action-queue", "").Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	wantSubs := []string{
		"Error",
		`aria-label="Remove Error filter"`,
		"DismissFilterChip", // generated script function name
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in rendered output:\n%s", s, out)
		}
	}
}

// TestActiveFilters_Empty verifies that an empty chip slice yields no chip
// row at all (the wrapping <div> only appears when there is at least one chip).
func TestActiveFilters_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := ActiveFilters("#target", "/clear", "Clear All", "Active:", nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	if out != "" {
		t.Errorf("expected no markup for empty chip slice; got: %q", out)
	}
}

// TestActiveFilters_RendersChipsAndClearAll renders multiple chips plus the
// Clear All anchor and verifies the row label, every chip label, and the
// clearAll href all appear.
func TestActiveFilters_RendersChipsAndClearAll(t *testing.T) {
	chips := []FilterChipSpec{
		{Label: "Error", Key: "severity"},
		{Label: "Library 1", Key: "library_id"},
	}
	var buf bytes.Buffer
	if err := ActiveFilters("#action-queue", "/clear-all", "Clear all", "Active:", chips).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	wantSubs := []string{
		"Active:",
		"Error",
		"Library 1",
		`href="/clear-all"`,
		"Clear all",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in rendered output:\n%s", s, out)
		}
	}
}

// TestActiveFilters_PerChipTargetSelOverride confirms that a FilterChipSpec
// with a non-empty TargetSel overrides the row-wide default, while chips
// with an empty TargetSel inherit the default. The selector is embedded in
// the generated DismissFilterChip onclick payload so we test by escape-aware
// substring presence rather than exact-match.
func TestActiveFilters_PerChipTargetSelOverride(t *testing.T) {
	chips := []FilterChipSpec{
		{Label: "Inherits", Key: "severity"},
		{Label: "Overrides", Key: "category", TargetSel: "#custom-target"},
	}
	var buf bytes.Buffer
	if err := ActiveFilters("#default-target", "", "", "Active:", chips).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// templ encodes the targetSel string inside the generated onclick
	// payload; both selectors must be present (either raw or HTML-escaped).
	for _, s := range []string{"default-target", "custom-target"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in rendered output:\n%s", s, out)
		}
	}
}

// TestDismissFilterChip_ScriptGuardsTheSelectAssignment pins the emitted SCRIPT
// BODY: that `opts.select` is assigned only inside a guard on selectSel.
//
// SCOPE, STATED HONESTLY: this guards the emitted text, not behavior. templ
// inlines a script body verbatim, so this assertion passes for any code that
// merely CONTAINS the matched characters. Two mutations survive it:
//
//  1. `opts.select = selectSel || '#artist-content'` inside the guard
//  2. `else { opts.select = 'body' }` after the guard
//
// The second makes EVERY caller emit select:'body' and this test stays green.
// The behavioral coverage lives in tests/unit/filter-chip-dismiss-select.test.js,
// which EXECUTES the generated function against a stubbed htmx and asserts on
// hasOwnProperty(opts, 'select'). Mutation 2 fails there. This test is kept as
// the cheap structural companion, not as the proof; the ARGUMENTS each caller
// passes are covered by TestFilterChipDismiss_SelectSelForwarding below.
func TestDismissFilterChip_ScriptGuardsTheSelectAssignment(t *testing.T) {
	tr := i18n.NewTranslator("en", map[string]string{
		"common.remove_filter": "Remove %s filter",
	})
	ctx := i18n.WithTranslator(context.Background(), tr)

	var buf bytes.Buffer
	if err := FilterChip("Error", "severity", "#action-queue", "").Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Precondition: the render produced the dismiss wiring. Without it the
	// assertions below would be satisfied by empty output.
	if !strings.Contains(out, "DismissFilterChip") {
		t.Fatalf("the render did not emit the DismissFilterChip wiring; the assertions below are vacuous:\n%s", out)
	}

	// The select guard must be present in the emitted script.
	//
	// It is DEFENSIVE, not load-bearing: htmx normalizes a falsy select to null
	// (`const F=i.select||null`, `if(i){d=i}`, `d||b`, `if(g.select)`), so a
	// present-but-empty select is indistinguishable from an absent one. What
	// the guard buys is an options object whose keys all carry meaning, which
	// is what lets the behavioral test assert on hasOwnProperty rather than on
	// a value that would mean nothing either way.
	if !strings.Contains(out, "if (selectSel)") {
		t.Errorf("the emitted DismissFilterChip script does not guard the select assignment, so the emitted "+
			"options object carries a `select` key for callers that never asked for one. Script:\n%s", out)
	}

	// The htmx guard must precede the URL mutation.
	//
	// Failing loudly and failing WITHOUT SIDE EFFECTS are different properties.
	// A guard below the pushState reports the missing dependency correctly and
	// still leaves the address bar stripped of a filter that nothing reloaded,
	// so the chip, the URL and the rendered rows disagree and a later manual
	// refresh applies a state the operator never saw applied. Asserted here as
	// TEXT ORDER; the executed version is in the js tier, which drives the
	// script with htmx absent and asserts history.pushState never ran.
	//
	// COMMENTS ARE STRIPPED FIRST. templ inlines a script body verbatim,
	// comments included, and both of these scripts DISCUSS the ordering in
	// prose sitting above the code. Matching the raw output finds the prose
	// mention of history.pushState above the guard and reports a violation on
	// correct code.
	code := stripJSLineComments(out)
	guardAt := strings.Index(code, "if (!window.htmx)")
	pushAt := strings.Index(code, "history.pushState")
	if guardAt < 0 {
		t.Fatalf("the emitted DismissFilterChip script has no htmx guard at all:\n%s", out)
	}
	if pushAt < 0 {
		t.Fatalf("the emitted DismissFilterChip script no longer calls history.pushState; this test's "+
			"ordering assertion is vacuous:\n%s", out)
	}
	if guardAt > pushAt {
		t.Errorf("the htmx guard is emitted AFTER history.pushState, so a page without htmx loses the filter "+
			"from its URL while nothing reloads. Script:\n%s", out)
	}
}

// stripJSLineComments drops whole-line `//` comments from rendered output so an
// ordering assertion measures CODE position rather than a prose mention of the
// same symbol. templ ships script comments verbatim, so the two are otherwise
// indistinguishable to a substring search.
func stripJSLineComments(out string) string {
	lines := strings.Split(out, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// TestFilterChipDismiss_SelectSelForwarding pins, for every branch of
// FilterChip2, the ARGUMENTS each chip's dismiss call site receives.
//
// Asserted at the CALL SITE, not in the script body: the script body is emitted
// once and always contains the guarded `opts.select = selectSel` assignment, so
// searching the whole output for "opts.select" finds the shared definition and
// proves nothing about any caller. What distinguishes a chip that opted in from
// one that did not is the ARGUMENT it passes.
//
// FilterChipSpec is ONE type serving BOTH branches: Value == "" routes to
// FilterChip/DismissFilterChip, Value != "" to DismissFilterValueChip. A branch
// that accepts SelectSel and drops it renders a perfectly ordinary chip whose
// dismiss swaps the whole response body -- the field accepted, ignored, and
// dropped with no error anywhere. Nothing throws, the chip looks right, it
// dismisses, and the only symptom is a duplicated page injected into the
// target. Both branches are rowed here so neither can regress unobserved.
//
// Every row renders through ActiveFilters, which is the entry point real
// callers use, so the row-wide target and the per-chip argument list are
// exercised together.
//
// Mutation proving teeth: passing a literal "" instead of c.SelectSel in either
// arm of FilterChip2 fails this and nothing else in the repo.
func TestFilterChipDismiss_SelectSelForwarding(t *testing.T) {
	tests := []struct {
		name string
		// rowTarget is ActiveFilters' row-wide target selector.
		rowTarget string
		chips     []FilterChipSpec
		// script is the dismiss script the chips must route to; the other one
		// must be absent, which is what proves the intended branch was taken.
		script string
		// wantArgs is the full argument list per chip, in render order.
		// DismissFilterChip:      key, targetSel, selectSel
		// DismissFilterValueChip: key, prefixedValue, targetSel, selectSel
		//
		// Whole lists rather than a single index: checking only the select lets
		// a change that passes SelectSel as BOTH target and select survive, and
		// such a chip reloads into whatever SelectSel names while ignoring the
		// target its caller asked for.
		wantArgs [][]string
	}{
		{
			// The shape a caller on a route with no fragment handler builds:
			// SelectSel set, Value empty, so it takes the single-value branch.
			name:      "single-value branch forwards SelectSel",
			rowTarget: "#report-pane",
			chips:     []FilterChipSpec{{Key: "class", Label: "Class: Blanked", SelectSel: "#report-pane"}},
			script:    "DismissFilterChip",
			wantArgs:  [][]string{{"class", "#report-pane", "#report-pane"}},
		},
		{
			// A selector containing a comma is a legitimate CSS selector list.
			// It is rowed because the argument extractor below has to split a
			// rendered argument list, and a naive split on "," reports four
			// arguments for a three-argument call -- a confusing len(args)
			// failure that has nothing to do with the code under test.
			name:      "single-value branch forwards a comma-bearing selector intact",
			rowTarget: "#report-pane",
			chips:     []FilterChipSpec{{Key: "class", Label: "Class: Blanked", SelectSel: "#report-pane, #report-summary"}},
			script:    "DismissFilterChip",
			wantArgs:  [][]string{{"class", "#report-pane", "#report-pane, #report-summary"}},
		},
		{
			name:      "multi-value branch forwards SelectSel",
			rowTarget: "#action-queue",
			chips:     []FilterChipSpec{{Key: "severity", Label: "Error", Value: "error", SelectSel: "#report-pane"}},
			script:    "DismissFilterValueChip",
			wantArgs:  [][]string{{"severity", "+error", "#action-queue", "#report-pane"}},
		},
		{
			// A spec that asks for nothing still gets nothing, on each branch,
			// so the capability did not hand every existing caller a select it
			// never requested.
			name:      "single-value branch declaring no SelectSel passes an empty argument",
			rowTarget: "#compliance-results",
			chips:     []FilterChipSpec{{Key: "status", Label: "Non-Compliant"}},
			script:    "DismissFilterChip",
			wantArgs:  [][]string{{"status", "#compliance-results", ""}},
		},
		{
			name:      "multi-value branch declaring no SelectSel passes an empty argument",
			rowTarget: "#action-queue",
			chips:     []FilterChipSpec{{Key: "severity", Label: "Error", Value: "error"}},
			script:    "DismissFilterValueChip",
			wantArgs:  [][]string{{"severity", "+error", "#action-queue", ""}},
		},
		{
			// The pre-existing live caller, shaped exactly like
			// complianceActiveChips' output: Label + Key, everything else zero.
			// If a future change made SelectSel required, or gave it a
			// non-empty default, this row fails.
			name:      "existing multi-chip caller emits no select on any chip",
			rowTarget: "#compliance-results",
			chips: []FilterChipSpec{
				{Key: "status", Label: "Non-Compliant"},
				{Key: "filter", Label: "Missing Metadata"},
				{Key: "library_id", Label: "Music"},
			},
			script: "DismissFilterChip",
			wantArgs: [][]string{
				{"status", "#compliance-results", ""},
				{"filter", "#compliance-results", ""},
				{"library_id", "#compliance-results", ""},
			},
		},
	}

	tr := i18n.NewTranslator("en", map[string]string{
		"common.remove_filter": "Remove %s filter",
	})
	ctx := i18n.WithTranslator(context.Background(), tr)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := ActiveFilters(tt.rowTarget, "", "", "Active:", tt.chips).Render(ctx, &buf); err != nil {
				t.Fatalf("render: %v", err)
			}
			out := buf.String()

			// Precondition: every chip rendered. Without it the argument
			// assertions could be satisfied by output that dropped a chip.
			for _, c := range tt.chips {
				if !strings.Contains(out, c.Label) {
					t.Fatalf("chip %q did not render; the assertions below would be vacuous:\n%s", c.Label, out)
				}
			}

			// Precondition: the intended branch was taken. A spec that fell
			// through to the other branch would be covered by a different row,
			// and this one would pass for the wrong reason.
			//
			// Matched on CALL SITES, not on the name appearing anywhere: each
			// script's inlined comments name its sibling, so a raw substring
			// search finds the other name in every render.
			other := otherDismissScript(tt.script)
			if n := len(dismissCallArgs(t, out, other)); n > 0 {
				t.Fatalf("the chips produced %d %s call site(s); this row exercises the %s branch:\n%s",
					n, other, tt.script, out)
			}

			got := dismissCallArgs(t, out, tt.script)
			if !reflect.DeepEqual(got, tt.wantArgs) {
				t.Errorf("%s call-site arguments\n got: %#v\nwant: %#v\n\nA branch that accepts SelectSel and "+
					"drops it renders a working chip whose dismiss swaps the whole response body, injecting a "+
					"second copy of the page into the target. A branch that passes one where none was asked for "+
					"changes what htmx extracts for a caller that never opted in.\nRendered:\n%s",
					tt.script, got, tt.wantArgs, out)
			}
		})
	}
}

// otherDismissScript names the dismiss script a given row must NOT emit, so a
// row asserts which branch of FilterChip2 it took rather than assuming it.
func otherDismissScript(script string) string {
	if script == "DismissFilterChip" {
		return "DismissFilterValueChip"
	}
	return "DismissFilterChip"
}

// dismissCallArgs extracts the argument lists from every generated onclick call
// to the named dismiss script in rendered output. Parameterized on the script
// name because the two extractors it replaced were identical but for that name.
//
// templ builds an onclick attribute by JSON-encoding each argument and then
// HTML-escaping the result, so the emitted form is
// onclick="__templ_DismissFilterChip_xxxx(&#34;key&#34;,&#34;#target&#34;,&#34;&#34;)".
// This reverses exactly that: unescape, wrap the comma-separated JSON values in
// brackets, and decode as a JSON array.
//
// DECODED, NOT SPLIT ON ",". A CSS selector list is a legitimate selector
// ("#a, #b"), and splitting the raw argument text on a bare comma reports one
// argument too many for such a call -- surfacing as a confusing len(args)
// mismatch in a helper the whole suite leans on rather than as anything to do
// with the code under test. JSON decoding also restores any escaped quote or
// backslash a selector carries, which no split can do.
//
// Anchored on `onclick="` so this matches CALL SITES only: an unanchored
// pattern also matches the script's own `function __templ_..._xxxx(...)`
// definition line, which makes the count come back one too high. Terminated on
// `)"` rather than the first `)` so an argument containing a parenthesis
// (":not(.x)" is a selector too) is not truncated -- a literal `"` inside an
// argument is escaped to `&#34;`, so `)"` cannot occur mid-attribute.
func dismissCallArgs(t *testing.T, out, script string) [][]string {
	t.Helper()
	re := regexp.MustCompile(`onclick="__templ_` + regexp.QuoteMeta(script) + `_[0-9a-f]+\((.*?)\)"`)
	var calls [][]string
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		raw := html.UnescapeString(m[1])
		var args []string
		if err := json.Unmarshal([]byte("["+raw+"]"), &args); err != nil {
			t.Fatalf("could not decode the %s argument list %q as JSON: %v. templ JSON-encodes each script "+
				"argument, so a decode failure means the emitted form changed and this helper no longer reads "+
				"it.", script, raw, err)
		}
		calls = append(calls, args)
	}
	return calls
}
