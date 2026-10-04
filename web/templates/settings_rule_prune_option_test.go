package templates

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/rule"
)

// Render tests for the "also delete near-duplicate backdrops on media servers"
// switch in the duplicate-images rule's Configure panel (#3138 S4a).
//
// NOTE FOR EDITORS: Tailwind scans this directory for class names, test files
// included. Assert on data- attributes, ids and aria- values only. A string
// literal shaped like a utility class would change the generated stylesheet.

// pruneSwitchRE captures the switch's opening tag (not the enable switch's).
var pruneSwitchRE = regexp.MustCompile(`<button[^>]*\bdata-prune-platform-copies\b[^>]*>`)

func renderRuleRow(t *testing.T, rl rule.Rule) string {
	t.Helper()
	var buf bytes.Buffer
	if err := ruleRow(rl, nil, true, false, false).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render ruleRow(%s): %v", rl.ID, err)
	}
	return buf.String()
}

// dupRule builds the duplicate-images rule with the given stored option and
// tolerance. Enabled is false, as seeded: the control renders the same either way.
func dupRule(pruneOn bool, tolerance float64) rule.Rule {
	return rule.Rule{
		ID:       rule.RuleImageDuplicate,
		Name:     "No duplicate images",
		Category: rule.RuleCategoryImage,
		Config:   rule.RuleConfig{Severity: "warning", Tolerance: tolerance, PrunePlatformCopies: pruneOn},
	}
}

// pruneSwitchTag returns the switch's opening tag; exactly one must render.
func pruneSwitchTag(t *testing.T, html string) string {
	t.Helper()
	tags := pruneSwitchRE.FindAllString(html, -1)
	if len(tags) != 1 {
		t.Fatalf("want exactly 1 prune switch, got %d", len(tags))
	}
	return tags[0]
}

func TestRulePruneOption_Off(t *testing.T) {
	html := renderRuleRow(t, dupRule(false, 0.90))

	if !strings.Contains(html, `id="rule-cfg-image_duplicate"`) {
		t.Fatal("image_duplicate renders no Configure panel")
	}
	tag := pruneSwitchTag(t, html)
	for _, want := range []string{
		`role="switch"`,
		`aria-checked="false"`,
		`data-initial="false"`,
		`aria-labelledby="prune-platform-copies-label"`,
		`aria-describedby="prune-platform-copies-help prune-platform-copies-note"`,
		`data-prune-confirm-title="Allow deletions on your media servers?"`,
		`data-prune-confirm-accept="Turn on"`,
		`data-prune-confirm-body="Every fix for`,
	} {
		if !strings.Contains(tag, want) {
			t.Errorf("off switch is missing %s\n%s", want, tag)
		}
	}
	for _, bad := range []string{"data-prune-blocked", "aria-disabled"} {
		if strings.Contains(tag, bad) {
			t.Errorf("off switch with an accepted tolerance carries %s\n%s", bad, tag)
		}
	}
	for _, id := range []string{"prune-platform-copies-label", "prune-platform-copies-help", "prune-platform-copies-note"} {
		if n := strings.Count(html, `id="`+id+`"`); n != 1 {
			t.Errorf("id %q appears %d times, want 1", id, n)
		}
	}
	// Label, help (its last sentence covers the rule-disabled state), threshold.
	for _, want := range []string{
		"Also delete near-duplicate backdrops on media servers",
		"Takes effect only while this rule is enabled.",
		"Similarity threshold in use: 90%",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("panel is missing text %q", want)
		}
	}
	if strings.Contains(html, "data-prune-refused") {
		t.Error("an accepted tolerance rendered the refused note")
	}
}

func TestRulePruneOption_On(t *testing.T) {
	// Precondition: the off render really is off, so "on" is not a constant.
	if off := pruneSwitchTag(t, renderRuleRow(t, dupRule(false, 0.90))); !strings.Contains(off, `aria-checked="false"`) {
		t.Fatalf("precondition: the off render is not off\n%s", off)
	}
	tag := pruneSwitchTag(t, renderRuleRow(t, dupRule(true, 0.90)))
	for _, want := range []string{`aria-checked="true"`, `data-initial="true"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("on switch is missing %s\n%s", want, tag)
		}
	}
}

func TestRulePruneOption_ToleranceCarried(t *testing.T) {
	hidden := regexp.MustCompile(`<input type="hidden" name="tolerance" value="([^"]*)"`)

	m := hidden.FindStringSubmatch(renderRuleRow(t, dupRule(true, 0.95)))
	if m == nil {
		t.Fatal("stored tolerance 0.95 rendered no hidden tolerance field; a save would reset it to the default")
	}
	if m[1] != "0.95" {
		t.Errorf("hidden tolerance = %q, want 0.95", m[1])
	}

	// An unset tolerance (0) renders none, so a save keeps it unset.
	html := renderRuleRow(t, dupRule(false, 0))
	if hidden.MatchString(html) {
		t.Error("stored tolerance 0 rendered a hidden tolerance field")
	}
	if !strings.Contains(html, "Similarity threshold in use: 90%") {
		t.Error("unset tolerance does not show the 90% default as the threshold in use")
	}

	// No VISIBLE tolerance input for this rule: the only field is the hidden one.
	if n := strings.Count(renderRuleRow(t, dupRule(true, 0.95)), `name="tolerance"`); n != 1 {
		t.Errorf("want exactly 1 tolerance field (the hidden one), got %d", n)
	}
}

