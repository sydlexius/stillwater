// Command gen-env-reference renders the SW_* environment variable reference
// table in docs/site/src/reference/environment-variables.md from struct tags
// on the Config type in internal/config. Each rendered row consists of the
// variable name, a doc-friendly type label, the documented default, and a
// description sentence. The generator walks the Config struct via reflection,
// requires every field that carries an env: tag to also carry a desc: tag
// (otherwise generation fails loudly), sorts the variables alphabetically for
// deterministic output, and writes the table between the well-known
// BEGIN/END markers in the docs file. Manual prose around the markers is
// preserved.
//
// Usage:
//
//	go run ./cmd/gen-env-reference              # rewrite the file in place
//	go run ./cmd/gen-env-reference -check       # exit non-zero if regen needed
//	go run ./cmd/gen-env-reference -output FILE # write to a different file
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/sydlexius/stillwater/internal/config"
	"github.com/sydlexius/stillwater/internal/filesystem"
)

const (
	beginMarker = "<!-- BEGIN GENERATED: env-reference -->"
	endMarker   = "<!-- END GENERATED: env-reference -->"
)

const defaultOutputPath = "docs/site/src/reference/environment-variables.md"

func main() {
	var (
		checkOnly bool
		outPath   string
	)
	flag.BoolVar(&checkOnly, "check", false, "exit non-zero if the env-var reference needs to be regenerated")
	flag.StringVar(&outPath, "output", defaultOutputPath, "path to the docs file to update")
	flag.Parse()

	if err := run(outPath, checkOnly); err != nil {
		fmt.Fprintln(os.Stderr, "gen-env-reference:", err)
		os.Exit(1)
	}
}

func run(outPath string, checkOnly bool) error {
	rows, err := collectRows(reflect.TypeOf(config.Config{}))
	if err != nil {
		return fmt.Errorf("collect rows: %w", err)
	}
	rendered := renderTable(rows)

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

// envRow is a single row in the rendered Markdown table.
type envRow struct {
	Name        string
	Type        string
	Default     string
	Description string
}

// collectRows reflects over t (which must be a struct type) and returns one
// envRow per field carrying an env: tag, recursively walking nested struct
// fields. Fields with an env: tag but no desc: tag fail with an error so that
// the docs page never silently emits an empty description column.
func collectRows(t reflect.Type) ([]envRow, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("collectRows: expected struct, got %s", t.Kind())
	}
	var rows []envRow
	if err := walkStruct(t, &rows); err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

func walkStruct(t reflect.Type, rows *[]envRow) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Type.Kind() == reflect.Struct {
			if err := walkStruct(f.Type, rows); err != nil {
				return err
			}
			continue
		}
		envName := f.Tag.Get("env")
		if envName == "" {
			continue
		}
		desc := f.Tag.Get("desc")
		if desc == "" {
			return fmt.Errorf("field %s has env:%q tag but no desc: tag; add a one-sentence description so the env-var reference page is never empty", f.Name, envName)
		}
		def := f.Tag.Get("default")
		*rows = append(*rows, envRow{
			Name:        envName,
			Type:        docType(f.Type, envName),
			Default:     renderDefault(def),
			Description: desc,
		})
	}
	return nil
}

// docType maps a Go reflect.Type to the user-facing type label used in docs.
// User docs deliberately avoid Go-specific notation (no *string, no
// time.Duration, no []string); the labels here are deployment-operator
// vocabulary instead.
func docType(t reflect.Type, envName string) string {
	switch t.Kind() {
	case reflect.String:
		// Path-like variables get a clearer label so operators know to mount a
		// volume or supply an absolute path.
		if strings.Contains(envName, "PATH") {
			return "path"
		}
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "list (comma-separated)"
	default:
		return "string"
	}
}

// renderDefault converts the raw default: tag value into the cell text used in
// the table. Empty strings render as an em-dash-free "(none)" so the column is
// always populated; the literal "unset" passes through unchanged.
func renderDefault(def string) string {
	if def == "" {
		return "(none)"
	}
	if def == "unset" {
		return "unset"
	}
	return "`" + def + "`"
}

// renderTable returns the Markdown table body (without surrounding markers).
func renderTable(rows []envRow) string {
	var b strings.Builder
	b.WriteString("| Variable | Type | Default | Description |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", r.Name, r.Type, r.Default, r.Description)
	}
	return b.String()
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
