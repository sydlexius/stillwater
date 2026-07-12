package static

// Structural guards for the seed-and-derive token layer (M55 #2377).
//
// These are the CSS analog of a compile error. The bug that motivated #2377
// was that --sw-danger was REFERENCED by input.css but never DECLARED anywhere:
// it survived only on a var() fallback, so nothing failed and the wrong color
// shipped. No Go compiler and no unit test could see that, and the stylelint
// gate that catches it permanently is a separate PR (#2402). Until it lands --
// and afterwards, as a cheap belt-and-braces that runs in the normal `go test`
// path -- these tests hold the invariants.
//
// The invariants:
//  1. Every --sw-* / --swd-* custom property that input.css USES is DECLARED in
//     design-tokens.css. This is the --sw-danger bug, made impossible.
//  2. input.css DECLARES no theme tokens at all. Tokens live in design-tokens.css
//     (the stylelint gate's importFrom is that single file, so a token declared
//     in input.css would be reported as unknown). Component-scoped REBINDS of an
//     already-declared token stay legal -- that is how --sw-halo-hue makes
//     destructive work -- so the check is "declares a token that exists nowhere
//     else", not "contains a -- anywhere".
//  3. The accent literal is gone. A rule that retypes rgba(59, 130, 246, ...)
//     has escaped the halo, which is the drift #2377 exists to stop.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	inputCSSPath  = "css/input.css"
	tokensCSSPath = "css/design-tokens.css"
)

var (
	// A declaration: `  --sw-foo: value;` (start of a line, before the colon).
	declRE = regexp.MustCompile(`(?m)^\s*(--sw[a-z0-9-]*)\s*:`)
	// A usage: `var(--sw-foo` / `var(--sw-foo, fallback)`.
	useRE = regexp.MustCompile(`var\(\s*(--sw[a-z0-9-]*)`)
	// Comments, stripped before scanning so prose never counts as code.
	commentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// The accent literal the halo replaced. Matches only the rgba() form,
	// deliberately NOT the hex form (#3b82f6): widening this now would turn a
	// green branch red against pre-existing, in-scope-for-#2379 debt (measured
	// at #2377 time: 0 hex-literal `#3b82f6` occurrences outside comments in
	// input.css -- the two textual occurrences are both inside `/* */` prose,
	// not code -- but other blue-family hex values used for AA-contrast splits
	// remain, and #2379 is where consumer call sites, hex included, get
	// migrated). Widen this regex there, not here.
	accentLiteralRE = regexp.MustCompile(`rgba\(\s*59\s*,\s*130\s*,\s*246`)
)

// readCSS returns the file with CSS block comments (/* ... */) stripped, so a
// token named in a CSS comment is never mistaken for a declaration or a usage.
// It does not strip HTML comments (<!-- ... -->) or line comments (//); those
// forms do not appear in the CSS/templ sources this scans in a way that would
// produce a false match.
func readCSS(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return commentRE.ReplaceAllString(string(b), "")
}

// declaredIn returns the set of custom properties declared in src.
func declaredIn(src string) map[string]bool {
	out := map[string]bool{}
	for _, m := range declRE.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	return out
}

// runtimeSetProperties are custom properties that no stylesheet declares because
// JAVASCRIPT sets them at runtime via element.style.setProperty(). They are
// legitimately "undeclared" in CSS and each carries a var() fallback for the
// pre-hydration frame. Anything added here needs that fallback, or it is just an
// undeclared token wearing a note.
var runtimeSetProperties = map[string]string{
	// artists.templ publishes the live sticky-toolbar height so the selection
	// strip can pin below it, even when the toolbar wraps at narrow widths.
	"--sw-next-toolbar-h": "set by the sticky-offset script in web/templates/artists.templ",
}

// TestEveryUsedTokenIsDeclared is THE --sw-danger regression test, and the reason
// this file exists. Any --sw-* / --swd-* property read anywhere in the stylesheets
// must be DECLARED somewhere in them.
//
// It is deliberately namespace-agnostic rather than scoped to the decorator layer
// #2377 introduced: the bug is "a name that resolves nowhere", and it does not
// care which family the name belongs to. Run against HEAD before this PR it fails
// on FOUR properties -- --sw-danger and --sw-warning (both named in the design of
// record) plus --sw-surface-overlay and --sw-accent-primary-hover, which were not,
// and which this test is what found. --sw-surface-overlay had no fallback at all,
// so the icon-only sidebar tooltip was rendering with no background.
func TestEveryUsedTokenIsDeclared(t *testing.T) {
	input := readCSS(t, inputCSSPath)
	tokens := readCSS(t, tokensCSSPath)

	// A token may be declared in either file: design-tokens.css is the home for
	// tokens, but input.css legitimately REBINDS an existing token on a component
	// (--sw-halo-hue on .sw-btn-destructive is the whole destructive treatment)
	// and still carries the per-screen type ramps from #1853.
	declared := declaredIn(tokens)
	for name := range declaredIn(input) {
		declared[name] = true
	}

	var undeclared []string
	seen := map[string]bool{}
	for _, src := range []string{input, tokens} {
		for _, m := range useRE.FindAllStringSubmatch(src, -1) {
			name := m[1]
			if seen[name] || declared[name] {
				continue
			}
			if _, ok := runtimeSetProperties[name]; ok {
				continue
			}
			seen[name] = true
			undeclared = append(undeclared, name)
		}
	}

	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		t.Errorf("%d custom properties are read but never declared.\n\n"+
			"This is the --sw-danger bug: the property resolves to nothing, so either a var()\n"+
			"fallback silently supplies a value nobody reviewed, or -- with no fallback -- the\n"+
			"whole declaration is invalid at computed-value time and the style simply vanishes.\n"+
			"Nothing fails, and the wrong thing ships.\n\n"+
			"Declare it in %s. If a script sets it at runtime, add it to runtimeSetProperties\n"+
			"WITH a var() fallback for the pre-hydration frame.\n\nUndeclared:\n  %s",
			len(undeclared), tokensCSSPath, strings.Join(undeclared, "\n  "))
	}
}

