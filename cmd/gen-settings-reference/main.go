// Command gen-settings-reference renders the "Settings, by tab" reference page
// in docs/site/src/reference/settings-by-tab.md from two complementary
// sources: the i18n locale file (internal/i18n/locales/en.json) supplies the
// labels, descriptions, help, and visibility text for every documented setting;
// the settings templ files (web/templates/settings*.templ) supply the
// tab-to-section binding (which i18n key belongs under which Settings tab).
//
// The generator writes Markdown between the BEGIN/END markers in the docs file
// and emits a sorted companion file `_settings-anchors.txt` next to it. The
// companion file is the machine-readable contract consumed by the in-app
// HelpHint component (#1132): every HelpHint deep-link target is validated
// against this set at test time, so renaming an i18n key (which regenerates an
// anchor) cannot silently leave an unreachable HelpHint link.
//
// Usage:
//
//	go run ./cmd/gen-settings-reference              # rewrite the file in place
//	go run ./cmd/gen-settings-reference -check       # exit non-zero if regen needed
//	go run ./cmd/gen-settings-reference -output FILE # write to a different file
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

const (
	beginMarker = "<!-- BEGIN GENERATED: settings-reference -->"
	endMarker   = "<!-- END GENERATED: settings-reference -->"
)

const (
	defaultOutputPath  = "docs/site/src/reference/settings-by-tab.md"
	defaultAnchorsPath = "docs/site/src/reference/_settings-anchors.txt"
	defaultI18nPath    = "internal/i18n/locales/en.json"
	// componentsAnchorsMirror keeps an in-package copy of the anchors file
	// adjacent to the ContextHelp component so its tests can validate
	// docAnchor arguments via go:embed without escaping the package
	// directory. The codegen writes both paths atomically; check-generated
	// asserts both are in sync via the same -check pass.
	componentsAnchorsMirror = "web/components/_settings-anchors.txt"
)

// templTrunkPath is the page-rendering templ file: it owns the per-section
// @nextSettingsSection("id", ...) wrapper calls that define the section
// boundaries the scanner uses for key attribution. After the tabbed settings
// page retired (#1757 PR-5) the promoted single-scroll page (settings_page.templ)
// is the trunk; it composes the same shared Section*/Settings*Tab funcs the old
// tabbed page did, so key attribution is unchanged apart from the boundary
// marker (a section wrapper instead of a data-tab-panel div). The doc reference
// is therefore organized by the promoted SECTIONS, not the retired tabs.
const templTrunkPath = "web/templates/settings_page.templ"

// extraHelperSources names helper-bearing templ files that back the trunk's
// @-calls but are NOT matched by templSubTemplateGlob (`settings_*.templ`).
// settings.templ (no underscore) holds ProviderKeyCard, serviceConnectionCard,
// ruleRow, SettingsUpdatesTab and their descendants -- helpers the promoted
// trunk and the shared Section* funcs @-call into. It was the trunk itself
// before the promote, so its helpers were always in the recursive index; now
// that settings_page.templ is the trunk, it must be folded in explicitly or the
// walk cannot follow @ProviderKeyCard / @SettingsUpdatesTab / ... and would drop
// every key those helpers render.
var extraHelperSources = []string{"web/templates/settings.templ"}

// subTemplatePanelOverride remaps a single-panel sub-template to a section id
// that differs from its filename stem. settings_auth_providers.templ's stem is
// "auth_providers" but the promoted page's section id is "auth" (the shorter
// rail/section id), so its keys attribute to the "auth" section, not a
// non-existent "auth_providers" one. Keyed on basename for worktree portability.
var subTemplatePanelOverride = map[string]string{
	"settings_auth_providers.templ": "auth",
}

// templSubTemplateGlob matches every settings_*.templ partial that backs a
// specific tab panel. Files matching this glob are auto-discovered at
// generation time, with subTemplateExclude-listed files removed -- so a new
// settings_billing.templ shipping in the future is included automatically
// without anyone needing to remember to update this generator.
const templSubTemplateGlob = "web/templates/settings_*.templ"

// subTemplateExclude names files (by basename) that look like sub-templates
// by glob match but are not consumed by the Settings page's @-call tree.
// The standalone User Preferences page (route /preferences) lives in
// preferences.templ, which does not match the settings_*.templ glob and
// therefore needs no entry here.
//
// Keying on basename rather than full path keeps the exclude list portable
// across worktrees and test fixtures.
var subTemplateExclude = map[string]struct{}{}

// subTemplateHelperOnly names settings_*.templ files (by basename) that are
// NOT backed by a single Settings tab panel. Their templ functions are shared
// renderers @-called from MULTIPLE panels in the trunk (e.g. the M55 #1809
// Section* funcs that contribute to both "general" and "libraries"), so the
// filename-stem-derived panel ID is meaningless for them.
//
// These files are folded into the GLOBAL helper index (so the trunk's
// recursive panel walk can step into their bodies and attribute each i18n key
// to the panel it is actually rendered FROM), but they are deliberately
// EXCLUDED from the filename-stem second-pass attribution that real
// single-panel sub-templates (settings_users.templ -> "users") use. Keying on
// basename keeps the set portable across worktrees and test fixtures.
var subTemplateHelperOnly = map[string]struct{}{
	"settings_sections.templ": {},
	// settings_sections_next.templ holds the next/-only redesigned Connections
	// section (M55 #2117). Its templ funcs are @-called from the next/ settings
	// trunk (web/templates/next/settings.templ), NOT from a stable Settings tab
	// panel, so the filename-stem-derived panel ID ("sections_next") is
	// meaningless and has no data-tab-panel div in the stable trunk. Every
	// ContextHelp docAnchor it renders (manage-title, base-url, api-key,
	// feature-image-write, connections) is already emitted by the stable
	// serviceConnectionCard, so folding it into the global helper index (and
	// skipping filename-stem attribution) keeps the docs correct.
	"settings_sections_next.templ": {},
}

// discoverTemplSources auto-discovers the trunk + sub-template files the
// panel scanner walks, replacing what used to be a hand-maintained allowlist.
// Returns:
//
//   - sources: the trunk path first, then single-panel sub-templates in sorted
//     order, then helper-only sources in sorted order (deterministic output).
//   - owner: the single-panel sub-template-to-panel map, deriving the panel ID
//     from the filename (`settings_users.templ` -> `users`). Helper-only files
//     are NOT in this map (they have no single panel).
//   - helperOnly: the set (by full path) of sources that are helper-only, so
//     scanPanels folds their helpers into the global index but skips them in
//     the filename-stem attribution pass.
//
// If a future single-panel sub-template uses a panel ID that doesn't match its
// filename, add it to subTemplateExclude (skip discovery) and pass it
// explicitly; if it is a multi-panel shared-helper file, add it to
// subTemplateHelperOnly instead.
//
// glob is the filesystem-style pattern for sub-template discovery; it is
// resolved with filepath.Glob, so callers can override during tests.
func discoverTemplSources(trunk, glob string) (sources []string, owner map[string]string, helperOnly map[string]struct{}, err error) {
	subPaths, err := filepath.Glob(glob)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("glob %q: %w", glob, err)
	}
	sort.Strings(subPaths)

	sources = []string{trunk}
	owner = make(map[string]string)
	helperOnly = make(map[string]struct{})
	// Collect helper-only sources separately so they sort after the
	// single-panel sub-templates in the returned slice, keeping the
	// filename-attributed pass (sources[1:] minus helperOnly) contiguous and
	// the overall order deterministic.
	var helperOnlyPaths []string
	for _, p := range subPaths {
		// Glob includes the trunk file when it matches `settings_*.templ`
		// (it doesn't, since the trunk is `settings.templ`), but we filter
		// regardless to be defensive against renames.
		if p == trunk {
			continue
		}
		base := filepath.Base(p) // settings_users.templ
		if _, skip := subTemplateExclude[base]; skip {
			continue
		}
		if _, isHelperOnly := subTemplateHelperOnly[base]; isHelperOnly {
			helperOnly[p] = struct{}{}
			helperOnlyPaths = append(helperOnlyPaths, p)
			continue
		}
		panelID := strings.TrimPrefix(strings.TrimSuffix(base, ".templ"), "settings_") // settings_users -> users
		if override, ok := subTemplatePanelOverride[base]; ok {
			panelID = override // e.g. settings_auth_providers -> "auth"
		}
		owner[p] = panelID
		sources = append(sources, p)
	}
	// Fold in helper-bearing files that back the trunk's @-calls but do not match
	// the settings_*.templ glob (settings.templ). They open no panels of their
	// own; the recursive panel walk steps into their helper bodies via the global
	// index. Skipped if absent so tests that pass a synthetic glob still work.
	for _, p := range extraHelperSources {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		helperOnly[p] = struct{}{}
		helperOnlyPaths = append(helperOnlyPaths, p)
	}
	sources = append(sources, helperOnlyPaths...)
	return sources, owner, helperOnly, nil
}

func main() {
	var (
		checkOnly   bool
		outPath     string
		anchorsPath string
		i18nPath    string
	)
	flag.BoolVar(&checkOnly, "check", false, "exit non-zero if the settings reference needs to be regenerated")
	flag.StringVar(&outPath, "output", defaultOutputPath, "path to the docs file to update")
	flag.StringVar(&anchorsPath, "anchors", defaultAnchorsPath, "path to the anchors companion file")
	flag.StringVar(&i18nPath, "i18n", defaultI18nPath, "path to the i18n locale file")
	flag.Parse()

	if err := run(outPath, anchorsPath, i18nPath, checkOnly); err != nil {
		fmt.Fprintln(os.Stderr, "gen-settings-reference:", err)
		os.Exit(1)
	}
}

