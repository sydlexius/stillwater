package components

import (
	"bytes"
	"context"
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

// TestDismissFilterChip_SelectIsGenuinelyOptional pins the RENDERED SHAPE of
// the `select` parameter added for #3093.
//
// SCOPE, STATED HONESTLY: this test guards the emitted text, not behavior. It
// proves the guard is present in the script and that a supplied selector
// reaches the payload while an omitted one does not. It CANNOT prove what the
// script does, because templ inlines a script body verbatim, so any of these
// assertions passes for code that merely CONTAINS the matched characters.
//
// That is not hypothetical. Two mutations survived this file:
//  1. `opts.select = selectSel || '#artist-content'` inside the guard
//  2. `else { opts.select = 'body' }` after the guard
//
// The second makes EVERY caller emit select:'body' and this test stayed green.
// The behavioral coverage lives in tests/unit/filter-chip-dismiss-select.test.js,
// which EXECUTES the generated function against a stubbed htmx and asserts on
// hasOwnProperty(opts, 'select'). Mutation 2 fails there. This test is kept as
// the cheap structural companion, not as the proof.
//
// WHO CALLS THIS. DismissFilterChip is shared through ActiveFilters/FilterChip2,
// but the only LIVE caller today is the compliance report
// (web/templates/compliance.templ:154). The artists and logs panes mention the
// script only in comments explaining why they do NOT use it. An earlier version
// of this comment claimed five callers; that was wrong, and the count matters
// because it is the whole blast radius of a change to the shared default.
func TestDismissFilterChip_SelectIsGenuinelyOptional(t *testing.T) {
	tr := i18n.NewTranslator("en", map[string]string{
		"common.remove_filter": "Remove %s filter",
	})
	ctx := i18n.WithTranslator(context.Background(), tr)

	var omitted bytes.Buffer
	if err := FilterChip("Error", "severity", "#action-queue", "").Render(ctx, &omitted); err != nil {
		t.Fatalf("render (omitted select): %v", err)
	}
	var supplied bytes.Buffer
	if err := FilterChip("Error", "severity", "#blast-radius-pane", "#blast-radius-pane").Render(ctx, &supplied); err != nil {
		t.Fatalf("render (supplied select): %v", err)
	}

	// Precondition: both renders produced the dismiss wiring. Without it every
	// assertion below would be satisfied by empty output.
	for name, out := range map[string]string{"omitted": omitted.String(), "supplied": supplied.String()} {
		if !strings.Contains(out, "DismissFilterChip") {
			t.Fatalf("%s render did not emit the DismissFilterChip wiring; the assertions below are vacuous:\n%s", name, out)
		}
	}

	// The guard must be present in the emitted script.
	//
	// It is DEFENSIVE, not load-bearing: htmx 2.0.8 normalizes a falsy select to
	// null (`const F=i.select||null`, `if(i){d=i}`, `d||b`, `if(g.select)`), so
	// a present-but-empty select is indistinguishable from an absent one --
	// verified by reading the vendored file and by driving a real Chromium page
	// where {}, {select:''} and a real selector were compared. What the guard
	// buys is an options object whose keys all carry meaning, which is what lets
	// the behavioral test assert on hasOwnProperty rather than on a value that
	// would mean nothing either way.
	if !strings.Contains(omitted.String(), "if (selectSel)") {
		t.Errorf("the emitted DismissFilterChip script does not guard the select assignment, so the emitted "+
			"options object carries a `select` key for callers that never asked for one. Script:\n%s",
			omitted.String())
	}

	// The caller that DOES ask for one has its selector carried through, and
	// the omitted case does not smuggle one in from anywhere.
	if !strings.Contains(supplied.String(), "blast-radius-pane") {
		t.Errorf("a supplied select selector did not reach the emitted onclick payload:\n%s", supplied.String())
	}
	if strings.Contains(omitted.String(), "blast-radius-pane") {
		t.Errorf("the omitted-select render leaked a selector into its payload:\n%s", omitted.String())
	}
}

// TestActiveFilters_ExistingCallersEmitNoSelect pins the pre-existing caller's
// shape at the level it actually uses: a FilterChipSpec with no SelectSel,
// routed through ActiveFilters.
//
// The specs here mirror the real ones in compliance.templ
// (complianceActiveChips), which is the only live ActiveFilters caller in the
// app: Label + Key, everything else zero. If a future change made SelectSel
// required, or gave it a non-empty default, this fails.
func TestActiveFilters_ExistingCallersEmitNoSelect(t *testing.T) {
	// Shaped exactly like complianceActiveChips' output.
	chips := []FilterChipSpec{
		{Key: "status", Label: "Non-Compliant"},
		{Key: "filter", Label: "Missing Metadata"},
		{Key: "library_id", Label: "Music"},
	}
	// Precondition: none of these declares a select, which is the property
	// under test. A fixture that set one would test the opposite case.
	for i := range chips {
		if chips[i].SelectSel != "" {
			t.Fatalf("fixture chip %d declares SelectSel=%q; this test is about callers that do NOT", i, chips[i].SelectSel)
		}
	}

	var buf bytes.Buffer
	if err := ActiveFilters("#compliance-results", "/clear", "Clear all", "Active:", chips).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Precondition: the chips rendered at all.
	for _, want := range []string{"Non-Compliant", "Missing Metadata", "Music", "compliance-results"} {
		if !strings.Contains(out, want) {
			t.Fatalf("chip row did not render %q; the assertion below would be vacuous:\n%s", want, out)
		}
	}

	// Every chip's ONCLICK CALL passes an empty third argument.
	//
	// Asserting on the call site, not the script body: the script body is
	// emitted once and always contains the guarded `opts.select = selectSel`
	// assignment, so searching the whole output for "opts.select" finds the
	// shared definition and proves nothing about any caller. What distinguishes
	// a pre-existing caller from an opted-in one is the ARGUMENT it passes.
	// (This assertion was first written against the script body and failed on
	// its first run for exactly that reason, which is how the distinction was
	// found -- and it is the same confusion that later let two behavior-changing
	// mutations survive this file. See the header above.)
	calls := dismissChipCallArgs(out)
	if len(calls) != len(chips) {
		t.Fatalf("found %d DismissFilterChip call sites, want %d (one per chip); the assertion below would "+
			"not cover every chip:\n%s", len(calls), len(chips), out)
	}
	for i, args := range calls {
		if len(args) != 3 {
			t.Fatalf("call site %d has %d arguments, want 3 (key, targetSel, selectSel): %v", i, len(args), args)
		}
		if args[2] != "" {
			t.Errorf("chip %d passes selectSel=%q despite declaring none. A non-empty select on a caller that "+
				"did not ask for one changes what htmx swaps into that caller's content region.", i, args[2])
		}
		if args[1] != "#compliance-results" {
			t.Errorf("chip %d targets %q, want the row-wide %q", i, args[1], "#compliance-results")
		}
	}
}

// dismissChipCallArgs extracts the argument lists from every generated
// DismissFilterChip onclick call in rendered output.
//
// templ HTML-escapes the quotes inside an onclick attribute, so the emitted
// form is onclick="__templ_DismissFilterChip_xxxx(&#34;key&#34;,&#34;#target&#34;,&#34;&#34;)".
// Anchored on `onclick="` so this matches CALL SITES only: an unanchored
// pattern also matches the script's own `function __templ_DismissFilterChip_xxxx(...)`
// definition line, which made the count come back one too high on first run.
func dismissChipCallArgs(out string) [][]string {
	re := regexp.MustCompile(`onclick="__templ_DismissFilterChip_[0-9a-f]+\(([^)]*)\)"`)
	var calls [][]string
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		raw := strings.ReplaceAll(m[1], "&#34;", `"`)
		var args []string
		for _, part := range strings.Split(raw, ",") {
			args = append(args, strings.Trim(strings.TrimSpace(part), `"`))
		}
		calls = append(calls, args)
	}
	return calls
}

