// Command gen-cli-reference renders the CLI flags reference table in
// docs/site/src/reference/cli.md from struct tags on the Flags type in
// internal/cli. Each rendered row consists of the flag name, a doc-friendly
// type label, the documented default, and a description sentence. The
// generator reflects over the Flags struct, requires every field that carries
// a flag: tag to also carry a desc: tag (coverage enforcement -- generation
// fails loudly if the tag is absent), and writes the table between the
// well-known BEGIN/END markers in the docs file. Manual prose around the
// markers is preserved.
//
// Coverage contract:
//   - CLI flags (internal/cli.Flags): every flag: field must have a desc: tag;
//     the generator fails at generation time if any is missing. Adding a new
//     CLI flag without a desc: tag breaks the build.
//   - Subcommands (internal/cli.Subcommands): the generator renders every entry
//     in the Subcommands slice as a separate section. Adding a subcommand entry
//     automatically documents it; there is no additional coverage check because
//     the slice is the authoritative registry.
//
// Usage:
//
//	go run ./cmd/gen-cli-reference              # rewrite the file in place
//	go run ./cmd/gen-cli-reference -check       # exit non-zero if regen needed
//	go run ./cmd/gen-cli-reference -output FILE # write to a different file
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	icli "github.com/sydlexius/stillwater/internal/cli"
	"github.com/sydlexius/stillwater/internal/filesystem"
)

const (
	beginMarker = "<!-- BEGIN GENERATED: cli-reference -->"
	endMarker   = "<!-- END GENERATED: cli-reference -->"
)

const defaultOutputPath = "docs/site/src/reference/cli.md"

func main() {
	var (
		checkOnly bool
		outPath   string
	)
	flag.BoolVar(&checkOnly, "check", false, "exit non-zero if the CLI reference needs to be regenerated")
	flag.StringVar(&outPath, "output", defaultOutputPath, "path to the docs file to update")
	flag.Parse()

	if err := run(outPath, checkOnly); err != nil {
		fmt.Fprintln(os.Stderr, "gen-cli-reference:", err)
		os.Exit(1)
	}
}

func run(outPath string, checkOnly bool) error {
	rows, err := collectRows(reflect.TypeOf(icli.Flags{}))
	if err != nil {
		return fmt.Errorf("collect rows: %w", err)
	}
	rendered := renderContent(rows, icli.Subcommands)

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

// flagRow is a single row in the rendered Markdown table.
type flagRow struct {
	Name        string
	Type        string
	Default     string
	Description string
}

// collectRows reflects over t (which must be a struct type) and returns one
// flagRow per field carrying a flag: tag. Fields with a flag: tag but no
// desc: tag fail with an error so that the docs page never silently emits an
// empty description column. This is the coverage enforcement: every registered
// CLI flag must have a documented description.
func collectRows(t reflect.Type) ([]flagRow, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("collectRows: expected struct, got %s", t.Kind())
	}
	var rows []flagRow
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		flagName := f.Tag.Get("flag")
		if flagName == "" {
			continue
		}
		desc := f.Tag.Get("desc")
		if desc == "" {
			return nil, fmt.Errorf(
				"field %s has flag:%q tag but no desc: tag; add a one-sentence description "+
					"so the CLI reference page is never empty (coverage enforcement)",
				f.Name, flagName)
		}
		def := f.Tag.Get("default")
		rows = append(rows, flagRow{
			Name:        flagName,
			Type:        flagDocType(f.Type),
			Default:     renderDefault(def),
			Description: desc,
		})
	}
	return rows, nil
}

// flagDocType maps a Go reflect.Type to the user-facing type label used in
// docs. User docs avoid Go-specific notation; the labels here are deployment-
// operator vocabulary.
func flagDocType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "boolean"
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	default:
		return "string"
	}
}

// renderDefault converts the raw default: tag value into the cell text used
// in the table. Empty strings render as "(none)"; "false" and other literals
// render in backticks.
func renderDefault(def string) string {
	if def == "" {
		return "(none)"
	}
	return "`" + def + "`"
}

// renderContent returns the full Markdown body to place between the BEGIN/END
// markers. It includes a flags table and a subcommands section.
func renderContent(rows []flagRow, subs []icli.SubcommandInfo) string {
	var b strings.Builder

	// Flags table.
	b.WriteString("## Flags\n\n")
	b.WriteString("| Flag | Type | Default | Description |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| `--%s` | %s | %s | %s |\n",
			escapeMarkdownCell(r.Name),
			escapeMarkdownCell(r.Type),
			escapeMarkdownCell(r.Default),
			escapeMarkdownCell(r.Description),
		)
	}

	if len(subs) == 0 {
		return b.String()
	}

	// Subcommands section.
	b.WriteString("\n## Subcommands\n\n")
	b.WriteString("Subcommands are passed as the first positional argument before any flags ")
	b.WriteString("(e.g. `stillwater reset-credentials`).\n\n")
	b.WriteString("| Subcommand | Summary |\n")
	b.WriteString("|---|---|\n")
	for _, s := range subs {
		fmt.Fprintf(&b, "| `%s` | %s |\n",
			escapeMarkdownCell(s.Name),
			escapeMarkdownCell(s.Summary),
		)
	}

	// Per-subcommand detail sections.
	for _, s := range subs {
		b.WriteString("\n### `")
		b.WriteString(s.Name)
		b.WriteString("`\n\n")
		b.WriteString(s.Details)
		b.WriteString("\n")
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