func run(outPath, anchorsPath, i18nPath string, checkOnly bool) error {
	keys, err := loadI18nKeys(i18nPath)
	if err != nil {
		return fmt.Errorf("load i18n: %w", err)
	}

	sources, owner, helperOnly, err := discoverTemplSources(templTrunkPath, templSubTemplateGlob)
	if err != nil {
		return fmt.Errorf("discover templ sources: %w", err)
	}
	tabs, err := scanPanels(sources, owner, helperOnly)
	if err != nil {
		return fmt.Errorf("scan panels: %w", err)
	}

	doc, err := buildDocument(tabs, keys)
	if err != nil {
		return fmt.Errorf("build document: %w", err)
	}
	rendered := renderDocument(doc)
	anchors, err := collectAnchors(doc)
	if err != nil {
		return fmt.Errorf("collect anchors: %w", err)
	}

	if err := writeOrCheck(outPath, beginMarker, endMarker, rendered, checkOnly); err != nil {
		return err
	}
	// The components mirror is hard-coded to a repo-relative path. Only
	// fan out to it when the caller is writing to the canonical anchors
	// location; if -anchors redirected to a fixture or alternate path,
	// respect that and skip the mirror so the run stays self-contained.
	paths := []string{anchorsPath}
	if filepath.Clean(anchorsPath) == filepath.Clean(defaultAnchorsPath) {
		paths = append(paths, componentsAnchorsMirror)
	}
	return writeAnchorMirrors(paths, anchors, checkOnly)
}

// writeAnchorMirrors writes the anchors body to every path in paths,
// stopping at the first error. The settings reference codegen needs the
// same anchor set in two places: docs/site/src/reference/ for the
// rendered settings docs and web/components/ as the contract source for
// the in-app HelpHint component (#1132). Pulling the loop out of run()
// keeps the multi-path semantics independently testable -- run() itself
// is hard to unit-test because of its working-directory dependency on
// discoverTemplSources().
func writeAnchorMirrors(paths []string, anchors []string, checkOnly bool) error {
	for _, p := range paths {
		if err := writeAnchorsOrCheck(p, anchors, checkOnly); err != nil {
			return err
		}
	}
	return nil
}

// writeOrCheck applies rendered output to outPath between begin/end markers.
// In checkOnly mode it returns a non-zero error if regeneration would change
// the file; otherwise it writes atomically only when the file actually changed.
//
// begin and end are parameters (not pulled from the package-level constants
// directly) so the helper is exercisable from tests with bespoke markers.
func writeOrCheck(outPath, begin, end, rendered string, checkOnly bool) error { //nolint:unparam // begin/end are exposed as parameters for testability
	// outPath comes from the -output flag; this is a developer-only build-time
	// tool, so a configurable path is intended.
	existing, err := os.ReadFile(outPath) //nolint:gosec // G304: developer CLI, path is intentionally configurable
	if err != nil {
		return fmt.Errorf("read %s: %w", outPath, err)
	}
	updated, err := replaceBetweenMarkers(existing, begin, end, rendered)
	if err != nil {
		return fmt.Errorf("update %s: %w", outPath, err)
	}
	if checkOnly {
		if !bytes.Equal(existing, updated) {
			return fmt.Errorf("%s is stale; run `make generate-docs` to regenerate", filepath.ToSlash(outPath))
		}
		return nil
	}
	if bytes.Equal(existing, updated) {
		return nil
	}
	return filesystem.WriteFileAtomic(outPath, updated, 0o644)
}

// writeAnchorsOrCheck writes the companion anchors file. The file is the
// machine-readable contract for #1132 HelpHint validation; it has no manual
// content, so we overwrite the entire file rather than splicing markers.
func writeAnchorsOrCheck(anchorsPath string, anchors []string, checkOnly bool) error {
	body := strings.Join(anchors, "\n") + "\n"
	// Anchors path is configurable for the same reason as outPath.
	existing, readErr := os.ReadFile(anchorsPath) //nolint:gosec // G304: developer CLI, path is intentionally configurable
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("read %s: %w", anchorsPath, readErr)
	}
	if checkOnly {
		if !bytes.Equal(existing, []byte(body)) {
			return fmt.Errorf("%s is stale; run `make generate-docs` to regenerate", filepath.ToSlash(anchorsPath))
		}
		return nil
	}
	if bytes.Equal(existing, []byte(body)) {
		return nil
	}
	return filesystem.WriteFileAtomic(anchorsPath, []byte(body), 0o644)
}

// ---------------------------------------------------------------------------
// i18n loading
// ---------------------------------------------------------------------------

