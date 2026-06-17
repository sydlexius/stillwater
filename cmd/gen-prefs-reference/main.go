// Command gen-prefs-reference renders the preferences reference table in
// docs/site/src/reference/preferences.md from the canonical preference
// registry (internal/api.PreferenceRegistry). Each rendered row includes the
// preference key, its default value, allowed values or range, and a
// description resolved from the i18n locale file.
//
// Usage:
//
//	go run ./cmd/gen-prefs-reference              # rewrite the file in place
//	go run ./cmd/gen-prefs-reference -check       # exit non-zero if regen needed
//	go run ./cmd/gen-prefs-reference -output FILE # write to a different file
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sydlexius/stillwater/internal/api"
	"github.com/sydlexius/stillwater/internal/filesystem"
)

const (
	beginMarker = "<!-- BEGIN GENERATED: prefs-reference -->"
	endMarker   = "<!-- END GENERATED: prefs-reference -->"

	defaultOutputPath = "docs/site/src/reference/preferences.md"
	defaultI18nPath   = "internal/i18n/locales/en.json"
)

func main() {
	var (
		checkOnly bool
		outPath   string
		i18nPath  string
	)
	flag.BoolVar(&checkOnly, "check", false, "exit non-zero if the preferences reference needs to be regenerated")
	flag.StringVar(&outPath, "output", defaultOutputPath, "path to the docs file to update")
	flag.StringVar(&i18nPath, "i18n", defaultI18nPath, "path to the i18n locale file (en.json)")
	flag.Parse()

	if err := run(outPath, i18nPath, checkOnly); err != nil {
		fmt.Fprintln(os.Stderr, "gen-prefs-reference:", err)
		os.Exit(1)
	}
}

func run(outPath, i18nPath string, checkOnly bool) error {
	// Load i18n locale strings for label/description resolution.
	i18n, err := loadI18n(i18nPath)
	if err != nil {
		return fmt.Errorf("load i18n: %w", err)
	}

	// Read the registry; it is already sorted alphabetically.
	entries := api.PreferenceRegistry()

	rendered := renderTable(entries, i18n)

	// outPath comes from the -output flag; this is a developer-only build-time
	// tool, so a configurable path is intended.
	existing, err := os.ReadFile(outPath) //nolint:gosec // G304: developer CLI, path is intentionally configurable
	if err != nil {
		return fmt.Errorf("read %s: %w", outPath, err)
	}

	updated, err := replaceBetweenMarkers(existing, beginMarker, endMarker, rendered)
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
	if err := filesystem.WriteFileAtomic(outPath, updated, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	return nil
}

// loadI18n reads the locale JSON file and returns a flat key-to-string map.
// The en.json file is a flat object: { "prefs.theme.label": "Theme", ... }.
func loadI18n(path string) (map[string]string, error) {
	// path comes from the -i18n flag; this is a developer-only build-time tool.
	data, err := os.ReadFile(path) //nolint:gosec // G304: developer CLI, path is intentionally configurable
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// prefRow is a single row in the rendered Markdown table.
type prefRow struct {
	Key         string
	Label       string
	Default     string
	AllowedStr  string // formatted allowed values or range
	Description string
}

// buildRows converts registry entries into table rows with i18n labels resolved.
func buildRows(entries []api.PreferenceDef, i18n map[string]string) []prefRow {
	rows := make([]prefRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, prefRow{
			Key:         e.Key,
			Label:       resolveLabel(e.Key, i18n),
			Default:     e.Default,
			AllowedStr:  formatAllowed(e),
			Description: resolveDescription(e.Key, i18n),
		})
	}
	// Entries from PreferenceRegistry are already sorted; sort here defensively
	// so the output is deterministic even if the registry order changes.
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Key < rows[j].Key
	})
	return rows
}

// keyI18nNamespace maps preference keys whose i18n entries live under a
// namespace that does not follow the default prefs.{key}.* convention. Each
// value is the full namespace prefix (without trailing dot) to use in place of
// the conventional lookup. Keys absent from this map use the two-step fallback
// chain: prefs.{key} then settings.appearance.{key}.
//
// Cases requiring an override:
//   - auto_fetch_images: i18n authors named the entries prefs.auto_fetch.*
//     (the UI label is "Prefetch Images", not "Auto Fetch Images").
//   - sidebar_state: the settings namespace uses sidebar_default_state as the
//     disambiguating suffix, so the i18n key is
//     settings.appearance.sidebar_default_state.label.
var keyI18nNamespace = map[string]string{
	"auto_fetch_images": "prefs.auto_fetch",
	"sidebar_state":     "settings.appearance.sidebar_default_state",
}

