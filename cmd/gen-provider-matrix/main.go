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
// AllMusic is intentionally skipped: the adapter exists but is not currently
// part of AllProviderNames(), so the matrix only reflects in-use providers.
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

func renderFields(fields []string) string {
	if len(fields) == 0 {
		return "Image only"
	}
	// Preserve declaration order so providers can group related fields
	// (born/formed/died/disbanded) for readability. Defensive copy avoids
	// mutating the registry slice.
	out := make([]string, len(fields))
	copy(out, fields)
	return strings.Join(out, ", ")
}

func renderImages(images []string) string {
	if len(images) == 0 {
		return "None"
	}
	out := make([]string, len(images))
	copy(out, images)
	return strings.Join(out, ", ")
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