// loadI18nKeys returns the subset of i18n keys under the "settings." prefix.
// en.json is a flat string map (single-level keys with dotted names), so the
// loader is a single Unmarshal + filter pass.
func loadI18nKeys(path string) (map[string]string, error) {
	// Path is configurable via -i18n flag; same developer-tool justification.
	data, err := os.ReadFile(path) //nolint:gosec // G304: developer CLI, path is intentionally configurable
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var all map[string]string
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make(map[string]string, len(all))
	for k, v := range all {
		if strings.HasPrefix(k, "settings.") {
			out[k] = v
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Templ panel scanning
// ---------------------------------------------------------------------------

// panel captures one Settings tab's name plus the i18n keys observed inside
// its templ region, in source-encounter order.
type panel struct {
	ID   string
	Keys []string
}

// panelOpenRE matches data-tab-panel="X" attributes that mark the start of a
// Settings tab's div in the trunk templ file. The string literal form is what
// appears in inline JS querySelector references (and any panel div that has
// not yet migrated to the typed SettingsTabID interpolation below).
var panelOpenRE = regexp.MustCompile(`data-tab-panel="([a-z_]+)"`)

// sectionOpenerRE matches the promoted settings page's per-section wrapper call,
// @nextSettingsSection("id", "group"), which defines the section boundaries the
// scanner uses for key attribution after the tabbed page retired (#1757 PR-5).
// The captured group is the section id (the panel ID); ids may contain hyphens
// (e.g. "config-file"). The old data-tab-panel forms remain supported so the
// synthetic-fixture unit tests below keep exercising the scanner.
var sectionOpenerRE = regexp.MustCompile(`@nextSettingsSection\(\s*"([a-z0-9-]+)"`)

// panelOpenTypedRE matches the typed-constant form,
// data-tab-panel={ string(TabFoo) }, that panel divs use after the migration
// to compile-time-checked tab IDs. The captured group is the bare suffix of
// the Tab* constant name (e.g. "General", "AuthProviders") which we resolve
// to its underlying SettingsTabID string via tabConstRE on the same file.
var panelOpenTypedRE = regexp.MustCompile(`data-tab-panel=\{\s*string\(Tab([A-Z][A-Za-z0-9]+)\)\s*\}`)

// tabConstRE matches the `TabFoo SettingsTabID = "foo"` declarations inside
// the Tab constant block at the top of the trunk templ file. The two capture
// groups are the constant suffix and the underlying string value, which the
// scanner uses to resolve typed panel attributes back to their panel ID.
var tabConstRE = regexp.MustCompile(`Tab([A-Z][A-Za-z0-9]+)\s+SettingsTabID\s*=\s*"([a-z_]+)"`)

// templFuncRE matches the start of a templ function declaration (e.g.
// `templ resetConfirmPrefsScript() {`). The trunk file's panel divs all live
// inside a single page-rendering templ function (currently `SettingsPage`);
// every templ declaration that follows is a helper renderer whose i18n keys
// must NOT be attributed to whichever panel happened to open last. Scanning
// for the next `^templ ` after the last data-tab-panel match gives us a
// stable boundary without requiring a sentinel comment in the source.
var templFuncRE = regexp.MustCompile(`(?m)^templ ([A-Za-z]+)\s*\(`)

// panelHelperCallRE matches @helperName( invocations inside a panel region.
// When a panel div delegates its content to a templ function defined in the
// same trunk file (e.g. `<div data-tab-panel="updates">@settingsUpdatesTab(...)</div>`),
// the helper's body sits *outside* the panel region and would otherwise be
// missed. The regex captures the helper name so we can locate its body via
// templFuncRE and fold its keys back into the panel's key list.
var panelHelperCallRE = regexp.MustCompile(`@([A-Za-z][A-Za-z0-9]*)\s*\(`)

// i18nCallRE matches t(ctx, "settings...") and tf(ctx, "settings...") calls in
// templ source. tf is the format-string variant; its keys belong in the docs
// reference too. Whitespace is permissive because templ wraps calls across lines.
var i18nCallRE = regexp.MustCompile(`\btf?\s*\(\s*ctx\s*,\s*"(settings\.[A-Za-z0-9_.]+)"`)

// helperRef points at one templ-helper body, carrying the source bytes the
// body lives in along with its [start, end) byte range. It lets the trunk's
// recursive panel walk step into a helper whose declaration sits in a
// DIFFERENT file (the M55 #1809 Section* funcs in settings_sections.templ),
// not just helpers declared in the trunk itself.
//
// extractionLayer marks a helper that lives in a helper-only source
// (settings_sections.templ): such helpers are a TRANSPARENT extraction layer
// the panel walk recurses through freely, so a @Section* call resolves to the
// same effective key set the inline markup produced before the extraction.
// Trunk helpers (extractionLayer=false) keep the historical one-level
// expansion contract: the panel walk emits their direct keys but does NOT
// follow their own @-calls deeper, matching the pre-extraction generator
// output exactly (the docs byte-for-byte gate). Recursing into trunk helpers
// would newly surface keys the established docs never listed (e.g. ruleRow's
// @ruleConfigForm form-control keys), which is out of scope for this fix.
type helperRef struct {
	src             []byte
	start, end      int
	extractionLayer bool
}

// buildGlobalHelperIndex assembles a single name -> helperRef map spanning the
// trunk PLUS every helper-only source file. The trunk's recursive panel walk
// resolves @-calls against this map so it can follow a call into a helper body
// that lives in another file.
//
// On a name collision the trunk definition wins: the trunk is the
// authoritative page renderer, and a same-named helper in a partial would be a
// genuine ambiguity we prefer to resolve deterministically rather than letting
// filesystem-glob order decide. In practice there is no collision today (the
// Section* funcs are uniquely named), so this is purely defensive.
//
// helperSources maps each helper-only file's path to its raw bytes; the caller
// reads those files once and passes them in so this function stays
// I/O-free and testable.
func buildGlobalHelperIndex(trunkData []byte, trunkPath string, helperSources map[string][]byte) map[string]helperRef {
	index := make(map[string]helperRef)
	for name, rng := range indexTemplHelpers(trunkData, trunkPath) {
		index[name] = helperRef{src: trunkData, start: rng[0], end: rng[1], extractionLayer: false}
	}
	// Iterate helper-only files in sorted path order for deterministic
	// collision resolution if two partials ever shared a helper name.
	paths := make([]string, 0, len(helperSources))
	for p := range helperSources {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		data := helperSources[p]
		for name, rng := range indexTemplHelpers(data, p) {
			if _, exists := index[name]; exists {
				// Trunk (or an earlier-sorted partial) already owns this name;
				// keep the existing definition.
				continue
			}
			index[name] = helperRef{src: data, start: rng[0], end: rng[1], extractionLayer: true}
		}
	}
	return index
}

// scanPanels walks the trunk and sub-template files and returns one panel per
// data-tab-panel="X" region in trunk-source order.
//
// The trunk panel pass expands @-calls RECURSIVELY across a global helper
// index spanning the trunk plus helper-only sources (helperOnly set), so each
// i18n key is attributed to the panel it is rendered FROM -- even when the
// helper body lives in another file (the M55 #1809 Section* funcs) and itself
// @-calls back into trunk renderers (@settingsLibraryRow, @profileCard, ...).
//
// Single-panel sub-template files (those in sources[1:] but NOT in helperOnly)
// are attributed wholly to the panel named in subOwner via the second pass.
func scanPanels(sources []string, subOwner map[string]string, helperOnly map[string]struct{}) ([]panel, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("no templ sources provided")
	}
	trunk := sources[0]
	// Trunk source is the primary panel-bearing file; sub-templates contribute
	// keys but never open new panels.
	trunkData, err := os.ReadFile(trunk) //nolint:gosec // G304: developer CLI, path is intentionally configurable
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", trunk, err)
	}

	// First pass: locate every data-tab-panel opener in trunk and record its
	// byte offset and panel ID. Both string-literal and typed-constant attribute
	// forms are accepted; see findPanelOpeners for details.
	openMatches, openIDs, err := findPanelOpeners(trunk, trunkData)
	if err != nil {
		return nil, err
	}

	// pageFuncEnd locates the end of the page-rendering templ function that
	// contains every panel div: the byte offset of the next `^templ X(` after
	// the last panel opener, or len(trunkData) if no further declarations exist.
	// This bounds the last panel's region away from the trailing helper-function
	// renderers (sortableInitScript, resetConfirmPrefsScript, etc.) whose
	// `t(ctx, ...)` calls would otherwise leak into the last tab.
	lastOpener := openMatches[len(openMatches)-1][0]
	pageFuncEnd := len(trunkData)
	if loc := templFuncRE.FindIndex(trunkData[lastOpener:]); loc != nil {
		pageFuncEnd = lastOpener + loc[0]
	}

	// Read every helper-only source so its helpers join the global index. These
	// files contribute helper bodies that the trunk panel walk steps INTO, but
	// they never open panels of their own.
	helperSources := make(map[string][]byte, len(helperOnly))
	for _, src := range sources[1:] {
		if _, ok := helperOnly[src]; !ok {
			continue
		}
		data, err := os.ReadFile(src) //nolint:gosec // G304: developer CLI, path is intentionally configurable
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", src, err)
		}
		helperSources[src] = data
	}

	// globalHelpers indexes every `templ FUNC(...) {` declaration in the trunk
	// AND in the helper-only sources to its body, so panel regions can fold in
	// the keys of helpers they invoke via `@FUNC(...)` -- including cross-file
	// helpers like the M55 #1809 Section* funcs and the trunk renderers those
	// funcs in turn @-call back into.
	globalHelpers := buildGlobalHelperIndex(trunkData, trunk, helperSources)

	panels := make([]panel, 0, len(openMatches))
	for i, m := range openMatches {
		id := openIDs[i]
		start := m[0]
		// Region ends at the next deduped panel opener; for the last panel,
		// we stop at pageFuncEnd (the start of the next `^templ X(` after the
		// last panel) so the trailing helper functions don't pollute it.
		end := pageFuncEnd
		if i+1 < len(openMatches) {
			end = openMatches[i+1][0]
		}
		panels = append(panels, panel{ID: id, Keys: panelRegionKeys(trunkData, start, end, globalHelpers)})
	}

	// Second pass: each single-panel sub-template file contributes its keys to
	// the panel named in subOwner. Helper-only sources are skipped here -- they
	// have no single panel and were already folded into the global index for
	// the trunk pass above. The keys are appended in sub-file order after the
	// trunk-region keys for that panel.
	for _, src := range sources[1:] {
		if _, ok := helperOnly[src]; ok {
			continue
		}
		owner, ok := subOwner[src]
		if !ok {
			return nil, fmt.Errorf("sub-template %s has no entry in subTemplateOwner", src)
		}
		data, err := os.ReadFile(src) //nolint:gosec // G304: developer CLI, path is intentionally configurable
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", src, err)
		}
		// Walk the sub-template starting from its "entry" templ functions
		// (those not @-invoked from anywhere else in the same file), expanding
		// @helperName(...) calls inline. The flat extractKeys(data) we used
		// before returned keys in textual file order, which placed helper-
		// resident keys after every key in the entry function's body --
		// breaking key-to-section attribution when an @-helper invoked early
		// in the entry function was defined late in the file (e.g. the
		// settings_users.templ delete-user-dialog helper bucketed under the
		// Pending Invites H4 because Pending Invites was the last H4 promoted
		// in the entry function's textual span). #1681.
		extra := extractSubTemplateKeys(data, src)
		// Find the trunk panel this sub-template's keys belong to. If the
		// owner ID doesn't exist in the trunk -- typically because someone
		// added settings_X.templ without ever adding a data-tab-panel="X"
		// div in the page -- error out rather than silently dropping the
		// keys. A silent drop would let the docs page miss the entire tab
		// while every check on the PR still passed.
		matched := false
		for i := range panels {
			if panels[i].ID == owner {
				panels[i].Keys = append(panels[i].Keys, extra...)
				matched = true
				break
			}
		}
		if !matched && len(extra) > 0 {
			return nil, fmt.Errorf("sub-template %s maps to panel %q which has no matching section opener in the trunk file", src, owner)
		}
	}

	return panels, nil
}

// findPanelOpeners scans trunkData for every data-tab-panel attribute that
// opens a Settings tab region and returns parallel slices of [start, end]
// byte spans (suitable for the existing m[0]/m[1] indexing in scanPanels)
// and the resolved panel ID strings, deduped by panel ID in source order.
// Two attribute forms are recognized:
//
//   - data-tab-panel="foo"               (string-literal form)
//   - data-tab-panel={ string(TabFoo) }  (typed-constant form)
//
// The trunk file embeds inline JavaScript that uses the same attribute as a
// CSS selector (document.querySelector('[data-tab-panel=...]')), so the
// literal regex also matches those JS references. Dedupe keeps the textually
// earliest occurrence per panel ID; panel divs sit above the JS helpers.
func findPanelOpeners(trunk string, trunkData []byte) ([][]int, []string, error) {
	// Build a TabFoo -> "foo" lookup from the Tab constant block so the
	// typed-constant form below can resolve its capture group to a panel ID.
	tabConstSuffixToID := make(map[string]string)
	for _, m := range tabConstRE.FindAllSubmatch(trunkData, -1) {
		tabConstSuffixToID[string(m[1])] = string(m[2])
	}

	type panelOpener struct {
		start, end int
		id         string
	}
	var rawOpens []panelOpener
	// Promoted per-section wrappers (@nextSettingsSection("id", ...)) -- the
	// primary opener form for the current settings_page.templ trunk.
	for _, m := range sectionOpenerRE.FindAllSubmatchIndex(trunkData, -1) {
		rawOpens = append(rawOpens, panelOpener{start: m[0], end: m[1], id: string(trunkData[m[2]:m[3]])})
	}
	for _, m := range panelOpenRE.FindAllSubmatchIndex(trunkData, -1) {
		rawOpens = append(rawOpens, panelOpener{start: m[0], end: m[1], id: string(trunkData[m[2]:m[3]])})
	}
	for _, m := range panelOpenTypedRE.FindAllSubmatchIndex(trunkData, -1) {
		suffix := string(trunkData[m[2]:m[3]])
		id, ok := tabConstSuffixToID[suffix]
		if !ok {
			return nil, nil, fmt.Errorf(
				"typed panel attribute data-tab-panel={ string(Tab%s) } in %s has no matching Tab%s SettingsTabID = \"...\" declaration",
				suffix, trunk, suffix)
		}
		rawOpens = append(rawOpens, panelOpener{start: m[0], end: m[1], id: id})
	}
	if len(rawOpens) == 0 {
		return nil, nil, fmt.Errorf("no section openers (@nextSettingsSection) or data-tab-panel divs found in %s", trunk)
	}
	sort.Slice(rawOpens, func(i, j int) bool { return rawOpens[i].start < rawOpens[j].start })
	openMatches := make([][]int, 0, len(rawOpens))
	openIDs := make([]string, 0, len(rawOpens))
	seenID := make(map[string]struct{}, len(rawOpens))
	for _, po := range rawOpens {
		if _, ok := seenID[po.id]; ok {
			continue
		}
		seenID[po.id] = struct{}{}
		openMatches = append(openMatches, []int{po.start, po.end})
		openIDs = append(openIDs, po.id)
	}
	return openMatches, openIDs, nil
}