// i18nNamespaces returns the ordered list of i18n namespace prefixes to
// search for a given preference key. Overridden keys get a single canonical
// namespace; all others get the two-step fallback chain.
func i18nNamespaces(key string) []string {
	if ns, ok := keyI18nNamespace[key]; ok {
		return []string{ns}
	}
	return []string{"prefs." + key, "settings.appearance." + key}
}

// resolveLabel looks up the human-readable label for a preference key using
// the i18n namespace chain for that key (see i18nNamespaces). If no i18n
// label is found it converts the snake_case key to Title Case as a last resort
// (e.g. "metadata_name_romanization_fallback" -> "Metadata Name Romanization Fallback").
func resolveLabel(key string, i18n map[string]string) string {
	for _, ns := range i18nNamespaces(key) {
		if v, ok := i18n[ns+".label"]; ok {
			return v
		}
	}
	return snakeToTitle(key)
}

// snakeToTitle converts a snake_case identifier to Title Case words
// (e.g. "auto_fetch_images" -> "Auto Fetch Images"). Used as a fallback
// label when no i18n entry is found for a preference key.
func snakeToTitle(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// resolveDescription looks up a description for a preference key. For each
// namespace in i18nNamespaces(key) it tries the .description suffix and then
// the .help suffix, returning the first match found.
func resolveDescription(key string, i18n map[string]string) string {
	for _, ns := range i18nNamespaces(key) {
		for _, suffix := range []string{".description", ".help"} {
			if v, ok := i18n[ns+suffix]; ok {
				return v
			}
		}
	}
	return ""
}

// formatAllowed returns the "Allowed Values" cell text for a preference entry.
// Enum prefs list values comma-separated in backticks. Range prefs show the
// numeric range and step.
//
// The sentinel contract (from PreferenceDef): BOTH RangeMin == 0 AND
// RangeMax == 0 means enum (no numeric range). A range whose min or max is
// non-zero -- including ranges with a zero max, e.g. -10..0 -- must be
// formatted as a range. The old guard `e.RangeMax > 0` incorrectly skipped
// any range with RangeMax <= 0; this guard checks the correct both-zero
// condition.
func formatAllowed(e api.PreferenceDef) string {
	if len(e.AllowedValues) > 0 {
		parts := make([]string, len(e.AllowedValues))
		for i, v := range e.AllowedValues {
			parts[i] = "`" + v + "`"
		}
		return strings.Join(parts, ", ")
	}
	// A preference is a numeric range when at least one bound is non-zero.
	// Both RangeMin == 0 AND RangeMax == 0 is the enum sentinel (no range).
	if e.RangeMin != 0 || e.RangeMax != 0 {
		if e.RangeStep > 1 {
			return fmt.Sprintf("%d..%d (step %d)", e.RangeMin, e.RangeMax, e.RangeStep)
		}
		return fmt.Sprintf("%d..%d", e.RangeMin, e.RangeMax)
	}
	return ""
}

// renderTable returns the Markdown table body (without surrounding markers).
func renderTable(entries []api.PreferenceDef, i18n map[string]string) string {
	rows := buildRows(entries, i18n)
	var b strings.Builder
	b.WriteString("| Key | Label | Default | Allowed Values | Description |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, r := range rows {
		// Each key cell includes an HTML anchor for deep linking: #pref-{key}
		anchor := `<a id="pref-` + r.Key + `"></a>`
		fmt.Fprintf(&b, "| %s`%s` | %s | `%s` | %s | %s |\n",
			anchor,
			escapeMarkdownCell(r.Key),
			escapeMarkdownCell(r.Label),
			escapeMarkdownCell(r.Default),
			escapeMarkdownCell(r.AllowedStr),
			escapeMarkdownCell(r.Description),
		)
	}
	return b.String()
}

// escapeMarkdownCell sanitizes a string for use inside a Markdown table cell.
// Pipes break the column boundary; newlines break the row boundary.
func escapeMarkdownCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", "<br>")
	return s
}

// replaceBetweenMarkers returns src with the region between begin and end
// replaced by body. The markers themselves are preserved. body is sandwiched
// in newlines so the rendered table has clean blank-line separation from the
// markers; trailing newlines on body are normalized.
//
// begin and end are parameters (not constants) so the helper can be exercised
// with bespoke markers in tests.
func replaceBetweenMarkers(src []byte, begin, end, body string) ([]byte, error) { //nolint:unparam // begin/end are exposed as parameters for testability
	beginIdx := bytes.Index(src, []byte(begin))
	if beginIdx < 0 {
		return nil, fmt.Errorf("begin marker %q not found", begin)
	}
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