// TestDecoratorTokensLiveInTokenFile enforces the seed-and-derive split for the
// layer #2377 owns: a DECORATOR is declared once, in design-tokens.css, outside
// any theme block. If input.css mints a new decorator, the "a theme cannot forget
// one because a theme never writes one" guarantee is gone.
//
// Scoped to the decorator namespace on purpose. input.css still declares the
// per-screen type ramps (--sw-type-*, --sw-ad-*) that #1853 is rolling out
// component-by-component; hoisting those is a different axis of work and would
// flatten a deliberate staged rollout. A component-scoped REBIND of a decorator
// that design-tokens.css already declares stays legal -- that is --sw-halo-hue.
func TestDecoratorTokensLiveInTokenFile(t *testing.T) {
	decoratorPrefixes := []string{
		"--swd-", "--sw-halo", "--sw-focus-ring", "--sw-selected-",
		"--sw-hover-", "--sw-disabled-", "--sw-tap-",
	}
	isDecorator := func(name string) bool {
		for _, p := range decoratorPrefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}

	declared := declaredIn(readCSS(t, tokensCSSPath))

	var orphans []string
	for name := range declaredIn(readCSS(t, inputCSSPath)) {
		if isDecorator(name) && !declared[name] {
			orphans = append(orphans, name)
		}
	}

	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Errorf("input.css declares %d decorator token(s) that %s does not.\n"+
			"Decorators are DERIVED once, in the token file, outside every theme block --\n"+
			"that is what stops a theme silently forgetting one. Move them there.\nOrphans:\n  %s",
			len(orphans), tokensCSSPath, strings.Join(orphans, "\n  "))
	}
}

// TestNoAccentLiteralOutsideSeed asserts the focus-glow literal is gone. Every
// halo is var(--sw-halo); every other accent expression derives from the
// --swd-accent seed. A rule that retypes the literal has escaped the token
// system, which is how the glow drifted across a dozen call sites to begin with.
//
// It scans the .templ SOURCES as well as the stylesheets, because CSS is not the
// only place a literal can hide: an inline style= attribute in a component is
// just as much a hard-coded accent, and is invisible to a CSS-only sweep and to
// the stylelint gate alike. The first version of this test checked only the two
// CSS files and MISSED exactly that -- badge.templ's "syncing" pill carried
// `background: rgba(59, 130, 246, 0.12)` in an inline style. Generated *_templ.go
// is skipped: it is a build artifact of the .templ file, so flagging it would
// report the same defect twice and could not be fixed in place.
func TestNoAccentLiteralOutsideSeed(t *testing.T) {
	paths := []string{inputCSSPath, tokensCSSPath}

	for _, dir := range []string{"../components", "../templates"} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), ".templ") {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk dir %s: %v", dir, err)
		}
	}

	for _, path := range paths {
		src := readCSS(t, path) // strips /* */ comments; harmless on .templ
		if hits := accentLiteralRE.FindAllString(src, -1); len(hits) > 0 {
			t.Errorf("%s: found %d hard-coded accent literal(s) rgba(59, 130, 246, ...).\n"+
				"Use var(--sw-halo) for the halo, or derive from var(--swd-accent) with\n"+
				"color-mix(). This applies to inline style= attributes in .templ too, which is\n"+
				"where one of these hid through the first pass of this very test.",
				path, len(hits))
		}
	}
}

// TestTouchFloorTokensExist guards the tap-target contract that the preference,
// the media query, and the icon-button primitive all depend on. Density scales
// SPACING; it may never scale --sw-tap-min.
func TestTouchFloorTokensExist(t *testing.T) {
	tokens := readCSS(t, tokensCSSPath)

	for _, want := range []string{"--sw-tap-min", "--sw-tap-touch"} {
		if !declaredIn(tokens)[want] {
			t.Errorf("design-tokens.css does not declare %s; the touch floor has no basis", want)
		}
	}

	// The floor must be raised by BOTH the input-device query and the preference.
	if !strings.Contains(tokens, "pointer: coarse") {
		t.Error("design-tokens.css has no (pointer: coarse) rule; touch devices would keep the 32px floor")
	}
	if !strings.Contains(tokens, `[data-touch-friendly="on"]`) {
		t.Error(`design-tokens.css has no [data-touch-friendly="on"] rule; the Touch Friendly Controls preference would be inert`)
	}

	// Density must never touch the tap floor. If a [data-density] block ever
	// declares --sw-tap-min, the clamp has become a scale and the floor is gone.
	for _, block := range regexp.MustCompile(`(?s)\[data-density="[a-z]+"\]\s*\{.*?\}`).FindAllString(tokens, -1) {
		if strings.Contains(block, "--sw-tap-min") {
			t.Errorf("a [data-density] block declares --sw-tap-min. The touch floor is a CLAMP, not a scale:\n%s", block)
		}
	}
}