// templBlockCloseRE matches the closing `}` of a templ block, which the
// templ-generator-aware formatter always places at column 0. Inner Go
// constructs inside the body (if/else, for, switch) close with indented
// `}` characters and never trigger this anchor. The next-templ-declaration
// boundary is used only as a fallback when no `^}` is found between a
// templ opener and end-of-file (defensive against unformatted source).
var templBlockCloseRE = regexp.MustCompile(`(?m)^\}`)

// indexTemplHelpers maps each `templ FUNC(...) {` name in src to the
// EXACT byte range of its body. The body runs from the byte after the
// declaration's match to the byte index immediately before the closing
// `}` that sits at column 0 (gofmt-style indentation). Earlier versions
// of this function used "to the start of the next templ declaration" as
// the upper bound, which silently swept any plain Go function placed
// between two templ helpers into the prior helper's range -- those
// keys would then disappear from the top-level walk (skipRanges) and
// re-appear inside the helper at @-expansion time, out of source order.
// Computing the exact body avoids that misattribution.
//
// Returns an empty map if templFuncRE finds no matches. Callers must
// tolerate missing entries because `@helperName(...)` invocations of
// cross-file or cross-package helpers are filtered upstream (the
// dotted-namespace form does not match panelHelperCallRE) and bare
// identifier @-calls to non-existent names are surfaced as warnings by
// walkOrdered, not as errors here.
//
// srcPath is used only to label the warning emitted when a templ block's
// closing column-0 `}` cannot be located before the next templ
// declaration. The fallback to the approximate range mirrors the old
// behavior, so output stays compatible -- but a silent fallback would
// reintroduce the very class of bug the exact-body fix was meant to
// prevent, so an operator-visible warning is preferable to silence.
func indexTemplHelpers(src []byte, srcPath string) map[string][2]int {
	matches := templFuncRE.FindAllSubmatchIndex(src, -1)
	out := make(map[string][2]int, len(matches))
	for i, m := range matches {
		name := string(src[m[2]:m[3]])
		start := m[1] // byte after the `templ FUNC(` opener

		// Upper bound for the search window: the start of the next templ
		// declaration, or end-of-src for the last one. The `^}` we find
		// within that window is the closing brace of THIS templ block.
		searchEnd := len(src)
		if i+1 < len(matches) {
			searchEnd = matches[i+1][0]
		}
		end := searchEnd
		if loc := templBlockCloseRE.FindIndex(src[start:searchEnd]); loc != nil {
			// loc[0] is the offset of `^}` within the window; end excludes
			// the `}` byte itself, matching the half-open `[start, end)`
			// semantics used by callers (extractKeys, walkOrdered).
			end = start + loc[0]
		} else {
			fmt.Fprintf(os.Stderr, "gen-settings-reference: %s: templ %s: no column-0 closing brace found before next templ declaration; falling back to approximate body range (key ordering for plain Go in this gap may regress)\n", srcPath, name)
		}
		out[name] = [2]int{start, end}
	}
	return out
}

// extractSubTemplateKeys returns every i18n key in a sub-template's source
// in walk order, inlining keys from @-invoked helper bodies at the position
// of the @-call site rather than at the helper's declaration position. This
// preserves correct positional ordering when an @-helper invoked early in
// an entry function is declared late in the file (e.g. settings_users.templ's
// @deleteUserDialog() call sits before the Pending Invites H4, but the
// dialog's templ helper is declared after; the flat scan misattributed the
// dialog keys to Pending Invites). #1681.
//
// The walk treats the file in source order and skips the bodies of templ
// helpers that are @-invoked from elsewhere in the file -- those keys are
// emitted via inline expansion at the @-call sites instead. Templ functions
// not @-invoked locally (entries called from the trunk or as HTMX fragments)
// are walked in place. Plain Go function bodies (e.g. formatLastLogin) are
// also walked in place, since their keys live wherever they are textually
// declared and the generator does not follow plain Go call graphs.
//
// srcPath is used solely to prefix the warning emitted when a same-token
// @-call resolves to a helper that isn't declared in this file. The
// generator does not follow cross-file @-calls, so this is a typo or
// in-flight rename signal. The dotted-namespace form (e.g.
// @components.ContextHelp) is filtered upstream by panelHelperCallRE.
func extractSubTemplateKeys(src []byte, srcPath string) []string {
	helpers := indexTemplHelpers(src, srcPath)

	// Templ helpers that appear as @-call targets anywhere in the file: their
	// declaration-site bodies are skipped during the file walk, since the
	// @-call expansion emits their keys at the correct caller position.
	called := make(map[string]struct{})
	for _, hm := range panelHelperCallRE.FindAllSubmatch(src, -1) {
		called[string(hm[1])] = struct{}{}
	}
	skipRanges := make([][2]int, 0, len(called))
	for name, rng := range helpers {
		if _, ok := called[name]; ok {
			skipRanges = append(skipRanges, rng)
		}
	}
	sort.Slice(skipRanges, func(i, j int) bool { return skipRanges[i][0] < skipRanges[j][0] })

	var out []string
	seen := make(map[string]struct{})
	visiting := make(map[string]bool)
	walkOrdered(src, srcPath, 0, len(src), helpers, skipRanges, visiting, seen, &out)
	return out
}

// walkOrdered emits keys found in src[start:end] in source order, with two
// transformations: (1) byte ranges in skipRanges are skipped entirely (used
// to suppress the declaration-site body of a templ helper that is emitted
// via @-expansion at its call site instead), and (2) at every @helperName(...)
// site whose target is in helpers, the helper's body is walked recursively
// and its keys are inlined at the call position.
//
// seen is the shared dedup set so a key visible from multiple paths (e.g.
// via two callers of the same helper) appears only at its first-encounter
// position. visiting guards against @-call cycles, which are defensively
// handled even though the production templ tree is acyclic.
func walkOrdered(
	src []byte,
	srcPath string,
	start, end int,
	helpers map[string][2]int,
	skipRanges [][2]int,
	visiting map[string]bool,
	seen map[string]struct{},
	out *[]string,
) {
	type event struct {
		pos    int
		isCall bool
		key    string
		helper string
	}
	region := src[start:end]
	var events []event
	for _, m := range i18nCallRE.FindAllSubmatchIndex(region, -1) {
		events = append(events, event{pos: start + m[0], key: string(region[m[2]:m[3]])})
	}
	for _, m := range panelHelperCallRE.FindAllSubmatchIndex(region, -1) {
		events = append(events, event{pos: start + m[0], isCall: true, helper: string(region[m[2]:m[3]])})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].pos < events[j].pos })

	for _, ev := range events {
		if inSkipRange(ev.pos, skipRanges) {
			continue
		}
		if ev.isCall {
			if visiting[ev.helper] {
				continue
			}
			rng, ok := helpers[ev.helper]
			if !ok {
				// Same-token @-call with no matching helper in this file:
				// either a typo or an in-flight rename. The dotted-namespace
				// form (e.g. @components.ContextHelp) is filtered upstream by
				// panelHelperCallRE, so this branch fires only for bare
				// identifiers we expected to resolve locally. Surface it on
				// stderr so the next regen pass attributes the lost keys to a
				// human-visible warning, not a silent docs drift.
				fmt.Fprintf(os.Stderr, "gen-settings-reference: %s: @%s called but not declared in this file\n", srcPath, ev.helper)
				continue
			}
			visiting[ev.helper] = true
			// Recursive walks pass nil skipRanges: we are intentionally
			// stepping into the helper body to emit its keys at this @-call
			// site, so the file-level "suppress @-invoked helper bodies"
			// rule must not also suppress the body we are deliberately
			// expanding.
			walkOrdered(src, srcPath, rng[0], rng[1], helpers, nil, visiting, seen, out)
			delete(visiting, ev.helper)
			continue
		}
		if isNoiseKey(ev.key) {
			continue
		}
		if _, ok := seen[ev.key]; ok {
			continue
		}
		seen[ev.key] = struct{}{}
		*out = append(*out, ev.key)
	}
}

// panelRegionKeys returns the i18n keys for one panel region src[start:end] in
// the order the pre-extraction generator produced them (the docs byte-for-byte
// gate). It runs the two-phase walk:
//
//   - Phase A (positional): walkPanelRegion emits direct t() keys and recurses
//     THROUGH the transparent extraction-layer helpers (@Section*) in source
//     order, while recording trunk-helper @-calls (@ProviderKeyCard,
//     @settingsLibraryRow, ...) in encounter order without expanding them.
//   - Phase B (deferred): flat-expand the recorded trunk helpers in encounter
//     order, appended after Phase A. This matches the legacy "extractKeys(
//     region) first, then append the @-helper bodies' keys" contract.
//
// The panel region contains no helper DECLARATIONS, only calls, so the walk
// needs no skipRanges. seen dedups across both phases; visiting cycle-guards
// the extraction-layer recursion.
func panelRegionKeys(src []byte, start, end int, helpers map[string]helperRef) []string {
	var keys []string
	seen := make(map[string]struct{})
	visiting := make(map[string]bool)
	var deferred []deferredHelper
	walkPanelRegion(src, start, end, helpers, visiting, seen, &keys, &deferred)
	for _, d := range deferred {
		if visiting[d.name] {
			continue
		}
		emitHelperKeysFlat(d.ref.src, d.ref.start, d.ref.end, seen, &keys)
	}
	return keys
}