func TestRulePruneOption_Refused(t *testing.T) {
	// Stored off, tolerance refused: note, block marker, locked switch.
	html := renderRuleRow(t, dupRule(false, 0.80))
	tag := pruneSwitchTag(t, html)
	for _, want := range []string{"data-prune-blocked", `aria-disabled="true"`, `aria-checked="false"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("refused, off switch is missing %s\n%s", want, tag)
		}
	}
	// The locked look, read from the class attribute only (the data-sw-* values
	// legitimately keep the pointer cursor for the switch's later unlocked state).
	cls := regexp.MustCompile(`\bclass="([^"]*)"`).FindStringSubmatch(tag)
	if cls == nil {
		t.Fatalf("locked switch has no class attribute\n%s", tag)
	}
	for _, want := range []string{"cursor-not-allowed", "opacity-50"} {
		if !strings.Contains(cls[1], want) {
			t.Errorf("locked switch is missing %s\n%s", want, tag)
		}
	}
	if strings.Contains(cls[1], "cursor-pointer") {
		t.Errorf("locked switch still shows the pointer cursor\n%s", tag)
	}
	if !strings.Contains(html, "data-prune-refused") {
		t.Error("refused tolerance rendered no refused note")
	}
	const note = "The server cleanup will not run: this rule&#39;s similarity threshold is 80%, and the cleanup only runs between 85% and 100%."
	if !strings.Contains(html, note) {
		t.Errorf("refused note text missing or malformed; want %q", note)
	}
	if strings.Contains(html, "data-prune-threshold") {
		t.Error("refused tolerance also rendered the threshold-in-use line")
	}
	if !strings.Contains(html, `name="tolerance" value="0.8"`) {
		t.Error("refused tolerance 0.80 is not carried as a hidden field")
	}

	// A refused value must never be printed rounded up to an accepted one.
	if h := renderRuleRow(t, dupRule(false, 0.849999)); !strings.Contains(h, "similarity threshold is 84.9999%, and") {
		t.Error("refused tolerance 0.849999 is not shown as 84.9999%")
	}

	// Stored ON and refused: still marked, NOT locked (off must always work).
	onTag := pruneSwitchTag(t, renderRuleRow(t, dupRule(true, 0.80)))
	if !strings.Contains(onTag, "data-prune-blocked") || !strings.Contains(onTag, `aria-checked="true"`) {
		t.Errorf("refused, on switch must be on and carry the block marker\n%s", onTag)
	}
	if strings.Contains(onTag, "aria-disabled") {
		t.Errorf("refused, on switch is locked; turning it off must stay possible\n%s", onTag)
	}
}

func TestRulePruneOption_OtherRulesUntouched(t *testing.T) {
	html := renderRuleRow(t, rule.Rule{
		ID:       "thumb_square",
		Name:     "Square thumbnail",
		Category: rule.RuleCategoryImage,
		Config:   rule.RuleConfig{Severity: "warning", Tolerance: 0.1},
	})
	// Precondition: the form rendered, so the absence below is not an empty page.
	if !strings.Contains(html, `id="rule-cfg-thumb_square"`) {
		t.Fatal("precondition: thumb_square rendered no Configure panel")
	}
	if pruneSwitchRE.MatchString(html) || strings.Contains(html, "prune-platform-copies") {
		t.Error("thumb_square rendered the prune switch")
	}
	if strings.Contains(html, `type="hidden" name="tolerance"`) {
		t.Error("thumb_square rendered the hidden tolerance field; it has its own visible input")
	}
}

func TestSimilarityPercent(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.9, "90%"},
		{1, "100%"},
		{0, "0%"},
		{0.849999, "84.9999%"},
		{0.005, "0.5%"},
		{-0.5, "-50%"},
		{-0.05, "-5%"},
	} {
		if got := similarityPercent(tc.in); got != tc.want {
			t.Errorf("similarityPercent(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