// TestFilterChip2_MultiValueBranchForwardsSelectSel closes a silent-failure gap
// in the tri-state (multi-value) chip branch.
//
// FilterChipSpec is ONE type serving BOTH branches of FilterChip2: the
// single-value branch routes to DismissFilterChip, the Value != "" branch to
// DismissFilterValueChip. When the select parameter was added, only the first
// branch was wired. A spec that set both Value and SelectSel therefore rendered
// a perfectly ordinary chip whose dismiss did a bare full-page swap -- the field
// was accepted, ignored, and dropped with no error anywhere.
//
// That is the silent-failure shape this repo forbids, and the failure mode is
// the nastiest kind: nothing throws, the chip looks right, it dismisses, and the
// only symptom is a duplicated page injected into the target. It reintroduced
// the exact defect the parameter exists to prevent, on the branch nobody was
// looking at.
//
// Asserted at the CALL SITE, like its single-value sibling: the emitted script
// body is shared, so only the argument distinguishes a forwarded selector from
// a dropped one. The behavioral half (that the forwarded value actually reaches
// htmx's options object) lives in tests/unit/filter-chip-dismiss-select.test.js.
//
// Mutation proving teeth: reverting FilterChip2's else-branch call to
// DismissFilterValueChip(c.Key, prefixed, targetSel) fails this.
func TestFilterChip2_MultiValueBranchForwardsSelectSel(t *testing.T) {
	tr := i18n.NewTranslator("en", map[string]string{
		"common.remove_filter": "Remove %s filter",
	})
	ctx := i18n.WithTranslator(context.Background(), tr)

	// Value != "" selects the tri-state branch; SelectSel is what must survive it.
	spec := FilterChipSpec{Key: "severity", Label: "Error", Value: "error", SelectSel: "#blast-radius-pane"}
	var buf bytes.Buffer
	if err := FilterChip2(spec, "#action-queue").Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Precondition: this really is the multi-value branch. If the spec fell
	// through to the single-value one, the assertion below would pass for the
	// wrong reason and the gap would stay open.
	if !strings.Contains(out, "DismissFilterValueChip") {
		t.Fatalf("a spec with Value=%q did not route to the multi-value branch; this test is not exercising "+
			"the branch it exists to cover:\n%s", spec.Value, out)
	}

	calls := dismissValueChipCallArgs(out)
	if len(calls) != 1 {
		t.Fatalf("found %d DismissFilterValueChip call sites, want exactly 1:\n%s", len(calls), out)
	}
	args := calls[0]
	if len(args) != 4 {
		t.Fatalf("call site has %d arguments, want 4 (key, prefixedValue, targetSel, selectSel): %v", len(args), args)
	}
	// args[2] is the TARGET, args[3] the SELECT. Checking only the select lets a
	// change that passes c.SelectSel as BOTH survive: the chip would then reload
	// into whatever SelectSel names and ignore the row-wide target entirely. Its
	// single-value sibling checks the equivalent position for the same reason.
	if args[2] != "#action-queue" {
		t.Errorf("the multi-value chip targets %q, want the row-wide %q; a chip that reloads into its own "+
			"select selector ignores the target its caller asked for", args[2], "#action-queue")
	}
	if args[3] != "#blast-radius-pane" {
		t.Errorf("the multi-value chip passes selectSel=%q, want %q. A spec that sets SelectSel alongside "+
			"Value renders a working chip whose dismiss swaps the whole response body -- silently "+
			"reintroducing the duplicated-page defect on the branch nobody looks at.", args[3], "#blast-radius-pane")
	}

	// And a spec that asks for nothing still gets nothing, so the fix did not
	// hand every tri-state caller a select they never requested.
	var bare bytes.Buffer
	if err := FilterChip2(FilterChipSpec{Key: "severity", Label: "Error", Value: "error"}, "#action-queue").Render(ctx, &bare); err != nil {
		t.Fatalf("render (no SelectSel): %v", err)
	}
	bareCalls := dismissValueChipCallArgs(bare.String())
	if len(bareCalls) != 1 {
		t.Fatalf("found %d call sites for the bare spec, want 1", len(bareCalls))
	}
	if bareCalls[0][3] != "" {
		t.Errorf("a tri-state chip declaring no SelectSel passes %q; it must pass an empty argument",
			bareCalls[0][3])
	}
}