// deferredHelper records a trunk-helper @-call encountered during the
// positional panel walk so its keys can be appended AFTER the walk, in
// encounter order. See walkPanelRegion / panelRegionKeys for why trunk helpers
// are deferred while extraction-layer helpers are expanded inline.
type deferredHelper struct {
	name string
	ref  helperRef
}

// walkPanelRegion is the positional, cross-file panel-region walk (Phase A in
// scanPanels). It emits the i18n keys found in src[start:end] in source order
// and resolves @helperName(...) call sites against the GLOBAL helper index
// (name -> helperRef), whose entries may live in another source file.
//
// Two helper kinds are handled differently to reproduce the pre-extraction
// generator's exact key ordering (the docs byte-for-byte gate):
//
//   - Extraction-layer helpers (the M55 #1809 @Section* funcs in
//     settings_sections.templ) are a TRANSPARENT layer: the walk recurses INTO
//     their body at the call position, so their direct keys and inline content
//     land exactly where the markup used to sit before extraction.
//   - Trunk helpers (@ProviderKeyCard, @settingsLibraryRow, @profileCard, ...)
//     are NOT expanded inline. Their refs are appended to *deferred in
//     encounter order; scanPanels flat-expands them after the positional pass.
//     This matches the legacy "extractKeys(region) first, then append the
//     @-helper bodies' keys" contract -- the panel walk never followed a trunk
//     helper's own @-calls, and its keys always trailed the region's direct
//     keys.
//
// seen is the shared per-panel dedup set (a key reachable via multiple paths
// appears once, at first encounter); visiting cycle-guards the extraction-layer
// recursion so a cyclic @-graph terminates; isNoiseKey filters runtime UI text.
func walkPanelRegion(
	src []byte,
	start, end int,
	helpers map[string]helperRef,
	visiting map[string]bool,
	seen map[string]struct{},
	out *[]string,
	deferred *[]deferredHelper,
) {
	type event struct {
		pos    int
		isCall bool
		key    string
		helper string
	}
	region := src[start:end]
	var events []event
	for _, m := range i18nCallRE.FindAllSubmatchIndex(region, -1) {
		events = append(events, event{pos: start + m[0], key: string(region[m[2]:m[3]])})
	}
	for _, m := range panelHelperCallRE.FindAllSubmatchIndex(region, -1) {
		events = append(events, event{pos: start + m[0], isCall: true, helper: string(region[m[2]:m[3]])})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].pos < events[j].pos })

	for _, ev := range events {
		if ev.isCall {
			if visiting[ev.helper] {
				continue
			}
			ref, ok := helpers[ev.helper]
			if !ok {
				// A bare @-call with no matching helper anywhere in the global
				// index. The dotted-namespace form (@components.ContextHelp) is
				// filtered upstream by panelHelperCallRE, so this fires only for
				// bare identifiers we expected to resolve. In the trunk panel
				// pass these are overwhelmingly inline layout/control helpers
				// that carry no i18n keys of their own; suppress the per-call
				// noise the sub-template self-walk surfaces for typos, since the
				// panel region legitimately calls many key-free helpers.
				continue
			}
			if ref.extractionLayer {
				// Transparent extraction layer: recurse INTO the body at the
				// call position so its keys and nested @-calls are placed in
				// source order.
				visiting[ev.helper] = true
				walkPanelRegion(ref.src, ref.start, ref.end, helpers, visiting, seen, out, deferred)
				delete(visiting, ev.helper)
			} else {
				// Trunk helper: defer flat expansion to after the positional
				// pass (legacy append-last ordering).
				*deferred = append(*deferred, deferredHelper{name: ev.helper, ref: ref})
			}
			continue
		}
		if isNoiseKey(ev.key) {
			continue
		}
		if _, ok := seen[ev.key]; ok {
			continue
		}
		seen[ev.key] = struct{}{}
		*out = append(*out, ev.key)
	}
}

