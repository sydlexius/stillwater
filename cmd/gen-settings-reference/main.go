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

// templTrunkPath is the page-rendering templ file: it owns the
// data-tab-panel="X" div tree that defines the tab boundaries the scanner
// uses for key attribution.
const templTrunkPath = "web/templates/settings.templ"

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

// discoverTemplSources auto-discovers the trunk + sub-template files the
// panel scanner walks, replacing what used to be a hand-maintained allowlist.
// Returns the trunk path first, followed by sub-templates in sorted order
// for deterministic output. The sub-template-to-panel map derives the panel
// ID from the filename (`settings_users.templ` -> `users`); if a future
// sub-template uses a panel ID that doesn't match its filename, add it to
// subTemplateExclude (skip discovery) and pass it explicitly.
//
// glob is the filesystem-style pattern for sub-template discovery; it is
// resolved with filepath.Glob, so callers can override during tests.
func discoverTemplSources(trunk, glob string) ([]string, map[string]string, error) {
	subPaths, err := filepath.Glob(glob)
	if err != nil {
		return nil, nil, fmt.Errorf("glob %q: %w", glob, err)
	}
	sort.Strings(subPaths)

	sources := []string{trunk}
	owner := make(map[string]string)
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
		stem := strings.TrimSuffix(base, ".templ")       // settings_users
		panelID := strings.TrimPrefix(stem, "settings_") // users
		owner[p] = panelID
		sources = append(sources, p)
	}
	return sources, owner, nil
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

	sources, owner, err := discoverTemplSources(templTrunkPath, templSubTemplateGlob)
	if err != nil {
		return fmt.Errorf("discover templ sources: %w", err)
	}
	tabs, err := scanPanels(sources, owner)
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
	return writeAnchorMirrors([]string{anchorsPath, componentsAnchorsMirror}, anchors, checkOnly)
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
// Settings tab's div in the trunk templ file.
var panelOpenRE = regexp.MustCompile(`data-tab-panel="([a-z_]+)"`)

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

// i18nCallRE matches t(ctx, "settings...") invocations in templ source. The
// pattern accepts arbitrary whitespace between t and (, and between , and the
// quoted key, since templ source can wrap calls across lines.
var i18nCallRE = regexp.MustCompile(`\bt\s*\(\s*ctx\s*,\s*"(settings\.[A-Za-z0-9_.]+)"`)

// scanPanels walks the trunk and sub-template files and returns one panel per
// data-tab-panel="X" region in trunk-source order. Sub-template files are
// attributed wholly to the panel named in subTemplateOwner.
func scanPanels(sources []string, subOwner map[string]string) ([]panel, error) {
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

	// First pass: locate every data-tab-panel="X" in trunk and record its
	// byte offset. The trunk file embeds inline JavaScript that uses the same
	// attribute as a CSS selector (document.querySelector('[data-tab-panel=...]')),
	// so the regex matches both panel-div openers AND those JS references.
	// Dedupe by panel ID, keeping the first occurrence -- panel divs sit near
	// the top of the file, JS query helpers below them.
	allMatches := panelOpenRE.FindAllSubmatchIndex(trunkData, -1)
	if len(allMatches) == 0 {
		return nil, fmt.Errorf("no data-tab-panel openers found in %s", trunk)
	}
	openMatches := make([][]int, 0, len(allMatches))
	seenID := make(map[string]struct{}, len(allMatches))
	for _, m := range allMatches {
		id := string(trunkData[m[2]:m[3]])
		if _, ok := seenID[id]; ok {
			continue
		}
		seenID[id] = struct{}{}
		openMatches = append(openMatches, m)
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

	// trunkHelpers indexes every `templ FUNC(...) {` declaration in the trunk
	// to its body byte range, so panel regions can fold in the keys of
	// in-trunk helpers they invoke via `@FUNC(...)` (e.g. settingsUpdatesTab,
	// which renders the entire Updates tab content from outside the panel div).
	trunkHelpers := indexTemplHelpers(trunkData)

	panels := make([]panel, 0, len(openMatches))
	for i, m := range openMatches {
		id := string(trunkData[m[2]:m[3]])
		start := m[0]
		// Region ends at the next deduped panel opener; for the last panel,
		// we stop at pageFuncEnd (the start of the next `^templ X(` after the
		// last panel) so the trailing helper functions don't pollute it.
		end := pageFuncEnd
		if i+1 < len(openMatches) {
			end = openMatches[i+1][0]
		}
		region := trunkData[start:end]
		keys := extractKeys(region)
		// Follow @helperName(...) calls inside the panel region. Any keys in
		// the helper's body belong to this panel, since the @call delegates
		// rendering to the helper.
		for _, hm := range panelHelperCallRE.FindAllSubmatch(region, -1) {
			helperRange, ok := trunkHelpers[string(hm[1])]
			if !ok {
				continue
			}
			helperKeys := extractKeys(trunkData[helperRange[0]:helperRange[1]])
			keys = appendUnique(keys, helperKeys)
		}
		panels = append(panels, panel{ID: id, Keys: keys})
	}

	// Second pass: each sub-template file contributes its keys to the panel
	// named in subOwner. The keys are appended in sub-file order after the
	// trunk-region keys for that panel.
	for _, src := range sources[1:] {
		owner, ok := subOwner[src]
		if !ok {
			return nil, fmt.Errorf("sub-template %s has no entry in subTemplateOwner", src)
		}
		data, err := os.ReadFile(src) //nolint:gosec // G304: developer CLI, path is intentionally configurable
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", src, err)
		}
		extra := extractKeys(data)
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
			return nil, fmt.Errorf("sub-template %s maps to panel %q which has no data-tab-panel div in the trunk file", src, owner)
		}
	}

	return panels, nil
}