// dismissValueChipCallArgs is the DismissFilterValueChip counterpart of
// dismissChipCallArgs. Anchored on `onclick="` for the same reason: an
// unanchored pattern also matches the script's own function definition line.
func dismissValueChipCallArgs(out string) [][]string {
	re := regexp.MustCompile(`onclick="__templ_DismissFilterValueChip_[0-9a-f]+\(([^)]*)\)"`)
	var calls [][]string
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		raw := strings.ReplaceAll(m[1], "&#34;", `"`)
		var args []string
		for _, part := range strings.Split(raw, ",") {
			args = append(args, strings.Trim(strings.TrimSpace(part), `"`))
		}
		calls = append(calls, args)
	}
	return calls
}

// TestFilterChip2_SingleValueBranchForwardsSelectSel is the sibling of the
// multi-value test above, and it covers THE BRANCH THE FIRST CONSUMER ACTUALLY
// TAKES.
//
// blastRadiusChips builds specs with SelectSel set and Value EMPTY, so every
// chip on the blast-radius pane routes through `c.Value == ""` to FilterChip.
// The multi-value test proved forwarding on the tri-state branch; this one was
// missing, and its absence meant a change dropping SelectSel on the
// single-value branch left the whole suite green while restoring the
// full-page-injection defect for the one caller that exists:
//
//	baseline  ARGS: [[class #blast-radius-pane #blast-radius-pane]]
//	mutated   ARGS: [[class #blast-radius-pane <empty>]]
//
// The gap was closed on the branch nobody uses and left open on the branch the
// consumer takes, which is the more dangerous half of the same defect.
//
// Mutation proving teeth: changing FilterChip2's `c.Value == ""` arm to pass a
// literal "" instead of c.SelectSel fails this and nothing else in the repo.
func TestFilterChip2_SingleValueBranchForwardsSelectSel(t *testing.T) {
	tr := i18n.NewTranslator("en", map[string]string{
		"common.remove_filter": "Remove %s filter",
	})
	ctx := i18n.WithTranslator(context.Background(), tr)

	// Shaped exactly like blastRadiusChips' output: SelectSel set, Value empty.
	spec := FilterChipSpec{Key: "class", Label: "Class: Blanked", SelectSel: "#blast-radius-pane"}
	var buf bytes.Buffer
	if err := FilterChip2(spec, "#blast-radius-pane").Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Precondition: this really is the SINGLE-value branch. A spec that fell
	// through to the tri-state one would be covered by the other test, and this
	// one would pass for the wrong reason.
	if strings.Contains(out, "DismissFilterValueChip") {
		t.Fatalf("a spec with an empty Value routed to the multi-value branch; this test is not exercising "+
			"the branch it exists to cover:\n%s", out)
	}

	calls := dismissChipCallArgs(out)
	if len(calls) != 1 {
		t.Fatalf("found %d DismissFilterChip call sites, want exactly 1:\n%s", len(calls), out)
	}
	args := calls[0]
	if len(args) != 3 {
		t.Fatalf("call site has %d arguments, want 3 (key, targetSel, selectSel): %v", len(args), args)
	}
	if args[1] != "#blast-radius-pane" {
		t.Errorf("the chip targets %q, want %q", args[1], "#blast-radius-pane")
	}
	if args[2] != "#blast-radius-pane" {
		t.Errorf("the single-value chip passes selectSel=%q, want %q. Every blast-radius chip takes this "+
			"branch, so dropping the select here means the dismiss swaps the whole response body: two panes, "+
			"~71 duplicated DOM ids, and a stale caveat band standing over freshly loaded rows.",
			args[2], "#blast-radius-pane")
	}

	// And a spec asking for nothing still gets nothing, so the fix did not hand
	// every single-value caller a select it never requested.
	var bare bytes.Buffer
	if err := FilterChip2(FilterChipSpec{Key: "status", Label: "Non-Compliant"}, "#compliance-results").Render(ctx, &bare); err != nil {
		t.Fatalf("render (no SelectSel): %v", err)
	}
	bareCalls := dismissChipCallArgs(bare.String())
	if len(bareCalls) != 1 {
		t.Fatalf("found %d call sites for the bare spec, want 1", len(bareCalls))
	}
	if bareCalls[0][2] != "" {
		t.Errorf("a chip declaring no SelectSel passes %q; it must pass an empty argument", bareCalls[0][2])
	}
}
