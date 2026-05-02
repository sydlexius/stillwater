// Command gen-provider-matrix renders the "Supported Providers" capability
// matrix in docs/site/src/reference/providers.md from the live provider
// registry. It walks AllProviderNames() in display order, reads each
// provider's ProviderCapability, and writes a Markdown table between the
// well-known BEGIN/END markers in the docs file.
//
// Usage:
//
//	go run ./cmd/gen-provider-matrix              # rewrite the file in place
//	go run ./cmd/gen-provider-matrix -check       # exit non-zero if regen needed
//	go run ./cmd/gen-provider-matrix -output FILE # write to a different file
//
// AllMusic is intentionally excluded: the adapter exists in
// internal/provider/allmusic but is not part of AllProviderNames() in normal
// builds. The renderer also filters it defensively in case the registry order
// changes; see project_allmusic_cleanup / issue #1275.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/stillwater/internal/provider"
)

const (
	beginMarker = "<!-- BEGIN GENERATED: provider-matrix -->"
	endMarker   = "<!-- END GENERATED: provider-matrix -->"
)

// defaultOutputPath is the docs file the generator writes to. It is a
// repo-relative path resolved from the current working directory.
const defaultOutputPath = "docs/site/src/reference/providers.md"

func main() {
	var (
		checkOnly bool
		outPath   string
	)
	flag.BoolVar(&checkOnly, "check", false, "exit non-zero if the matrix needs to be regenerated")
	flag.StringVar(&outPath, "output", defaultOutputPath, "path to the docs file to update")
	flag.Parse()

	if err := run(outPath, checkOnly); err != nil {
		fmt.Fprintln(os.Stderr, "gen-provider-matrix:", err)
		os.Exit(1)
	}
}

func run(outPath string, checkOnly bool) error {
	// outPath comes from the -output flag (or the default docs path); this
	// is a developer-only build-time tool, so a configurable path is intended.
	existing, err := os.ReadFile(outPath) //nolint:gosec // G304: developer CLI, path is intentionally configurable
	if err != nil {
		return fmt.Errorf("read %s: %w", outPath, err)
	}

	rendered := renderMatrix(provider.AllProviderNames(), provider.ProviderCapabilities())
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
	if err := os.WriteFile(outPath, updated, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	return nil
}

// renderMatrix returns the Markdown table body (without surrounding markers)
// for the providers in names, looked up in caps. Providers absent from caps
// are skipped, as is the AllMusic adapter (not part of AllProviderNames in
// practice, but defensively skipped here).
func renderMatrix(names []provider.ProviderName, caps map[provider.ProviderName]provider.ProviderCapability) string {
	var b strings.Builder
	b.WriteString("| Provider | Tier | Sign-up | Rate limit | Mirror | Metadata fields | Image types |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, name := range names {
		if name == provider.NameAllMusic {
			continue
		}
		c, ok := caps[name]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
			name.DisplayName(),
			renderTier(c.Tier),
			renderSignup(c.HelpURL),
			renderRateLimit(c.RateLimit),
			renderYesNo(c.SupportsBaseURL),
			renderFields(c.SupportedFields),
			renderImages(c.SupportedImages),
		)
	}
	return b.String()
}

func renderTier(t provider.AccessTier) string {
	switch t {
	case provider.TierFree:
		return "Free"
	case provider.TierFreeKey:
		return "Free key"
	case provider.TierFreemium:
		return "Freemium"
	case provider.TierPaid:
		return "Paid"
	default:
		return string(t)
	}
}

func renderSignup(helpURL string) string {
	if helpURL == "" {
		return "Not required"
	}
	return fmt.Sprintf("[Sign up](%s)", helpURL)
}

func renderRateLimit(rl *provider.RateLimitInfo) string {
	if rl == nil {
		return "Unknown"
	}
	var parts []string
	if rl.RequestsPerSecond > 0 {
		parts = append(parts, formatPerSecond(rl.RequestsPerSecond))
	}
	if rl.RequestsPerDay > 0 {
		parts = append(parts, fmt.Sprintf("%d/day", rl.RequestsPerDay))
	}
	if rl.RequestsPerMonth > 0 {
		parts = append(parts, fmt.Sprintf("%d/month", rl.RequestsPerMonth))
	}
	if len(parts) == 0 {
		return "Unknown"
	}
	return strings.Join(parts, ", ")
}

// formatPerSecond renders requests-per-second as a compact human label.
// Whole-number rates (1, 3, 5) render as "N/sec"; sub-second rates render in
// per-minute terms ("30/min" for 0.5 rps) so the matrix doesn't display
// "0.5/sec".
func formatPerSecond(rps float64) string {
	if rps >= 1 {
		// Trim a trailing ".0" if the value is integral.
		if rps == float64(int(rps)) {
			return fmt.Sprintf("%d/sec", int(rps))
		}
		return fmt.Sprintf("%.1f/sec", rps)
	}
	perMin := rps * 60
	if perMin == float64(int(perMin)) {
		return fmt.Sprintf("%d/min", int(perMin))
	}
	return fmt.Sprintf("%.1f/min", perMin)
}

func renderYesNo(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

// renderFields joins the provider's declared metadata fields with commas.
// Declaration order is preserved so registries can group related fields
// (born/formed/died/disbanded) for readability. Returns "Image only" when no
// metadata fields are declared (e.g., Fanart.tv). Field names are emitted in
// user-facing form: snake_case identifiers are detokenized into space-separated
// words with sentence-case capitalization (e.g., sort_name -> Sort name).
func renderFields(fields []string) string {
	if len(fields) == 0 {
		return "Image only"
	}
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = friendlyFieldName(f)
	}
	return strings.Join(out, ", ")
}

// renderImages joins the provider's declared image types. Returns "None" for
// metadata-only providers. Image type identifiers are emitted as-is (lowercase,
// matching the convention used elsewhere in user docs: thumb, fanart, hdlogo,
// widethumb, etc.).
func renderImages(images []string) string {
	if len(images) == 0 {
		return "None"
	}
	return strings.Join(images, ", ")
}

// friendlyFieldName converts a snake_case metadata-field identifier into the
// sentence-case user-facing form used in docs prose (sort_name -> "Sort name",
// years_active -> "Years active"). Identifiers without underscores are returned
// with only the first letter capitalized (name -> "Name").
func friendlyFieldName(s string) string {
	if s == "" {
		return s
	}
	spaced := strings.ReplaceAll(s, "_", " ")
	return strings.ToUpper(spaced[:1]) + spaced[1:]
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
	endIdx := bytes.Index(src, []byte(end))
	if endIdx < 0 {
		return nil, fmt.Errorf("end marker %q not found", end)
	}
	if endIdx < beginIdx {
		return nil, fmt.Errorf("end marker appears before begin marker")
	}

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