// indexTemplHelpers maps each `templ FUNC(...) {` name in src to the byte
// range of its body. The body runs from the byte after the declaration's
// match to the start of the next `^templ X(` declaration (or end-of-src for
// the last one). The range is approximate -- it does not respect a closing
// `}` -- but for the codegen's purpose (collecting i18n keys called inside
// the function), inclusivity is harmless: spurious bytes after the function's
// real `}` would be the next function's body, which is also templ source we
// trust.
func indexTemplHelpers(src []byte) map[string][2]int {
	matches := templFuncRE.FindAllSubmatchIndex(src, -1)
	out := make(map[string][2]int, len(matches))
	for i, m := range matches {
		name := string(src[m[2]:m[3]])
		start := m[1] // byte after the `templ FUNC(` opener
		end := len(src)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		out[name] = [2]int{start, end}
	}
	return out
}

// appendUnique appends every element of extras to base that is not already
// present. Used to fold helper-function keys into a panel's key list while
// preserving first-encounter order and preventing duplicates when a panel and
// its helper both reference the same key.
func appendUnique(base, extras []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extras))
	for _, k := range base {
		seen[k] = struct{}{}
	}
	for _, k := range extras {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		base = append(base, k)
	}
	return base
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
	last := lastSegmentOf(k)
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
	tab := docTab{
		ID:    p.ID,
		Label: lookupLabel(keys, "settings.tab."+p.ID, p.ID),
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

// renderControl emits a bullet-list entry for a control plus the canonical
// attr_list block-form line ({: #anchor }) that attaches an HTML id to the
// rendered <li>. Description, visibility, and help fold into the bullet's
// prose with simple inline markers; if the control has none of those, only
// the label and anchor render.
//
// We use the block form ({: #anchor } on its own line) rather than the
// inline form (- **Label** {#anchor}) because Python-Markdown's attr_list
// extension only attaches inline {#...} to the immediately preceding inline
// element (the <strong>), not to the <li>; and on bullet items without an
// adjacent inline element it leaks as raw text into the rendered prose.
// The block form is the documented way to attach attributes to list items
// and produces <li id="anchor"> as required by the HelpHint deep-link
// contract for #1132.
func renderControl(b *strings.Builder, tabID, secID string, ctrl docControl) {
	fmt.Fprintf(b, "- **%s**", markdownEscape(ctrl.Label))

	prose := composeControlProse(ctrl)
	if prose != "" {
		b.WriteString(" -- ")
		b.WriteString(prose)
	}
	b.WriteString("\n")
	fmt.Fprintf(b, "{: #%s }\n", controlAnchor(tabID, secID, ctrl.ID))
}

// composeControlProse returns the inline prose that follows the bullet's
// label. Description leads; visibility and help append in italic / bold tags
// when present. Empty when the control carries no prose at all (label-only
// bullet, anchor still emitted for HelpHint deep links).
func composeControlProse(ctrl docControl) string {
	parts := []string{}
	if ctrl.Description != "" {
		parts = append(parts, markdownEscape(ctrl.Description))
	}
	if ctrl.Visibility != "" {
		parts = append(parts, "*Visibility:* "+markdownEscape(ctrl.Visibility))
	}
	if ctrl.Help != "" {
		parts = append(parts, "**Help:** "+markdownEscape(ctrl.Help))
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
func replaceBetweenMarkers(src []byte, begin, end, body string) ([]byte, error) { //nolint:unparam // begin/end are exposed as parameters for testability
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