// emitHelperKeysFlat appends every i18n key found textually in src[start:end]
// to out in source order, filtering noise and deduping against the shared seen
// set. It does NOT follow @-calls inside the body -- it is the one-level
// expansion the trunk panel pass historically applied to a panel-region
// @-call, preserved here so the Section* extraction does not change which keys
// a panel lists. (The fully-recursive walkOrderedGlobal is used only for the
// transparent helper-only extraction layer.)
func emitHelperKeysFlat(src []byte, start, end int, seen map[string]struct{}, out *[]string) {
	for _, m := range i18nCallRE.FindAllSubmatchIndex(src[start:end], -1) {
		key := string(src[start+m[2] : start+m[3]])
		if isNoiseKey(key) {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		*out = append(*out, key)
	}
}

// inSkipRange reports whether pos falls within any [start,end) range in
// ranges. ranges is expected to be sorted by start; a linear scan is fine at
// the scale of one sub-template file's helper count.
func inSkipRange(pos int, ranges [][2]int) bool {
	for _, r := range ranges {
		if pos >= r[0] && pos < r[1] {
			return true
		}
		if pos < r[0] {
			break
		}
	}
	return false
}

// extractKeys returns every i18n key inside src in encounter order, filtering
// out runtime UI strings that are not user-configurable settings (toasts,
// validation errors, ARIA labels, placeholders). The denylist is conservative
// by design: it filters by recognizable substrings in the key name. New noise
// patterns get added here when they show up.
//
// Keys are deduplicated (a key seen twice in the same source region is only
// listed once, on first appearance) since the docs page should not enumerate
// the same control twice for one rendering.
func extractKeys(src []byte) []string {
	matches := i18nCallRE.FindAllSubmatch(src, -1)
	out := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		k := string(m[1])
		if isNoiseKey(k) {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// noiseTokens enumerates substring tokens that mark a key segment as runtime
// UI text rather than a user-facing setting. Runtime text lives in the i18n
// file because it needs to be localized; it does not belong on the settings
// reference docs page.
//
// Matching is segment-based: the key is split on '.', and a segment qualifies
// as noise when any token in this list is a substring of the segment. This
// lets us catch all of `toast_saved`, `error_network`, `aria_pill_label`,
// `confirm_delete`, etc. with a small set of tokens, and avoids false hits
// like a hypothetical setting whose name happens to contain "error" inside a
// neighboring segment.
var noiseTokens = []string{
	"toast",
	"error",
	"aria",
	"placeholder",
	// Underscore-anchored to spare the `confirm_dialogs.*` section namespace,
	// which is a real user-facing area in the Maintenance tab. Without the
	// underscore, the unanchored `confirm` token would filter every key under
	// settings.confirm_dialogs.* (title, description, and the reset controls)
	// because each of their segments contains the substring `confirm`.
	"confirm_",
	"failed",
	// Suffix-anchored to keep the filter intentional: `_hint` matches every
	// real noise key (default_hint, aria_pill_hint, oidc_logo_url_hint,
	// admin_groups_hint, etc.) without false-matching a section namespace
	// whose own segment happens to contain `hint`. The OIDC `*_hint` keys
	// stay filtered out by design -- they are inline form-help annotations
	// shown next to the input, not standalone controls -- so the docs page
	// surfaces those controls via their .label / .description metadata
	// (e.g. settings.auth.oidc.label / .description) rather than the
	// transient hint copy.
	"_hint",
	"message",
	"saved",   // status labels: settings.X.saved
	"failure", // status labels: settings.X.failure
	"status",  // settings.X.status indicators
	// Post-action banners (e.g. "restart required to apply..."). These render
	// in-app after a state-changing save and are not part of the static settings
	// surface a docs reader needs. Matching the prefix keeps related siblings
	// like restart_required_body, restart_required_docker_instruction together.
	"restart_required",
	// Visibility-style runtime callouts (env-var override, docker-mode banner,
	// "config_hint" inline notes). These belong in .visibility metadata when we
	// want to surface them; otherwise they are runtime UI noise.
	"override",
	"notice",
	// Underscore-anchored prefixes for runtime label families: state_applying,
	// interval_12h, channel_nightly. The trailing underscore prevents false hits
	// on legitimate setting names that happen to end in the same word
	// (e.g. check_interval, the Release channel control).
	"state_",
	"interval_",
	"channel_",
	"_version", // current_version, latest_version, etc.
	// Runtime state and button families. These are surface elements the user
	// sees while operating the app, not configurable settings. The token list
	// is intentionally specific (rather than catching, say, "_check") to keep
	// real settings names that overlap (e.g. auto_check.label) safe.
	"available",    // update_available, *_available
	"_checked",     // last_checked, not_checked
	"_notes",       // release_notes
	"apply_",       // apply_update, apply_action
	"check_now",    // "Check for updates" button
	"actions_",     // actions_title, actions_*
	"up_to",        // up_to_date
	"docker_",      // docker_notice, docker_instruction (banner copy, not configurable)
	"_instruction", // restart_required_native_instruction, etc.
	"config_title", // sub-section subtitle, redundant with the Config control's label
	// Empty-state, loading, and clipboard feedback render as runtime UI
	// (lists waiting for data, "copied" toasts, etc.) and are never settings
	// the user can configure. Anchored to suffix forms so legitimate names
	// like "info_section" or "success_rate" aren't false-positives.
	"empty",   // X.empty: empty-state placeholders ("No items configured.")
	"loading", // X_loading, loading_X: in-flight indicators
	"copied",  // *_copied: clipboard feedback ("Link copied")
	"success", // *_success: post-action success banners
	"_info",   // suffix-only: catches reset_info etc. without eating "info_section"
	"_link",   // suffix-only: catches invite_link, copy_link, etc. (URLs/buttons, not settings)
	// Sentence-fragment provider notes: two i18n keys carrying halves of one
	// sentence concatenated at runtime ("Requires X..." / "...app."). They
	// render as broken bullets if surfaced in the docs.
	"_note_prefix",
	"_note_suffix",
	// Underscore-suffixed _description keys are runtime state/banner text
	// (e.g. settings.symlinks.unsupported_description renders only when the
	// filesystem doesn't support symlinks). Distinct from the dotted .description
	// metadata suffix which IS a real control description and stays parsed.
	// The substring match here catches `supported_description`,
	// `unsupported_description`, `manage_description`, etc.
	"_description",
	// Multi-line banner prose split across keys: settings.X.description_line1,
	// description_line2. These compose at render-time and aren't per-control
	// content the docs reader can navigate to.
	"description_line",
	// Sub-section ContextHelp popover prose attached to a parent section by
	// the in-app help-icon convention (e.g. settings.rules.help_nfo backs the
	// NFO category header's popover). Distinct from section-level .help which
	// is handled by buildSections. Substring match catches help_nfo, help_image,
	// help_metadata, and any future per-sub-section help_X variants without
	// false-matching real settings keys (no settings name happens to contain
	// the literal "help_" today).
	"help_",
	// Mouse-hover tooltip strings rendered via title= attributes or sr-only
	// spans on small affordances (status pills, conflict-gated chips,
	// disabled-rule reasons). They are runtime hover affordances, not
	// configurable controls, and surface as bare prose-as-label bullets if
	// not filtered (e.g. settings.connections.feature_image_write_tooltip,
	// settings.rules.requires_local_tooltip / _tooltip_short,
	// settings.rules.cannot_enable_tooltip).
	"_tooltip",
	// Inline note prose composed alongside a primary control (rule_schedule.note,
	// db_maintenance.schedule_note, backup.retention_note, etc.). Renders as
	// "Tip: ..." or footnote text in the panel, never as a navigable control.
	// Without filtering, the prose surfaces as a long-prose-as-label bullet on
	// the docs reference page.
	"note",
	// Inline form-help paragraphs (settings.provider_config.custom_help and
	// any future *_help variant). Distinct from the dotted .help metadata
	// suffix which is handled by composeControlProse; the underscore form
	// renders as a bare prose-as-label bullet on the docs page if not filtered.
	"_help",
	// Modal/hx-confirm dialog text (settings.users.revoke_confirm,
	// settings.priorities.confirm_restore is already prefix-matched). Suffix
	// form catches X_confirm pairs without false-matching the
	// confirm_dialogs.* section namespace which the existing confirm_ token
	// already handles.
	"_confirm",
	// Toast / status banner text shown after a state change (e.g.
	// settings.users.invite_revoked, *_revoked). Existing toast/saved/failed
	// tokens cover most banner copy; _revoked picks up the action-pasttense
	// variant the others miss.
	"_revoked",
	// tf()-only runtime strings (not configurable controls):
	//
	// _format: format-string display variants beside a control (e.g.
	// settings.active_profile.nfo_enabled_format "Enabled (%s format)").
	"_format",
	// _header: dynamic sub-section headings substituted at runtime
	// (e.g. settings.rules.category_header "%s Rules").
	"_header",
	// change_role_for, copy_invite_for, automation_mode_for: aria-label
	// templates personalized with a subject name (e.g. "Change role for %s",
	// "Automation mode for %s"). These are tf()-only keys; the plain "for"
	// substring cannot be used as a token here because
	// settings.users.role_for_invite (a real documented control) also contains
	// it. The specific leaf patterns are listed instead.
	"change_role_for",
	"copy_invite_for",
	"automation_mode_for",
	// deactivate_: destructive action label templates that include a user name
	// subject (e.g. settings.users.deactivate_user "Deactivate %s"). Prefix
	// form targets only the parametrized variant; the plain "deactivate" key
	// (button label, if it existed) would not match because it has no trailing
	// underscore.
	"deactivate_",
	// _at: timestamp display strings (e.g. settings.api_tokens.created_at
	// "Created %s").
	"_at",
	// last_used: last-used timestamp string specific to API tokens display.
	// Too short to anchor by suffix alone without false positives.
	"last_used",
	// revoke_invite: aria-label for the per-row invite revocation action
	// ("Revoke invite %s"). Not covered by the existing _revoked suffix token
	// since the key stem is the verb, not the past-tense form.
	"revoke_invite",
	// Option-label and action-button families surfaced when the General and
	// Maintenance cards were extracted (M55 #1809) and their previously
	// hardcoded English labels were localized. These are operate-the-app
	// affordances (select options, action buttons, in-flight spinners), not
	// configurable settings, so they must not appear on the docs reference
	// page. Leaf-segment matching keeps the real controls safe: the image
	// cache "max_size" control's leaf has no "size_" substring, and the
	// "confirm_vacuum" dialog is already covered by the confirm_ token.
	"size_",      // image_cache.size_256mb/512mb/1gb/2gb/size_custom select options
	"optimize",   // db_maintenance.optimize ("Optimize Now" button)
	"optimizing", // db_maintenance.optimizing ("Optimizing..." spinner)
	"vacuum",     // db_maintenance.vacuum + .vacuuming (button + spinner)
	"creating",   // backup.creating ("Creating backup..." spinner)
	// NOTE: backup.create ("Create Backup") is filtered via noiseLeavesExact,
	// not a "create" substring token -- a bare "create" token also matches
	// settings.users.users.create_invite ("Create Invite"), a documented control.
}

// noiseLeavesExact lists leaf segments that mark a key as runtime UI text only
// when they match the leaf EXACTLY. This is for noise words that are also a
// substring of a real, documented control's leaf, where the substring-based
// noiseTokens would over-match. "create" is the motivating case:
// settings.backup.create ("Create Backup" button) is runtime chrome, but
// settings.users.users.create_invite ("Create Invite") is a documented control
// whose leaf contains "create".
var noiseLeavesExact = map[string]struct{}{
	"create": {}, // backup.create ("Create Backup" button); must not match create_invite
}

// noiseKeysExact lists FULL dotted keys (not just leaves) that are runtime UI
// text and must not surface as docs controls, used when leaf-based matching
// would over-reach into a real control elsewhere. The motivating case is the
// API-token action buttons localized in the S3 review pass (M55 #1841):
// settings.api_tokens.revoke / .delete are operate-the-app buttons, but their
// leaves "revoke" / "delete" are ALSO the leaves of documented Users-tab
// controls (settings.users.users.revoke = "Revoke" invite,
// settings.users.users.delete = "Delete" user), so a leaf-exact filter would
// wrongly drop those. A full-key match scopes the filter to exactly the
// API-token affordances. The .generate / .show_archive / .hide_archive /
// .name_label keys are listed here too (rather than as leaves) to keep the whole
// localized-button family in one place. The .revoke_aria / .delete_aria keys
// are already filtered by the "aria" noiseTokens entry.
var noiseKeysExact = map[string]struct{}{
	"settings.api_tokens.revoke":       {}, // "Revoke" button (per active token)
	"settings.api_tokens.delete":       {}, // "Delete" button (per revoked token)
	"settings.api_tokens.generate":     {}, // "Generate Token" button
	"settings.api_tokens.show_archive": {}, // "Show Archive" toggle
	"settings.api_tokens.hide_archive": {}, // "Hide Archive" toggle
	"settings.api_tokens.name_label":   {}, // sr-only "Token name" input label
	// Maintenance/Logs action-button + spinner labels localized when the System
	// cards were extracted (M55 #1809 S4). These are operate-the-app affordances,
	// not configurable settings, so they must not surface on the docs reference
	// page. Listed as full keys because their leaves ("reset_button",
	// "export_button", "exporting", "importing", "cleanup") are too generic to
	// token-match without risking a real control elsewhere.
	"settings.confirm_dialogs.reset_button": {}, // "Reset all confirmation preferences" button
	"settings.export_import.export_button":  {}, // "Export Settings" submit button
	"settings.export_import.exporting":      {}, // "Exporting..." in-flight spinner
	"settings.export_import.importing":      {}, // "Importing..." in-flight spinner
	"settings.log_settings.cleanup":         {}, // "Clean up old logs" button
}

// isNoiseKey returns true when the LAST segment of k contains a noiseTokens
// entry, indicating runtime UI text rather than a user-facing setting.
//
// Matching is restricted to the leaf segment (rather than any segment) because
// noise patterns describe what the *control* is -- a toast, an error, a button
// state -- never what the *section* is. Without this restriction, a section
// namespace like settings.confirm_dialogs.* would match the `confirm_` token
// and silently drop every key beneath it, including the section's own
// .title and .description that the docs page needs to render.
func isNoiseKey(k string) bool {
	if _, ok := noiseKeysExact[k]; ok {
		return true
	}
	last := lastSegmentOf(k)
	if _, ok := noiseLeavesExact[last]; ok {
		return true
	}
	for _, tok := range noiseTokens {
		if strings.Contains(last, tok) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Document model
// ---------------------------------------------------------------------------

// document is the in-memory tree the renderer walks. It mirrors the three-tier
// output structure: tab > section > control.
type document struct {
	Tabs []docTab
}

type docTab struct {
	ID       string // e.g. "general", used for tab-id slug
	Label    string // resolved from settings.tab.{ID}; falls back to humanized ID
	Intro    string // from settings.tab.{ID}.intro (optional relationship prose)
	Sections []docSection
}

type docSection struct {
	ID          string // tier-2 i18n namespace, e.g. "platform_profile"
	Title       string // from settings.{ID}.title (or humanized ID)
	Description string // from settings.{ID}.description (optional)
	Controls    []docControl
}

type docControl struct {
	ID          string // tier-3 path under section, e.g. "builtin_profiles"
	Label       string // from .label or root key value
	Description string // from .description (short, inline)
	Help        string // from .help (long, surfaced by HelpHint)
	Visibility  string // from .visibility (optional caveat)
}

// metadataSuffixes are i18n key suffixes that decorate a control rather than
// constitute a new control. Order matters: the renderer strips these to derive
// the control's stable ID.
var metadataSuffixes = []string{
	".label",
	".description",
	".help",
	".visibility",
	".placeholder",
	".desc", // legacy; will be swept out in Phase 3
}

// buildDocument assembles the full tab > section > control tree from the
// scanned panels and the loaded i18n keys. Returns the first error from
// buildControl when a control's i18n keys are malformed (missing label).
func buildDocument(tabs []panel, keys map[string]string) (document, error) {
	doc := document{Tabs: make([]docTab, 0, len(tabs))}
	for _, p := range tabs {
		t, err := buildTab(p, keys)
		if err != nil {
			return document{}, err
		}
		doc.Tabs = append(doc.Tabs, t)
	}
	return doc, nil
}

func buildTab(p panel, keys map[string]string) (docTab, error) {
	// Prefer the promoted rail's canonical section label
	// (settings.next.section.{id}); the section ids use hyphens (config-file)
	// while the i18n keys use underscores (config_file). Fall back to a legacy
	// settings.tab.{id} label, then a humanized id, so a section without a rail
	// label still renders (#1757 PR-5).
	sectionLabelKey := "settings.next.section." + strings.ReplaceAll(p.ID, "-", "_")
	label := keys[sectionLabelKey]
	if label == "" {
		label = lookupLabel(keys, "settings.tab."+p.ID, p.ID)
	}
	tab := docTab{
		ID:    p.ID,
		Label: label,
		Intro: keys["settings.tab."+p.ID+".intro"],
	}
	sections, err := buildSections(p.Keys, keys)
	if err != nil {
		return docTab{}, fmt.Errorf("tab %q: %w", p.ID, err)
	}
	tab.Sections = sections
	return tab, nil
}

// buildSections groups the panel's keys by tier-2 namespace (the section ID)
// and returns one docSection per group, preserving panel-source order for both
// sections and their controls.
//
//nolint:gocognit // Two-level grouping with first-appearance ordering at both section and control granularity; the i18n key segmenter has to handle title/desc-only keys, control composite IDs, and orphaned keys in a single pass to preserve panel-source order, and splitting the segmentation rules would multiply map lookups.
func buildSections(panelKeys []string, allKeys map[string]string) ([]docSection, error) {
	type sectionAccum struct {
		id          string
		controlIDs  []string            // first-appearance order of control IDs
		controlKeys map[string][]string // controlID -> list of full i18n keys
		titleKey    string
		descKey     string
	}

	sectionOrder := []string{}
	seenSection := make(map[string]int) // id -> index in sectionOrder
	accums := []*sectionAccum{}

	for _, k := range panelKeys {
		parts := strings.Split(k, ".")
		// k looks like "settings.{section}.{rest...}".
		// Skip ambient (2-tier) keys and the tab-label namespace.
		if len(parts) < 3 {
			continue
		}
		if parts[1] == "tab" {
			continue
		}
		secID := parts[1]
		idx, ok := seenSection[secID]
		if !ok {
			idx = len(accums)
			seenSection[secID] = idx
			sectionOrder = append(sectionOrder, secID)
			accums = append(accums, &sectionAccum{
				id:          secID,
				controlKeys: map[string][]string{},
			})
		}
		acc := accums[idx]

		rest := strings.Join(parts[2:], ".")
		switch rest {
		case "title":
			acc.titleKey = k
			continue
		case "description":
			acc.descKey = k
			continue
		case "help":
			// Section-level .help i18n keys back the in-app ContextHelp
			// popover next to the section heading and are deliberately
			// not rendered in the docs reference (the matching .description
			// is the docs surface). Skip the key here so it isn't treated
			// as an orphaned metadata bag with no parent control.
			continue
		}

		ctrlID := controlIDFor(parts[2:])
		if _, seen := acc.controlKeys[ctrlID]; !seen {
			acc.controlIDs = append(acc.controlIDs, ctrlID)
		}
		acc.controlKeys[ctrlID] = append(acc.controlKeys[ctrlID], k)
	}

	// Fold sibling metadata keys from allKeys into discovered controls. The
	// panel scan only sees keys actually referenced by t(ctx, "...") in the
	// templ; for docs-only metadata such as settings.X.Y.description that
	// nothing renders inline, the corresponding key never enters panelKeys.
	// Without this pass, contributors writing prose in en.json would see
	// the key dropped silently from the rendered reference.
	for _, acc := range accums {
		for _, ctrlID := range acc.controlIDs {
			for _, suffix := range metadataSuffixes {
				metaKey := "settings." + acc.id + "." + ctrlID + suffix
				if _, ok := allKeys[metaKey]; !ok {
					continue
				}
				present := false
				for _, existing := range acc.controlKeys[ctrlID] {
					if existing == metaKey {
						present = true
						break
					}
				}
				if !present {
					acc.controlKeys[ctrlID] = append(acc.controlKeys[ctrlID], metaKey)
				}
			}
		}
	}

	out := make([]docSection, 0, len(sectionOrder))
	for _, secID := range sectionOrder {
		acc := accums[seenSection[secID]]
		sec := docSection{
			ID:          secID,
			Title:       lookupLabel(allKeys, acc.titleKey, secID),
			Description: allKeys[acc.descKey],
		}
		for _, ctrlID := range acc.controlIDs {
			ctrl, err := buildControl(ctrlID, acc.controlKeys[ctrlID], allKeys)
			if err != nil {
				return nil, fmt.Errorf("section %q: %w", secID, err)
			}
			sec.Controls = append(sec.Controls, ctrl)
		}
		out = append(out, sec)
	}
	return out, nil
}

// controlIDFor returns the stable control ID derived from a key's tier-3+
// path. For a key like settings.platform_profile.builtin_profiles.help, the
// control ID is "builtin_profiles". For a single-segment leaf like
// settings.platform_profile.preset, the ID is "preset" -- the leaf key itself
// is the control's primary label.
func controlIDFor(rest []string) string {
	if len(rest) == 0 {
		return ""
	}
	last := "." + rest[len(rest)-1]
	for _, suf := range metadataSuffixes {
		if last == suf {
			// Drop the metadata suffix so the ID names the control, not its
			// metadata role. Multi-segment paths preserve their parent
			// segments (e.g. settings.X.Y.Z.help -> Y.Z control on section X).
			if len(rest) > 1 {
				return strings.Join(rest[:len(rest)-1], ".")
			}
			// Defensive fallback: a bare ".help" with no parent name shouldn't
			// happen in practice, but treat it as orphaned metadata.
			return rest[0]
		}
	}
	return strings.Join(rest, ".")
}

// buildControl assembles a docControl from the bag of keys belonging to one
// control ID. The label resolves from .label first, then the bare-control
// key. If neither exists but the control carries description/help/visibility
// metadata, that's an i18n mismatch the docs page has no clean fallback for
// -- humanizing the slug into "Multi user" or "Oidc" silently masks the
// missing label and lets the rendered doc drift away from the UI. Return an
// error in that case so the generator surfaces the broken key shape rather
// than papering over it.
//
// A control with no metadata at all (no description/help/visibility) AND no
// label still humanizes the slug -- that case is genuinely ambiguous (could
// be a runtime UI key the noise filter missed) and erroring on it would
// produce constant churn, so we keep the existing fallback there.
func buildControl(ctrlID string, ctrlKeys []string, allKeys map[string]string) (docControl, error) {
	ctrl := docControl{ID: ctrlID}
	for _, k := range ctrlKeys {
		switch suffix := lastSegmentOf(k); suffix {
		case "label":
			ctrl.Label = allKeys[k]
		case "description":
			ctrl.Description = allKeys[k]
		case "help":
			ctrl.Help = allKeys[k]
		case "visibility":
			ctrl.Visibility = allKeys[k]
		case "placeholder":
			// Placeholders are UI hints, not user-facing reference content.
			// Skip silently.
		default:
			// Bare control key: settings.{section}.{control} with no metadata
			// suffix. Use its value as the label when no .label is set.
			if ctrl.Label == "" {
				ctrl.Label = allKeys[k]
			}
		}
	}
	if ctrl.Label == "" {
		hasMetadata := ctrl.Description != "" || ctrl.Help != "" || ctrl.Visibility != ""
		if hasMetadata {
			return ctrl, fmt.Errorf("control %q has description/help/visibility but no .label or bare-key label; keys: %s", ctrlID, strings.Join(ctrlKeys, ", "))
		}
		ctrl.Label = humanize(ctrlID)
	}
	return ctrl, nil
}

// lastSegmentOf returns the final dot-segment of a key (the metadata role for
// that key, when one applies).
func lastSegmentOf(k string) string {
	if i := strings.LastIndex(k, "."); i >= 0 {
		return k[i+1:]
	}
	return k
}

// lookupLabel returns the i18n value at key, or fallback humanized when the
// key is empty or absent.
func lookupLabel(keys map[string]string, key, fallback string) string {
	if key == "" {
		return humanize(fallback)
	}
	if v, ok := keys[key]; ok && v != "" {
		return v
	}
	return humanize(fallback)
}

// humanize converts an underscore_id to "Underscore id" form for display when
// no i18n label is available.
func humanize(id string) string {
	if id == "" {
		return ""
	}
	parts := strings.Split(id, "_")
	for i, p := range parts {
		if i == 0 && p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// subsectionControls names controls that render as H4 sub-section headings
// instead of bulleted list items within their parent section. The mapping
// is keyed by section ID; each entry is the control ID to promote.
var subsectionControls = map[string]map[string]struct{}{
	"users": {
		"multi_user_mode": {},
		"pending_invites": {},
	},
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// markdownEscape entity-encodes the angle brackets in user-facing prose so
// XML/HTML-like tokens (e.g. <lockdata>true</lockdata>) render as literal
// text in MkDocs Material output rather than being silently dropped as
// unknown inline HTML elements. Ampersands are intentionally NOT escaped:
// prose contains them legitimately ("save & restart"), and the runtime UI
// reads the same i18n strings unescaped (tooltips, screen readers) so
// escaping the source would leak literal entities into the live UI.
func markdownEscape(s string) string {
	return mdEscapeReplacer.Replace(s)
}

var mdEscapeReplacer = strings.NewReplacer("<", "&lt;", ">", "&gt;")

// renderDocument walks the document tree and emits the Markdown body that
// goes between the BEGIN/END markers. Tabs render as `##` headings, sections
// as `###` headings with optional prose description, and controls as bullet
// items inside each section. Each control's bullet carries an inline anchor
// (via MkDocs Material's attr_list extension) so HelpHint deep links can
// target individual controls without exploding the heading hierarchy.
func renderDocument(doc document) string {
	var b strings.Builder
	// Lead the body with a single newline so the BEGIN marker line is
	// followed by a blank line before the first `## Tab` heading
	// (markdownlint MD022 requires headings to be surrounded by blanks).
	// Subsequent tab headings sit one blank line below the previous tab's
	// last section, which already emits a trailing blank in renderSection.
	b.WriteString("\n")
	for _, tab := range doc.Tabs {
		fmt.Fprintf(&b, "## %s  {#%s}\n\n", markdownEscape(tab.Label), tabAnchor(tab.ID))
		// Optional intro paragraph (settings.tab.{id}.intro) between H2 and first H3.
		if tab.Intro != "" {
			b.WriteString(markdownEscape(tab.Intro))
			b.WriteString("\n\n")
		}
		for _, sec := range tab.Sections {
			renderSection(&b, tab.ID, sec)
		}
	}
	return b.String()
}

func renderSection(b *strings.Builder, tabID string, sec docSection) {
	fmt.Fprintf(b, "### %s  {#%s}\n\n", markdownEscape(sec.Title), sectionAnchor(tabID, sec.ID))
	if sec.Description != "" {
		b.WriteString(markdownEscape(sec.Description))
		b.WriteString("\n\n")
	}
	if len(sec.Controls) == 0 {
		return
	}
	for _, ctrl := range sec.Controls {
		renderControl(b, tabID, sec.ID, ctrl)
	}
	b.WriteString("\n")
}

// renderControl emits either a bullet-list entry or an H4 sub-section heading,
// depending on whether the section+control pair appears in subsectionControls.
// Promoted controls share the bullet path's anchor scheme so HelpHint deep links
// continue to resolve.
//
// The bullet path uses the attr_list block form ({: #anchor } on its own line)
// rather than the inline form because Python-Markdown's attr_list extension
// only attaches inline {#...} to the immediately preceding inline element; the
// block form produces <li id="anchor"> as required by the HelpHint contract.
func renderControl(b *strings.Builder, tabID, secID string, ctrl docControl) {
	if promoted, ok := subsectionControls[secID]; ok {
		if _, isSubsection := promoted[ctrl.ID]; isSubsection {
			renderSubsectionControl(b, tabID, secID, ctrl)
			return
		}
	}

	fmt.Fprintf(b, "- **%s**", markdownEscape(ctrl.Label))

	prose := composeControlProse(ctrl)
	if prose != "" {
		b.WriteString(" -- ")
		b.WriteString(prose)
	}
	b.WriteString("\n")
	fmt.Fprintf(b, "{: #%s }\n", controlAnchor(tabID, secID, ctrl.ID))
}

// renderSubsectionControl emits an H4 heading for a promoted control. The anchor
// uses the same scheme as the bullet path so HelpHint deep links are unaffected.
// Description and visibility render as paragraphs below the heading; help is
// intentionally omitted (same policy as the bullet path: it backs the in-app
// ContextHelp popover, not the docs surface).
func renderSubsectionControl(b *strings.Builder, tabID, secID string, ctrl docControl) {
	// Ensure a blank line above the H4 (markdownlint MD022 requires it). When
	// the H4 follows a bullet item the buffer ends in a single newline; we need
	// one more. When it follows a description paragraph the buffer already ends
	// in \n\n so no extra newline is needed.
	if !strings.HasSuffix(b.String(), "\n\n") {
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "#### %s  {#%s}\n\n", markdownEscape(ctrl.Label), controlAnchor(tabID, secID, ctrl.ID))
	if ctrl.Description != "" {
		b.WriteString(markdownEscape(ctrl.Description))
		b.WriteString("\n\n")
	}
	if ctrl.Visibility != "" {
		b.WriteString("*Visibility:* ")
		b.WriteString(markdownEscape(ctrl.Visibility))
		b.WriteString("\n\n")
	}
}

// composeControlProse returns the inline prose that follows the bullet's
// label. Description leads, visibility appends in italic when present.
// Empty when the control carries no prose at all (label-only bullet,
// anchor still emitted for HelpHint deep links).
//
// .help i18n keys are deliberately NOT rendered: they back the in-app
// ContextHelp popover (terse, one-sentence) and are paired with a
// longer-form .description that is the docs surface. Surfacing both
// produces visible duplication on the rendered reference page.
func composeControlProse(ctrl docControl) string {
	parts := []string{}
	if ctrl.Description != "" {
		parts = append(parts, markdownEscape(ctrl.Description))
	}
	if ctrl.Visibility != "" {
		parts = append(parts, "*Visibility:* "+markdownEscape(ctrl.Visibility))
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------------------
// Anchor scheme
// ---------------------------------------------------------------------------

// tabAnchor returns the slug for a Settings tab heading. Format: "tab-{id}"
// with underscores rewritten to hyphens.
func tabAnchor(tabID string) string {
	return "tab-" + strings.ReplaceAll(tabID, "_", "-")
}

// sectionAnchor returns the slug for a section heading. Format:
// "settings-{tab}-{section}" with underscores rewritten to hyphens. The tab
// prefix disambiguates section names that legitimately repeat across tabs --
// for example, the Rules tab and the Maintenance tab both expose a "Schedule"
// section under different i18n namespaces, and an unscoped slug would emit
// duplicate {#settings-schedule} IDs that break HelpHint deep-link routing
// and produce invalid HTML.
func sectionAnchor(tabID, secID string) string {
	return "settings-" + hyphenate(tabID) + "-" + hyphenate(secID)
}

// controlAnchor returns the slug for a control heading. Format:
// "settings-{tab}-{section}-{control}" with underscores rewritten to hyphens
// and dot-separated control IDs flattened to hyphens. Same tab-scoping
// rationale as sectionAnchor.
func controlAnchor(tabID, secID, ctrlID string) string {
	flat := strings.ReplaceAll(ctrlID, ".", "-")
	return "settings-" + hyphenate(tabID) + "-" + hyphenate(secID) + "-" + hyphenate(flat)
}

// hyphenate normalizes underscores to hyphens for slug safety.
func hyphenate(s string) string {
	return strings.ReplaceAll(s, "_", "-")
}

// collectAnchors walks the document and returns every anchor it emits, sorted
// for deterministic output. Returns a non-nil error when two anchors collide,
// so duplicates (which would silently break HelpHint deep links and produce
// invalid HTML) fail generation rather than getting glossed over by a
// dedupe pass.
func collectAnchors(doc document) ([]string, error) {
	seen := make(map[string]string) // anchor -> first source (debug context)
	anchors := make([]string, 0)
	add := func(anchor, source string) error {
		if prior, ok := seen[anchor]; ok {
			return fmt.Errorf("duplicate anchor %q: emitted by %s and %s", anchor, prior, source)
		}
		seen[anchor] = source
		anchors = append(anchors, anchor)
		return nil
	}

	for _, tab := range doc.Tabs {
		if err := add(tabAnchor(tab.ID), "tab "+tab.ID); err != nil {
			return nil, err
		}
		for _, sec := range tab.Sections {
			if err := add(sectionAnchor(tab.ID, sec.ID), "section "+tab.ID+"/"+sec.ID); err != nil {
				return nil, err
			}
			for _, ctrl := range sec.Controls {
				if err := add(controlAnchor(tab.ID, sec.ID, ctrl.ID), "control "+tab.ID+"/"+sec.ID+"/"+ctrl.ID); err != nil {
					return nil, err
				}
			}
		}
	}
	sort.Strings(anchors)
	return anchors, nil
}

// ---------------------------------------------------------------------------
// Marker replacement (mirrors gen-rules-catalogue, gen-env-reference)
// ---------------------------------------------------------------------------

// replaceBetweenMarkers returns src with the region between begin and end
// replaced by body. The markers themselves are preserved. Trailing newlines
// on body are normalized.
//
// begin and end are parameters (not constants) so the helper can be exercised
// with bespoke markers in tests.
func replaceBetweenMarkers(src []byte, begin, end, body string) ([]byte, error) {
	beginIdx := bytes.Index(src, []byte(begin))
	if beginIdx < 0 {
		return nil, fmt.Errorf("begin marker %q not found", begin)
	}
	// Search for the end marker only after the begin marker so an incidental
	// occurrence of the end marker text earlier in the file (for example, in
	// a fenced code block illustrating the convention) cannot be mistaken for
	// the closing marker.
	relEndIdx := bytes.Index(src[beginIdx:], []byte(end))
	if relEndIdx < 0 {
		return nil, fmt.Errorf("end marker %q not found after begin marker", end)
	}
	endIdx := beginIdx + relEndIdx

	body = strings.TrimRight(body, "\n")
	var out bytes.Buffer
	out.Write(src[:beginIdx])
	out.WriteString(begin)
	out.WriteString("\n")
	out.WriteString(body)
	out.WriteString("\n")
	out.Write(src[endIdx:])
	return out.Bytes(), nil
}
