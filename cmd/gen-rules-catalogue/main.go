// Command gen-rules-catalogue renders the "Rules catalogue" reference page in
// docs/site/src/reference/rules-catalogue.md from the built-in rule definitions
// in internal/rule (rule.DefaultRules() for the authoritative rule list and
// rule.CatalogueEntry() for per-rule fix-behavior documentation metadata).
// It writes Markdown between the well-known BEGIN/END markers in the docs file.
//
// Usage:
//
//	go run ./cmd/gen-rules-catalogue              # rewrite the file in place
//	go run ./cmd/gen-rules-catalogue -check       # exit non-zero if regen needed
//	go run ./cmd/gen-rules-catalogue -output FILE # write to a different file
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/rule"
)

const (
	beginMarker = "<!-- BEGIN GENERATED: rules-catalogue -->"
	endMarker   = "<!-- END GENERATED: rules-catalogue -->"
)

// defaultOutputPath is the docs file the generator writes to. It is a
// repo-relative path resolved from the current working directory.
const defaultOutputPath = "docs/site/src/reference/rules-catalogue.md"

func main() {
	var (
		checkOnly bool
		outPath   string
	)
	flag.BoolVar(&checkOnly, "check", false, "exit non-zero if the catalogue needs to be regenerated")
	flag.StringVar(&outPath, "output", defaultOutputPath, "path to the docs file to update")
	flag.Parse()

	if err := run(outPath, checkOnly); err != nil {
		fmt.Fprintln(os.Stderr, "gen-rules-catalogue:", err)
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

	rendered := renderCatalogue(rule.DefaultRules())
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

// categoryOrder defines the display order for rule categories in the catalog.
var categoryOrder = []rule.RuleCategory{
	rule.RuleCategoryNFO,
	rule.RuleCategoryMetadata,
	rule.RuleCategoryImage,
}

// categoryDisplayName returns the user-facing label for a rule category.
func categoryDisplayName(cat rule.RuleCategory) string {
	switch cat {
	case rule.RuleCategoryNFO:
		return "NFO"
	case rule.RuleCategoryMetadata:
		return "Metadata"
	case rule.RuleCategoryImage:
		return "Image"
	default:
		return string(cat)
	}
}

// defaultState returns the human-readable default-state label for a rule
// (e.g. "Enabled, auto" or "Disabled, manual").
func defaultState(r rule.Rule) string {
	enabledLabel := "Disabled"
	if r.Enabled {
		enabledLabel = "Enabled"
	}
	mode := r.AutomationMode
	if mode == "" {
		mode = "auto"
	}
	return enabledLabel + ", " + mode
}

// nameToAnchor converts a rule name to the MkDocs Material heading anchor
// format: lowercase, spaces become hyphens, non-alphanumeric/non-hyphen chars
// are removed.
func nameToAnchor(name string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(name) {
		switch {
		case ch == ' ':
			b.WriteRune('-')
		case ch == '-' || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9'):
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// renderWhenFires returns a "When this fires:" bullet list for a rule entry.
// Returns empty string when the entry has no Examples.
func renderWhenFires(entry rule.RuleCatalogueEntry) string {
	if len(entry.Examples) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("**When this fires:**\n\n")
	for _, ex := range entry.Examples {
		sb.WriteString("- ")
		sb.WriteString(ex)
		sb.WriteString("\n")
	}
	return sb.String()
}

// renderFixExample returns a fenced before/after block for a rule entry.
// Returns empty string when FixExample is empty.
func renderFixExample(entry rule.RuleCatalogueEntry) string {
	if entry.FixExample == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("```\n")
	sb.WriteString(entry.FixExample)
	sb.WriteString("\n```\n")
	return sb.String()
}

// renderConfigurable returns the "Configurable:" block for a rule. When only
// severity is configurable it emits a compact inline form; otherwise it emits
// a bullet list.
func renderConfigurable(cfg rule.RuleConfig) string {
	var params []string

	if cfg.MinWidth > 0 && cfg.MinHeight > 0 {
		params = append(params, fmt.Sprintf("Minimum resolution (default %d &times; %d px)", cfg.MinWidth, cfg.MinHeight))
	} else if cfg.MinWidth > 0 {
		params = append(params, fmt.Sprintf("Minimum width (default %d px)", cfg.MinWidth))
	} else if cfg.MinHeight > 0 {
		params = append(params, fmt.Sprintf("Minimum height (default %d px)", cfg.MinHeight))
	}

	if cfg.AspectRatio > 0 {
		params = append(params, fmt.Sprintf("Aspect ratio (default %.4g, tolerance &plusmn;%.0f%%)", cfg.AspectRatio, cfg.Tolerance*100))
	} else if cfg.Tolerance > 0 {
		params = append(params, fmt.Sprintf("Tolerance (default %.2f)", cfg.Tolerance))
	}

	if cfg.MinLength > 0 {
		params = append(params, fmt.Sprintf("Minimum biography length (default %d characters)", cfg.MinLength))
	}

	if cfg.ThresholdPercent > 0 {
		params = append(params, fmt.Sprintf("Padding threshold (default %.0f%% of image area)", cfg.ThresholdPercent))
		params = append(params, fmt.Sprintf("Trim margin (default %d px)", cfg.TrimMargin))
	}

	if cfg.MinCount > 0 {
		params = append(params, fmt.Sprintf("Minimum backdrop count (default %d)", cfg.MinCount))
	}

	if cfg.ArticleMode != "" {
		params = append(params, fmt.Sprintf("Article handling (default: %s)", cfg.ArticleMode))
	}

	if cfg.SelectBestCandidate {
		params = append(params, "Auto-select best candidate")
	}

	if len(params) == 0 {
		return "**Configurable:** Severity only."
	}

	var sb strings.Builder
	sb.WriteString("**Configurable:**\n\n")
	for _, p := range params {
		sb.WriteString("- ")
		sb.WriteString(p)
		sb.WriteString("\n")
	}
	sb.WriteString("- Severity (default: ")
	sb.WriteString(cfg.Severity)
	sb.WriteString(")")
	return sb.String()
}

// renderCatalogue returns the full Markdown body to place between the
// BEGIN/END markers. Rules are grouped in categoryOrder and within each
// group ordered by their position in rules (registration order).
//
//nolint:gocognit // Markdown generator: emits per-category section headers and per-rule subsections with field-conditional rendering (description, severity, automation, frontmatter); splitting the conditionals would scatter the Markdown structure across helpers and obscure the rendered output's shape.
func renderCatalogue(rules []rule.Rule) string {
	// Partition rules by category, preserving registration order within each group.
	byCategory := make(map[rule.RuleCategory][]rule.Rule)
	for _, r := range rules {
		byCategory[r.Category] = append(byCategory[r.Category], r)
	}

	var b strings.Builder

	// --- "At a glance" table ---
	b.WriteString("## At a glance\n\n")
	b.WriteString("| Rule | Category | Default | Fixable |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, cat := range categoryOrder {
		for _, r := range byCategory[cat] {
			entry := rule.CatalogueEntry(r.ID)
			fixable := "Detection-only"
			if entry.FixBehavior != "" {
				fixable = "Yes"
				if entry.Conditional {
					fixable = "Sometimes"
				}
			}
			fmt.Fprintf(&b, "| [%s](#%s) | %s | %s | %s |\n",
				r.Name,
				nameToAnchor(r.Name),
				categoryDisplayName(cat),
				defaultState(r),
				fixable,
			)
		}
	}
	b.WriteString("\n")
	b.WriteString("A rule marked **Detection-only** has no automated fix; you resolve the violations manually (or by adding artwork that satisfies the check).\n")

	// --- Per-rule sections ---
	for _, cat := range categoryOrder {
		for _, r := range byCategory[cat] {
			entry := rule.CatalogueEntry(r.ID)

			b.WriteString("\n---\n\n")
			b.WriteString("## ")
			b.WriteString(r.Name)
			b.WriteString("\n\n")

			// Attribute line
			b.WriteString("**Category:** ")
			b.WriteString(categoryDisplayName(cat))
			b.WriteString(" &middot; **Default:** ")
			b.WriteString(defaultState(r))
			b.WriteString(" &middot; **Severity:** ")
			b.WriteString(r.Config.Severity)
			if r.FilesystemDependent {
				b.WriteString(" &middot; **Filesystem-dependent:** Yes")
			}
			b.WriteString("\n\n")

			// Description
			b.WriteString(r.Description)
			b.WriteString("\n\n")

			// Guards paragraph (2-4 sentences expanding on the description)
			if entry.Guards != "" {
				b.WriteString(entry.Guards)
				b.WriteString("\n\n")
			}

			// When this fires examples
			if wf := renderWhenFires(entry); wf != "" {
				b.WriteString(wf)
				b.WriteString("\n")
			}

			// Fix block
			if entry.FixBehavior != "" {
				b.WriteString("**What the fix does:** ")
				b.WriteString(entry.FixBehavior)
				b.WriteString("\n\n")
				// FixExample before/after block
				if fe := renderFixExample(entry); fe != "" {
					b.WriteString(fe)
					b.WriteString("\n")
				}
			} else {
				b.WriteString("**Fix:** No automated fix.\n\n")
			}

			// Configurable block
			b.WriteString(renderConfigurable(r.Config))
			b.WriteString("\n")

			// Caveats block
			if len(entry.Caveats) > 0 {
				b.WriteString("\n**Caveats:**\n\n")
				for _, c := range entry.Caveats {
					b.WriteString("- ")
					b.WriteString(c)
					b.WriteString("\n")
				}
			}
		}
	}

	return b.String()
}

// replaceBetweenMarkers returns src with the region between begin and end
// replaced by body. The markers themselves are preserved. body is sandwiched
// in newlines so the rendered content has clean blank-line separation from the
// markers; trailing newlines on body are normalized.
//
// begin and end are parameters (not constants) so the helper can be exercised
// with bespoke markers in tests.
func replaceBetweenMarkers(src []byte, begin, end, body string) ([]byte, error) { //nolint:unparam // begin/end are exposed as parameters for testability
	beginIdx := bytes.Index(src, []byte(begin))
	if beginIdx < 0 {
		return nil, fmt.Errorf("begin marker %q not found", begin)
	}
	// Search for the end marker only after the begin marker so an incidental
	// occurrence of the end marker text earlier in the file (for example, in a
	// fenced code block illustrating the convention) cannot be mistaken for
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
